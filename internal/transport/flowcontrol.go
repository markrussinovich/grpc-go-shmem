/*
 *
 * Copyright 2014 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package transport

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
)

// writeQuota is a soft limit on the amount of data a stream can
// schedule before some of it is written out.
type writeQuota struct {
	_ noCopy
	// get waits on read from when quota goes less than or equal to zero.
	// replenish writes on it when quota goes positive again.
	ch chan struct{}
	// done is triggered in error case.
	done <-chan struct{}
	// replenish is called by loopyWriter to give quota back to.
	// It is implemented as a field so that it can be updated
	// by tests.
	replenish func(n int)
	quota     int32
}

// init allows a writeQuota to be initialized in-place, which is useful for
// resetting a buffer or for avoiding a heap allocation when the buffer is
// embedded in another struct.
func (w *writeQuota) init(sz int32, done <-chan struct{}) {
	w.quota = sz
	w.ch = make(chan struct{}, 1)
	w.done = done
	w.replenish = w.realReplenish
}

func (w *writeQuota) get(sz int32) error {
	for {
		if atomic.LoadInt32(&w.quota) > 0 {
			atomic.AddInt32(&w.quota, -sz)
			return nil
		}
		select {
		case <-w.ch:
			continue
		case <-w.done:
			return errStreamDone
		}
	}
}

func (w *writeQuota) realReplenish(n int) {
	sz := int32(n)
	newQuota := atomic.AddInt32(&w.quota, sz)
	previousQuota := newQuota - sz
	if previousQuota <= 0 && newQuota > 0 {
		select {
		case w.ch <- struct{}{}:
		default:
		}
	}
}

// trInFlow is the connection-level inbound flow controller. In addition
// to the standard HTTP/2 (limit, unacked) book-keeping it also carries a
// `delta` field that records WINDOW_UPDATE credit promised to the peer
// by maybeAdjust but not yet repaid by inbound DATA. This mirrors the
// stream-level inFlow.delta mechanism and lets the SHM receiver
// pre-credit the sender for an entire LPM at parse-time, so a large
// message larger than the fair conn window does not stall the sender
// for one round-trip per `limit/4` bytes.
//
// Invariants (all reads / writes under `mu`):
//
//	estimatedPeerConnQuota = limit + delta - unacked
//
// The estimated peer quota is what the peer believes its conn window
// is, summing the baseline limit plus every WU we have emitted minus
// the bytes we have observed in DATA. maybeAdjust(n) ensures
// estimatedPeerConnQuota >= n by emitting a WINDOW_UPDATE for the
// shortfall. The emitted increment is recorded in delta so that
// subsequent onData(n) calls first repay this debt before counting
// towards the ordinary unacked tally; without this, the same bytes
// would later trigger a second WindowUpdate via the limit/4 threshold
// path and silently inflate the peer's conn window indefinitely.
type trInFlow struct {
	mu                  sync.Mutex
	limit               uint32
	unacked             uint32
	delta               uint32
	effectiveWindowSize uint32
}

// newLimit updates the baseline conn-level limit (e.g. on BDP-driven
// window growth). Returns the delta that must be advertised to the
// peer as a WINDOW_UPDATE.
func (f *trInFlow) newLimit(n uint32) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := n - f.limit
	f.limit = n
	f.updateEffectiveWindowSizeLocked()
	return d
}

// maybeAdjust returns the conn-level WINDOW_UPDATE delta to emit when
// the receiver expects an inbound message of n bytes that does not fit
// within the peer's current conn quota. Caller is expected to emit
// the returned increment as a streamID=0 WINDOW_UPDATE BEFORE the
// inbound DATA can drain the peer's window past zero, and the
// emission MUST bypass the WindowUpdate batching threshold (this is
// pre-credit needed RIGHT NOW, not drip credit).
//
// Algorithm (all in uint64 to avoid wrap near 2 GiB):
//
//   - est = limit + delta - unacked          (peer's view of conn quota)
//   - if n <= est: return 0 (peer already has enough conn quota)
//   - needed = n - est, capped at maxWindowSize - est so we never push
//     the peer's view of conn quota past the HTTP/2 31-bit ceiling
//   - delta += needed
//
// The returned value is also added to delta; subsequent onData(n)
// repays this debt first (see comment on trInFlow).
func (f *trInFlow) maybeAdjust(n uint32) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	est := uint64(f.limit) + uint64(f.delta) - uint64(f.unacked)
	if uint64(n) <= est {
		return 0
	}
	needed := uint64(n) - est
	room := uint64(maxWindowSize) - est
	if needed > room {
		needed = room
	}
	if needed == 0 {
		return 0
	}
	f.delta = uint32(uint64(f.delta) + needed)
	f.updateEffectiveWindowSizeLocked()
	return uint32(needed)
}

// onData is called when an inbound DATA frame is parsed. Stock
// HTTP/2 conn-level path (http2_client.go / http2_server.go) uses
// the result to decide when to emit a WindowUpdate based on the
// limit/4 drip threshold. SHM does NOT call this on the data path
// anymore (it would conflict with the SHM-specific wuThreshold
// batching). SHM instead uses settleDebt + sendWindowUpdate.
//
// For TCP/UDS callers this method still observes the limit/4
// threshold and returns the WU increment to emit. Pre-credit debt
// (set by maybeAdjust on the SHM path) is repaid here for safety —
// it should always be zero in TCP/UDS deployment since no caller
// invokes maybeAdjust on a trInFlow used by the TCP/UDS path.
func (f *trInFlow) onData(n uint32) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delta > 0 {
		c := f.delta
		if c > n {
			c = n
		}
		f.delta -= c
		n -= c
	}
	if n == 0 {
		f.updateEffectiveWindowSizeLocked()
		return 0
	}
	f.unacked += n
	if f.unacked < f.limit/4 {
		f.updateEffectiveWindowSizeLocked()
		return 0
	}
	return f.resetLocked()
}

// settleDebt repays outstanding pre-credit debt with the n bytes
// just received in an inbound DATA frame and returns the bytes that
// were NOT absorbed by debt. The SHM transport routes those
// residual bytes into its own batched WindowUpdate emission path
// (sendWindowUpdate, gated by per-transport wuThreshold rather
// than limit/4).
//
// This split exists because the SHM transport batches WU emission
// based on a SHM-tuned threshold that can differ from the conn-level
// inFlow's limit/4 (e.g. when the user clamps the stream window via
// grpc.WithInitialWindowSize but leaves the conn limit at the
// default maxWindowSize). Routing SHM through trInFlow.onData would
// either never emit (limit/4 of maxWindowSize is ~512 MiB) or emit
// at the wrong granularity. settleDebt isolates the debt-repayment
// concern so SHM can keep its own emission cadence.
func (f *trInFlow) settleDebt(n uint32) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delta > 0 {
		c := f.delta
		if c > n {
			c = n
		}
		f.delta -= c
		n -= c
	}
	f.updateEffectiveWindowSizeLocked()
	return n
}

// reset returns the current unacked bytes and zeroes the counter.
// Public method kept for callers that already hold equivalent
// invariants externally (none currently). Internal implementation
// uses resetLocked.
func (f *trInFlow) reset() uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resetLocked()
}

func (f *trInFlow) resetLocked() uint32 {
	w := f.unacked
	f.unacked = 0
	f.updateEffectiveWindowSizeLocked()
	return w
}

func (f *trInFlow) updateEffectiveWindowSize() {
	f.mu.Lock()
	f.updateEffectiveWindowSizeLocked()
	f.mu.Unlock()
}

func (f *trInFlow) updateEffectiveWindowSizeLocked() {
	// Saturating subtraction in uint64 to avoid underflow when delta
	// is briefly larger than what limit-unacked represents (does not
	// happen with current callers, but kept defensive).
	available := uint64(f.limit) + uint64(f.delta)
	if available > uint64(f.unacked) {
		available -= uint64(f.unacked)
	} else {
		available = 0
	}
	if available > uint64(maxWindowSize) {
		available = uint64(maxWindowSize)
	}
	atomic.StoreUint32(&f.effectiveWindowSize, uint32(available))
}

func (f *trInFlow) getSize() uint32 {
	return atomic.LoadUint32(&f.effectiveWindowSize)
}

// inFlow deals with inbound flow control
type inFlow struct {
	mu sync.Mutex
	// The inbound flow control limit for pending data.
	limit uint32
	// pendingData is the overall data which have been received but not been
	// consumed by applications.
	pendingData uint32
	// The amount of data the application has consumed but grpc has not sent
	// window update for them. Used to reduce window update frequency.
	pendingUpdate uint32
	// delta is the extra window update given by receiver when an application
	// is reading data bigger in size than the inFlow limit.
	delta uint32
}

// newLimit updates the inflow window to a new value n.
// It assumes that n is always greater than the old limit.
func (f *inFlow) newLimit(n uint32) {
	f.mu.Lock()
	f.limit = n
	f.mu.Unlock()
}

func (f *inFlow) maybeAdjust(n uint32) uint32 {
	if n > uint32(math.MaxInt32) {
		n = uint32(math.MaxInt32)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// estSenderQuota is the receiver's view of the maximum number of bytes the sender
	// can send without a window update.
	estSenderQuota := int32(f.limit - (f.pendingData + f.pendingUpdate))
	// estUntransmittedData is the maximum number of bytes the sends might not have put
	// on the wire yet. A value of 0 or less means that we have already received all or
	// more bytes than the application is requesting to read.
	estUntransmittedData := int32(n - f.pendingData) // Casting into int32 since it could be negative.
	// This implies that unless we send a window update, the sender won't be able to send all the bytes
	// for this message. Therefore we must send an update over the limit since there's an active read
	// request from the application.
	if estUntransmittedData > estSenderQuota {
		// Sender's window shouldn't go more than 2^31 - 1 as specified in the HTTP spec.
		if f.limit+n > maxWindowSize {
			f.delta = maxWindowSize - f.limit
		} else {
			// Send a window update for the whole message and not just the difference between
			// estUntransmittedData and estSenderQuota. This will be helpful in case the message
			// is padded; We will fallback on the current available window(at least a 1/4th of the limit).
			f.delta = n
		}
		return f.delta
	}
	return 0
}

// onData is invoked when some data frame is received. It updates pendingData.
func (f *inFlow) onData(n uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.pendingData += n
	if f.pendingData+f.pendingUpdate > f.limit+f.delta {
		limit := f.limit
		rcvd := f.pendingData + f.pendingUpdate
		return fmt.Errorf("received %d-bytes data exceeding the limit %d bytes", rcvd, limit)
	}
	return nil
}

// onRead is invoked when the application reads the data. It returns the window size
// to be sent to the peer.
func (f *inFlow) onRead(n uint32) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.pendingData == 0 {
		return 0
	}
	f.pendingData -= n
	if n > f.delta {
		n -= f.delta
		f.delta = 0
	} else {
		f.delta -= n
		n = 0
	}
	f.pendingUpdate += n
	if f.pendingUpdate >= f.limit/4 {
		wu := f.pendingUpdate
		f.pendingUpdate = 0
		return wu
	}
	return 0
}
