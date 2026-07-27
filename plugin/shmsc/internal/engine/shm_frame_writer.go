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

package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc/mem"
)

// shmFrameWriter provides a dedicated writer goroutine with an MPSC queue for
// serializing frame writes to a shared-memory ring buffer. This eliminates
// contention from multiple stream goroutines competing for a write lock.
//
// Design:
//   - Callers enqueue frameEntry structs via the channel (lock-free producer side).
//   - A single writer goroutine drains the channel and writes frames to the ring.
//   - Callers that need synchronous completion (e.g., HEADERS that must know if
//     the write failed) set a doneCh on the entry and wait for it.
//
// Shutdown safety:
//   - close() marks the writer as closed and closes the channel.
//   - Channel-send paths (trySend, enqueueOrInlineNonBlocking,
//     enqueueAndWait, enqueueMessageAndWait) hold closeMu.RLock
//     around the closed check + chan send so the channel is never
//     sent to after close.
//   - The inline-write path (tryInlineWrite) does NOT hold closeMu;
//     it relies on inlineMu + the post-lock closed.Load() check.
//     close() drains inlineMu (drainInline barrier) after wg.Wait,
//     so any inline writer that already TryLocked inlineMu before
//     close set the closed flag runs to completion against a still-
//     mapped ring before close proceeds to teardown.
type shmFrameWriter struct {
	tx     *ShmRing
	ch     chan frameEntry // data + control frames from app goroutines
	wg     sync.WaitGroup
	closed atomic.Bool
	// closeMu synchronizes channel close with concurrent senders.
	// Senders hold RLock; close() holds Lock.
	closeMu sync.RWMutex
	// inlineMu protects direct ring writes in the enqueueAndWait fast path.
	// The writer goroutine also holds this when active, ensuring no two
	// writers access the ring simultaneously.
	inlineMu sync.Mutex

	// onAsyncError, if non-nil, is invoked when an entry without a doneCh
	// (fire-and-forget control-frame enqueue, used by HEADERS / GOAWAY
	// senders) fails to write to the ring. Without this hook the error
	// would be silently dropped, leaving the peer waiting forever for
	// a frame that was never sent. The callback is fired at most once
	// per writer to avoid amplifying a single ring failure into N
	// close attempts when a queue full of doomed entries drains. Set
	// via setAsyncErrorHandler after construction; nil callback means
	// "swallow".
	onAsyncError func(error)
	errReported  atomic.Bool

	// wuRetryWake is signalled by the lockless WU emit path
	// (sendConnWindowUpdate / sendStreamWindowUpdate) whenever an
	// errFrameWriterFull restore happens. The writer loop's select
	// observes this channel and, on wake, calls drainPendingWUFn to
	// Swap+emit any pending WU bytes that the failed enqueue left
	// behind in the transport's atomic accumulators. Buffer=1 because
	// only one outstanding "please retry" notification is meaningful
	// — drain semantics are level-triggered (drain everything that
	// is pending), so coalescing multiple signals into one wake is
	// safe and desirable.
	//
	// This channel closes the force-WU liveness gap that the previous
	// mutex-based restore had: a force pre-credit (onMessageStart)
	// whose enqueue failed could otherwise sit in pending indefinitely
	// because there was no scheduled trigger to retry. With the retry
	// wake, every Swap+restore is guaranteed to be visited by the
	// writer loop within one scheduler tick.
	wuRetryWake chan struct{}
	// drainPendingWUFn, if non-nil, is invoked by the writer loop
	// after acquiring inlineMu when a wuRetryWake (or normal wake
	// in the drain prologue, see writeLoop) is observed. The callback
	// is set by the owning transport after construction and is
	// expected to iterate the transport's atomic WU accumulators
	// (conn-level and per-stream) and emit one WINDOW_UPDATE frame
	// for each non-zero Swap'd value. The callback is responsible
	// for writing under the existing inlineMu invariant.
	drainPendingWUFn func()

	// piggybackWUFn, if non-nil, is invoked by the writer goroutine
	// after each DATA chunk emit (advanceDeferred / processWholeMessage;
	// control frames via processEntry) WHILE STILL HOLDING inlineMu,
	// just before the function returns and releases the lock. The
	// callback receives the streamID of the just-emitted DATA chunk
	// and is expected to drain the transport's connection-level WU
	// accumulator AND that stream's pending WU into additional ring
	// writes that ride out in the same SPSC writer position.
	//
	// This is the "outbound piggyback" optimisation: a WU emitted in
	// the same inlineMu hold as an outbound DATA frame costs ~100 ns
	// marginal (just the additional Reserve+Commit; the ring batch's
	// reader-wake signal is amortised across both writes) versus
	// ~200-250 ns for a standalone WU that goes through
	// enqueueOrInlineNonBlocking and either re-acquires inlineMu or
	// detours via the writer channel + writeLoop. Under sustained
	// 1000-stream traffic the receiver-side WU producer effectively
	// piggybacks onto every outbound DATA chunk for ~free, removing
	// the standalone-emit cost from the hot path almost entirely.
	//
	// Callback contract:
	//   - Runs under inlineMu (do not re-acquire)
	//   - May call writeFrame(w.tx, ...) directly to add frames
	//   - Must do O(1) work — typically drain conn pendingWU + the
	//     specific stream's pendingWU only. Walking all streams is
	//     the responsibility of drainPendingWUFn (called on
	//     wuRetryWake), not the per-chunk piggyback.
	//   - Must be non-blocking (no external IO)
	//
	// Implementations on ShmClientTransport and ShmServerTransport
	// look up the *streamBase by streamID, check streamDone state, and
	// Swap+writeFrame for any non-zero pending WU. Set in the owning
	// transport's constructor before the writer goroutine begins
	// servicing real traffic.
	piggybackWUFn func(streamID uint32)

	// Whole-message dispatch state — WL-private (no lock).
	//
	// connQuota points at the transport's connSendQuota atomic.Int64.
	// WL uses it via CompareAndSwap to deduct outbound flow-control
	// credit per emitted chunk. Senders no longer touch the conn
	// quota; only WL (via processWholeMessage / retryDeferred) does.
	//
	// deferred maps streamID -> the partially-emitted whole-message
	// entry whose remaining bytes are blocked on outbound FC credit.
	// Because each sender goroutine blocks on a doneCh until the
	// whole MESSAGE finishes emitting, there is at most ONE deferred
	// entry per stream at any time. WL adds to this map when a chunk
	// CAS fails; on incoming WU credit (signalled via wuRetryWake)
	// WL walks the map and retries each deferred entry.
	//
	// deferredProto maps streamID -> FIFO slice of ZC proto entries
	// (writeProto's async fire-and-forget path) whose CAS attempt
	// at processProtoEntry time failed for lack of outbound send
	// quota. Each slice preserves the original chan-arrival order so
	// retryDeferred drains in FIFO. The sender's inline TryLock path
	// inspects `len(deferredProto[streamID]) > 0` under inlineMu and
	// re-routes to async if non-empty, guaranteeing that no inline
	// write for stream X overtakes an already-deferred async entry
	// for stream X. Empty / absent slices have zero overhead in the
	// map (a single hash lookup) — the common case under non-stalled
	// FC.
	//
	// All three fields are accessed only from the writer goroutine
	// (plus the sender under inlineMu for the deferredProto peek);
	// no extra mutex is needed. setConnQuotaPtr publishes the
	// connQuota pointer via happens-before-channel-send before any
	// sender enqueueMessageAndWait can race here.
	connQuota     *atomic.Int64
	deferred      map[uint32]*deferredMessage
	deferredProto map[uint32][]frameEntry

	// deferredTrailers stashes a TRAILERS frameEntry (server-side
	// writeStatus) that arrived through the writer chan while DATA
	// for the same stream was still queued in deferred[sid] or
	// deferredProto[sid]. The writer's processTrailerEntry stashes
	// here instead of emitting immediately; the DATA-drain terminals
	// in retryDeferredProto / advanceDeferred / processProtoEntry
	// call either flushDeferredTrailer (DATA landed on ring → emit
	// trailer) or discardDeferredTrailer (DATA dropped on
	// streamDone / ctx-cancel / ring-write-err → signal errStreamDone
	// to writeStatus sender, do NOT emit trailer; emitting OK-status
	// after dropping DATA would put the peer in a cardinality
	// violation, which is exactly the bug the original synchronous
	// server writeProto path was carved out to avoid).
	//
	// This replaces the previous server-side restriction (no async
	// writeProto path on the server) which forced server sends to
	// take the sync chunked-vec path and park ~660 doneCh-waits/op
	// on the N=1000/4 KiB bench. With the trailer-sentinel, server
	// writeProto can use async + the writer's FIFO chan naturally
	// orders DATA-before-TRAILERS without busy-spinning in
	// writeStatus. See grpc-go-shm-server-async-trailer-sentinel-
	// design memo for the full rationale.
	//
	// At most ONE pending TRAILERS per stream (gRPC's serial
	// writeStatus contract). map zero-value is acceptable; lazy-
	// allocated in newShmFrameWriter.
	deferredTrailers map[uint32]frameEntry

	// pendingEndStream holds a parked zero-length client half-close
	// (END_STREAM, from CloseSend) that arrived while the stream still
	// had DATA queued (a deferred whole-message in w.deferred or async
	// proto entries in w.deferredProto). flushPendingEndStream emits it
	// at the DATA-drain terminal once BOTH DATA queues for the stream
	// are empty, so the peer never sees the half-close before the
	// preceding DATA (gRPC per-stream message order). Sentinel pattern
	// mirrors deferredTrailers; at most one per stream (gRPC's one-Last-
	// per-stream contract). Lazy-allocated in newShmFrameWriter.
	pendingEndStream map[uint32]frameEntry
}

// deferredMessage holds the partial state of a whole-message entry
// that ran out of flow-control credit mid-chunk. Stored in
// shmFrameWriter.deferred[streamID]; revisited on wuRetryWake.
//
// The cursor cur and remaining count track how much of the original
// (hdr || data BufferSlice) virtual stream still needs to be emitted.
// On WU arrival, WL CAS-reserves the smaller of (remaining, connQ,
// streamQ) and calls emitH2DataFromCursor; on full completion (cur
// drained) it signals doneCh nil and deletes the map entry. On
// stream-local close (closeStream flips state to streamDone and
// fires wuRetryWake) advanceDeferred sees streamDone on its next
// pass, signals doneCh with errStreamDone, and deletes the entry
// without emitting further bytes — any RST/CANCEL/TRAILERS the
// close path enqueued is processed normally by writeLoop after the
// deferred drain completes.
type deferredMessage struct {
	ctx       context.Context
	streamPtr *streamBase
	fh        FrameHeader
	cur       vecCursor // embedded by value (was *vecCursor) — see Stream.shmDeferred
	// origData preserves the original BufferSlice header captured at
	// processWholeMessage time. cur.data is destructively re-sliced
	// by vecCursor.writeTo (each emitted segment is dropped via
	// `c.data = c.data[1:]`), so by the time release() runs cur.data
	// is typically the empty tail. Freeing cur.data would be a no-op
	// and the Ref taken in enqueueMessageAndWait would never get
	// balanced — leaking pooled buffers. Free origData instead.
	origData  mem.BufferSlice
	remaining int
	isLast    bool
	doneCh    chan error
}

// release frees the BufferSlice ref AND nulls the cur slices so that
// the underlying *mem.Buffer pointers and lpmHdr byte slice can be
// reclaimed by GC. Used by every terminal path in advanceDeferred /
// processWholeMessage / close-drain. Frees d.origData (the original
// caller-supplied BufferSlice header) NOT d.cur.data, because
// vecCursor.writeTo destructively re-slices cur.data as it emits —
// see the origData field doc on deferredMessage.
func (d *deferredMessage) release() {
	d.origData.Free()
	d.origData = nil
	d.cur.data = nil
	d.cur.lpmHdr = nil
}

// frameEntry represents a single frame to be written to the ring.
type frameEntry struct {
	ctx     context.Context
	fh      FrameHeader
	payload []byte          // simple payload (HEADERS, TRAILERS, CANCEL, etc.)
	hdr     []byte          // optional header prefix for BufferSlice payloads
	data    mem.BufferSlice // zero-copy payload (MESSAGE)
	doneCh  chan error      // if non-nil, writer sends result and caller waits

	// Whole-message dispatch.
	//
	// When `wholeMsg` is true, this entry carries a complete logical
	// MESSAGE (hdr + data BufferSlice covering the full payload)
	// that the writer goroutine MUST chunk internally according to
	// the current outbound flow-control window. The sender does not
	// touch flow control; it pushes the whole message once and waits
	// on doneCh until the LAST chunk lands on the ring (FC defer +
	// retry is handled transparently by WL via the deferred map).
	//
	// streamPtr (the embedded base Stream) is used by WL for atomic
	// CAS on the per-stream send quota during chunking. isLast tells
	// WL whether the final chunk carries the HTTP/2 END_STREAM bit
	// (vs MORE for intermediate streaming messages).
	wholeMsg  bool
	streamPtr *streamBase
	isLast    bool

	// Async writeProto fallback: PRE-MARSHALLED proto body.
	//
	// When `protoBytes != nil`, this entry carries the already-
	// serialized gRPC LPM proto body. The sender (ShmClient/
	// ServerTransport.writeProto) marshals the proto.Message into a
	// pooled buffer on its OWN SendMsg goroutine — so the live message
	// is never retained across SendMsg, which would race an application
	// that legally reuses the message per the grpc-go SendMsg contract
	// — and the writer copies these bytes into a ring reservation via
	// writeProtoBytesToRingH2Blocking. The buffer is owned by the writer
	// once enqueued and returned to asyncProtoBufPool via
	// putAsyncProtoBuf on every terminal path (written / errored /
	// dropped). Used as the queued fallback when writeProto's
	// inlineMu.TryLock fails, the quota CAS fails, or the stream already
	// has async entries in flight.
	//
	// Caller MUST pre-validate single-frame size bounds. protoSize
	// equals len(protoBytes) (kept explicit for the 5 + protoSize quota
	// calc). fh.Flags carries MessageFlagEndStream / MORE.
	protoBytes []byte
	protoSize  int
}

const (
	// frameWriterQueueSize is the channel buffer size. Large enough to absorb
	// bursts without blocking callers, small enough to bound memory.
	// At N=1000 concurrent streams, 256 is too small — senders block on
	// channel full. 2048 absorbs typical fanout without back-pressure on
	// the async fire-and-forget path.
	frameWriterQueueSize = 2048

	// maxDrainPerPass caps the greedy drain in writeLoop per outer-select
	// trip. Bounds inlineMu hold time and ensures the outer select can
	// observe wuRetryWake signals in a timely manner. Matches .NET's
	// 512-frame BeginBatch/EndBatch drain. Large enough that high-conc
	// 4 KiB cells (where ~1000 producers contend on chan-send) coalesce
	// many late arrivals into one writev+SignalData cycle.
	maxDrainPerPass = 512
)

// newShmFrameWriter creates and starts a frame writer for the given ring.
func newShmFrameWriter(tx *ShmRing) *shmFrameWriter {
	w := &shmFrameWriter{
		tx:               tx,
		ch:               make(chan frameEntry, frameWriterQueueSize),
		wuRetryWake:      make(chan struct{}, 1),
		deferred:         make(map[uint32]*deferredMessage),
		deferredProto:    make(map[uint32][]frameEntry),
		deferredTrailers: make(map[uint32]frameEntry),
		pendingEndStream: make(map[uint32]frameEntry),
	}
	w.wg.Add(1)
	go w.writeLoop()
	return w
}

// setAsyncErrorHandler registers a callback to be invoked at most once
// when a fire-and-forget entry (no doneCh) fails to write to the ring.
// The handler is expected to run in its own goroutine if it needs to
// tear down the owning transport (which would otherwise deadlock on
// frame writer close). Must be called before the first enqueue. Pass
// nil to clear (no-op for the test path).
func (w *shmFrameWriter) setAsyncErrorHandler(fn func(error)) {
	w.onAsyncError = fn
}

// setDrainPendingWUFn registers a callback invoked by the writer
// loop while holding inlineMu to drain the transport's lockless WU
// pending atomics (transport.pendingConnWU and per-stream
// Stream.pendingWU). Called whenever a wuRetryWake signal is
// observed — see writeLoop for the trigger conditions. The callback
// MUST write any required WINDOW_UPDATE frames directly to w.tx
// (the ring) because inlineMu is already held; routing back through
// enqueueOrInlineNonBlocking would deadlock on the chan or fail
// TryLock indefinitely. Must be called before the first WU emit
// path call (typically in the transport constructor, right after
// newShmFrameWriter). The wuRetryWake channel send / receive
// provides the happens-before edge that publishes drainPendingWUFn
// to the writer goroutine.
func (w *shmFrameWriter) setDrainPendingWUFn(fn func()) {
	w.drainPendingWUFn = fn
}

// setPiggybackWUFn registers the per-chunk piggyback callback
// invoked by the writer goroutine (advanceDeferred /
// processWholeMessage) under inlineMu. See the piggybackWUFn field
// comment for the contract. Set by the owning transport's
// constructor before any outbound write can occur.
func (w *shmFrameWriter) setPiggybackWUFn(fn func(streamID uint32)) {
	w.piggybackWUFn = fn
}

// setConnQuotaPtr publishes the transport's connSendQuota atomic
// pointer to the writer goroutine so processWholeMessage can
// CAS-reserve outbound conn-level flow-control credit. Called
// once at construction time before any sender enqueueMessageAndWait
// can fire; the happens-before of constructor → writer goroutine
// start guarantees visibility without explicit synchronisation.
func (w *shmFrameWriter) setConnQuotaPtr(p *atomic.Int64) {
	w.connQuota = p
}

// writeLoop is the single writer goroutine. It drains the channel and writes
// frames to the ring sequentially, eliminating the need for writeMu.
//
// Batching: when multiple frames are pending in the channel we wrap the
// loop in BeginBatch / EndBatch so per-Commit reader wakes are suppressed
// across the burst, paying one wake at EndBatch instead of one per frame.
// That's fine for small bursts (a dozen control frames + a few MESSAGEs)
// but turns into a producer/consumer LOCKSTEP when the burst contains
// many large MESSAGE entries: the writer can fill the ring while the
// reader is still parked on the pre-batch DataSequence, blocks waiting
// for space, and only a single deadlock-guard wake from ReserveWrite
// lets the reader make progress before the entire batch finishes.
//
// Empirical evidence: BenchmarkGRPCShmConcurrent / 1000 streams /
// size=65536 under shm-tuned dropped from ~1280 MB/s (fair-default,
// 16 KiB H2 frames) to ~460 MB/s (shm-tuned, single 64 KiB frames).
// Adding SHM_MAX_FRAME_SIZE=16384 on top of shm-tuned recovered to
// ~1240 MB/s, isolating the variable to H2 frame cadence -- i.e. the
// reader's per-Commit wake granularity. With chunking the inner
// emitH2DataFromCursor already opens nested ring/8 signal-batches,
// but those inner EndBatch calls only DECREMENT batchDepth (the
// signal still waits for the outer EndBatch). So chunking only helps
// because each entry's body is committed in finer reservation chunks,
// making the deadlock-guard wake fire earlier in single-entry mode.
// To pipeline at the writer-loop scale we need to release the batch
// every signalBatchBytes of accumulated body, opening a fresh one
// immediately so subsequent entries continue under suppression.
//
// signalBatchBytes is bounded to ring.Capacity()/8 to mirror the
// inner emitH2DataFromCursor pacing (and .NET's RingFrameStream
// chunkSize). Smaller values would amortise the wake cost too
// thinly; larger values bring back the lockstep.
func (w *shmFrameWriter) writeLoop() {
	defer w.wg.Done()

	for {
		// Fast path: non-blocking check. In unary ping-pong, the next
		// frame often arrives within microseconds of the previous write.
		// A Gosched+select avoids the full gopark/goready cycle (~1-3µs).
		var entry frameEntry
		var ok bool
		var haveEntry bool
		var retryWake bool
		select {
		case entry, ok = <-w.ch:
			if !ok {
				return
			}
			haveEntry = true
		default:
		}
		if !haveEntry {
			// Try wuRetryWake non-blocking before yielding so a pending
			// WU restore is observed without an extra scheduler round.
			select {
			case <-w.wuRetryWake:
				retryWake = true
			default:
			}
		}
		if !haveEntry && !retryWake {
			// Channel empty — yield briefly to let sender goroutine run,
			// then block. This mirrors the .NET WriterLoop yield pattern.
			runtime.Gosched()
			select {
			case entry, ok = <-w.ch:
				if !ok {
					return
				}
				haveEntry = true
			case <-w.wuRetryWake:
				retryWake = true
			}
		}

		w.inlineMu.Lock()
		// On wuRetryWake (errFrameWriterFull restore left bytes in the
		// pending atomic accumulators), drain them BEFORE processing any
		// queued data frames. This guarantees the restored WU credit
		// reaches the wire ahead of the next batch's DATA, closing the
		// force-WU liveness hole in the previous mutex-based restore.
		if retryWake && w.drainPendingWUFn != nil {
			w.drainPendingWUFn()
		}
		// On any wake (retryWake OR fresh entry), revisit deferred
		// whole-message entries. Incoming WU credit applied by the
		// reader's addSendQuota signals wuRetryWake; the deferred
		// entries may now be satisfiable. Running this on every wake
		// (not just retryWake) costs a near-noop len(map) check when
		// no entries are deferred — the common case.
		w.retryDeferred()
		if !haveEntry {
			// wuRetryWake-only iteration: nothing more to do after drain.
			w.inlineMu.Unlock()
			continue
		}
		// Check if more frames are queued behind this one.
		// Greedy non-blocking drain: bundles late arrivals that hit
		// the chan during processEntry into the same writev+SignalData
		// cycle (snapshot-then-drain would defer them to the next
		// outer-select trip). Capped at maxDrainPerPass so inlineMu
		// hold is bounded and wuRetryWake can be observed promptly.
		if len(w.ch) > 0 {
			signalBatchBytes := int(w.tx.Capacity() / 8)
			batchBytes := 0
			w.tx.BeginBatch()
			// connWUCoalescer merges adjacent conn-level WINDOW_UPDATE
			// frames into a single frame per drain pass. Cuts the WU
			// frame count at high stream concurrency (1000 streams
			// concurrently hitting onMessageStart can otherwise emit
			// 1000+ separate streamID==0 WU frames). hot-path safe
			// because absorb() is a tag check + uint64 add.
			coalescer := connWUCoalescer{w: w}
			if !coalescer.absorb(entry) {
				eb := entryBytes(entry)
				w.processEntry(entry)
				batchBytes += eb
			}
		Drain:
			for drained := 1; drained < maxDrainPerPass; drained++ {
				var (
					next frameEntry
					ok   bool
				)
				select {
				case next, ok = <-w.ch:
					if !ok {
						break Drain
					}
				default:
					break Drain
				}
				if coalescer.absorb(next) {
					// 4-byte WU frame contributes a fixed minor cost
					// regardless of how many are coalesced; account
					// roughly as a single WU payload to keep the
					// signal-batch bytes counter honest.
					batchBytes += 4
				} else {
					// Flush any accumulated WUs so they emit BEFORE
					// the non-conn-WU entry preserves HTTP/2 wire
					// ordering relative to the entry being processed.
					coalescer.flush()
					neb := entryBytes(next)
					w.processEntry(next)
					batchBytes += neb
				}
				// Periodically release the batch so the reader gets a
				// wake mid-burst and can drain in parallel with the
				// next group's writes. Skip when chan is already empty
				// to avoid wasted BeginBatch+EndBatch oscillation on
				// the final iteration.
				if batchBytes >= signalBatchBytes && len(w.ch) > 0 {
					// Flush any pending WU coalesce before signal so
					// the reader sees credits in this signal cycle,
					// not the next.
					coalescer.flush()
					w.tx.EndBatch()
					w.tx.BeginBatch()
					batchBytes = 0
				}
			}
			// Final flush before ending the batch to ensure no WU
			// bytes are stranded after the drain pass.
			coalescer.flush()
			w.tx.EndBatch()
		} else {
			w.processEntry(entry)
		}
		w.inlineMu.Unlock()
	}
}

// entryBytes returns an estimate of how many ring bytes a frameEntry
// consumes when written. Used by writeLoop's signal-batch flush
// accounting. Cheap to compute (no allocations) and only a hint --
// off-by-a-few-bytes is harmless since the threshold is ring/8.
func entryBytes(e frameEntry) int {
	if e.wholeMsg {
		return len(e.hdr) + e.data.Len()
	}
	if e.data != nil {
		return len(e.hdr) + e.data.Len()
	}
	return len(e.payload)
}

// connWUCoalescer accumulates adjacent connection-level WINDOW_UPDATE
// frames within a single writeLoop batch drain. It is a pure
// optimisation: emitting one combined `streamID==0` WU with the
// summed increment is semantically equivalent to emitting N separate
// WUs and reduces ring-write count + reader parse cost. Stream-level
// WUs are deliberately NOT coalesced (they target different
// streamIDs; merging across them would require a more complex
// per-stream map and the gains are smaller because stream WUs
// already fire infrequently under the SHM-tuned threshold).
//
// HTTP/2 ordering is preserved: coalescing only happens between
// adjacent entries in the writer's drain pass. A non-conn-WU entry
// (DATA, HEADERS, TRAILERS, GOAWAY, RST_STREAM, stream-level WU)
// forces the accumulator to flush before the entry is processed.
// This guarantees the wire order observable by the peer never
// differs from "all the WUs we would have sent individually, with
// the same relative ordering to DATA/HEADERS/etc."
//
// Coalescing is bounded by maxWindowSize (HTTP/2 31-bit cap): if an
// incoming increment would overflow, the accumulator first flushes
// (emitting the existing pending as one WU) and then absorbs the new
// entry into a fresh accumulator. In practice this never happens
// for SHM workloads (a single drain pass sees drip-on-receive WUs
// at limit/4 cadence whose sum is bounded by the negotiated conn
// window, well under the 2 GiB cap) but the guard is necessary
// for spec compliance.
type connWUCoalescer struct {
	w       *shmFrameWriter
	pending uint64
	ctx     context.Context
	hasAny  bool
}

// absorb returns true if the entry was consumed by the coalescer
// (added to the pending sum). Returns false if the caller must
// flush and process the entry normally. Entries that own data /
// have a doneCh are never absorbed because flushing them through a
// merged frame would lose the per-entry caller signal.
func (c *connWUCoalescer) absorb(entry frameEntry) bool {
	if entry.fh.Type != FrameTypeWindowUpdate || entry.fh.StreamID != 0 {
		return false
	}
	if entry.doneCh != nil || entry.data != nil {
		return false
	}
	if len(entry.payload) != 4 {
		return false
	}
	inc := uint64(binary.BigEndian.Uint32(entry.payload))
	if inc == 0 {
		return true // drop zero-increment WUs (no-op)
	}
	if c.pending+inc > uint64(maxWindowSize) {
		// Would overflow the HTTP/2 31-bit window cap; flush what we
		// have and start fresh with this entry. Caller doesn't see
		// "false" because we still absorbed (just under a flush).
		c.flush()
	}
	c.pending += inc
	c.hasAny = true
	if c.ctx == nil {
		c.ctx = entry.ctx
	}
	return true
}

// flush emits the accumulated pending increment as a single
// streamID==0 WINDOW_UPDATE frame. Must be called BEFORE processing
// any non-conn-WU entry that follows in the same drain pass, and at
// the end of every drain pass to ensure no pending bytes are left
// stranded if no follow-up arrives.
func (c *connWUCoalescer) flush() {
	if !c.hasAny {
		return
	}
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(c.pending))
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	c.w.processEntry(frameEntry{
		ctx:     ctx,
		fh:      FrameHeader{Type: FrameTypeWindowUpdate, StreamID: 0},
		payload: buf,
	})
	shmConnWUCoalesced.Add(1)
	c.pending = 0
	c.ctx = nil
	c.hasAny = false
}

// processEntry writes a single frame entry to the ring and signals
// completion to the caller if doneCh is set.
//
// Fire-and-forget entries (doneCh == nil) have no caller waiting for
// the result. If the ring write failed, we surface the error via
// onAsyncError (fired at most once per writer) so the owning transport
// can tear down rather than silently drop bytes.
func (w *shmFrameWriter) processEntry(entry frameEntry) {
	// Whole-message entries are dispatched via the per-stream
	// defer/retry machinery in processWholeMessage — short-circuit
	// the standard one-shot frame path below.
	if entry.wholeMsg {
		w.processWholeMessage(entry)
		return
	}
	// ZC marshal entries: marshal the proto.Message DIRECTLY into a
	// ring reservation here (under inlineMu), bypassing the upper-
	// layer codec.Marshal + tightBufferPool allocation that the
	// sender would otherwise pay. See enqueueProtoAndWait for the
	// caller-side contract.
	if entry.protoBytes != nil {
		w.processProtoEntry(entry)
		return
	}
	// TRAILERS frames carry the server-side end-of-stream + status
	// payload (per gRPC HTTP/2 mapping). They MUST be emitted strictly
	// AFTER any DATA already queued for the same stream — the server's
	// re-enabled async writeProto path puts DATA in deferredProto[sid]
	// or w.deferred[sid], and a naive emit here would overtake that
	// DATA and cause cardinality violation on the client. Route via
	// processTrailerEntry which defers into deferredTrailers when DATA
	// is still pending; the DATA-drain terminals call
	// maybeFlushDeferredTrailer to fire the pending TRAILERS once the
	// stream's DATA queue is empty. See grpc-go-shm-server-async-
	// trailer-sentinel-design memo.
	if entry.fh.Type == FrameTypeTRAILERS {
		w.processTrailerEntry(entry)
		return
	}
	var err error
	switch {
	case entry.data != nil:
		err = writeFrameBuffers(entry.ctx, w.tx, entry.fh, entry.hdr, entry.data)
	default:
		err = writeFrame(entry.ctx, w.tx, entry.fh, entry.payload)
	}
	if entry.doneCh != nil {
		entry.doneCh <- err
		return
	}
	if err != nil && w.onAsyncError != nil && w.errReported.CompareAndSwap(false, true) {
		w.onAsyncError(err)
	}
}

// processProtoEntry handles a ZC marshal request: the sender supplied
// an unmarshalled proto.Message; we marshal it directly into a ring
// reservation here under writeLoop's inlineMu. This is the queued
// fallback for senders whose writeProto fast path could not commit
// inline — either inlineMu.TryLock failed OR the inline lock-free
// CAS for outbound send-quota failed (window depleted / lost a race
// with a concurrent reservation). Instead of parking the sender on a
// per-stream signal under sendQuotaMu (the legacy slow path) the
// sender enqueues a fire-and-forget ZC entry and writeLoop owns FC
// reservation + defer + retry symmetrically with the chunked whole-
// message path.
//
// Caller MUST have:
//   - Pre-validated single-frame size bounds (Capacity/3,
//     h2MaxFramePayload, shmMaxFrameSize) — writeProto does this
//     before TryLock, so by the time the bail enqueue happens, the
//     entry is guaranteed to fit a single H2 DATA frame.
//
// Flow control: the writer goroutine owns the CAS reservation. On
// CAS failure the entry is appended to w.deferredProto[streamID]
// (per-stream FIFO slice — see field comment for ordering rationale)
// and retried on the next wuRetryWake via retryDeferred. On stream-
// local close (closeStream flips state to streamDone and fires
// wuRetryWake) the deferred entry is discarded silently — the upper
// layer already observed write success at enqueue time, so there is
// no caller to notify; downstream Send/Recv calls will observe
// errStreamDone via the stream state machine.
//
// Fire-and-forget semantics: entries arrive with doneCh == nil. Ring
// write errors surface through onAsyncError exactly as for other
// fire-and-forget control frame paths (HEADERS / GOAWAY senders).
// The transport tears down on a single failure; subsequent senders
// observe ErrConnClosing via t.closed.Load().
func (w *shmFrameWriter) processProtoEntry(entry frameEntry) {
	if w.closed.Load() {
		// Drop silently; sender already returned success. The
		// protoInFlight counter still needs to drain so future
		// transport.Close-time test assertions hold; on close all
		// streams go to streamDone anyway, so the counter value
		// becomes irrelevant.
		putAsyncProtoBuf(entry.protoBytes)
		if entry.streamPtr != nil {
			entry.streamPtr.protoInFlight.Add(-1)
		}
		return
	}
	s := entry.streamPtr
	if s == nil || w.connQuota == nil {
		// Misuse: ZC entry pushed without the required stream
		// pointer or before setConnQuotaPtr was wired up. Surface
		// via onAsyncError so the transport tears down.
		//
		// If s != nil (only connQuota wiring is missing), still
		// drain its in-flight counter so the upper layer's
		// resource-teardown invariants hold even on this
		// construction-order misuse path.
		putAsyncProtoBuf(entry.protoBytes)
		if s != nil {
			s.protoInFlight.Add(-1)
		}
		if w.onAsyncError != nil && w.errReported.CompareAndSwap(false, true) {
			w.onAsyncError(ErrConnClosing)
		}
		return
	}
	if s.getState() == streamDone || s.shmDataDropped.Load() {
		putAsyncProtoBuf(entry.protoBytes)
		s.protoInFlight.Add(-1)
		// Stream gone; discard any TRAILERS sentinel parked for it
		// (NOT flush — emitting OK-status after dropping DATA
		// would put the peer in a cardinality-violation state).
		// writeStatus sender will surface errStreamDone.
		w.discardPendingEndStream(entry.fh.StreamID)
		w.discardDeferredTrailer(entry.fh.StreamID, s)
		return
	}
	// Drop on ctx cancel before quota reservation — mirror the early
	// drop retryDeferredProto already does on its head entries.
	// Without this, a late cancel races into quota CAS + ring write
	// (ReserveWrite usually returns ctx err and the error path
	// classifies it as benign, but it still burns writer cycles).
	if entry.ctx.Err() != nil {
		putAsyncProtoBuf(entry.protoBytes)
		s.protoInFlight.Add(-1)
		// DATA dropped on ctx cancel — discard parked TRAILERS
		// rather than flush (same cardinality argument as
		// streamDone branch above).
		w.discardPendingEndStream(entry.fh.StreamID)
		w.discardDeferredTrailer(entry.fh.StreamID, s)
		return
	}
	sid := entry.fh.StreamID
	// FIFO order preservation: if this stream already has deferred
	// proto entries, append rather than attempt CAS. The sender's
	// inline path also checks s.protoInFlight under inlineMu before
	// doing its inline CAS — between the sender's check and now no
	// new entry for this stream can have raced past us via the
	// inline path (inlineMu serialises both). Within the writer
	// goroutine the chan-arrival order is FIFO and matches the
	// sender's call order; appending preserves the gRPC per-stream
	// message order invariant.
	if existing := w.deferredProto[sid]; len(existing) > 0 {
		w.deferredProto[sid] = append(existing, entry)
		// protoInFlight stays at its incremented value — entry
		// remains in flight until retryDeferredProto drains it.
		return
	}
	n := int64(5 + entry.protoSize)
	if !tryReserveSendQuota(w.connQuota, &s.sendQuota, n) {
		// Stalled on FC — defer for retry. Single-slot allocation
		// optimised for the common case (only one pending per
		// stream); slice growth covers back-to-back streaming sends.
		w.deferredProto[sid] = []frameEntry{entry}
		return
	}
	err := writeProtoBytesToRingH2Blocking(entry.ctx, w.tx, sid,
		entry.protoBytes, entry.fh.Flags)
	putAsyncProtoBuf(entry.protoBytes)
	if err != nil {
		// Refund the quota we just reserved — these bytes did not
		// land on the wire. ReserveWrite returns BEFORE Commit on
		// error, so the receiver never sees / charges them.
		w.connQuota.Add(n)
		s.sendQuota.Add(n)
		if w.onAsyncError != nil && w.errReported.CompareAndSwap(false, true) {
			w.onAsyncError(err)
		}
	}
	s.protoInFlight.Add(-1)
	// After this proto entry resolves the per-stream DATA queue may
	// now be empty. Choose flush vs discard by whether DATA landed:
	// success → emit parked TRAILERS; failure → discard (peer would
	// see TRAILERS without the missing DATA, cardinality violation).
	if err != nil {
		w.discardPendingEndStream(sid)
		w.discardDeferredTrailer(sid, s)
	} else {
		w.flushPendingEndStream(sid)
		w.flushDeferredTrailer(sid)
	}
}

// processTrailerEntry handles a TRAILERS frame entry submitted by the
// server-side writeStatus. If the stream still has DATA queued in
// w.deferred[sid] or w.deferredProto[sid], stash the entry in
// w.deferredTrailers[sid] (single-slot per stream; gRPC's one-
// writeStatus-per-stream contract guarantees no overlap). When the
// last pending DATA resolves at a DATA-drain terminal, that terminal
// calls flushDeferredTrailer (DATA landed on ring → emit trailer) or
// discardDeferredTrailer (DATA dropped → signal errStreamDone, skip
// wire write to avoid cardinality violation).
//
// If the stream has already transitioned to streamDone (RST_STREAM /
// closeStream / transport teardown raced ahead), the TRAILERS would
// be wire-noise to a peer that already saw the cancel; signal the
// sender with errStreamDone so its writeStatus surfaces the right
// error and skip the ring write.
//
// Runs under writeLoop's inlineMu — same context as every other
// w.processEntry dispatch — so accesses to w.deferred / w.deferredProto
// / w.deferredTrailers are race-free.
func (w *shmFrameWriter) processTrailerEntry(entry frameEntry) {
	sid := entry.fh.StreamID
	if entry.streamPtr != nil && (entry.streamPtr.getState() == streamDone || entry.streamPtr.shmDataDropped.Load()) {
		// Stream forcibly closed (streamDone) or a DATA entry was
		// already dropped (shmDataDropped) before we got here; don't
		// emit OK TRAILERS — the peer already saw RST or is missing
		// DATA (cardinality violation).
		if entry.doneCh != nil {
			entry.doneCh <- errStreamDone
		}
		return
	}
	// FIFO with same-stream DATA: if DATA is queued, defer.
	if _, ok := w.deferred[sid]; ok {
		w.deferredTrailers[sid] = entry
		return
	}
	if len(w.deferredProto[sid]) > 0 {
		w.deferredTrailers[sid] = entry
		return
	}
	w.emitTrailerEntry(entry)
}

// emitTrailerEntry writes the TRAILERS frame to the ring, transitions
// the stream's state to streamDone (the drop-signal for any later
// stray DATA — though by construction there should be none after
// writeStatus), clears late-credit, and signals the writeStatus
// sender's doneCh. Caller MUST have already verified no DATA is
// pending for this stream.
func (w *shmFrameWriter) emitTrailerEntry(entry frameEntry) {
	err := writeFrame(entry.ctx, w.tx, entry.fh, entry.payload)
	if entry.streamPtr != nil {
		// Swap state inside the writer at TRAILERS-emit time so
		// processProtoEntry's streamDone drop check happens-after
		// every DATA we just drained. compareAndSwapState is no-op
		// if state already advanced (close raced).
		entry.streamPtr.compareAndSwapState(streamActive, streamDone)
		// Match the previous writeStatus behaviour: clear pending
		// stream-level WU credit so a late producer's restore does
		// not leak into the next stream id reuse.
		entry.streamPtr.pendingWU.Store(0)
	}
	if entry.doneCh != nil {
		entry.doneCh <- err
	} else if err != nil && w.onAsyncError != nil && w.errReported.CompareAndSwap(false, true) {
		// Defensive: writeStatus always supplies doneCh; this branch
		// keeps parity with other fire-and-forget control frames.
		w.onAsyncError(err)
	}
}

// flushDeferredTrailer is called at every DATA-drain terminal where
// the DATA was successfully committed to the ring. If a TRAILERS
// entry is parked behind the just-drained DATA for stream `sid` AND
// no other DATA is still queued, emit the TRAILERS now.
//
// Cheap fast path: bare map probe on w.deferredTrailers[sid]. The
// map is typically empty (TRAILERS hit much less frequently than
// DATA), so the average cost is one hash lookup. When the entry
// exists we re-validate the deferral guards: another DATA queue may
// still hold entries for this stream (sibling map drains
// independently), in which case the trailer stays parked and the
// sibling's drain terminal will fire this helper again.
//
// Runs under writeLoop's inlineMu (same context as every DATA-drain
// terminal); accesses to w.deferred / w.deferredProto /
// w.deferredTrailers are race-free.
func (w *shmFrameWriter) flushDeferredTrailer(sid uint32) {
	entry, ok := w.deferredTrailers[sid]
	if !ok {
		return
	}
	if _, hasWhole := w.deferred[sid]; hasWhole {
		return
	}
	if len(w.deferredProto[sid]) > 0 {
		return
	}
	delete(w.deferredTrailers, sid)
	// Drop-tombstone gate: if a prior DATA-drop terminal already
	// tombstoned this stream (via discardDeferredTrailer setting
	// shmDataDropped), do NOT emit OK trailers — that drop means the
	// peer is missing at least one DATA frame, and emitting OK trailers
	// here would produce the cardinality violation the trailer-sentinel
	// design exists to prevent. This branch is reached only in the rare
	// sibling-pending interleave: one DATA queue dropped (tombstoned)
	// while the other queue's drain eventually succeeded and called
	// flush. (streamDone is also checked in case a real closer raced.)
	if entry.streamPtr != nil && (entry.streamPtr.getState() == streamDone || entry.streamPtr.shmDataDropped.Load()) {
		if entry.doneCh != nil {
			entry.doneCh <- errStreamDone
		}
		return
	}
	w.emitTrailerEntry(entry)
}

// discardDeferredTrailer is called at every DATA-drain terminal
// where the DATA was DISCARDED (stream went streamDone, ctx
// cancelled, ring write errored mid-emission, partial-commit). If a
// TRAILERS entry is parked behind that DATA, emitting it now would
// put the peer in a cardinality-violation state (it sees TRAILERS
// with one or more missing DATA messages — exactly the bug that the
// original synchronous server writeProto path was carved out to
// avoid). Instead, signal errStreamDone to the writeStatus sender
// and SKIP the ring write.
//
// CRITICAL — TOCTOU close: the `s *streamBase` parameter is the stream
// whose DATA was just dropped. We unconditionally CAS it to
// streamDone BEFORE inspecting the parked trailer. This closes the
// race where:
//
//  1. async DATA enqueued by server writeProto
//  2. async DATA drops at this terminal (ctx.Err / ring-write-err /
//     already-streamDone)
//  3. writeStatus has NOT yet enqueued the trailer when we get here
//     → w.deferredTrailers[sid] absent → the parked-trailer check
//     below early-returns
//  4. writeStatus later enqueues the trailer
//  5. processTrailerEntry sees no deferred DATA AND streamActive
//     (the old bug: state was never transitioned because no trailer
//     was parked to swap on) → emits OK trailers on the wire →
//     peer cardinality violation
//
// Doing the CAS first makes processTrailerEntry's existing
// streamDone check (at the top of that function) fire on the
// late-arriving trailer, signalling errStreamDone to the
// writeStatus sender and skipping the wire write.
//
// Sibling-queue guard: if the OTHER DATA queue still has entries
// for this stream, the parked trailer (if any) stays parked until
// the sibling drains. The stream-state CAS is still applied so
// processTrailerEntry's check would also catch a fresh
// late-arriving trailer — but in this branch a trailer was
// already parked at trailer-enqueue time, so processTrailerEntry
// already deferred it correctly; this path is just the cleanup
// when the sibling's drain terminal eventually fires.
//
// Idempotent (the CAS and the delete are the only state-mutating
// steps; both are no-ops on repeat).
//
// Runs under writeLoop's inlineMu (see flushDeferredTrailer comment).
func (w *shmFrameWriter) discardDeferredTrailer(sid uint32, s *streamBase) {
	if s != nil {
		// Tombstone the stream FIRST (via the dedicated shmDataDropped
		// flag, NOT the streamDone state) so a late-arriving trailer
		// (writeStatus called AFTER us) is rejected by
		// processTrailerEntry's drop check, while leaving the streamDone
		// state reserved for closeStream. Setting streamDone here would
		// impersonate a closer and deadlock a later closeStream on
		// <-s.done (nothing else closes a ClientStream's done channel).
		s.shmDataDropped.Store(true)
		s.pendingWU.Store(0)
	}
	entry, ok := w.deferredTrailers[sid]
	if !ok {
		return
	}
	if _, hasWhole := w.deferred[sid]; hasWhole {
		return
	}
	if len(w.deferredProto[sid]) > 0 {
		return
	}
	delete(w.deferredTrailers, sid)
	if entry.doneCh != nil {
		entry.doneCh <- errStreamDone
	}
}

// flushPendingEndStream emits a parked zero-length END_STREAM (client
// half-close, from CloseSend) for stream sid once ALL earlier DATA for
// that stream has drained. Co-located with flushDeferredTrailer at every
// successful DATA-drain terminal. No-op when no END_STREAM is parked or
// when DATA is still queued for the stream. Runs under writeLoop's
// inlineMu.
func (w *shmFrameWriter) flushPendingEndStream(sid uint32) {
	entry, ok := w.pendingEndStream[sid]
	if !ok {
		return
	}
	// Still ordered behind pending DATA — a later DATA-drain terminal
	// fires this helper again once both queues are empty.
	if _, hasWhole := w.deferred[sid]; hasWhole {
		return
	}
	if len(w.deferredProto[sid]) > 0 {
		return
	}
	delete(w.pendingEndStream, sid)
	// Drop rather than emit if the stream was closed/tombstoned or the
	// CloseSend ctx was cancelled while the END_STREAM was parked.
	if s := entry.streamPtr; (s != nil && (s.getState() == streamDone || s.shmDataDropped.Load())) || entry.ctx.Err() != nil {
		if entry.doneCh != nil {
			entry.doneCh <- errStreamDone
		}
		entry.data.Free()
		return
	}
	w.emitEmptyEndStream(entry)
}

// discardPendingEndStream drops a parked zero-length END_STREAM WITHOUT
// emitting it (the DATA it was ordered behind was dropped). Wakes the
// blocked CloseSend caller with errStreamDone. Co-located with
// discardDeferredTrailer at every DATA-drop terminal. No-op when none is
// parked. Runs under writeLoop's inlineMu.
func (w *shmFrameWriter) discardPendingEndStream(sid uint32) {
	entry, ok := w.pendingEndStream[sid]
	if !ok {
		return
	}
	delete(w.pendingEndStream, sid)
	if entry.doneCh != nil {
		entry.doneCh <- errStreamDone
	}
	entry.data.Free()
}

// emitEmptyEndStream writes the single zero-length H2 DATA frame that
// carries a client half-close (END_STREAM when isLast, else MORE),
// wakes the blocked CloseSend caller, and releases the retained data
// reference. Shared by the immediate (no pending DATA) path in
// processWholeMessage and the deferred flushPendingEndStream path.
func (w *shmFrameWriter) emitEmptyEndStream(entry frameEntry) {
	fh := entry.fh
	if entry.isLast {
		fh.Flags = MessageFlagEndStream
	} else {
		fh.Flags = MessageFlagMORE
	}
	_, h2f := translateCustomToH2(fh)
	err := writeH2Single(entry.ctx, w.tx, H2FrameDATA, h2f, fh.StreamID, nil)
	if err == nil && w.piggybackWUFn != nil {
		w.piggybackWUFn(fh.StreamID)
	}
	if entry.doneCh != nil {
		entry.doneCh <- err
	}
	// Balance the Ref taken in enqueueMessageAndWait.
	entry.data.Free()
}

// enqueueProtoAsync pushes a ZC marshal request onto the writer
// channel fire-and-forget. The sender does NOT block on completion.
//
// Used by (*ShmClientTransport|ShmServerTransport).writeProto when
// either the inline TryLock fails OR the inline lock-free CAS for
// outbound send-quota fails. Replaces the legacy slow path
// (acquireSendQuota park on per-stream signal under sendQuotaMu +
// connWaiters FIFO + register/unregister/notifyQuotaChangeLocked
// dispatch) with a single chan-hop: the writer goroutine owns FC
// reservation + defer + retry symmetrically with the chunked whole-
// message path, eliminating the parallel slow-path machinery that
// previously parked ~10% of senders at fair-default 1000/4K.
//
// Pre-conditions (caller MUST satisfy):
//   - Single-frame size bounds pre-validated.
//   - opts.Last → stream state already CAS'd to streamWriteDone
//     (semantic transition happens-before the upper-layer return).
//   - NO send-quota pre-reserved. The writer's processProtoEntry
//     does the CAS reservation under its own context.
//
// Errors: returns ErrConnClosing if the chan is full or the writer
// is closed. The frameWriterQueueSize=2048 buffer makes "full"
// extremely rare under realistic 1000-stream workloads; if it does
// occur the transport is so backed up that returning an error to
// the caller is the correct semantics. On success the entry is
// guaranteed to be processed (or silently dropped on stream/
// transport close, both of which the upper layer observes via the
// stream state machine).
func (w *shmFrameWriter) enqueueProtoAsync(ctx context.Context, streamPtr *streamBase, fh FrameHeader, protoBytes []byte, pSize int) error {
	entry := frameEntry{
		ctx:        ctx,
		fh:         fh,
		streamPtr:  streamPtr,
		protoBytes: protoBytes,
		protoSize:  pSize,
	}
	if !w.trySend(entry) {
		return ErrConnClosing
	}
	return nil
}

// processWholeMessage handles a whole-message entry. The caller
// goroutine is blocked on entry.doneCh until the LAST chunk lands
// on the ring; WL chunks the message internally according to the
// current outbound flow-control window.
//
// CAS protocol (mirrors tryReserveSendQuota): for each chunk,
// atomically deduct min(connQ, streamQ, remaining) from
// both the conn and per-stream quotas. On CAS race-loss, retry.
// On insufficient quota, install the partial state into
// w.deferred[streamID] and return — WL moves on to the next entry
// and revisits this deferred message when wuRetryWake fires.
//
// Runs under inlineMu (writeLoop has already acquired it). The
// piggyback-WU callback (if installed) fires once after each
// successfully-emitted chunk so receiver-side WU credit accrued
// during this drain pass rides out alongside our DATA.
func (w *shmFrameWriter) processWholeMessage(entry frameEntry) {
	if entry.streamPtr == nil || w.connQuota == nil {
		// Misuse: whole-message entry pushed without the required
		// stream pointer or before setConnQuotaPtr was wired up.
		if entry.doneCh != nil {
			entry.doneCh <- ErrConnClosing
		}
		// Balance the Ref taken in enqueueMessageAndWait.
		entry.data.Free()
		return
	}
	payloadLen := len(entry.hdr) + entry.data.Len()
	if payloadLen == 0 {
		// Zero-length MESSAGE — most commonly a client-side half-close
		// (cs.Write(nil, nil, {Last:true})). No flow-control bytes to
		// reserve; emit a single H2 DATA frame with empty payload
		// carrying END_STREAM (when isLast) or MORE.
		//
		// Honour the writer tombstone / stream close: if this stream's
		// DATA was already dropped (shmDataDropped) or the stream is
		// closed (streamDone), do NOT emit — signal errStreamDone to the
		// blocked CloseSend caller. Per-stream FIFO ordering (the
		// END_STREAM must not overtake still-deferred DATA on the same
		// stream) is enforced immediately below via the pendingEndStream
		// sentinel.
		if entry.streamPtr.getState() == streamDone || entry.streamPtr.shmDataDropped.Load() {
			entry.doneCh <- errStreamDone
			// Balance the Ref taken in enqueueMessageAndWait.
			entry.data.Free()
			return
		}
		// Per-stream FIFO: if this stream still has DATA queued — a
		// chunked whole-message in w.deferred[sid] or async proto
		// entries in w.deferredProto[sid] — the END_STREAM MUST wait
		// behind them; emitting it now would let the peer see the
		// half-close BEFORE the DATA (gRPC per-stream message-order
		// violation). Park it in the pendingEndStream sentinel; a
		// later DATA-drain terminal fires flushPendingEndStream once
		// both queues are empty.
		sid := entry.fh.StreamID
		if _, dup := w.pendingEndStream[sid]; dup {
			// Duplicate CloseSend (unreachable given gRPC's one-Last-
			// per-stream contract); reject rather than overwrite (that
			// would leak the prior data reference).
			entry.doneCh <- errStreamDone
			entry.data.Free()
			return
		}
		_, hasWhole := w.deferred[sid]
		if hasWhole || len(w.deferredProto[sid]) > 0 {
			w.pendingEndStream[sid] = entry
			return
		}
		// No pending DATA — emit immediately.
		w.emitEmptyEndStream(entry)
		return
	}
	// PR-B: reuse Stream's inline-allocated deferred slot instead of
	// fresh heap alloc. gRPC's one-SendMsg-per-stream invariant
	// guarantees s.shmDeferred is not currently held by another
	// in-flight Send (any previous SendMsg has already signalled
	// its doneCh and the caller-blocking sender returned, releasing
	// the slot back to us logically). The writer's
	// w.deferred[streamID] map still owns the lifecycle pointer.
	d := &entry.streamPtr.shmDeferred
	d.ctx = entry.ctx
	d.streamPtr = entry.streamPtr
	d.fh = entry.fh
	d.cur = vecCursor{lpmHdr: entry.hdr, data: entry.data}
	d.origData = entry.data
	d.remaining = payloadLen
	d.isLast = entry.isLast
	d.doneCh = entry.doneCh
	// Per-stream FIFO vs the async ZC proto path. If this stream
	// already has deferred proto entries waiting on FC, the chan
	// arrival order put those entries BEFORE this whole-message
	// (the sender's inline path bails when len(w.ch) > 0 OR when
	// any stream has deferred proto pending — see tryInlineWrite).
	// Emitting this whole-message before retryDeferredProto drains
	// the proto queue would violate gRPC per-stream message order.
	// Install in w.deferred[sid] (sender already holds doneCh; the
	// at-most-one-whole-message-per-stream invariant guarantees no
	// pre-existing entry is overwritten — write() callers block on
	// doneCh until resolution).
	if len(w.deferredProto[entry.fh.StreamID]) > 0 {
		w.deferred[entry.fh.StreamID] = d
		return
	}
	w.advanceDeferred(entry.fh.StreamID, d)
}

// advanceDeferred attempts to emit as many chunks of d as the
// current outbound FC window permits. Three outcomes:
//
//  1. d.remaining drops to 0 — message fully sent; doneCh receives
//     nil; map entry deleted.
//  2. Ring write fails — doneCh receives the error; map entry
//     deleted.
//  3. CAS shows insufficient quota — d is installed into
//     w.deferred[streamID]; WL returns to drain the next entry,
//     will revisit d on the next wuRetryWake.
//
// Runs under inlineMu. The CAS rollback path (conn-CAS fails after
// stream-CAS succeeded) Adds the deducted bytes back to the stream
// quota; this can transiently push stream quota above its initial
// limit but is harmless under HTTP/2 semantics (the 2^31-1 cap is
// the only true upper bound and a single chunk is < 16 MiB).
func (w *shmFrameWriter) advanceDeferred(streamID uint32, d *deferredMessage) {
	for d.remaining > 0 {
		// Stream-local close: closeStream swaps state to streamDone
		// and fires wuRetryWake so we land here on the next pass. If
		// we observe streamDone we must signal errStreamDone and
		// remove the entry — emitting more DATA for a locally-closed
		// stream is a wire-protocol violation against any peer that
		// has already seen the RST/CANCEL the close path enqueued.
		if d.streamPtr.getState() == streamDone || d.streamPtr.shmDataDropped.Load() {
			select {
			case d.doneCh <- errStreamDone:
			default:
			}
			delete(w.deferred, streamID)
			// Balance the Ref taken in enqueueMessageAndWait.
			d.release()
			// DATA dropped on stream-local close — if a TRAILERS
			// sentinel is parked behind it, discard rather than
			// emit. Emitting OK-status TRAILERS after dropping DATA
			// would put the peer in a cardinality-violation state.
			w.discardPendingEndStream(streamID)
			w.discardDeferredTrailer(streamID, d.streamPtr)
			return
		}
		// Observe ctx cancellation — the sender goroutine in
		// enqueueMessageAndWait selects on the SAME ctx and will
		// have already left with ctx.Err(), so any further chunk
		// emission for this entry is wasted work. Signal doneCh
		// (buffered; sender already drained or never will) and
		// remove the entry.
		if d.ctx.Err() != nil {
			select {
			case d.doneCh <- ContextErr(d.ctx.Err()):
			default:
			}
			delete(w.deferred, streamID)
			// Balance the Ref taken in enqueueMessageAndWait.
			d.release()
			// DATA dropped on ctx cancellation — discard the
			// parked TRAILERS sentinel rather than emit (see
			// streamDone branch above for rationale).
			w.discardPendingEndStream(streamID)
			w.discardDeferredTrailer(streamID, d.streamPtr)
			return
		}
		streamQ := d.streamPtr.sendQuota.Load()
		if streamQ <= 0 {
			w.deferred[streamID] = d
			return
		}
		connQ := w.connQuota.Load()
		if connQ <= 0 {
			w.deferred[streamID] = d
			return
		}
		grant := int64(d.remaining)
		if streamQ < grant {
			grant = streamQ
		}
		if connQ < grant {
			grant = connQ
		}
		// LPM-header atomicity. The 5-byte gRPC LPM header MUST be
		// delivered to the receiver in a contiguous DATA-frame body
		// because the receive-side lpmAccumulator can only set its
		// expectedTotal (which gates the onMessageStart stream-FC
		// pre-credit hook) AFTER the full 5-byte header is parsed
		// in a single feed() call. If the sender emits a chunk
		// shorter than the remaining LPM-header bytes, the receiver
		// sees a partial header, expectedTotal stays 0, the
		// onMessageStart hook does not fire, no stream-level
		// pre-credit is emitted, and the receiver's onData check on
		// the NEXT chunk trips because pendingData crosses the
		// per-stream limit while delta is still 0. This produces
		// the 1 MiB-jumbo `Send: EOF` bench failure (rounds 0-31
		// depending on concurrency) — confirmed by GRPC_SHM_DEBUG=1
		// reproducer showing `delta=0` at the violation site.
		//
		// Defer until conn / stream credit can cover at least the
		// remaining header bytes.
		if hdrRemaining := int64(len(d.cur.lpmHdr)); hdrRemaining > 0 && grant < hdrRemaining {
			w.deferred[streamID] = d
			return
		}
		if !d.streamPtr.sendQuota.CompareAndSwap(streamQ, streamQ-grant) {
			continue // CAS lost — retry
		}
		if !w.connQuota.CompareAndSwap(connQ, connQ-grant) {
			d.streamPtr.sendQuota.Add(grant) // rollback
			shmCASRollback.Add(1)
			continue
		}
		// Determine the last-chunk flag: END_STREAM only fires when
		// this grant covers the rest of the message AND the caller
		// asked for end-of-stream.
		fh := d.fh
		if grant == int64(d.remaining) && d.isLast {
			fh.Flags = MessageFlagEndStream
		} else {
			fh.Flags = MessageFlagMORE
		}
		_, h2f := translateCustomToH2(fh)
		committed, err := emitH2DataFromCursor(d.ctx, w.tx, streamID, &d.cur, int(grant), h2f)
		if err != nil {
			// Partial-commit-aware refund. emitH2DataFromCursor may
			// have committed `committed` bytes to the ring BEFORE
			// failing (the peer has those bytes and will charge them
			// against its inbound window). Refund only the uncommitted
			// remainder; refunding the full grant would inflate the
			// conn-level send quota by `committed` bytes and let a
			// later send on a different stream overshoot the receiver's
			// actual window. Track stream send quota the same way.
			refund := grant - int64(committed)
			if refund > 0 {
				d.streamPtr.sendQuota.Add(refund)
				w.connQuota.Add(refund)
			}
			d.doneCh <- err
			delete(w.deferred, streamID)
			// Balance the Ref taken in enqueueMessageAndWait.
			d.release()
			// Ring write errored mid-emission — peer may have a
			// partial LPM on the wire already. Discard the parked
			// TRAILERS sentinel; emitting OK-status now would
			// compound the protocol violation.
			w.discardPendingEndStream(streamID)
			w.discardDeferredTrailer(streamID, d.streamPtr)
			return
		}
		d.remaining -= int(grant)
		if w.piggybackWUFn != nil {
			w.piggybackWUFn(streamID)
		}
	}
	// Fully sent.
	d.doneCh <- nil
	delete(w.deferred, streamID)
	// Balance the Ref taken in enqueueMessageAndWait.
	d.release()
	// DATA successfully on the ring — fire any parked END_STREAM
	// (CloseSend half-close) and TRAILERS sentinel for this stream.
	w.flushPendingEndStream(streamID)
	w.flushDeferredTrailer(streamID)
}

// retryDeferred is called by the writeLoop on every wuRetryWake.
// It walks the deferred maps and attempts to make progress on each
// stalled entry. Runs under inlineMu.
//
// Two maps are walked:
//
//   - w.deferred (whole-message chunked path): iteration is map-
//     random; advanceDeferred may delete sid during the loop, which
//     Go's spec permits during range.
//   - w.deferredProto (ZC proto fire-and-forget path): for each
//     stream's FIFO slice, pop entries from the head as long as CAS
//     succeeds. On first CAS failure for a stream, stop and leave
//     the rest of the slice for the next wuRetryWake — preserving
//     per-stream message order. If a stream's slice empties, delete
//     the map entry.
//
// Iteration across streams is map-random for fairness under high
// concurrency. A starving stream will eventually be reached as WU
// credits accumulate; in practice the receiver-side WU emitter
// drives wuRetryWake at sub-millisecond cadence so the latency
// cost of map-random is negligible.
//
// Ordering invariant: process w.deferredProto FIRST. Any whole-
// message entry sitting in w.deferred[sid] was queued AFTER the
// proto entries that landed in deferredProto[sid] (processWholeMessage
// installs the whole-message in w.deferred[sid] when it observes a
// non-empty deferredProto[sid] head). Emitting the whole-message
// before the proto queue drains would violate gRPC per-stream
// message order.
func (w *shmFrameWriter) retryDeferred() {
	if len(w.deferredProto) > 0 {
		for sid, queue := range w.deferredProto {
			w.retryDeferredProto(sid, queue)
		}
	}
	if len(w.deferred) > 0 {
		for sid, d := range w.deferred {
			// Skip if this stream still has pending async proto
			// entries — they must drain first to preserve FIFO.
			// retryDeferredProto above may have left some entries
			// in deferredProto[sid] if FC was insufficient; revisit
			// on the next wuRetryWake.
			if len(w.deferredProto[sid]) > 0 {
				continue
			}
			// advanceDeferred may delete sid from the map; Go's
			// spec guarantees this is safe during a range loop
			// (the iterator observes the new state going forward).
			w.advanceDeferred(sid, d)
		}
	}
}

// retryDeferredProto drains as many head entries of queue as the
// current outbound FC window permits, preserving per-stream FIFO.
// Stops at the first head whose CAS fails (insufficient credit) or
// whose stream has closed in the interim. Updates / deletes the
// map entry as needed. Runs under inlineMu (caller's invariant).
//
// Each "resolved" entry (written, errored, dropped on close, dropped
// on ctx cancel) decrements its stream's protoInFlight counter so
// that subsequent senders can resume the inline fast path once the
// async pipeline drains.
func (w *shmFrameWriter) retryDeferredProto(sid uint32, queue []frameEntry) {
	emitted := 0
	dropped := false
	// dropStream tracks the Stream object whose entries we drop so
	// that discardDeferredTrailer below can tombstone it to
	// streamDone for the TOCTOU close (late-arriving writeStatus on
	// a stream whose async DATA was dropped must NOT emit OK
	// trailers). All entries for a given sid share the same Stream
	// pointer, so the first non-nil one observed at a drop site is
	// authoritative.
	var dropStream *streamBase
	for emitted < len(queue) {
		entry := queue[emitted]
		s := entry.streamPtr
		// Stream-local close → drop remaining entries silently;
		// upper layer sees errStreamDone via stream state machine
		// the next time it touches the stream.
		if s == nil || s.getState() == streamDone || s.shmDataDropped.Load() {
			// Decrement protoInFlight for every drained entry —
			// the count must drain to zero so the stream's resource
			// teardown can complete without a leaked debit.
			for j := emitted; j < len(queue); j++ {
				putAsyncProtoBuf(queue[j].protoBytes)
				if queue[j].streamPtr != nil {
					queue[j].streamPtr.protoInFlight.Add(-1)
					if dropStream == nil {
						dropStream = queue[j].streamPtr
					}
				}
			}
			emitted = len(queue)
			dropped = true
			break
		}
		if entry.ctx.Err() != nil {
			// Context cancellation: same as stream close — drop and
			// move to the next head. Upper layer's deadline already
			// fired and surfaced via ctx.Err() on the recv side.
			putAsyncProtoBuf(entry.protoBytes)
			s.protoInFlight.Add(-1)
			emitted++
			dropped = true
			if dropStream == nil {
				dropStream = s
			}
			continue
		}
		n := int64(5 + entry.protoSize)
		if !tryReserveSendQuota(w.connQuota, &s.sendQuota, n) {
			// Still stalled at this head; leave the queue alone and
			// revisit on the next wuRetryWake.
			break
		}
		err := writeProtoBytesToRingH2Blocking(entry.ctx, w.tx, sid,
			entry.protoBytes, entry.fh.Flags)
		putAsyncProtoBuf(entry.protoBytes)
		if err != nil {
			// Refund and tear down; subsequent senders see
			// ErrConnClosing via t.closed.Load(). Break out: a
			// ring-write error means the transport is dying;
			// retrying the next queued entry will hit the same
			// failure and just burn writer cycles. The remaining
			// entries are accounted for at the if-emitted-not-
			// equal-len-queue tail-compaction branch below; on the
			// next wuRetryWake the transport-close path will drain
			// them via the close-time deferredProto walk.
			w.connQuota.Add(n)
			s.sendQuota.Add(n)
			if w.onAsyncError != nil && w.errReported.CompareAndSwap(false, true) {
				w.onAsyncError(err)
			}
			dropped = true
			if dropStream == nil {
				dropStream = s
			}
			s.protoInFlight.Add(-1)
			emitted++
			break
		}
		s.protoInFlight.Add(-1)
		emitted++
	}
	if emitted >= len(queue) {
		delete(w.deferredProto, sid)
		// If ANY entry was dropped (streamDone, ctx-cancel, ring
		// write err), the parked TRAILERS sentinel must NOT fire on
		// the wire — emitting OK-status TRAILERS after dropping one
		// or more DATA messages would put the peer in a cardinality
		// violation. discardDeferredTrailer signals errStreamDone
		// to the writeStatus sender and skips the wire write. Only
		// the all-success path flushes (emits) the trailer.
		if dropped {
			w.discardPendingEndStream(sid)
			w.discardDeferredTrailer(sid, dropStream)
		} else {
			w.flushPendingEndStream(sid)
			w.flushDeferredTrailer(sid)
		}
		return
	}
	if emitted > 0 {
		// Compact: drop the drained prefix. Re-slice keeps the
		// backing array; under steady-state the queue rarely grows
		// beyond 1-2 entries so the wasted prefix capacity is
		// negligible. A fresh allocation here would be measurable
		// at high CAS-fail rates.
		w.deferredProto[sid] = append(queue[:0], queue[emitted:]...)
	}
}

// trySend attempts to send an entry to the channel under closeMu.RLock.
// Returns false if the writer has been closed.
func (w *shmFrameWriter) trySend(entry frameEntry) bool {
	w.closeMu.RLock()
	defer w.closeMu.RUnlock()
	if w.closed.Load() {
		return false
	}
	w.ch <- entry
	return true
}

// errFrameWriterFull signals that an enqueue attempted via
// enqueueOrInlineNonBlocking could not complete because the writer was
// neither idle nor able to accept an entry on its async channel without
// blocking. Callers MUST treat this as recoverable (e.g., re-buffer the
// pending state) rather than dropping the work; the frame writer will
// catch up and a future enqueue attempt will succeed.
var errFrameWriterFull = errors.New("shm frame writer: channel full, would block")

// doneChPool reuses buffered-1 error channels across slow-path
// enqueue calls (enqueueAndWait + enqueueMessageAndWait). The chan
// itself can't live on the caller's stack (Go channels are heap), so
// pooling is the only no-alloc option. Steady-state under the
// N=1000/4 K fair-default bench: ~800 K make(chan error, 1) calls per
// second eliminated.
//
// Safety: after each sender's recv(<-doneCh), the chan is empty (it
// was buffered=1 and contained exactly one value the writer sent).
// We assert empty via a non-blocking recv before returning to the
// pool to defend against future misuse (e.g., a writer sending twice).
// The ctx-cancel branch of enqueueMessageAndWait intentionally does
// NOT return to the pool because the writer may still send into
// doneCh after the cancel (race window) — a pooled re-use would
// then leak that late result to a different sender. Heap-allocating
// on cancel keeps that path safe.
var doneChPool = sync.Pool{
	New: func() any { return make(chan error, 1) },
}

func getDoneCh() chan error {
	return doneChPool.Get().(chan error)
}

func putDoneCh(ch chan error) {
	// Defensive drain — under correct use this is always empty already.
	select {
	case <-ch:
	default:
	}
	doneChPool.Put(ch)
}

// enqueueOrInlineNonBlocking is a strictly non-blocking inline-or-async
// enqueue. It is the ONLY safe enqueue path for callers that MUST NOT
// block — most importantly the SHM reader goroutine, which is
// responsible for committing inbound ring bytes and waking peers.
//
// If the reader were to block on the outbound writer (which is what a
// blocking trySend can cause), it would create a transport-level
// deadlock: the outbound ring fills because the peer reader is
// blocked the same way; the writer can't drain its channel because
// its ring writes block; the reader can't enqueue the WINDOW_UPDATE
// that would unblock the peer.
//
// Behavior:
//   - inlineMu available → write inline, return nil on success.
//   - inlineMu busy + async channel has space → enqueue, return nil.
//   - inlineMu busy + async channel full → return errFrameWriterFull
//     IMMEDIATELY without touching the channel. The caller is
//     responsible for re-buffering whatever state was committed to
//     this emission (e.g., transport.pendingConnWU /
//     Stream.pendingWU atomics) and signalling wuRetryWake so the
//     writer loop drains the restored value on its next tick.
//
// Use this from sendWindowUpdate when called via reader callbacks
// (onDataFrameReceived, onMessageStart). App goroutines on the
// sender Write path use trySend / enqueueMessageAndWait instead.
func (w *shmFrameWriter) enqueueOrInlineNonBlocking(entry frameEntry) error {
	w.closeMu.RLock()
	if w.closed.Load() {
		w.closeMu.RUnlock()
		return ErrConnClosing
	}
	if w.inlineMu.TryLock() {
		// Available-precheck: ensure the ring has space for this
		// frame BEFORE entering writeFrame (which would block in
		// ReserveWrite). The current callers of this function are
		// reader-side WU emitters (sendWindowUpdate from both client
		// and server transports, fired by notifyDataFrameConsumed
		// during the inbound DATA reservation window). If the outbound
		// ring is full AND inline TryLock succeeds, writeFrame's
		// ReserveWrite parks the reader goroutine WHILE the reader is
		// still holding an uncommitted inbound DATA reservation —
		// symmetrically the peer's reader can be in the same state
		// and neither side frees ring space for the other. The
		// Available-check breaks this deadlock window: when the
		// outbound is full we bail to the (non-blocking) chan path,
		// which lets the caller continue + restore credit via
		// errFrameWriterFull and ping wuRetryWake; the reader then
		// commits its inbound DATA, peer's writer unblocks, and the
		// queued WU eventually drains via writeLoop.
		//
		// The Available() Load is racy vs concurrent producers, but
		// (a) the per-WU frame size is small (~13 B) so a false
		// positive is essentially impossible in practice, and (b) a
		// false negative just means we take the chan path that one
		// time — correctness-neutral.
		var size int
		if entry.data != nil {
			size = h2FrameHeaderSize + len(entry.hdr) + entry.data.Len()
		} else {
			size = h2FrameHeaderSize + len(entry.payload)
		}
		if w.tx.Available() >= uint64(size) {
			var err error
			if entry.data != nil {
				err = writeFrameBuffers(entry.ctx, w.tx, entry.fh, entry.hdr, entry.data)
			} else {
				err = writeFrame(entry.ctx, w.tx, entry.fh, entry.payload)
			}
			w.inlineMu.Unlock()
			w.closeMu.RUnlock()
			return err
		}
		// Ring lacks space — release inlineMu and fall through to
		// the non-blocking chan send. Writer goroutine will pick up
		// the entry when ring space frees.
		w.inlineMu.Unlock()
	}
	// inlineMu busy or ring full; try non-blocking channel send.
	select {
	case w.ch <- entry:
		w.closeMu.RUnlock()
		return nil
	default:
		w.closeMu.RUnlock()
		return errFrameWriterFull
	}
}

// tryEnqueueNonBlocking attempts to send a frame without blocking.
// Used for best-effort frames (GOAWAY) in Close() where blocking would
// deadlock if the channel is full (writer goroutine stuck on ring write).
func (w *shmFrameWriter) tryEnqueueNonBlocking(entry frameEntry) bool {
	w.closeMu.RLock()
	defer w.closeMu.RUnlock()
	if w.closed.Load() {
		return false
	}
	select {
	case w.ch <- entry:
		return true
	default:
		return false
	}
}

// enqueue submits a frame for asynchronous writing. Returns ErrConnClosing
// if the writer has been closed or is racing with close.
func (w *shmFrameWriter) enqueue(entry frameEntry) error {
	if w.closed.Load() {
		return ErrConnClosing
	}
	if !w.trySend(entry) {
		return ErrConnClosing
	}
	return nil
}

// enqueueAndWait submits a frame and blocks until the write completes.
//
// Fast path: if the inline mutex is available (writer goroutine is idle
// or between entries), the caller executes the write directly in its own
// goroutine. This avoids channel send + goroutine scheduling (~100-200ns).
//
// The fast path holds closeMu.RLock around the closed check + inlineMu
// acquisition + ring write to prevent close() from completing (and the
// transport from unmapping the segment) while the write is in progress.
func (w *shmFrameWriter) enqueueAndWait(entry frameEntry) error {
	// Fast path: try to write inline under closeMu protection.
	w.closeMu.RLock()
	if w.closed.Load() {
		w.closeMu.RUnlock()
		return ErrConnClosing
	}
	if w.inlineMu.TryLock() {
		var err error
		if entry.data != nil {
			err = writeFrameBuffers(entry.ctx, w.tx, entry.fh, entry.hdr, entry.data)
		} else {
			err = writeFrame(entry.ctx, w.tx, entry.fh, entry.payload)
		}
		w.inlineMu.Unlock()
		w.closeMu.RUnlock()
		return err
	}
	w.closeMu.RUnlock()

	// Slow path: writer goroutine is busy, enqueue to channel.
	entry.doneCh = getDoneCh()
	if !w.trySend(entry) {
		putDoneCh(entry.doneCh)
		return ErrConnClosing
	}
	err := <-entry.doneCh
	putDoneCh(entry.doneCh)
	return err
}

// tryInlineWrite attempts to emit the whole message as a single H2
// DATA frame directly from the sender goroutine, bypassing the
// channel + writer-goroutine handoff that enqueueMessageAndWait
// normally takes.
//
// Motivation. At low stream concurrency (~10s-100s of streams) the
// existing channel path's per-message wall time is dominated by
// goroutine scheduling: sender sends to channel (~100 ns), runtime
// schedules the writer goroutine (~1 µs), writer processes one
// entry, futex_wakes the reader (~1 µs syscall), reader scheduler
// fires (~1 µs). The actual ring memcpy is a small fraction. At
// high concurrency the existing path amortises beautifully — one
// writer wake drains many entries, one futex_wake covers many
// frames — and convincingly beats UDS by 15+% (see
// grpc-go-shm-beat-uds-roadmap-2026-05-28.md). The hybrid added
// here keeps the high-concurrency win intact while reclaiming the
// low-concurrency latency.
//
// Return contract. Returns (true, err) once the inline path has
// taken responsibility for the message — the caller MUST NOT fall
// back to the channel path even if err is non-nil. Returns
// (false, nil) for all eligibility bails; the caller continues to
// the existing channel + writer-goroutine path with no state change.
//
// Eligibility checks, ordered cheapest-first so each bail returns
// fast:
//
//  1. payloadLen ∈ (0, shmMaxFrameSize]. Zero-length messages
//     (client half-close, etc.) go through the channel path's
//     specialised handler. Oversized messages need the writer
//     goroutine's chunking + FC-defer machinery.
//
//  2. inlineMu.TryLock(). The writer goroutine holds inlineMu for
//     its entire drain pass. A successful TryLock proves no
//     writer-goroutine batch is in flight; the inline path will
//     run alone until it Unlocks.
//
//  3. !closed, stream not done, ctx live. Each is checked AFTER
//     the lock so a concurrent close racing with the TryLock loses
//     to the close path's lock acquisition.
//
//  4. len(w.ch) == 0 AND len(w.deferred) == 0. This is the
//     batching-preservation gate. Whenever ANY work is queued the
//     channel path's batched drain is strictly better (one wake
//     covers many frames). Bailing here means the high-concurrency
//     workload (N=1000 streams ping-ponging) virtually never
//     fires the inline path — its batched throughput stays unchanged.
//
//  5. stream and conn outbound FC quotas each cover payloadLen.
//     Insufficient quota means we'd need the writer goroutine's
//     deferred-retry machinery; bailing back to the channel path
//     keeps that one canonical path.
//
//  6. CAS-deduct both quotas atomically. A CAS race here can only
//     come from the reader's addSendQuota (incoming WINDOW_UPDATE
//     applied on conn quota); we bail rather than retry-spin
//     because the channel path can pick up the larger window cleanly.
//
// Concurrency invariant. inlineMu serialises EVERY ring write —
// both this inline path and the writer goroutine — so at most one
// goroutine touches the ring at a time. Publish order equals lock
// acquisition order. There is no reservation list, no CAS-reserve
// race, no commit-vs-publish split (in contrast to any multi-anchor
// ZC publish scheme, where concurrent reserve-but-not-yet-published
// anchors can race the prefix-walk publisher into back-pressure
// jams). Even when our CAS on connQuota loses to a reader's
// addSendQuota, the explicit rollback + fall-through to the
// channel path preserves the lock-acquisition publish order.
func (w *shmFrameWriter) tryInlineWrite(
	ctx context.Context,
	streamPtr *streamBase,
	hdr []byte,
	data mem.BufferSlice,
	isLast bool,
) (handled bool, err error) {
	// Cheapest gate FIRST: at high stream concurrency (N=1000) the
	// channel is almost never empty, so this single non-atomic chan
	// length check bails ~100 % of calls without touching any
	// expensive field. Reordering matters: prior arrangement
	// (payloadLen check first) called BufferSlice.Len() — which
	// iterates segments — before realising we were going to bail.
	// Linux fair-default bench shows N=1000/4K drops 1-3 % when the
	// payloadLen path runs first, fully recovered by moving the
	// channel-length gate to position zero.
	//
	// We deliberately do NOT include `len(w.deferredProto)` here:
	// that map is mutated by the writer goroutine under inlineMu,
	// and a `len(map)` read off-lock is a data race (go vet -race
	// flags it; production can panic on concurrent map read/write).
	// The post-lock check below covers the same ordering invariant
	// safely. The pre-lock gate stays as a chan-only fast bail.
	if len(w.ch) > 0 {
		atomic.AddUint64(&shmInlineWriteBailQueued, 1)
		return false, nil
	}
	payloadLen := len(hdr) + data.Len()
	if payloadLen == 0 {
		atomic.AddUint64(&shmInlineWriteBailZeroLen, 1)
		return false, nil
	}
	if payloadLen > shmMaxFrameSize {
		atomic.AddUint64(&shmInlineWriteBailFrameSize, 1)
		return false, nil
	}
	if !w.inlineMu.TryLock() {
		atomic.AddUint64(&shmInlineWriteBailLocked, 1)
		return false, nil
	}

	// All paths from here must Unlock.

	if w.closed.Load() {
		w.inlineMu.Unlock()
		atomic.AddUint64(&shmInlineWriteBailClosed, 1)
		return false, nil
	}
	if streamPtr.getState() == streamDone || streamPtr.shmDataDropped.Load() {
		w.inlineMu.Unlock()
		atomic.AddUint64(&shmInlineWriteBailStreamDone, 1)
		return false, nil
	}
	if ctx.Err() != nil {
		w.inlineMu.Unlock()
		atomic.AddUint64(&shmInlineWriteBailCtxDone, 1)
		return false, nil
	}
	// Re-check len(w.ch) / deferred / deferredProto under the lock —
	// between the pre-lock check and TryLock, another goroutine may
	// have enqueued. Bail preserves the batched-drain invariant AND
	// the per-stream FIFO invariant (see pre-lock comment for the
	// deferredProto-overtaking-by-inline ordering bug).
	if len(w.ch) > 0 || len(w.deferred) > 0 || len(w.deferredProto) > 0 {
		w.inlineMu.Unlock()
		atomic.AddUint64(&shmInlineWriteBailQueued, 1)
		return false, nil
	}
	if w.connQuota == nil {
		// setConnQuotaPtr has not been called yet (transport in the
		// middle of construction). Fall back to channel path which
		// also depends on connQuota and will bail more cleanly via
		// processWholeMessage's misuse check.
		w.inlineMu.Unlock()
		atomic.AddUint64(&shmInlineWriteBailQueued, 1)
		return false, nil
	}
	streamQ := streamPtr.sendQuota.Load()
	if streamQ < int64(payloadLen) {
		w.inlineMu.Unlock()
		atomic.AddUint64(&shmInlineWriteBailQuota, 1)
		return false, nil
	}
	connQ := w.connQuota.Load()
	if connQ < int64(payloadLen) {
		w.inlineMu.Unlock()
		atomic.AddUint64(&shmInlineWriteBailQuota, 1)
		return false, nil
	}
	if !streamPtr.sendQuota.CompareAndSwap(streamQ, streamQ-int64(payloadLen)) {
		w.inlineMu.Unlock()
		atomic.AddUint64(&shmInlineWriteBailQuota, 1)
		return false, nil
	}
	if !w.connQuota.CompareAndSwap(connQ, connQ-int64(payloadLen)) {
		streamPtr.sendQuota.Add(int64(payloadLen))
		shmCASRollback.Add(1)
		w.inlineMu.Unlock()
		atomic.AddUint64(&shmInlineWriteBailQuota, 1)
		return false, nil
	}

	// All eligibility gates passed. Emit the single H2 DATA frame
	// carrying the whole MESSAGE. emitH2DataFromCursor handles its
	// own ring reservation, segment-spanning copy, and reader signal
	// (no BeginBatch wrapper needed for a single-chunk emit — the
	// final Commit fires the wake).
	fh := FrameHeader{StreamID: streamPtr.id, Type: FrameTypeMESSAGE}
	if isLast {
		fh.Flags = MessageFlagEndStream
	} else {
		fh.Flags = MessageFlagMORE
	}
	_, h2f := translateCustomToH2(fh)
	// PR-B: reuse the Stream's inline-allocated shmDeferred.cur slot
	// as scratch instead of fresh heap alloc. tryInlineWrite's
	// post-lock check (`len(w.deferred) > 0` → bail) guarantees no
	// in-flight whole-msg owns shmDeferred right now, and inlineMu
	// serialises with the writer-goroutine drain path. After the
	// emit, we clear cur.data/lpmHdr to drop the BufferSlice ref
	// (sender's outer scope still holds it for the synchronous
	// inline path — see enqueueMessageAndWait — so no Free here).
	cur := &streamPtr.shmDeferred.cur
	*cur = vecCursor{lpmHdr: hdr, data: data}
	if committed, emitErr := emitH2DataFromCursor(ctx, w.tx, streamPtr.id, cur, payloadLen, h2f); emitErr != nil {
		// Partial-commit-aware refund. When shmMaxFrameSize exceeds
		// h2MaxFramePayload (e.g. shm-tuned mode), emitH2DataFromCursor
		// may chunk a single whole-message into several H2 DATA frames
		// and may commit some prefix before failing on a later chunk.
		// Refund only the uncommitted remainder; refunding the full
		// payloadLen would inflate conn-level send quota by `committed`
		// bytes the peer has already received and charged.
		refund := int64(payloadLen - committed)
		if refund > 0 {
			streamPtr.sendQuota.Add(refund)
			w.connQuota.Add(refund)
		}
		// Clear scratch refs so a subsequent SendMsg on this stream
		// doesn't inherit stale BufferSlice / lpmHdr pinning pooled
		// buffers in the GC's view.
		cur.data = nil
		cur.lpmHdr = nil
		w.inlineMu.Unlock()
		// Return handled=true so the caller does NOT fall back to
		// the channel path (the message has terminally failed).
		return true, emitErr
	}
	if w.piggybackWUFn != nil {
		w.piggybackWUFn(streamPtr.id)
	}
	// Clear scratch refs (success path) — same rationale as above.
	cur.data = nil
	cur.lpmHdr = nil
	// Piggyback amortization (§7.3 / §7.9 of shm-rfc/C-bench-results.md,
	// v3 design after v1+v2 regressions). The inline writer just paid
	// the inlineMu acquire cost — while still holding it, opportunistically
	// drain up to maxInlinePiggyback frames from the existing w.ch
	// (NO second queue, NO new mutex, NO new wake mechanism). Reuses the
	// Go runtime's MPSC-tuned chan as the only producer→writer queue, and
	// the bound (8) keeps the inlineMu hold from blowing up into a
	// livelock that starves writeLoop's retryDeferred.
	//
	// Wake model: my own emit just fired its single reader-wake immediately
	// (Reserve/Commit above, no batch wrap), so latency for the calling
	// stream is unaffected. The drained entries are coalesced inside one
	// BeginBatch/EndBatch scope so they share a single reader-wake at the
	// end — same wake economics the writer goroutine would have produced.
	//
	// Ordering: chan FIFO preserves the same ordering writeLoop sees, so
	// nothing new at the protocol layer. The connWUCoalescer used by
	// writeLoop is intentionally NOT used here — at low concurrency the
	// drained entries are unlikely to be back-to-back conn WUs (those go
	// through the lockless WU pending path piggybacked onto outbound
	// DATA, see piggybackWUFn above); coalescing logic is the writer
	// goroutine's specialty.
	const maxInlinePiggyback = 8
	if len(w.ch) > 0 {
		w.tx.BeginBatch()
		for i := 0; i < maxInlinePiggyback; i++ {
			select {
			case next, ok := <-w.ch:
				if !ok {
					i = maxInlinePiggyback // chan closed mid-drain; stop
					break
				}
				w.processEntry(next)
				atomic.AddUint64(&shmInlinePiggybackDrain, 1)
			default:
				i = maxInlinePiggyback // chan empty; stop
			}
		}
		w.tx.EndBatch()
	}
	w.inlineMu.Unlock()
	atomic.AddUint64(&shmInlineWriteFire, 1)
	return true, nil
}

// enqueueMessageAndWait submits a whole MESSAGE (header + payload)
// for chunked emission by the writer goroutine and blocks until
// the LAST chunk has been written to the ring.
//
// The sender does not touch outbound flow-control state; WL owns
// CAS-reserve + chunk emission + FC-defer-and-retry internally.
// Net effect on fair-default 1000×1MB: sender parks ONCE per
// MESSAGE rather than once per window-grant chunk (1 MiB ÷ 65 KiB
// window = 16 chunks), giving a ~16× reduction in goroutine wake
// events on workloads that otherwise stress the scheduler.
//
// On stream-level credit arrival (incoming WINDOW_UPDATE handled
// by the reader's addSendQuota), the reader signals wuRetryWake
// which causes writeLoop to call retryDeferred on this writer.
// The deferred message's remaining chunks emit until either the
// FC window exhausts again (back into deferred) or the message
// finishes (doneCh signalled nil).
//
// Callers MUST NOT mutate hdr or data until this function returns.
// On error the entry's accumulated reservations are refunded; the
// caller's data BufferSlice is NOT freed (the caller owns it).
//
// streamPtr MUST be &cs.Stream or &ss.Stream — the *streamBase embedded
// in the caller's ClientStream / ServerStream so WL can CAS-deduct
// from streamPtr.sendQuota.
//
// isLast indicates whether the LAST chunk emitted will carry the
// HTTP/2 END_STREAM flag (vs MORE). For streaming requests that
// will be followed by another MESSAGE on this stream, pass false;
// for the final MESSAGE that closes the request half, pass true.
func (w *shmFrameWriter) enqueueMessageAndWait(ctx context.Context, streamPtr *streamBase, hdr []byte, data mem.BufferSlice, isLast bool) error {
	if w.closed.Load() {
		return ErrConnClosing
	}
	if streamPtr == nil {
		return errStreamDone
	}
	// Inline-write fast path: when the writer goroutine is idle and
	// no other work is queued, emit the message directly from this
	// goroutine. Bypasses the channel send + writer-goroutine wake
	// + writer-side futex_wake-to-reader handoffs (~3 µs of
	// scheduler latency per message). See tryInlineWrite's doc for
	// the full eligibility set and the GPT-5.5-style adversarial
	// review.
	//
	// data is consumed synchronously inside the inline path; no Ref
	// bump is required because the caller's existing reference
	// keeps the BufferSlice alive for the duration of this function.
	if handled, ierr := w.tryInlineWrite(ctx, streamPtr, hdr, data, isLast); handled {
		return ierr
	}

	fh := FrameHeader{StreamID: streamPtr.id, Type: FrameTypeMESSAGE}
	doneCh := getDoneCh()
	entry := frameEntry{
		ctx:       ctx,
		fh:        fh,
		hdr:       hdr,
		data:      data,
		doneCh:    doneCh,
		wholeMsg:  true,
		streamPtr: streamPtr,
		isLast:    isLast,
	}
	// Bump the BufferSlice refcount BEFORE handing the entry to the
	// writer. The writer's chunk-emit path (emitH2DataFromCursor via
	// vecCursor) reads bytes from data after this function may have
	// already returned via the ctx.Done() select branch below. If the
	// caller Free()s on Write's early return, the writer would hit a
	// use-after-free ("Cannot read freed buffer" panic). The extra
	// Ref keeps the underlying buffer alive until the writer matches
	// it with a Free() at every exit path (processWholeMessage early
	// return, advanceDeferred success/streamDone/ctx.Err/emit-err, or
	// close()-time drain). Buffers backed by mem.DefaultBufferPool are
	// refcounted so the cost is one atomic add per call.
	data.Ref()
	if !w.trySend(entry) {
		// trySend failed (writer closed before we enqueued); roll back
		// the Ref so the caller's Free is balanced.
		data.Free()
		putDoneCh(doneCh)
		return ErrConnClosing
	}
	// Wait for the writer goroutine to either fully transmit the
	// message (doneCh nil), surface a ring/IO error (doneCh err),
	// or observe ctx cancellation. The ctx select is critical for
	// back-pressure-stalled flows: a sender parked here while WL
	// has the message in its deferred map would otherwise leak
	// forever if the peer never sends a WINDOW_UPDATE (e.g. server
	// crash, transport close, or call-level cancellation). WL
	// observes the same ctx via d.ctx and will clean up the
	// deferred entry on its next retry pass (or close drain).
	// Writer-side Free of the entry's data Ref happens in every
	// such cleanup path; this select branch does NOT free.
	//
	// Note on the doneCh pool: we ONLY return doneCh to the pool on
	// the normal completion branch. On ctx.Done() the writer may
	// still send a late result into doneCh (race: ctx fires AFTER
	// writer dispatched but BEFORE we read). Returning doneCh to the
	// pool then risks a second sender reading our late result. The
	// allocation cost on the ctx.Done() branch is tolerable because
	// it is the rare cancellation path; the common steady-state
	// completion path captures the GC win.
	select {
	case err := <-doneCh:
		putDoneCh(doneCh)
		return err
	case <-ctx.Done():
		return ContextErr(ctx.Err())
	}
}

// close shuts down the writer goroutine. It closes the channel, causing
// writeLoop to drain remaining entries and exit. Blocks until completion.
//
// Shutdown sequence:
//  1. closeMu.Lock — blocks new enqueueAndWait fast-path and trySend callers.
//  2. closed.Swap(true) + close(ch) — marks writer as done, drains channel.
//  3. closeMu.Unlock — lets in-flight RLock holders complete.
//  4. wg.Wait — waits for writeLoop goroutine to exit.
//  5. inlineMu.Lock/Unlock — waits for any inline writer that acquired
//     inlineMu before step 1 (they hold closeMu.RLock, so step 3 must
//     come before step 4 to avoid deadlock with writeLoop).
//
// After close returns, no goroutine is accessing the ring.
func (w *shmFrameWriter) close() {
	w.closeMu.Lock()
	if w.closed.Swap(true) {
		w.closeMu.Unlock()
		return // already closed
	}
	close(w.ch)
	w.closeMu.Unlock()
	w.wg.Wait()
	// Drain any inline writer that acquired inlineMu before closed was set.
	// After this returns, no goroutine is accessing the ring through the
	// frame writer, so the caller can safely unmap the segment. The
	// Lock/Unlock pair acts as a barrier: any in-flight inline writer
	// holding inlineMu will release it before we proceed.
	w.drainInline()
	// Signal ErrConnClosing to any whole-message senders still
	// parked in the deferred map. After wg.Wait the writer
	// goroutine has exited so the map is no longer mutated and is
	// safe to walk without inlineMu. Each doneCh has buffered slot
	// 1; the sender may have already left via ctx.Done() in which
	// case the send is a harmless no-op (slot then garbage-
	// collected with the channel). If the sender is still parked
	// on doneCh receive, it wakes with ErrConnClosing.
	for sid, d := range w.deferred {
		select {
		case d.doneCh <- ErrConnClosing:
		default:
		}
		delete(w.deferred, sid)
		// Balance the Ref taken in enqueueMessageAndWait.
		d.release()
	}
	// Drain any ZC proto entries still pending in the deferredProto
	// map. Senders for these returned success at enqueue time (the
	// fire-and-forget contract), so there is no doneCh to signal.
	// The transport's close path (which called us) has already set
	// t.closed.Load() == true, so any subsequent upper-layer Send /
	// Recv on the affected streams will surface ErrConnClosing via
	// the stream state machine. We still need to decrement each
	// entry's protoInFlight counter so that test-side assertions on
	// stream resource teardown (counter must reach zero) hold.
	for sid, queue := range w.deferredProto {
		for _, entry := range queue {
			putAsyncProtoBuf(entry.protoBytes)
			if entry.streamPtr != nil {
				entry.streamPtr.protoInFlight.Add(-1)
			}
		}
		delete(w.deferredProto, sid)
	}
	// Drain any TRAILERS sentinels parked behind DATA that the
	// transport teardown has just discarded. The writeStatus sender
	// is blocked on doneCh; signal ErrConnClosing so it surfaces
	// the right error and the server's response handler / gRPC
	// infrastructure can complete teardown.
	for sid, entry := range w.deferredTrailers {
		if entry.doneCh != nil {
			select {
			case entry.doneCh <- ErrConnClosing:
			default:
			}
		}
		delete(w.deferredTrailers, sid)
	}
	// Drain any parked zero-length END_STREAM (CloseSend half-close)
	// sentinels: wake the blocked CloseSend caller and release the
	// retained data reference.
	for sid, entry := range w.pendingEndStream {
		if entry.doneCh != nil {
			select {
			case entry.doneCh <- ErrConnClosing:
			default:
			}
		}
		entry.data.Free()
		delete(w.pendingEndStream, sid)
	}
}

// drainInline waits for any in-flight inline writer to release inlineMu.
// It exists as a separate method so the Lock/Unlock pair isn't flagged
// by static analysis as an empty critical section — the empty body is
// the intended barrier semantics.
func (w *shmFrameWriter) drainInline() {
	w.inlineMu.Lock()
	defer w.inlineMu.Unlock()
}
