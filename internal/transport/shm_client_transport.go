//go:build linux || windows

/*
 *
 * Copyright 2025 gRPC authors.
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
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/internal/grpcutil"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

// clientStreamCache holds a cached stream pointer and its ID for lock-free
// lookup in the frame dispatch hot path. Loaded/stored atomically.
type clientStreamCache struct {
	stream   *ClientStream
	streamID uint32
}

// ShmClientTransport implements the gRPC ClientTransport interface
// for shared memory communication.
type ShmClientTransport struct {
	// Core state
	segment        *Segment // The shared memory segment
	clientToServer *ShmRing // Ring for client->server data
	serverToClient *ShmRing // Ring for server->client data
	segmentName    string   // Segment identifier for cleanup

	// Windows event handles for cross-mapping synchronization
	readEvents  *RingEvents
	writeEvents *RingEvents

	// Connection state
	localAddr  net.Addr
	remoteAddr net.Addr

	// Lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	closed atomic.Bool
	// draining indicates GracefulClose or server GOAWAY has been initiated.
	// When draining, NewStream must fail and the transport should close once all
	// active streams finish.
	draining atomic.Bool
	// frameWriter serializes writes to the client->server ring via a dedicated
	// goroutine, eliminating races between concurrent stream writers.
	frameWriter *shmFrameWriter
	mu          sync.RWMutex

	// Stream management
	streams  map[uint32]*ClientStream
	streamID uint32 // next stream ID to assign

	// cachedStream caches the only active stream for single-stream connections,
	// allowing frame dispatch to skip the map lookup + RLock.
	//
	// Loaded atomically without t.mu in the frame dispatch hot path.
	// Stored atomically under t.mu when the stream set changes.
	cachedStream atomic.Pointer[clientStreamCache]

	// singleStreamMode is negotiated via the CONNECT frame. When true,
	// the client requested single-stream optimizations and the transport
	// uses inline writes via inlineMu.TryLock and cachedStream fast path.
	singleStreamMode bool

	// Flow control (outbound send windows)
	//
	// connSendQuota and the per-stream Stream.sendQuota field are
	// atomic.Int64. writeProto's inline ZC path does a two-resource
	// lock-free CAS reservation; on CAS-fail or TryLock-fail the
	// sender enqueues a fire-and-forget proto entry to the writer
	// chan and the writer goroutine handles the CAS reservation +
	// defer-on-FC-stall symmetrically with the chunked whole-message
	// path. addSendQuota credits via a single atomic.Add and pings
	// wuRetryWake so the writer revisits deferred entries.
	//
	// The legacy slow path (sendQuotaMu + connWaiters FIFO +
	// streamQuotaSignals per-stream chans + register/unregister +
	// SC-Aware Dispatch) was retired in favour of this async-on-CAS-
	// fail design. Net effect on fair-default 1000-stream workloads:
	// no transport-wide mutex on every WU arrival; no sender park
	// on per-stream signal channels; no thundering-herd wake. The
	// writer's existing deferred-map machinery (originally for the
	// chunked whole-message path) now also services ZC proto entries
	// via shmFrameWriter.deferredProto.
	connSendQuota atomic.Int64
	streamInFlow          map[uint32]*inFlow
	connInFlow            trInFlow
	maxConcurrentStreams  uint32
	streamQuota           int64
	streamsQuotaAvailable chan struct{}
	waitingStreams        uint32

	// BDP estimation and dynamic flow control (RFC A73 Phase 5)
	bdpEst            *shmBDPEstimator
	initialWindowSize int32

	// initialStreamWindow is the per-stream send-quota value applied
	// to each new stream's Stream.sendQuota at NewStream time. When 0
	// (default), the transport falls back to t.initialWindowSize
	// (set to shmInitialWindowSize = 32 MiB at construction, or to
	// the value supplied by DialOptions.InitialWindowSize when the
	// user passes grpc.WithInitialWindowSize). The chunked write
	// path in the frame-writer goroutine (advanceDeferred CAS-deduct
	// + per-grant emit) enforces the window symmetrically with the
	// receiver's inFlow.limit.
	initialStreamWindow int64

	// wuThreshold is the per-transport WindowUpdate emission threshold,
	// captured at construction from shmWindowUpdateThreshold and
	// recomputed whenever t.initialWindowSize changes (DialOptions
	// override + BDP estimator updates). Previously sendWindowUpdate
	// read the package-global shmWindowUpdateThreshold directly,
	// which caused a real deadlock when a transport was dialed with
	// grpc.WithInitialWindowSize(65535) while the package global
	// was still the 8 MiB shm-tuned default: sender exhausted the
	// 64 KiB window and parked, receiver's onRead accumulated
	// 16 KiB credits per app read, sendWindowUpdate batched them but
	// never reached 8 MiB so no WU was ever emitted. computeWUThreshold
	// rebinds this to limit/4 of the transport's actual effective
	// window so the receiver's onRead drip credit triggers WU emission
	// in time for the sender to unblock.
	//
	// Atomic so the WindowUpdate emission path (emitWindowUpdate) can
	// read it without taking sendQuotaMu — see the "WU Lockless Path"
	// design that decoupled WU emission from outbound send-quota
	// state. updateFlowControl (BDP / DialOption) is the sole writer.
	wuThreshold atomic.Uint32

	// pendingConnWU is the connection-level WindowUpdate credit
	// accumulator. Producers (onDataFrameReceived drip-on-receive,
	// BDP updateFlowControl) Add(delta); the emission path Swap(0)
	// to drain and emits a single streamID=0 WINDOW_UPDATE for the
	// swept value. The per-stream equivalent lives on
	// Stream.pendingWU. Together they replace the legacy
	// `pendingStreamWU map[uint32]uint32` and the `pendingConnWU
	// uint32` previously guarded by sendQuotaMu, so the WU hot path
	// no longer contends with 1000 stream goroutines reserving
	// outbound send quota.
	pendingConnWU atomic.Uint32

	// wuDirty is the per-stream restore-WU dirty list, used by
	// drainPendingWUForWriter to skip the legacy O(N=streams) walk.
	// Two-slot ping-pong design (Opus 4.8 review):
	//   - wuDirty[wuLiveIdx] is the LIVE slice that producers
	//     append into (under wuDirtyMu) when their stream-WU emit
	//     hit errFrameWriterFull and CAS-set Stream.pendingWUDirty.
	//   - wuDirty[wuLiveIdx^1] is the SPARE slice (pre-truncated
	//     to [:0]) that the drainer rotates into the live slot
	//     while it processes the ex-live snapshot.
	// Two backing arrays, amortised zero allocation per drain, NO
	// aliasing data race (drainer's snapshot and producer's
	// concurrent appends touch physically distinct arrays).
	//
	// Simple `wuDirtyList = nil` would force a fresh alloc per
	// drain; `wuDirtyList = wuDirtyList[:0]` would alias with the
	// drainer's snapshot and corrupt under concurrent producer
	// append. Ping-pong is the only zero-alloc design that is also
	// race-free.
	//
	// wuBuf is a 4-byte scratch for WINDOW_UPDATE frame payload,
	// safe to share across drain calls because drainPendingWUForWriter
	// runs under frameWriter.inlineMu (single-writer serialisation).
	wuDirtyMu sync.Mutex
	wuDirty   [2][]*ClientStream
	wuLiveIdx int
	wuBuf     [4]byte

	// Error handling
	closeOnce sync.Once
	errCh     chan struct{}
	goAwayCh  chan struct{}

	goAwayOnce         sync.Once
	goAwayReason       GoAwayReason
	goAwayDebugMessage string

	readerWG sync.WaitGroup

	// Keepalive
	lastRead         int64 // Unix nanos; updated atomically on each received frame
	kp               keepalive.ClientParameters
	keepaliveEnabled bool
	keepaliveDone    chan struct{} // closed when keepalive goroutine exits
	// kpDormancyCond signals the keepalive goroutine to exit dormant state.
	// Guarded by mu.
	kpDormancyCond *sync.Cond
	kpDormant      bool

	// onClose is a callback invoked when the transport is closed.
	// This is used by ClientConn/addrConn to track connectivity state.
	// RFC A73: Required for proper subchannel lifecycle management.
	onClose func(GoAwayInfo)

	// authInfo stores the authentication information from security handshake.
	authInfo credentials.AuthInfo
}

func (t *ShmClientTransport) setGoAwayReason(flags uint8, debug string) {
	t.goAwayOnce.Do(func() {
		// shm GOAWAY frames do not carry an HTTP/2 error code or debug data.
		// Mirror the http2 client default when a GOAWAY is received.
		t.goAwayReason = GoAwayNoReason
		if debug == "" {
			if flags&GoAwayFlagIMMEDIATE != 0 {
				t.goAwayDebugMessage = "received GOAWAY (immediate)"
				return
			}
			t.goAwayDebugMessage = "received GOAWAY (draining)"
			return
		}
		// Prefer peer-provided debug string when present.
		t.goAwayDebugMessage = debug
	})
}

// connWaiter is a snapshot of a stream's wait state used inside the
// transport's connWaiters FIFO list. Created when a sender registers
// for conn-quota wait, removed (and the per-stream signal channel
// kicked) when the SC reader's dispatch loop selects this waiter as
// satisfiable. The corresponding ClientStream.connWaiterElem holds
// the back-pointer for O(1) unlink without scanning the list.
//
// `wanted` is the minimum number of conn-quota bytes the waiter needs
// to be woken. acquireSendQuota registers with wanted=n (atomic
// exact-fit reservation for the zero-copy single-frame proto path).
// addSendQuota credits outbound send-quota and wakes the writer
// goroutine to retry any deferred entries that may now have
// sufficient credit.
//
// The legacy slow path — sendQuotaMu + connWaiters FIFO +
// streamQuotaSignals + per-sender park on a per-stream chan — was
// retired in favour of the async-on-CAS-fail design. The writer
// goroutine now owns FC reservation + defer + retry symmetrically
// for both the whole-message chunked path (advanceDeferred) and the
// ZC proto fast path (processProtoEntry / retryDeferredProto). On a
// WU arrival we just credit the atomic counter and ping wuRetryWake;
// writeLoop's drain pass observes the wake, calls retryDeferred,
// which makes progress on every deferred entry whose CAS now
// succeeds.
//
// This eliminates the thundering-herd wake on fair-default 1000-
// stream workloads (no FIFO walk under a transport-wide mutex on
// every WU arrival) AND eliminates the sender-side park entirely
// (writeProto's CAS-fail or TryLock-fail path now enqueues
// fire-and-forget to the writer chan and returns success).
func (t *ShmClientTransport) addSendQuota(streamID uint32, delta uint32) {
	if delta == 0 {
		return
	}
	if streamID == 0 {
		t.connSendQuota.Add(int64(delta))
	} else {
		// Stream-level credit: the Stream may already be closed
		// (closeStream removed it from t.streams); if so the credit
		// lands on a detached Stream value and never reaches a
		// goroutine — that's correct (no waiter to wake either).
		if s := t.lookupStream(streamID); s != nil {
			s.sendQuota.Add(int64(delta))
		} else {
			return
		}
	}
	// Signal the writer goroutine to revisit any deferred entries
	// (whole-message AND ZC proto) that may now be satisfiable. The
	// wake channel is buffer=1 with non-blocking send: multiple WU
	// arrivals coalesce into one wake which causes writeLoop to walk
	// both deferred maps under inlineMu (level-triggered semantics).
	select {
	case t.frameWriter.wuRetryWake <- struct{}{}:
	default:
	}
}

// tryReserveSendQuota attempts a single lock-free two-resource CAS
// reservation of `n` bytes from both the connection and per-stream
// send quotas. Returns true on success; false if either quota is
// insufficient OR a concurrent CAS won the race (caller should
// retry / defer as appropriate). On false, no quota is held.
//
// The rollback path runs when the stream CAS succeeded but the conn
// CAS failed — we Add(n) back to the stream quota. This may
// transiently push the stream quota above its current "ceiling" if
// a concurrent addSendQuota also incremented in the same window;
// HTTP/2 semantics permit this (the receiver's actual limit is
// tracked separately via inFlow, and outbound quota is bounded only
// by the protocol max of 2^31-1 which addSendQuota never approaches).
func tryReserveSendQuota(connQuota, streamQuota *atomic.Int64, n int64) bool {
	streamQ := streamQuota.Load()
	if streamQ < n {
		return false
	}
	connQ := connQuota.Load()
	if connQ < n {
		return false
	}
	if !streamQuota.CompareAndSwap(streamQ, streamQ-n) {
		return false
	}
	if !connQuota.CompareAndSwap(connQ, connQ-n) {
		// Conn CAS lost the race — restore stream quota.
		streamQuota.Add(n)
		shmCASRollback.Add(1)
		return false
	}
	return true
}

// acquireUpToSendQuota / tryReserveUpToSendQuota were retired when
// chunked-DATA flow-control state ownership moved into the writer
// goroutine. The current path is shmFrameWriter.enqueueMessageAndWait
// → advanceDeferred (CAS-deduct + emit + defer-on-FC-stall under
// inlineMu); the chunked acquire/park primitives are no longer needed.
//
// acquireSendQuota was retired in favour of the async-on-CAS-fail
// path; see writeProto for the new flow.

func (t *ShmClientTransport) sendWindowUpdate(streamID uint32, delta uint32) {
	if streamID == 0 {
		t.sendConnWindowUpdate(delta, false)
		return
	}
	s := t.lookupStream(streamID)
	if s == nil {
		return
	}
	t.sendStreamWindowUpdate(s, delta, false)
}

// sendWindowUpdateForce emits a stream-level WINDOW_UPDATE bypassing
// the shmWindowUpdateThreshold drip-credit batching. Required for
// stream-level maybeAdjust-style pre-credit (onMessageStart): the
// LPM cannot complete until the peer's stream window is large
// enough for the announced message size, so the WU MUST go out
// immediately, not be buffered until the next 16 KiB worth of
// inbound bytes have accumulated. Drip credit driven by
// inFlow.onRead / trInFlow.onData continues to use the batched
// path via sendWindowUpdate so per-DATA-frame chatter stays at
// HTTP/2 limit/4 cadence.
//
// Conn-level pre-credit is not used by this transport; if a future
// caller needs to force-emit a conn WU, route through
// sendConnWindowUpdate(delta, true) directly.
func (t *ShmClientTransport) sendWindowUpdateForce(streamID uint32, delta uint32) {
	s := t.lookupStream(streamID)
	if s == nil {
		return
	}
	t.sendStreamWindowUpdate(s, delta, true)
}

// sendConnWindowUpdate is the lockless emission path for conn-level
// WINDOW_UPDATE. The pending credit accumulator (t.pendingConnWU)
// is an atomic.Uint32, so producers (reader callbacks, BDP, drip)
// never touch sendQuotaMu — this is the central change that
// decouples the WU hot path from the 1000-way outbound send-quota
// contention pool.
//
// Algorithm (under "WU Lockless Path" design):
//
//  1. Add delta to pendingConnWU.
//  2. If !force and new value below wuThreshold: leave pending for
//     the next producer to (eventually) cross the threshold. No
//     emission this call.
//  3. Otherwise Swap(0) to claim the entire current pending sum;
//     emit one WINDOW_UPDATE frame for that sum.
//
// Concurrency correctness:
//
//   - Multiple producers can race Add+Swap. A producer that
//     observes pending >= threshold and Swap(0) returns 0 means a
//     concurrent producer already swept its (and our) bytes and
//     will emit the combined frame; bytes are not lost.
//   - The errFrameWriterFull recovery path restores `v` via Add
//     and signals wuRetryWake. The frame-writer goroutine drains
//     pending atomics on every wake, so restored credit is
//     guaranteed to be emitted within one wake cycle. This closes
//     the force-WU liveness hole that the previous mutex-based
//     restore had: previously a restored force pre-credit could
//     sit in pending indefinitely if no later producer arrived.
func (t *ShmClientTransport) sendConnWindowUpdate(delta uint32, force bool) {
	if delta == 0 || t.closed.Load() {
		return
	}
	newVal := t.pendingConnWU.Add(delta)
	if !force && newVal < t.wuThreshold.Load() {
		return
	}
	v := t.pendingConnWU.Swap(0)
	if v == 0 {
		// A concurrent producer just swept the accumulator. They will
		// emit a frame carrying our delta (plus theirs). No work to do.
		return
	}
	t.emitWindowUpdateFrame(0, v, nil)
}

// sendStreamWindowUpdate is the lockless emission path for stream-
// level WINDOW_UPDATE. The pending accumulator (s.pendingWU) lives
// on the Stream itself, eliminating the per-emission map lookup and
// the sendQuotaMu critical section. Caller MUST pass the live
// stream pointer (typically already in hand at the call site).
//
// Stream lifecycle handling: when the stream is observed closed
// (state == streamDone) at entry OR between Add/Swap, pending
// credit is dropped without emission. Stream close paths set
// s.pendingWU = 0 explicitly so a late producer's Add cannot
// resurrect already-cleared credit on a closed stream.
func (t *ShmClientTransport) sendStreamWindowUpdate(s *ClientStream, delta uint32, force bool) {
	if delta == 0 || s == nil || t.closed.Load() {
		return
	}
	if s.getState() == streamDone {
		// Drop: stream already closing. WU credit for a closed stream
		// is meaningless; the peer's window is reclaimed via close.
		return
	}
	newVal := s.pendingWU.Add(delta)
	if !force && newVal < t.wuThreshold.Load() {
		return
	}
	v := s.pendingWU.Swap(0)
	if v == 0 {
		return
	}
	if s.getState() == streamDone {
		// Raced close after our Swap. Credit is now ours; drop it.
		return
	}
	t.emitWindowUpdateFrame(s.id, v, s)
}

// emitWindowUpdateFrame writes a single WINDOW_UPDATE frame via the
// frame writer's non-blocking path. On errFrameWriterFull the
// captured value `v` is restored to the appropriate pending
// accumulator and the writer loop is signalled via wuRetryWake to
// drain pending atomics on its next tick. `s` is the stream
// pointer for stream-level frames (streamID != 0); pass nil for
// conn-level frames (streamID == 0).
//
// Liveness invariant: every Swap'd value that fails to enqueue is
// either (a) restored to the same atomic with a guaranteed retry
// signal, or (b) dropped because the stream closed. Therefore no
// WINDOW_UPDATE credit can stall indefinitely on the SHM transport
// once a value crosses Swap.
func (t *ShmClientTransport) emitWindowUpdateFrame(streamID uint32, v uint32, s *ClientStream) {
	buf := make([]byte, 4)
	// RFC 7540 §6.9.1: WINDOW_UPDATE Window Size Increment is a 31-bit
	// big-endian unsigned integer. Match the spec so the codec's
	// validate-non-zero check (which reads BigEndian) sees the
	// correct value, and so an external HTTP/2 peer parsing this
	// frame interprets the increment correctly.
	binary.BigEndian.PutUint32(buf, v)
	err := t.frameWriter.enqueueOrInlineNonBlocking(frameEntry{
		ctx:     context.Background(),
		fh:      FrameHeader{Type: FrameTypeWindowUpdate, StreamID: streamID},
		payload: buf,
	})
	if err == errFrameWriterFull {
		shmWUFramesBackpressured.Add(1)
		// Restore credit to the right accumulator and trigger a
		// guaranteed retry. The wuRetryWake channel is buffer=1;
		// a pending signal is sufficient — the writer loop drains
		// pending atomics on its next wake.
		if streamID == 0 {
			t.pendingConnWU.Add(v)
			// Conn-level: no dirty flag. drainPendingWUForWriter
			// always Swaps pendingConnWU unconditionally — one
			// atomic on every drain is cheaper than maintaining
			// a CAS-gated dirty bit. Saves the bit + a producer-
			// side atomic + an ordering surface.
		} else if s != nil && s.getState() != streamDone {
			s.pendingWU.Add(v)
			// Per-stream dirty enqueue: CAS-dedup ensures at most
			// one producer per dirty-cycle appends the stream
			// pointer to the dirty list. Order is Add(v) BEFORE
			// CAS(false→true): if the drainer's CAS-clear races
			// our CAS-set, the worst case is a redundant re-enqueue
			// (drainer's next pass Swap'ing 0); no WU is ever lost.
			// See drainPendingWUForWriter for the full ordering
			// proof.
			if s.pendingWUDirty.CompareAndSwap(false, true) {
				t.wuDirtyMu.Lock()
				t.wuDirty[t.wuLiveIdx] = append(t.wuDirty[t.wuLiveIdx], s)
				t.wuDirtyMu.Unlock()
			}
		}
		// If stream is closed, credit is dropped (the peer has no
		// further use for it; the close path emits RST/TRAILERS).
		select {
		case t.frameWriter.wuRetryWake <- struct{}{}:
		default:
		}
	}
}

// drainPendingWUForWriter is the writer-loop callback registered via
// frameWriter.setDrainPendingWUFn. Invoked under inlineMu when
// wuRetryWake fires (i.e., an earlier emitWindowUpdateFrame hit
// errFrameWriterFull and restored its captured credit + signalled
// the wake).
//
// Algorithm:
//  1. CONN-LEVEL: unconditionally Swap pendingConnWU. One atomic on
//     every drain is cheaper than maintaining a CAS-gated dirty
//     bit (Opus 4.8 review: "delete connWUDirty, always-drain").
//  2. PER-STREAM: rotate the live dirty slice into a snapshot under
//     the mutex (ping-pong with the spare slot), release the mutex,
//     then iterate the snapshot. For each stream: clear
//     pendingWUDirty 1→0 BEFORE Swap'ing pendingWU.
//
// LOST-WU PREVENTION INVARIANT (clear-dirty BEFORE Swap-pending):
//
//   Producer sequence: pendingWU.Add(v) THEN pendingWUDirty.CAS(f→t).
//   Drainer sequence: pendingWUDirty.Store(false) THEN pendingWU.Swap(0).
//
//   If we Swap'd pending FIRST then cleared dirty:
//     - Producer Adds v after our Swap (we got 0)
//     - Producer's CAS sees true (we haven't cleared) → FAILS, no
//       re-enqueue
//     - We then clear dirty
//     - State: pending=v, dirty=false, stream NOT in list → LOST WU
//
//   By clearing dirty FIRST:
//     - Producer Adds v after our clear-dirty
//     - Producer's CAS sees false (we just cleared) → SUCCEEDS,
//       re-enqueues stream
//     - We then Swap pending (get 0 or v, doesn't matter)
//     - Stream is in next drain's list → next drain picks it up
//     - Worst case: duplicate enqueue + wasted Swap of 0. NO lost WU.
//
// COMPLEXITY: O(D) where D is dirty count this cycle (usually 0-few
// at Jumbo32 since per-stream WU below threshold doesn't restore).
// Versus the previous O(N=streams) walk that fired ~15K/sec at
// 1000-stream Jumbo32 1000/4K — a 14% CPU savings on the writer's
// serial path (profile 2026-05-29).
//
// ORDERING: conn WU before per-stream WUs so the peer's conn window
// is refilled BEFORE any stream-level credit pre-credits an inbound
// LPM. This matches the wire ordering invariant the connWUCoalescer
// also enforces (flush before non-WU frames).
func (t *ShmClientTransport) drainPendingWUForWriter() {
	if t.closed.Load() {
		return
	}
	// CONN-LEVEL: unconditional Swap (no dirty gate).
	if v := t.pendingConnWU.Swap(0); v > 0 {
		binary.BigEndian.PutUint32(t.wuBuf[:], v)
		_ = writeFrame(context.Background(), t.frameWriter.tx,
			FrameHeader{Type: FrameTypeWindowUpdate, StreamID: 0}, t.wuBuf[:])
	}
	// PER-STREAM: ping-pong rotate under brief lock.
	t.wuDirtyMu.Lock()
	if len(t.wuDirty[t.wuLiveIdx]) == 0 {
		t.wuDirtyMu.Unlock()
		return
	}
	dirty := t.wuDirty[t.wuLiveIdx]
	// Rotate to the spare slot. Spare was pre-truncated to [:0]
	// either at construction or at the end of the previous drain.
	// Producers' next append will land in the spare slot (now live);
	// our snapshot `dirty` is the ex-live array, physically distinct.
	t.wuLiveIdx ^= 1
	t.wuDirtyMu.Unlock()

	for _, s := range dirty {
		// INVARIANT: clear dirty BEFORE Swap pending. See function
		// comment for the lost-WU prevention proof.
		s.pendingWUDirty.Store(false)
		if s.getState() == streamDone {
			s.pendingWU.Store(0)
			continue
		}
		if v := s.pendingWU.Swap(0); v > 0 {
			binary.BigEndian.PutUint32(t.wuBuf[:], v)
			_ = writeFrame(context.Background(), t.frameWriter.tx,
				FrameHeader{Type: FrameTypeWindowUpdate, StreamID: s.id}, t.wuBuf[:])
		}
	}

	// Recycle the snapshot array back into the spare slot for the
	// next drain. Keep the larger array if it grew (rare). Truncate
	// to [:0] so the next ping-pong rotation finds a clean spare.
	t.wuDirtyMu.Lock()
	spareIdx := t.wuLiveIdx ^ 1
	if cap(dirty) > cap(t.wuDirty[spareIdx]) {
		t.wuDirty[spareIdx] = dirty[:0]
	}
	t.wuDirtyMu.Unlock()
}

// piggybackWUForWriter is the per-chunk piggyback callback registered
// via frameWriter.setPiggybackWUFn. Called by the writer goroutine
// (advanceDeferred / processWholeMessage; fire-and-forget control
// frames via processEntry) UNDER inlineMu, just before the function
// unlocks. Drains the connection-level pending WU accumulator AND
// the just-written stream's pendingWU into additional ring writes
// that share the same SPSC writer position.
//
// LOCK ORDERING (must hold): this callback runs WITH inlineMu held
// and acquires t.mu.RLock via lookupStream's slow path (on stream-
// cache miss). The reverse order — t.mu.Lock acquired then waiting
// on inlineMu — must NEVER occur anywhere in the codebase or this
// will deadlock. Many paths take t.mu.Lock (NewStream, closeStream,
// updateFlowControl, SetAuthInfo, Close, keepalive, GracefulClose,
// etc.), but none of them call into the writer goroutine or acquire
// inlineMu while holding t.mu.Lock. The ZC fast-path acquireSendQuota
// takes inlineMu directly (without holding t.mu), and the writer
// goroutine acquires inlineMu in writeLoop (no t.mu held). Verified
// safe across both client and server transports.
//
// Performance: O(1) work — one conn-level atomic Swap, one stream
// pointer lookup (lock-free via cachedStream MRU fast path on
// single-stream connections; mu.RLock-protected map otherwise), and
// at most one per-stream atomic Swap, plus one writeFrame per
// non-zero Swap. When no WU is pending (the common case after the
// first chunk of a sustained transfer has already drained the
// accumulator), the function returns after two atomic Loads and adds
// ~3 ns to the chunk write.
//
// Why no enqueueOrInlineNonBlocking: we are already in the writer's
// inlineMu hold. Routing back through the non-blocking enqueue
// helper would either re-acquire the lock we hold (deadlock) or
// detour via the writer's async channel (defeats the piggyback).
// Direct writeFrame to t.frameWriter.tx is correct and required.
func (t *ShmClientTransport) piggybackWUForWriter(streamID uint32) {
	if t.closed.Load() {
		return
	}
	// Conn-level WU: always check, regardless of streamID.
	if v := t.pendingConnWU.Swap(0); v > 0 {
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, v)
		_ = writeFrame(context.Background(), t.frameWriter.tx,
			FrameHeader{Type: FrameTypeWindowUpdate, StreamID: 0}, buf)
	}
	// Stream-level WU: only meaningful for streamID != 0 and a still-
	// active stream. The piggyback callback is only fired for
	// MESSAGE/DATA frames so streamID is always non-zero here, but
	// the guard costs nothing.
	if streamID == 0 {
		return
	}
	s := t.lookupStream(streamID)
	if s == nil || s.getState() == streamDone {
		return
	}
	if v := s.pendingWU.Swap(0); v > 0 {
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, v)
		_ = writeFrame(context.Background(), t.frameWriter.tx,
			FrameHeader{Type: FrameTypeWindowUpdate, StreamID: streamID}, buf)
	}
}

// updateFlowControl updates the incoming flow control windows for the
// transport and all active streams based on the current BDP estimation.
// This mirrors HTTP/2's dynamic window adjustment behavior.
func (t *ShmClientTransport) updateFlowControl(n uint32) {
	t.mu.Lock()
	t.initialWindowSize = int32(n)
	t.mu.Unlock()
	// Recompute the per-transport WU emission threshold so that the
	// new (BDP-driven) effective window is honoured by sendWindowUpdate.
	// wuThreshold is atomic — readers (sendConnWindowUpdate /
	// sendStreamWindowUpdate) load it lock-free.
	t.wuThreshold.Store(computeWUThreshold(int32(n)))
	t.mu.Lock()
	for _, s := range t.streams {
		s.fc.newLimit(n)
	}
	t.mu.Unlock()

	// Send connection-level window update
	if wu := t.connInFlow.newLimit(n); wu > 0 {
		t.sendWindowUpdate(0, wu)
	}
}

// sendBDPPing sends a BDP estimation ping to the server.
func (t *ShmClientTransport) sendBDPPing() {
	if t.closed.Load() {
		return
	}
	t.bdpEst.timesnap()
	_ = t.frameWriter.enqueue(frameEntry{
		ctx:     context.Background(),
		fh:      FrameHeader{Type: FrameTypePING, Flags: PingFlagBDP},
		payload: bdpPing.data[:],
	})
}

// test hook: allow disabling the background reader in tests to avoid
// interference when a different client is used on the same segment.
var enableClientReader atomic.Bool

func init() { enableClientReader.Store(true) }

// NewShmClientTransport creates a new shared memory client transport.
func NewShmClientTransport(segment *Segment, localAddr, remoteAddr net.Addr) (*ShmClientTransport, error) {
	if segment == nil {
		return nil, errors.New("segment cannot be nil")
	}

	// Extract segment name for event naming
	segmentName := extractSegmentName(segment.Path)

	// Create rings for bidirectional communication
	// Ring A: client->server, Ring B: server->client
	clientToServer := NewShmRingFromSegment(segment.A, segment.Mem)
	serverToClient := NewShmRingFromSegment(segment.B, segment.Mem)

	segment.RegisterRing(clientToServer)
	segment.RegisterRing(serverToClient)

	// Open events for cross-mapping synchronization (Windows).
	// Client opens events created by the server. On Linux, these are no-ops.
	writeEvents, _ := OpenRingEvents(segmentName, "A")
	readEvents, _ := OpenRingEvents(segmentName, "B")

	// Attach events to rings
	clientToServer.SetEvents(writeEvents)
	serverToClient.SetEvents(readEvents)

	ctx, cancel := context.WithCancel(context.Background())

	segName := ""
	if addr, ok := remoteAddr.(*ShmAddr); ok {
		segName = addr.Name
	}

	t := &ShmClientTransport{
		segment:        segment,
		clientToServer: clientToServer,
		serverToClient: serverToClient,
		segmentName:    segName,
		readEvents:     readEvents,
		writeEvents:    writeEvents,
		localAddr:      localAddr,
		remoteAddr:     remoteAddr,
		ctx:            ctx,
		cancel:         cancel,
		streams:        make(map[uint32]*ClientStream),

		streamInFlow: make(map[uint32]*inFlow),
		errCh:           make(chan struct{}),
		goAwayCh:        make(chan struct{}),
		streamsQuotaAvailable: make(chan struct{}, 1),
		keepaliveDone:         make(chan struct{}),
	}
	// Start the dedicated frame writer goroutine for the client→server ring.
	t.frameWriter = newShmFrameWriter(clientToServer)
	// Surface async write failures (fire-and-forget control frames
	// such as HEADERS / GOAWAY) by tearing down the transport.
	// Without this hook the writer goroutine would silently drop the
	// failure, and the peer would wait forever for a frame that was
	// never sent. The handler runs in a fresh goroutine so it can
	// safely call Close (which waits for the writer goroutine that
	// is currently invoking the callback). Close is guarded by
	// closeOnce so concurrent invocations are idempotent.
	t.frameWriter.setAsyncErrorHandler(func(err error) {
		// Context cancellation on the per-stream context is benign —
		// the stream is gone and the client doesn't need the bytes.
		// Ring closed errors mean we are already tearing down.
		if err == context.Canceled || err == context.DeadlineExceeded {
			return
		}
		if t.closed.Load() {
			return
		}
		go t.Close(fmt.Errorf("shm client: async write failed: %w", err))
	})
	// Register the lockless WU drain callback. Invoked by writeLoop
	// when wuRetryWake fires (i.e. an errFrameWriterFull restore left
	// bytes in pendingConnWU or some Stream.pendingWU). Called under
	// inlineMu, so the callback writes WINDOW_UPDATE frames directly
	// to the ring rather than recursing back through the writer's
	// non-blocking enqueue path.
	t.frameWriter.setDrainPendingWUFn(t.drainPendingWUForWriter)
	// Register the per-chunk piggyback callback. Invoked by
	// emitMessageInlineVec under inlineMu just before unlock, this
	// drains conn pendingConnWU + the just-written stream's pendingWU
	// into additional ring writes that ride out in the same SPSC
	// writer hold. Sustained outbound traffic effectively piggybacks
	// receiver-side WU updates onto every DATA chunk for ~free,
	// avoiding the standalone-emit cost on the hot path.
	t.frameWriter.setPiggybackWUFn(t.piggybackWUForWriter)

	// Publish the conn-quota atomic pointer to the writer goroutine
	// so processWholeMessage / retryDeferred can CAS against it.
	// MUST happen before connSendQuota is initialised below so WL
	// sees the post-Store value, not the zero default.
	t.frameWriter.setConnQuotaPtr(&t.connSendQuota)

	// Initialize dormancy condition variable.
	t.kpDormancyCond = sync.NewCond(&t.mu)
	// Initialize connection-level flow control windows to the HTTP/2 maximum.
	t.connSendQuota.Store(int64(maxWindowSize))
	t.connInFlow = trInFlow{limit: uint32(maxWindowSize)}
	t.connInFlow.updateEffectiveWindowSize()

	// Initialize BDP estimation for dynamic flow control (RFC A73 Phase 5).
	// SHM uses a much larger initial window (32MB) than HTTP/2 (64KB) because
	// local memory has near-zero RTT and high bandwidth.
	t.initialWindowSize = int32(shmInitialWindowSize)
	// Capture the per-transport WU emission threshold for this initial
	// window. Recomputed when the dialer applies a grpc.WithInitialWindowSize
	// override (further down in the dialer) and when BDP adjusts the
	// effective window dynamically. See computeWUThreshold for the
	// deadlock this avoids. Atomic so lockless WU emission can read it
	// without taking sendQuotaMu.
	t.wuThreshold.Store(computeWUThreshold(t.initialWindowSize))
	t.bdpEst = newShmBDPEstimator(uint32(shmInitialWindowSize), t.updateFlowControl)

	max := segment.H.MaxStreams()
	if max == 0 {
		max = uint32(math.MaxUint32)
	}
	t.maxConcurrentStreams = max
	t.streamQuota = int64(max)

	// Start processing incoming data from the server (test hook guarded)
	if enableClientReader.Load() {
		t.readerWG.Add(1)
		go func() {
			defer t.readerWG.Done()
			t.processIncomingData(t.ctx)
		}()
	}

	return t, nil
}

// SetOnClose sets the callback to be invoked when the transport is closed.
// RFC A73: This integrates with gRPC's ClientConn connectivity state management.
func (t *ShmClientTransport) SetOnClose(f func(GoAwayInfo)) {
	t.onClose = f
}

// ConfigureKeepalive sets keepalive parameters and starts the keepalive
// goroutine if Time != infinity.
func (t *ShmClientTransport) ConfigureKeepalive(kp keepalive.ClientParameters) {
	// Apply defaults matching HTTP/2 transport.
	if kp.Time == 0 {
		kp.Time = defaultClientKeepaliveTime
	}
	if kp.Timeout == 0 {
		kp.Timeout = defaultClientKeepaliveTimeout
	}
	t.kp = kp
	if kp.Time != infinity {
		t.keepaliveEnabled = true
		go t.keepalive()
	}
}

// SetAuthInfo sets the authentication information from security handshake.
func (t *ShmClientTransport) SetAuthInfo(authInfo credentials.AuthInfo) {
	t.mu.Lock()
	t.authInfo = authInfo
	t.mu.Unlock()
}

// GetAuthInfo returns the authentication information from security handshake.
func (t *ShmClientTransport) GetAuthInfo() credentials.AuthInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.authInfo
}

// processIncomingData reads data from the server->client ring and processes gRPC frames
func (t *ShmClientTransport) processIncomingData(ctx context.Context) {
	if shmDebugEnabled {
		shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: STARTED")
	}
	// Install the per-DATA-frame flow-control callback on the H2
	// decoder for the server→client ring. See the matching block in
	// ShmServerTransport.processIncomingData for the design rationale
	// (decouple H2 conn-flow credit from gRPC LPM reassembly so
	// multi-DATA-frame responses don't deadlock under a small
	// per-stream send window).
	t.serverToClient.h2Decoder().onDataFrame = t.onDataFrameReceived
	// onMessageStart fires when the codec has just parsed a new LPM's
	// 5-byte header (and the LPM spans multiple DATA frames). The
	// transport uses it to drive receiver-side pre-credit
	// (inFlow.maybeAdjust) so the sender can complete a message
	// whose body exceeds the per-stream initial send window.
	t.serverToClient.h2Decoder().onMessageStart = t.onMessageStart
	defer func() {
		if shmDebugEnabled {
			shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: EXITING")
		}
		if !t.closed.Load() {
			go t.Close(errors.New("incoming data processing ended"))
		}
	}()

	// Bounded burst counter: number of MESSAGE frames delivered since the
	// last cooperative yield. When the ring keeps producing data the reader
	// stays on-CPU to drain it, but we cap the burst so app goroutines that
	// just got data on their recvBuffer get a chance to run and post their
	// next Send. See shmClientMaxMessageBurst doc-comment for the cap value
	// rationale.
	messageBurst := 0

	for {
		if t.closed.Load() {
			if shmDebugEnabled {
				shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: transport closed, exiting")
			}
			return
		}
		if shmDebugEnabled {
			shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: waiting for frame from server...")
		}
		// Event-driven: block on next frame from rx ring.
		fh, payloadBuf, err := readFrameView(ctx, t.serverToClient)
		if err != nil {
			if shmDebugEnabled {
				shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: readFrame error: %v", err)
			}
			if errors.Is(err, io.EOF) {
				return
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			if errors.Is(err, ErrRingClosed) || t.closed.Load() {
				return
			}
			continue
		}
		if shmDebugEnabled {
			shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: received frame type=%d, streamID=%d, length=%d", fh.Type, fh.StreamID, fh.Length)
		}

		// Update last read timestamp for keepalive tracking.
		atomic.StoreInt64(&t.lastRead, time.Now().UnixNano())

		payloadTransferred := false
		release := func() {
			if !payloadTransferred && payloadBuf != nil {
				payloadBuf.Free()
				payloadBuf = nil
			}
		}

		var payload []byte
		if payloadBuf != nil {
			payload = payloadBuf.ReadOnlyData()
		}

		// Transport-level frames are not associated with a particular stream.
		switch fh.Type {
		case FrameTypeGOAWAY:
			var dbg string
			if len(payload) > 0 {
				dbg = string(payload)
			}
			t.setGoAwayReason(fh.Flags, dbg)
			t.draining.Store(true)
			select {
			case <-t.goAwayCh:
				// already closed
			default:
				close(t.goAwayCh)
			}
			if fh.Flags&GoAwayFlagIMMEDIATE != 0 {
				release()
				go t.Close(errors.New("received GOAWAY (immediate)"))
				return
			}
			t.mu.RLock()
			active := len(t.streams)
			t.mu.RUnlock()
			if active == 0 {
				release()
				go t.Close(errors.New("received GOAWAY (draining) with no active streams"))
				return
			}
			release()
			continue
		case FrameTypeWindowUpdate:
			if len(payload) >= 4 {
				// RFC 7540 §6.9.1: increment is big-endian. Senders
				// (sendWindowUpdate above) write BigEndian so this matches.
				delta := binary.BigEndian.Uint32(payload[:4])
				t.addSendQuota(fh.StreamID, delta)
			}
			release()
			continue
		}

		// Dispatch frame to appropriate stream.
		// Fast path: if we have a cached single stream, skip map lookup + RLock.
		var stream *ClientStream
		if c := t.cachedStream.Load(); c != nil && c.streamID == fh.StreamID {
			stream = c.stream
		} else {
			t.mu.RLock()
			var ok bool
			stream, ok = t.streams[fh.StreamID]
			t.mu.RUnlock()
			if !ok {
				release()
				continue
			}
		}

		// Handle different frame types
		switch fh.Type {
		case FrameTypeHEADERS:
			// Server sent headers (response headers)
			h, err := decodeHeaders(payload)
			if err != nil {
				release()
				stream.write(recvMsg{err: err})
				continue
			}

			// Populate the received header metadata.
			md := make(metadata.MD)
			for _, kv := range h.Metadata {
				vals := make([]string, 0, len(kv.Values))
				for _, v := range kv.Values {
					vals = append(vals, string(v))
				}
				md[kv.Key] = vals
			}
			if v := md.Get("grpc-encoding"); len(v) > 0 {
				stream.recvCompress = v[0]
			}
			if v := md.Get("content-type"); len(v) > 0 {
				if contentSubtype, ok := grpcutil.ContentSubtype(v[0]); ok {
					stream.contentSubtype = contentSubtype
				} else {
					release()
					stream.write(recvMsg{err: errors.New("transport: received unexpected content-type")})
					continue
				}
			}
			stream.header = md
			stream.headerValid = true
			stream.noHeaders = false

			// Signal that headers have been received
			if atomic.CompareAndSwapUint32(&stream.headerChanClosed, 0, 1) {
				close(stream.headerChan)
			}
			release()

		case FrameTypeMESSAGE:
			// Server sent a message. Apply inbound flow control before delivering.
			if shmDebugEnabled {
				shmDebugf("[DEBUG] ShmClientTransport: MESSAGE handler entered for stream %d, payload size=%d", fh.StreamID, len(payload))
			}
			sz := uint32(len(payload))

			// BDP estimation: track bytes received and trigger BDP ping if needed
			var sendBDPPing bool
			if t.bdpEst != nil {
				sendBDPPing = t.bdpEst.add(sz)
			}

			// NOTE: connection + stream flow-control credit is done
			// per H2 DATA frame in onDataFrameReceived (installed on
			// the h2 decoder above). Crediting here would double-
			// count. See onDataFrameReceived for the design rationale.

			// Send BDP ping if BDP estimator requests it
			if sendBDPPing {
				// Send window update before BDP ping to avoid excessive ping detection
				if wu := t.connInFlow.reset(); wu > 0 {
					t.sendWindowUpdate(0, wu)
				}
				t.sendBDPPing()
			}

			// Transfer ownership of the ring-backed buffer to the stream for zero-copy delivery.
			if payloadBuf != nil {
				if shmDebugEnabled {
					shmDebugf("[DEBUG] ShmClientTransport: MESSAGE delivering payloadBuf (len=%d) to stream %d", payloadBuf.Len(), fh.StreamID)
				}
				payloadTransferred = true
				stream.write(recvMsg{buffer: payloadBuf})
				payloadBuf = nil
			} else {
				if shmDebugEnabled {
					shmDebugf("[DEBUG] ShmClientTransport: MESSAGE delivering copied payload (len=%d) to stream %d", len(payload), fh.StreamID)
				}
				buf := mem.Copy(payload, mem.DefaultBufferPool())
				stream.write(recvMsg{buffer: buf})
			}
			// Yield to the app goroutine that was just goready'd by the channel
			// send. The recvBuffer's channel put places the receiver G on the
			// current P's local runq head; without a Gosched the runtime's
			// wakep then tries to find an idle M on another P to run the
			// woken G in parallel — which costs a futex syscall on Linux.
			// For ping-pong RPCs the parallelism is illusory (the reader has
			// nothing else to do until the server replies, which itself waits
			// on the app's next Send), so co-locating the two Gs on this M
			// strictly wins. The runtime.Gosched is a cooperative yield, not
			// a spin: it costs no CPU when no other G is runnable.
			//
			// AT HIGH STREAM CONCURRENCY (N=1000+), this unconditional yield
			// costs N park/unpark cycles per RPC round. Skip the yield when
			// more frames are immediately ready in the ring — keep draining
			// instead of round-tripping through the scheduler. The ping-pong
			// win is preserved because in the 1-stream case the ring is
			// almost always empty after the MESSAGE is delivered. A burst
			// cap (shmClientMaxMessageBurst) bounds how many frames the
			// reader will process without yielding so that app goroutines
			// waiting on recvBuffer don't starve.
			//
			// SIZE-AWARE: only the small-payload case wins from skipping
			// the yield. At medium payloads (e.g. N=100 streams sending
			// 64 KiB messages) the parallel app goroutine work outweighs
			// the wakep cost — let work-stealing pick up the recvBuffer
			// reader on another P. Always yield when the just-delivered
			// payload is above shmYieldSkipMaxPayload bytes.
			messageBurst++
			yield := sz > shmYieldSkipMaxPayload ||
				messageBurst >= shmClientMaxMessageBurst ||
				!t.serverToClient.HasPendingData()
			if yield {
				runtime.Gosched()
				messageBurst = 0
			}
			if shmDebugEnabled {
				shmDebugf("[DEBUG] ShmClientTransport: MESSAGE delivered to stream %d", fh.StreamID)
			}

		case FrameTypePING:
			// Respond with PONG carrying the same opaque data.
			pongPayload := make([]byte, len(payload))
			copy(pongPayload, payload)
			_ = t.frameWriter.enqueue(frameEntry{
				ctx:     context.Background(),
				fh:      FrameHeader{Type: FrameTypePONG, Flags: fh.Flags},
				payload: pongPayload,
			})
			release()

		case FrameTypePONG:
			// Check if this is a BDP ping acknowledgment
			if t.bdpEst != nil && len(payload) >= 8 {
				var data [8]byte
				copy(data[:], payload[:8])
				if data == bdpPing.data {
					t.bdpEst.calculate()
				}
			}
			release()
			continue

		case FrameTypeTRAILERS:
			// Server sent trailers (end of stream)
			tr, err := decodeTrailers(payload)
			if err != nil {
				release()
				t.closeStream(stream, err, false, 0, nil, nil, false)
			} else {
				// Convert metadata from protocol format to map
				trailerMap := make(map[string][]string)
				for _, kv := range tr.Metadata {
					trailerMap[kv.Key] = make([]string, len(kv.Values))
					for i, v := range kv.Values {
						trailerMap[kv.Key][i] = string(v)
					}
				}

				// Convert status
				var st *status.Status
				if tr.GRPCStatusCode != 0 {
					st = status.New(codes.Code(tr.GRPCStatusCode), tr.GRPCStatusMsg)
					err = st.Err()
				} else {
					st = status.New(codes.OK, "")
					err = io.EOF
				}

				// Close the stream with trailers
				t.closeStream(stream, err, false, 0, st, trailerMap, true)
			}
			release()

		case FrameTypeCANCEL:
			// Server cancelled the stream
			stream.write(recvMsg{err: context.Canceled})
			release()

		default:
			// Unknown frame type - ignore
			release()
		}
	}
}

// Close tears down this transport. Once it returns, the transport
// should not be accessed any more. The caller must make sure this
// is called only once.
func (t *ShmClientTransport) Close(err error) {
	t.closeOnce.Do(func() {
		// Mark closed early so late closeStream calls won't attempt to write to the
		// rings while teardown is in progress.
		t.closed.Store(true)
		segClosed := t.segment != nil && t.segment.closed.Load()
		// Ping wuRetryWake so the writer goroutine observes closed
		// and drains any pending deferred entries on its next pass.
		// The frame writer's close() path will also walk
		// deferredProto and refund all in-flight async proto entries
		// (decrementing each stream's protoInFlight counter so the
		// resource teardown invariant holds).
		select {
		case t.frameWriter.wuRetryWake <- struct{}{}:
		default:
		}

		// Best-effort GOAWAY before tearing down rings.
		// Non-blocking: if the channel is full (writer stuck on ring write),
		// skip GOAWAY to avoid deadlocking Close.
		if t.clientToServer != nil && !segClosed {
			t.frameWriter.tryEnqueueNonBlocking(frameEntry{
				ctx:     context.Background(),
				fh:      FrameHeader{Type: FrameTypeGOAWAY, Flags: GoAwayFlagIMMEDIATE},
				payload: []byte("client closing"),
			})
		}

		// Cancel context to stop background reader goroutine and keepalive.
		t.cancel()

		// Close the rings FIRST so any writeFrame blocked inside the writer
		// goroutine gets ErrRingClosed and unblocks. This must happen before
		// waiting for keepalive, because keepalive's sendPing uses
		// enqueueAndWait which blocks on the writer goroutine.
		if !segClosed {
			if t.clientToServer != nil {
				_ = t.clientToServer.Close()
			}
			if t.serverToClient != nil {
				_ = t.serverToClient.Close()
			}
		}
		t.frameWriter.close()

		// Wake up the keepalive goroutine if it's dormant, so it can exit.
		t.mu.Lock()
		if t.kpDormant {
			t.kpDormancyCond.Signal()
		}
		t.mu.Unlock()

		// Wait for keepalive goroutine to exit.
		if t.keepaliveEnabled && t.keepaliveDone != nil {
			<-t.keepaliveDone
		}

		// Terminate all active streams before unmapping the segment.
		t.mu.Lock()
		streams := make([]*ClientStream, 0, len(t.streams))
		for _, stream := range t.streams {
			if stream != nil {
				streams = append(streams, stream)
			}
		}
		t.mu.Unlock()
		for _, stream := range streams {
			t.closeStream(stream, err, false, 0, status.Convert(err), nil, false)
		}

		// Stop the reader goroutine. Under the eventfd waker the reader
		// is parked in shmDataSegWaker.WaitForChange (an *os.File.Read
		// on the eventfd via Go netpoll); ring.Close above set
		// hdr.Closed but the parker has no way to observe that without
		// a wake. Closing the eventfd makes Read return EBADF, which
		// WaitForChange surfaces as ErrRingClosed, and the reader
		// outer-loop exits. No-op on the per-address eventfd / futex
		// path (signal* above already woke same-side parkers there).
		if t.segment != nil {
			t.segment.UnblockSameSideParkers()
		}

		t.readerWG.Wait()

		// Drain any unconsumed inbound messages queued on the closed
		// streams. The reader goroutine has now exited (readerWG.Wait
		// above), so the per-stream recvBuffers no longer have
		// producers; the app side may not have RecvMsg'd these yet,
		// in which case the recvMsg.buffer slices may reference
		// ring-mapped memory via the multi-anchor ZC path. Free them
		// HERE — before t.segment.Close() unmaps the backing memory —
		// to eliminate the use-after-free window where a late RecvMsg
		// would deref dangling slices.
		//
		// drainAndFree preserves the recvBuffer's b.err set by
		// closeStream's s.write(recvMsg{err: err}) above, so any
		// subsequent RecvMsg still returns the close error correctly
		// — only the queued data messages are released.
		//
		// Mirrors the server-side teardown loop in
		// ShmServerTransport.Close which already does this for the
		// same reason. Without this loop, a benchmark or test that
		// finishes mid-RPC + immediately Close()s could panic with
		// "fatal error: ..." pointing into unmapped memory.
		for _, stream := range streams {
			if stream != nil {
				stream.drainRecvBuffer()
			}
		}

		// Close the named events (Windows)
		if t.readEvents != nil {
			t.readEvents.Close()
		}
		if t.writeEvents != nil {
			t.writeEvents.Close()
		}

		// Close the segment last and unlink the backing file.
		if t.segment != nil {
			_ = t.segment.Close()
		}
		if t.segmentName != "" {
			_ = RemoveSegment(t.segmentName)
			// Release the dialer-side handshake-events reference held
			// since DialShm. With refcounting this is safe even when a
			// same-process listener also holds a reference on the same
			// registry entry; only the final caller actually closes the
			// underlying named-event handles. No-op on Linux.
			CloseHandshakeEvents(t.segmentName)
		}

		// Signal closure
		close(t.errCh)

		// RFC A73: Invoke onClose callback to notify ClientConn of transport closure.
		// This allows the addrConn to update connectivity state properly.
		if t.onClose != nil {
			t.onClose(GoAwayInfo{Reason: t.goAwayReason})
		}
	})
}

// GracefulClose starts to tear down the transport: the transport will stop
// accepting new RPCs and NewStream will return error. Once all streams are
// finished, the transport will close.
//
// It does not block.
func (t *ShmClientTransport) GracefulClose() {
	// Mirror http2 client semantics: move into draining, which prevents new
	// streams from being created. Close the transport only after the last active
	// stream completes.
	if t.closed.Load() {
		return
	}
	if !t.draining.CompareAndSwap(false, true) {
		return
	}

	// Best-effort notify the peer we're draining.
	if t.clientToServer != nil {
		_ = t.frameWriter.enqueue(frameEntry{
			ctx:     context.Background(),
			fh:      FrameHeader{Type: FrameTypeGOAWAY, Flags: GoAwayFlagDRAINING},
			payload: []byte("draining"),
		})
	}

	// If there are no active streams, close immediately.
	t.mu.RLock()
	active := len(t.streams)
	t.mu.RUnlock()
	if active == 0 {
		t.Close(errors.New("no active streams left to process while draining"))
	}
}

// NewStream creates a Stream for an RPC.
func (t *ShmClientTransport) NewStream(ctx context.Context, callHdr *CallHdr, handler stats.Handler) (ClientStreamIface, error) {
	if t.closed.Load() || t.draining.Load() {
		return nil, &NewStreamError{Err: ErrConnClosing, AllowTransparentRetry: true}
	}

	firstTry := true
	var ch chan struct{}
	var s *ClientStream
	var streamID uint32
	var transportDrainRequired bool
	for {
		t.mu.Lock()
		if t.closed.Load() || t.draining.Load() {
			t.mu.Unlock()
			return nil, &NewStreamError{Err: ErrConnClosing, AllowTransparentRetry: true}
		}
		if t.streamQuota <= 0 {
			if firstTry {
				t.waitingStreams++
			}
			ch = t.streamsQuotaAvailable
			t.mu.Unlock()
			firstTry = false
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, &NewStreamError{Err: ContextErr(ctx.Err())}
			case <-t.goAwayCh:
				return nil, &NewStreamError{Err: errStreamDrain, AllowTransparentRetry: true}
			case <-t.ctx.Done():
				return nil, &NewStreamError{Err: ErrConnClosing, AllowTransparentRetry: true}
			}
		}
		if !firstTry {
			t.waitingStreams--
		}
		t.streamQuota--

		// Assign stream ID (client uses odd IDs, starting from 1)
		streamID = t.streamID
		if streamID == 0 {
			streamID = 1
		}
		t.streamID = streamID + 2 // Increment by 2 to maintain odd IDs
		// Drain client transport if nextID > MaxStreamID which signals gRPC that
		// the connection is closed and a new one must be created for subsequent RPCs.
		transportDrainRequired = t.streamID > MaxStreamID

		// Create the client stream
		s = &ClientStream{
			Stream: Stream{
				id:             streamID,
				ctx:            ctx,
				method:         callHdr.Method,
				sendCompress:   callHdr.SendCompress,
				contentSubtype: callHdr.ContentSubtype,
			},
			ct:           t, // Set the client transport (now an interface, no unsafe needed)
			done:         make(chan struct{}),
			headerChan:   make(chan struct{}),
			doneFunc:     callHdr.DoneFunc,
			statsHandler: handler,
		}
		s.Stream.buf.init()
		// Initialise per-stream inFlow limit.
		//
		// Use the configured per-stream initial window so inFlow.onRead's
		// limit/4 threshold and maybeAdjust's pre-credit math operate on a
		// realistic window size. Default is shmInitialWindowSize (captured
		// into t.initialWindowSize at construction); explicit override
		// comes from grpc.WithInitialWindowSize via
		// DialOptions.InitialWindowSize → t.initialStreamWindow.
		switch {
		case t.initialStreamWindow > 0 && t.initialStreamWindow < int64(maxWindowSize):
			s.fc = inFlow{limit: uint32(t.initialStreamWindow)}
		default:
			ws := t.initialWindowSize
			if ws <= 0 {
				ws = int32(shmInitialWindowSize)
			}
			s.fc = inFlow{limit: uint32(ws)}
		}
		s.readRequester = s

		// Set up transport reader for this stream
		s.trReader = transportReader{
			reader: recvBufferReader{
				ctx:          s.ctx,
				ctxDone:      s.ctx.Done(),
				recv:         &s.buf,
				clientStream: s,
			},
			windowHandler: s,
		}

		// Register the stream
		t.streams[streamID] = s
		// Update single-stream cache.
		if len(t.streams) == 1 {
			t.cachedStream.Store(&clientStreamCache{stream: s, streamID: streamID})
		} else {
			t.cachedStream.Store(nil)
		}
		streamWindow := int64(t.initialWindowSize)
		if t.initialStreamWindow > 0 {
			streamWindow = t.initialStreamWindow
		}
		if streamWindow <= 0 {
			// Defensive fallback: transport not yet had its initialWindowSize
			// set. Use the package-global default rather than maxWindowSize
			// (~2 GiB) so the sender is symmetric with the receiver's
			// inFlow.limit (defaults to shmInitialWindowSize). The previous
			// maxWindowSize fallback left the sender effectively unbounded
			// while the receiver enforced its 32 MiB limit, silently violating
			// HTTP/2 stream-window semantics (overflow surfaced as a
			// swallowed onData error).
			streamWindow = int64(shmInitialWindowSize)
		}
		// Per-stream send quota lives on the Stream as atomic.Int64
		// (s.sendQuota via embedded Stream), not in a transport-side
		// map. The Stream zero-value starts at 0, so we Store the
		// initial window here.
		s.sendQuota.Store(streamWindow)
		t.streamInFlow[streamID] = &s.fc
		if t.streamQuota > 0 && t.waitingStreams > 0 {
			select {
			case t.streamsQuotaAvailable <- struct{}{}:
			default:
			}
		}
		// Wake up the keepalive goroutine if it's dormant, so it can start
		// monitoring the now-active connection.
		if t.kpDormant {
			t.kpDormancyCond.Signal()
		}

		t.mu.Unlock()

		break
	}

	// Send HEADERS frame to initiate the stream
	var deadlineUnixNano uint64
	if deadline, ok := ctx.Deadline(); ok {
		if unixNano := deadline.UnixNano(); unixNano > 0 {
			deadlineUnixNano = uint64(unixNano)
		}
	}
	var kvs []KV
	hasKey := func(key string) bool {
		for _, kv := range kvs {
			if kv.Key == key {
				return true
			}
		}
		return false
	}
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		for k, vals := range md {
			byteVals := make([][]byte, 0, len(vals))
			for _, v := range vals {
				byteVals = append(byteVals, []byte(v))
			}
			kvs = append(kvs, KV{Key: k, Values: byteVals})
		}
	}
	// Add gRPC-required/expected metadata fields if not already present.
	if !hasKey("content-type") {
		kvs = append(kvs, KV{Key: "content-type", Values: [][]byte{[]byte(grpcutil.ContentType(callHdr.ContentSubtype))}})
	}
	registeredCompressors := grpcutil.RegisteredCompressors()
	if callHdr.SendCompress != "" {
		if !hasKey("grpc-encoding") {
			kvs = append(kvs, KV{Key: "grpc-encoding", Values: [][]byte{[]byte(callHdr.SendCompress)}})
		}
		if !grpcutil.IsCompressorNameRegistered(callHdr.SendCompress) {
			if registeredCompressors != "" {
				registeredCompressors += ","
			}
			registeredCompressors += callHdr.SendCompress
		}
	}
	if registeredCompressors != "" && !hasKey("grpc-accept-encoding") {
		kvs = append(kvs, KV{Key: "grpc-accept-encoding", Values: [][]byte{[]byte(registeredCompressors)}})
	}
	hdr := HeadersV1{
		Version:          1,
		HdrType:          0, // client-initial
		Method:           callHdr.Method,
		Authority:        callHdr.Host,
		DeadlineUnixNano: deadlineUnixNano,
		Metadata:         kvs,
	}

	payload := encodeHeaders(hdr)
	fh := FrameHeader{
		StreamID: streamID,
		Type:     FrameTypeHEADERS,
		Flags:    HeadersFlagINITIAL,
	}

	if err := t.frameWriter.enqueueAndWait(frameEntry{
		ctx:     ctx,
		fh:      fh,
		payload: payload,
	}); err != nil {
		t.mu.Lock()
		delete(t.streams, streamID)
		t.streamQuota++
		if t.streamQuota > 0 && t.waitingStreams > 0 {
			select {
			case t.streamsQuotaAvailable <- struct{}{}:
			default:
			}
		}
		t.mu.Unlock()
		// If draining was initiated concurrently and there are no streams left,
		// ensure the transport completes draining.
		if t.draining.Load() {
			t.mu.RLock()
			active := len(t.streams)
			t.mu.RUnlock()
			if active == 0 {
				go t.Close(errors.New("draining with no active streams"))
			}
		}
		return nil, &NewStreamError{Err: err, AllowTransparentRetry: true}
	}

	// If stream ID exhaustion requires draining, initiate graceful close.
	// This mirrors http2Client behavior.
	if transportDrainRequired {
		t.GracefulClose()
	}

	return s, nil
}

// Error returns a channel that is closed when some I/O error
// happens. Typically the caller should have a goroutine to monitor
// this in order to take action (e.g., close the current transport
// and create a new one) in error case. It should not return nil
// once the transport is initiated.
func (t *ShmClientTransport) Error() <-chan struct{} {
	return t.errCh
}

// GoAway returns a channel that is closed when ClientTransport
// receives the draining signal from the server (e.g., GOAWAY frame in
// HTTP/2).
func (t *ShmClientTransport) GoAway() <-chan struct{} {
	return t.goAwayCh
}

// GetGoAwayReason returns the reason why GoAway frame was received, along
// with a human readable string with debug info.
func (t *ShmClientTransport) GetGoAwayReason() (GoAwayReason, string) {
	if !t.draining.Load() {
		return GoAwayInvalid, ""
	}
	return t.goAwayReason, t.goAwayDebugMessage
}

// RemoteAddr returns the remote network address.
func (t *ShmClientTransport) RemoteAddr() net.Addr {
	return t.remoteAddr
}

// Peer returns the peer information for this transport.
func (t *ShmClientTransport) Peer() *peer.Peer {
	return &peer.Peer{
		Addr:      t.remoteAddr,
		AuthInfo:  nil, // Shared memory transport does not use authentication
		LocalAddr: t.localAddr,
	}
}

// incrMsgRecv increments the message received counter.
// This is called by ClientStream.Read() when a message is successfully read.
func (t *ShmClientTransport) incrMsgRecv() {
	// For shm transport, we don't track channelz metrics yet
	// This is a no-op for now, but maintains compatibility with ClientStream
}

// adjustWindow sends out extra window update over the initial window size
// of stream if the application is requesting data larger in size than
// the window.
func (t *ShmClientTransport) adjustWindow(s *ClientStream, n uint32) {
	if w := s.fc.maybeAdjust(n); w > 0 {
		t.sendWindowUpdate(s.id, w)
	}
}

// updateWindow is the application-consumption drip-credit path on
// the HTTP/2 Compatible flow-control profile. ClientStream.Read calls
// this with the number of bytes the application just consumed;
// inFlow.onRead returns the accumulated delta to credit and resets
// pendingUpdate when threshold (limit/4) is met.
//
// Combined with onMessageStart's receiver-driven pre-credit, this
// gives stock HTTP/2 semantics: a single message larger than the
// initial window gets pre-credited via maybeAdjust to complete;
// subsequent reads drip-credit at limit/4 granularity for the next
// message. After a message completes, the sender's window has been
// fully refilled to its initial value; the NEXT message is paced by
// the (now drip-driven) App-Recv credit cycle.
func (t *ShmClientTransport) updateWindow(s *ClientStream, n uint32) {
	if n == 0 {
		return
	}
	if w := s.fc.onRead(n); w > 0 {
		t.sendWindowUpdate(s.id, w)
	}
}

// lookupStream resolves a stream id to its ClientStream. Uses the
// cachedStream MRU fast path for single-stream connections (the
// common case for unary RPC ping-pong); falls back to the streams
// map under RLock otherwise. Returns nil for unknown ids.
func (t *ShmClientTransport) lookupStream(streamID uint32) *ClientStream {
	if c := t.cachedStream.Load(); c != nil && c.streamID == streamID {
		return c.stream
	}
	t.mu.RLock()
	s := t.streams[streamID]
	t.mu.RUnlock()
	return s
}

// onMessageStart is invoked by the h2 codec the moment it parses a
// new gRPC LPM's 5-byte header (multi-DATA-frame case only). lpmSize
// is `5 + bodyLen`; the message is NOT yet fully assembled in the
// accumulator. The transport asks inFlow.maybeAdjust whether the
// peer needs an upfront WINDOW_UPDATE to admit the rest of the LPM
// and, if so, emits it now.
//
// This is the SHM analogue of stock grpc-go's "app.Read(length)
// triggers maybeAdjust pre-credit" path. Stock HTTP/2 streams DATA
// frame bodies directly to s.buf so the app sees the LPM header and
// requests the full read; SHM's lpmAccumulator hides the partial
// LPM from the app, so the receiver drives the pre-credit instead.
// Net behaviour matches TCP/UDS: a single message > window completes
// in one round; the NEXT message is paced by App-Recv drip credit.
//
// Stream-level pre-credit only — conn-level WindowUpdate is emitted
// on-receive via onDataFrameReceived's drip path (the same
// "decouple conn FC from app reads" design as stock HTTP/2;
// see http2_client.go handleData).
//
// The WU is emitted via sendWindowUpdateForce so it bypasses the
// limit/4 drip-credit batching threshold; otherwise the pre-credit
// (which is REQUIRED for the LPM to complete under small windows)
// could be buffered indefinitely, never reaching the threshold and
// stalling the sender forever.
func (t *ShmClientTransport) onMessageStart(streamID uint32, lpmSize uint32) {
	if lpmSize == 0 {
		return
	}
	s := t.lookupStream(streamID)
	if s == nil {
		return
	}
	// Use the additive variant: SHM's codec-driven pre-credit fires
	// per LPM at parse time, so multiple pipelined LPMs can be
	// in-flight before the application drains the recvBuffer. The
	// stock maybeAdjust SETs f.delta and would lose previously
	// outstanding pre-credit; maybeAdjustAdditive ADDs the
	// incremental credit needed on top of any existing delta debt.
	if w := s.fc.maybeAdjustAdditive(lpmSize); w > 0 {
		shmStreamPreCreditEmitted.Add(uint64(w))
		t.sendWindowUpdateForce(streamID, w)
	}
	// Conn-level pre-credit. The drip-on-receive path in
	// onDataFrameReceived emits conn WU at the wuThreshold AFTER
	// bytes are received, which fails to admit an LPM that exceeds
	// the current effective conn window: the sender stalls (no more
	// conn quota to send the remainder) and the receiver cannot drip
	// (no more bytes arriving). This is the 1 MiB-jumbo `Send: EOF`
	// failure mode that arises whenever SHM_MAX_FRAME_SIZE >= LPM
	// size (the writer would otherwise chunk the LPM into per-frame
	// pieces that individually fit in the conn window).
	//
	// Emit a one-shot conn WU equal to the deficit between lpmSize
	// and the current effective conn window. The peer's
	// connSendQuota grows by that amount and admits this LPM to
	// complete. Multiple concurrent streams firing this path can
	// over-emit (we do not track promised conn-level pre-credit), but
	// per-stream FC enforces the per-stream limit on receive so
	// over-emission only loosens the conn cap — it does not produce
	// wire-protocol errors.
	if connEff := t.connInFlow.getSize(); lpmSize > connEff {
		t.sendConnWindowUpdate(lpmSize-connEff, true)
	}
}

// onDataFrameReceived runs at parse-time for each H2 DATA frame
// (BEFORE the body is fed to lpmAccumulator). It performs:
//
//   - Connection-level WU drip on-receive (stock HTTP/2 decoupling
//     of conn FC from app reads; without this multi-frame LPMs would
//     starve the conn window while bytes buffer in the accumulator).
//     Bytes accumulate towards the per-transport wuThreshold drip
//     threshold inside sendConnWindowUpdate; the conn-level inFlow
//     (`connInFlow`) is consulted only for the effectiveWindowSize
//     counter, not for emission timing.
//   - Stream-level inFlow.onData accounting + RFC 7540 §5.2.2 / §6.9.1
//     enforcement (close stream on over-window receive).
func (t *ShmClientTransport) onDataFrameReceived(streamID uint32, size uint32) {
	if size == 0 {
		return
	}
	// Connection-level WU: drip on-receive at wuThreshold, matching
	// stock HTTP/2's "conn FC decoupled from app reads" design.
	// connInFlow.onData updates unacked + effectiveWindowSize but
	// the SHM transport emits via its own batched
	// sendConnWindowUpdate path (the inFlow drip return value is
	// not used here because the SHM threshold can differ from
	// limit/4; see shm_flow_control.go computeWUThreshold).
	t.connInFlow.onData(size)
	t.sendWindowUpdate(0, size)
	// Stream-level: track pendingData and enforce the receive
	// window per RFC 7540 §6.9.1 and §5.2.2. The CAS rollback path
	// in the writer goroutine refunds quota BEFORE emit so it
	// cannot produce over-window bytes on the wire; the only
	// scenarios that reach this error are genuine sender bugs or
	// a buggy/malicious peer. Closing the stream with
	// FLOW_CONTROL_ERROR matches the stock HTTP/2 transport (see
	// http2_client.go onData handler).
	s := t.lookupStream(streamID)
	if s == nil {
		return
	}
	if err := s.fc.onData(size); err != nil {
		// Snapshot stream + conn FC state before closing so a
		// GRPC_SHM_DEBUG=1 run captures exactly what tripped the
		// limit check. Useful for diagnosing the 1 MiB jumbo bug
		// where stream-level pre-credit (onMessageStart) was
		// expected to admit the LPM + 5 B header but didn't.
		lim, pd, pu, d := s.fc.snapshot()
		cLim, cUnacked, cEff := t.connInFlow.snapshot()
		shmDebugf("[FC-VIOLATION] client stream=%d frameSize=%d err=%v"+
			" | stream{limit=%d pendingData=%d pendingUpdate=%d delta=%d}"+
			" | conn{limit=%d unacked=%d effective=%d}",
			streamID, size, err, lim, pd, pu, d, cLim, cUnacked, cEff)
		t.closeStream(s, io.EOF, true, http2.ErrCodeFlowControl,
			status.New(codes.Internal, err.Error()), nil, false)
	}
}

// closeStream closes the given stream and cleans up resources.
// This is called by ClientStream.Close() to terminate the stream.
func (t *ShmClientTransport) closeStream(s *ClientStream, err error, rst bool, _ http2.ErrCode, st *status.Status, mdata map[string][]string, _ bool) {
	// Set stream state to done
	if s.swapState(streamDone) == streamDone {
		// Already done, wait for first closer to finish
		<-s.done
		return
	}
	// Clear any pending stream-level WU credit. After streamDone is
	// observed by emit-paths, restored credit (errFrameWriterFull)
	// is dropped, but a late drip producer could still Add into the
	// stream's pending atomic between our state swap and the writer
	// loop's next drain. Storing 0 here makes the cleanup
	// observable and prevents a permanently-leaked accumulator
	// value from being emitted as a WU referencing a dead stream.
	s.pendingWU.Store(0)

	// Update status and trailers
	s.status = st
	if len(mdata) > 0 {
		s.trailer = mdata
	}

	// Signal error to readers. This must happen BEFORE closing headerChan
	// so that gRPC can read any buffered data before seeing the error.
	// For graceful close (eosReceived=true), err is io.EOF which signals
	// the reader that the stream ended normally.
	if err != nil {
		s.write(recvMsg{err: err})
	}

	// Close header channel if not already closed
	if atomic.CompareAndSwapUint32(&s.headerChanClosed, 0, 1) {
		s.noHeaders = true
		close(s.headerChan)
	}

	// Remove stream from active streams map and return stream quota.
	var shouldClose bool
	t.mu.Lock()
	delete(t.streams, s.id)
	// Update single-stream cache.
	if len(t.streams) == 1 {
		for id, cs := range t.streams {
			t.cachedStream.Store(&clientStreamCache{stream: cs, streamID: id})
			break
		}
	} else {
		t.cachedStream.Store(nil)
	}
	// Per-stream send quota lives on the Stream itself
	// (s.sendQuota atomic.Int64); no map deletion needed. The
	// detached Stream remains GC-rooted only via the in-flight
	// writer chan entries (if any) until the writer goroutine
	// resolves them in processProtoEntry / retryDeferredProto
	// (where s.getState() == streamDone is observed and the entry
	// is dropped + protoInFlight decremented).
	//
	// Wake the writer goroutine so any deferred entry pinned to
	// this stream observes streamDone on its next retry pass and
	// drains out. Without this, a deferred entry could linger
	// until the next unrelated quota event (or transport close).
	select {
	case t.frameWriter.wuRetryWake <- struct{}{}:
	default:
	}
	delete(t.streamInFlow, s.id)
	t.streamQuota++
	if t.streamQuota > 0 && t.waitingStreams > 0 {
		select {
		case t.streamsQuotaAvailable <- struct{}{}:
		default:
		}
	}
	shouldClose = t.draining.Load() && len(t.streams) == 0 && !t.closed.Load()
	t.mu.Unlock()

	// Send CANCEL frame if requested
	if rst && !t.closed.Load() {
		fh := FrameHeader{
			StreamID: s.id,
			Type:     FrameTypeCANCEL,
			Flags:    0,
		}
		// Best effort - ignore errors since stream is closing anyway
		_ = t.frameWriter.enqueue(frameEntry{
			ctx:     context.Background(),
			fh:      fh,
			payload: nil,
		})
	}

	// Close the done channel to unblock waiters
	close(s.done)

	if shouldClose {
		go t.Close(errors.New("transport drained"))
	}

	// Call doneFunc if present
	if s.doneFunc != nil {
		s.doneFunc()
	}
}

// writeProto serializes a proto.Message directly into the ring buffer,
// bypassing the standard encode→copy path.
func (t *ShmClientTransport) writeProto(s *ClientStream, msg any, opts *WriteOptions) (bool, error) {
	pm, ok := msg.(protoMessage)
	if !ok {
		return false, nil
	}
	if t.closed.Load() {
		return false, ErrConnClosing
	}

	// Do NOT check/modify stream state here. If ZC fails, the caller falls
	// back to write() which does its own CAS. Doing it here would leave the
	// stream in streamWriteDone, causing the fallback to return errStreamDone.

	// Check stream is active (read-only — no CAS yet).
	if s.getState() != streamActive {
		return false, errStreamDone
	}

	pSize := protoSize(pm)
	ringSize := h2FrameHeaderSize + 5 + pSize // total bytes in ring (H2 header + gRPC LPM + proto)
	quotaSize := 5 + pSize                    // flow-control size (matches receiver WINDOW_UPDATE accounting)

	// Skip ZC if the message is too large for a single frame.
	// Must check before acquiring flow control quota to avoid double-acquire
	// when the caller falls back to the standard write path.
	if uint64(ringSize) > t.clientToServer.Capacity()/3 {
		return false, nil
	}

	// Skip ZC when the LPM body exceeds shmMaxFrameSize. Both ZC paths
	// (inline writeProtoToRingH2 + queued writeProtoToRingH2Blocking)
	// emit the entire LPM as a single H2 DATA frame regardless of
	// the chunking knob; under a fair-comparison bench profile that
	// constrains maxFrameSize to 16384, a 64 KiB LPM emits ~65549 B
	// in one frame, which the receiver's stream-level fc.onData rejects
	// with "received N-bytes data exceeding the limit M bytes" BEFORE
	// onMessageStart's pre-credit fires (single-frame paths skip the
	// codec lpmAccumulator feed hook that drives onMessageStart).
	// The fallback write() path uses enqueueMessageAndWait →
	// emitH2DataFromCursor which honours shmMaxFrameSize chunking;
	// each emitted H2 DATA frame is < shmMaxFrameSize so the receiver's
	// codec runs the accumulator path that fires onMessageStart on
	// the first chunk and pre-credits the full LPM via
	// sendWindowUpdateForce.
	if quotaSize > shmMaxFrameSize {
		atomic.AddUint64(&shmZCWriteSkipMaxFrame, 1)
		return false, nil
	}

	// Skip ZC when the message wouldn't fit in the current per-stream
	// send window. Advisory only — the real CAS happens under
	// inlineMu below (inline fast path) or by the writer goroutine
	// (async fallback). When the window is totally depleted, we bail
	// to the chunked path (enqueueMessageAndWait) which can emit
	// partial chunks as small credits drip in, instead of waiting
	// for a full single-frame-sized window. CAS-race losses on
	// non-depleted windows go through the async-on-CAS-fail path.
	//
	// Lockless quota inspect via atomic Loads.
	if s.sendQuota.Load() < int64(quotaSize) || t.connSendQuota.Load() < int64(quotaSize) {
		atomic.AddUint64(&shmZCWriteSkipQuota, 1)
		return false, nil
	}

	// Set frame flags based on the caller's "last message" signal:
	//
	//   - MessageFlagMORE: signals "more frames follow on this stream".
	//     The server's handleMessage uses MORE=0 on incoming MESSAGE
	//     to detect client half-close.
	//   - MessageFlagEndStream: signals "this is the last message I
	//     will send on this stream". writeProtoToRingH2 maps this to
	//     H2's END_STREAM bit on the emitted DATA frame; the
	//     server-side H2 reader translates END_STREAM back to MORE=0
	//     so the same handleMessage MORE=0 EOF logic fires.
	var frameFlags uint8
	if opts != nil && !opts.Last {
		frameFlags = MessageFlagMORE
	} else {
		frameFlags = MessageFlagEndStream
	}
	fh := FrameHeader{
		Type:     FrameTypeMESSAGE,
		StreamID: s.id,
		Flags:    frameFlags,
	}

	// Acquire the frame writer's inline mutex to serialize with writeLoop.
	// writeProtoToRing writes directly to the ring, bypassing the frame
	// writer channel. Without this lock, concurrent control frame writes
	// (PING, WINDOW_UPDATE, etc.) would violate the SPSC ring invariant.
	//
	// closeMu.RLock prevents close() from completing (and the transport
	// from unmapping the segment) while we're writing to the ring.
	t.frameWriter.closeMu.RLock()
	if t.frameWriter.closed.Load() {
		t.frameWriter.closeMu.RUnlock()
		return true, ErrConnClosing
	}
	if t.frameWriter.inlineMu.TryLock() {
		// Per-stream FIFO check. If this stream has ANY async entry
		// pending (queued on writer chan OR sitting in
		// deferredProto), we MUST also go async so the new entry
		// queues behind it — otherwise an inline write would
		// overtake an earlier-enqueued entry and violate the gRPC
		// per-stream message order invariant. The Load is
		// authoritative here because inlineMu serialises us against
		// the writer's decrement (which happens under inlineMu in
		// processProtoEntry / retryDeferredProto).
		if s.protoInFlight.Load() == 0 {
			// Try the lock-free two-resource CAS reservation. On
			// success, marshal directly into the ring under
			// inlineMu (the inline ZC fast path); on failure, drop
			// through to the async path where the writer does the
			// CAS reservation under its own ownership.
			if tryReserveSendQuota(&t.connSendQuota, &s.sendQuota, int64(quotaSize)) {
				ok2, err := writeProtoToRing(s.ctx, t.clientToServer, s.id, pm, pSize, frameFlags)
				t.frameWriter.inlineMu.Unlock()
				t.frameWriter.closeMu.RUnlock()
				if !ok2 {
					// ZC didn't handle the write (insufficient
					// contiguous ring space). Release quota; ping
					// wuRetryWake so the writer revisits any
					// deferred whole-message senders whose quota
					// gap may now be satisfiable.
					t.connSendQuota.Add(int64(quotaSize))
					s.sendQuota.Add(int64(quotaSize))
					select {
					case t.frameWriter.wuRetryWake <- struct{}{}:
					default:
					}
					return false, err
				}
				if err != nil {
					// ZC attempted (ok2==true) but failed AFTER
					// reservation — typical causes are
					// protoMarshalAppend error or ctx-cancel during
					// ReserveWrite. Either way the ring write did
					// not commit so we refund the quota.
					t.connSendQuota.Add(int64(quotaSize))
					s.sendQuota.Add(int64(quotaSize))
					select {
					case t.frameWriter.wuRetryWake <- struct{}{}:
					default:
					}
					return true, err
				}
				// ZC succeeded — transition stream state if last.
				if opts != nil && opts.Last {
					if !s.compareAndSwapState(streamActive, streamWriteDone) {
						// Race: stream was closed concurrently.
						// Data is already on the ring which is
						// harmless (reader will process it).
						return true, errStreamDone
					}
				}
				return true, nil
			}
			// CAS-fail (race-loss or quota concurrently drained).
			// Drop through to async — the writer will retry CAS
			// under its own ownership and defer if still stalled.
			atomic.AddUint64(&shmZCWriteSkipQuota, 1)
		}
		t.frameWriter.inlineMu.Unlock()
	} else {
		atomic.AddUint64(&shmZCWriteSkipInlineBusy, 1)
	}
	t.frameWriter.closeMu.RUnlock()

	// Async path: writer goroutine owns the CAS reservation and
	// defer-and-retry. Fire-and-forget — no sender park, no
	// per-stream signal channel, no FIFO bookkeeping under
	// sendQuotaMu. The writer's processProtoEntry handles ordering
	// vs already-deferred entries via deferredProto[sid] append.
	//
	// Marshal the proto into an owned pooled buffer on THIS (SendMsg)
	// goroutine, so the writer copies bytes into the ring rather than
	// reading the live message asynchronously — which would race an
	// application that reuses the message after SendMsg returns (see the
	// grpc-go SendMsg contract). The uncontended inline fast path above
	// is unchanged; only this contended fallback pays the marshal-buffer
	// + copy.
	protoBytes, merr := marshalProtoForAsync(pm, pSize)
	if merr != nil {
		return true, merr
	}
	// opts.Last state transition happens BEFORE enqueue so the
	// upper-layer observes the semantic "I'm done sending" at the
	// instant writeProto returns. The H2 END_STREAM bit on the
	// emitted frame is already encoded in fh.Flags.
	if opts != nil && opts.Last {
		if !s.compareAndSwapState(streamActive, streamWriteDone) {
			putAsyncProtoBuf(protoBytes)
			return true, errStreamDone
		}
	}
	// Increment BEFORE enqueue so a subsequent same-stream sender
	// (upper-layer SendMsg back-to-back) observes the in-flight
	// count and also routes through async, preserving FIFO. The
	// writer decrements after the entry is fully resolved.
	s.protoInFlight.Add(1)
	if err := t.frameWriter.enqueueProtoAsync(s.ctx, &s.Stream, fh, protoBytes, pSize); err != nil {
		s.protoInFlight.Add(-1)
		putAsyncProtoBuf(protoBytes)
		return true, err
	}
	return true, nil
}

// write writes data to the stream via the shared memory transport.
// This is called by ClientStream.Write() to send data.
func (t *ShmClientTransport) write(s *ClientStream, hdr []byte, data mem.BufferSlice, opts *WriteOptions) error {
	if shmDebugEnabled {
		shmDebugf("[DEBUG] ShmClientTransport.write: stream=%d, hdr_len=%d, data_bytes=%d, ring=%p", s.id, len(hdr), data.Len(), t.clientToServer)
	}
	// Check if transport is closed
	if t.closed.Load() {
		if shmDebugEnabled {
			shmDebugf("[DEBUG] ShmClientTransport.write: transport closed")
		}
		return ErrConnClosing
	}

	// Check stream state
	if opts != nil && opts.Last {
		// Last message - transition to write done state
		if !s.compareAndSwapState(streamActive, streamWriteDone) {
			if shmDebugEnabled {
				shmDebugf("[DEBUG] ShmClientTransport.write: stream done (Last=true)")
			}
			return errStreamDone
		}
	} else if s.getState() != streamActive {
		if shmDebugEnabled {
			shmDebugf("[DEBUG] ShmClientTransport.write: stream not active")
		}
		return errStreamDone
	}

	payloadLen := len(hdr) + data.Len()

	// Hand the whole MESSAGE to the writer goroutine. The writer
	// owns outbound flow-control state and chunks the payload
	// across per-window grants internally; the sender pushes once
	// and blocks on doneCh until the entire message has been
	// emitted. isLast follows opts.Last semantics: true means the
	// LAST chunk emitted carries HTTP/2 END_STREAM (closing the
	// client half); false means MORE (more MESSAGEs may follow on
	// the same stream).
	isLast := opts != nil && opts.Last
	if shmDebugEnabled {
		shmDebugf("[DEBUG] ShmClientTransport.write: enqueueing whole MESSAGE (%d bytes, isLast=%v)", payloadLen, isLast)
	}
	return t.frameWriter.enqueueMessageAndWait(s.ctx, &s.Stream, hdr, data, isLast)
}

// sendPing sends a PING frame with 8-byte opaque data.
func (t *ShmClientTransport) sendPing() error {
	// Check if transport is closed before attempting to write.
	if t.closed.Load() {
		return ErrConnClosing
	}
	var data [8]byte
	// Use current time nanos as opaque payload (not strictly required, just convenient).
	binary.LittleEndian.PutUint64(data[:], uint64(time.Now().UnixNano()))
	return t.frameWriter.enqueueAndWait(frameEntry{
		ctx:     t.ctx,
		fh:      FrameHeader{Type: FrameTypePING},
		payload: data[:],
	})
}

// keepalive monitors connection health and sends periodic PING frames.
// It follows the gRPC keepalive semantics:
// - Send PING after kp.Time of inactivity.
// - Close connection if no PONG within kp.Timeout.
// - Go dormant if no active streams and !PermitWithoutStream.
func (t *ShmClientTransport) keepalive() {
	var err error
	defer func() {
		close(t.keepaliveDone)
		if err != nil {
			t.Close(err)
		}
	}()

	// True iff a ping has been sent, and no data has been received since then.
	outstandingPing := false
	// Amount of time remaining before which we should receive an ACK for the
	// last sent ping.
	timeoutLeft := time.Duration(0)
	// Records the last value of t.lastRead before we go block on the timer.
	prevNano := time.Now().UnixNano()
	timer := time.NewTimer(t.kp.Time)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			lastRead := atomic.LoadInt64(&t.lastRead)
			if lastRead > prevNano {
				// There has been read activity since the last time we were here.
				outstandingPing = false
				// Next timer should fire at kp.Time seconds from lastRead time.
				timer.Reset(time.Duration(lastRead) + t.kp.Time - time.Duration(time.Now().UnixNano()))
				prevNano = lastRead
				continue
			}
			if outstandingPing && timeoutLeft <= 0 {
				err = connectionErrorf(true, nil, "keepalive ping failed to receive ACK within timeout")
				return
			}
			t.mu.Lock()
			if t.closed.Load() {
				// Transport is closing; exit.
				t.mu.Unlock()
				return
			}
			if len(t.streams) < 1 && !t.kp.PermitWithoutStream {
				// If a ping was sent out previously (because there were active
				// streams at that point) which wasn't acked and its timeout
				// hadn't fired, but we got here and are about to go dormant,
				// we should make sure that we unconditionally send a ping once
				// we awaken.
				outstandingPing = false
				t.kpDormant = true
				t.kpDormancyCond.Wait()
			}
			t.kpDormant = false
			t.mu.Unlock()

			// We get here either because we were dormant and a new stream was
			// created which unblocked the Wait() call, or because the
			// keepalive timer expired. In both cases, we need to send a ping.
			if !outstandingPing {
				if pingErr := t.sendPing(); pingErr != nil {
					// Failed to send ping; connection may be broken.
					err = connectionErrorf(true, pingErr, "keepalive failed to send ping")
					return
				}
				timeoutLeft = t.kp.Timeout
				outstandingPing = true
			}
			// The amount of time to sleep here is the minimum of kp.Time and
			// timeoutLeft. This will ensure that we wait only for kp.Time
			// before sending out the next ping (for cases where the ping is
			// acked).
			sleepDuration := min(t.kp.Time, timeoutLeft)
			timeoutLeft -= sleepDuration
			timer.Reset(sleepDuration)
		case <-t.ctx.Done():
			// Transport is shutting down.
			return
		}
	}
}

// Compile-time check to ensure ShmClientTransport implements clientTransport.
var _ clientTransport = (*ShmClientTransport)(nil)
