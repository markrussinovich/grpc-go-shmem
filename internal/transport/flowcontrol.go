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

// trInFlow is the connection-level inbound flow controller. It follows
// the standard HTTP/2 (limit, unacked) book-keeping: bytes received via
// onData accumulate in `unacked`; when unacked crosses the `limit/4`
// drip threshold a WINDOW_UPDATE for the accumulated amount is emitted
// and unacked is reset.
//
// Stock HTTP/2 callers (http2_client.go / http2_server.go) read the WU
// value returned from onData and emit directly. The SHM transport
// emits conn-level WindowUpdate on its own batched path
// (sendConnWindowUpdate, gated by a per-transport wuThreshold) and
// does not consult onData's return value; see
// shm_client_transport.go's onDataFrameReceived for the SHM path.
type trInFlow struct {
	mu                  sync.Mutex
	limit               uint32
	unacked             uint32
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

// onData is called when an inbound DATA frame is parsed. Updates
// unacked and returns the WindowUpdate increment to emit when the
// limit/4 drip threshold is crossed (zero otherwise). Stock HTTP/2
// callers act on the return value; SHM callers track conn-level
// drip independently and pass through unacked here only for the
// effectiveWindowSize counter.
func (f *trInFlow) onData(n uint32) uint32 {
	f.mu.Lock()
	f.unacked += n
	if f.unacked < f.limit/4 {
		f.updateEffectiveWindowSizeLocked()
		f.mu.Unlock()
		return 0
	}
	r := f.resetLocked()
	f.mu.Unlock()
	return r
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
	if f.limit > f.unacked {
		atomic.StoreUint32(&f.effectiveWindowSize, f.limit-f.unacked)
	} else {
		atomic.StoreUint32(&f.effectiveWindowSize, 0)
	}
}

func (f *trInFlow) getSize() uint32 {
	return atomic.LoadUint32(&f.effectiveWindowSize)
}

// snapshot returns a copy of the conn-level inflow state for
// diagnostics. Locks briefly; safe to call from anywhere.
func (f *trInFlow) snapshot() (limit, unacked, effective uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.limit, f.unacked, atomic.LoadUint32(&f.effectiveWindowSize)
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

// snapshot returns a copy of the inflow state for diagnostics.
// Locks briefly; not safe to call from atomic write paths.
func (f *inFlow) snapshot() (limit, pendingData, pendingUpdate, delta uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.limit, f.pendingData, f.pendingUpdate, f.delta
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

// maybeAdjustAdditive is the SHM variant of maybeAdjust used by the
// codec-driven pre-credit path (onMessageStart). Unlike maybeAdjust,
// which assumes one active application read at a time and SETs
// f.delta = n (stock HTTP/2 semantics), this method ADDs the
// incremental credit needed to admit a new in-flight LPM of size n
// on top of any outstanding pre-credit debt already present in
// f.delta.
//
// Motivation: the SHM codec assembles each LPM at the transport
// layer before the application sees it, so receiver-driven
// pre-credit fires per LPM at parse time (not per-app-Read as in
// stock HTTP/2). When two large LPMs are pipelined on a single
// stream and the application has not yet consumed the first, the
// pre-credit hook fires for the second LPM while f.pendingData is
// still inflated by the first. A bare maybeAdjust call would
// OVERWRITE f.delta with the second LPM's value, losing the
// previously-emitted credit and causing onData to falsely trip
// FLOW_CONTROL_ERROR on the second LPM's incoming DATA bytes.
//
// The additive variant computes the additional credit needed for
// the new LPM (cap: maxWindowSize - limit - delta) and accumulates
// it into f.delta. f.delta is drained by onRead as the application
// consumes bytes (existing behaviour), so the credit ledger stays
// balanced.
//
// Returns the additional credit (bytes) to emit as a stream-level
// WINDOW_UPDATE, or 0 if existing capacity already admits the LPM
// OR backpressure should fire (receiver already buffering too much
// pending data — sender MUST wait for the app to drain).
//
// Backpressure cap: we will not pre-credit beyond what would leave
// the total receiver-buffered bytes (pendingData + pendingUpdate +
// this LPM's `n`) exceeding `n + limit` — that is, "1 LPM in
// flight + 1 stream-window's worth of slack". When an app stops
// draining, pendingData accumulates; once it reaches the cap, this
// function returns 0, no WU is emitted, the sender's send-quota is
// not replenished, and Write parks correctly (HTTP/2-correct
// backpressure semantics).
//
// Without this cap, a slow-reading app on an unbounded-Send client
// would let delta grow to ~2 GiB (maxWindowSize) before saturation;
// at saturation onData's `pendingData + n > limit + delta` check
// trips and the server cancels the stream with
// "received N-bytes data exceeding the limit M bytes" — exactly
// the bug demo agents have observed under client-streaming with
// `response_size=0` and `payload >= window`.
func (f *inFlow) maybeAdjustAdditive(n uint32) uint32 {
	if n > uint32(math.MaxInt32) {
		n = uint32(math.MaxInt32)
	}
	f.mu.Lock()
	// Backpressure gate: refuse pre-credit if total outstanding
	// receiver-buffered bytes (pendingData + pendingUpdate) already
	// exceeds f.limit. The +n on both sides of an earlier formulation
	// cancelled algebraically; the cap is just "buffered <= limit", i.e.
	// "1 window's worth of slack can sit ahead of consumption before a
	// pending LPM is refused". Matches HTTP/2's "1 message in transit,
	// 1 ready to read" depth model — the new LPM (n bytes) can be
	// admitted only when the buffer has not yet absorbed a full window
	// of unread bytes.
	if int64(f.pendingData)+int64(f.pendingUpdate) > int64(f.limit) {
		f.mu.Unlock()
		return 0
	}
	// avail is the remaining receive capacity within current
	// enforcement bounds: limit + delta - (pendingData + pendingUpdate).
	avail := int64(f.limit) + int64(f.delta) - int64(f.pendingData) - int64(f.pendingUpdate)
	need := int64(n) - avail
	if need <= 0 {
		f.mu.Unlock()
		return 0
	}
	// Cap so f.limit + f.delta does not exceed HTTP/2 31-bit window.
	headroom := int64(maxWindowSize) - int64(f.limit) - int64(f.delta)
	if need > headroom {
		need = headroom
	}
	if need <= 0 {
		f.mu.Unlock()
		return 0
	}
	f.delta += uint32(need)
	f.mu.Unlock()
	return uint32(need)
}

// onData is invoked when some data frame is received. It updates pendingData.
func (f *inFlow) onData(n uint32) error {
	f.mu.Lock()

	f.pendingData += n
	if f.pendingData+f.pendingUpdate > f.limit+f.delta {
		limit := f.limit
		rcvd := f.pendingData + f.pendingUpdate
		f.mu.Unlock()
		return fmt.Errorf("received %d-bytes data exceeding the limit %d bytes", rcvd, limit)
	}
	f.mu.Unlock()
	return nil
}

// onRead is invoked when the application reads the data. It returns the window size
// to be sent to the peer.
func (f *inFlow) onRead(n uint32) uint32 {
	f.mu.Lock()

	if f.pendingData == 0 {
		f.mu.Unlock()
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
		f.mu.Unlock()
		return wu
	}
	f.mu.Unlock()
	return 0
}
