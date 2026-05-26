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
//   - enqueue/enqueueAndWait use closeMu.RLock to coordinate with close(),
//     ensuring the channel is never sent to after being closed.
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
	// look up the *Stream by streamID, check streamDone state, and
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
	// Both fields are accessed only from the writer goroutine; no
	// mutex is needed. setConnQuotaPtr publishes the connQuota
	// pointer via happens-before-channel-send before any sender
	// enqueueMessageAndWait can race here.
	connQuota *atomic.Int64
	deferred  map[uint32]*deferredMessage
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
	streamPtr *Stream
	fh        FrameHeader
	cur       *vecCursor
	remaining int
	isLast    bool
	doneCh    chan error
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
	streamPtr *Stream
	isLast    bool
}

const (
	// frameWriterQueueSize is the channel buffer size. Large enough to absorb
	// bursts without blocking callers, small enough to bound memory.
	// At N=1000 concurrent streams, 256 is too small — senders block on
	// channel full. 2048 absorbs typical fanout without back-pressure on
	// the async fire-and-forget path.
	frameWriterQueueSize = 2048
)

// newShmFrameWriter creates and starts a frame writer for the given ring.
func newShmFrameWriter(tx *ShmRing) *shmFrameWriter {
	w := &shmFrameWriter{
		tx:          tx,
		ch:          make(chan frameEntry, frameWriterQueueSize),
		wuRetryWake: make(chan struct{}, 1),
		deferred:    make(map[uint32]*deferredMessage),
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
		pending := len(w.ch)
		if pending > 0 {
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
			for i := 0; i < pending; i++ {
				next, ok := <-w.ch
				if !ok {
					break
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
				// next group's writes, instead of waiting for the
				// entire pending queue to complete.
				if batchBytes >= signalBatchBytes && i < pending-1 {
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
// for SHM workloads (1000 streams × 1 MiB pre-credit = 1 GiB total
// << 2 GiB cap) but the guard is necessary for spec compliance.
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
		return
	}
	payloadLen := len(entry.hdr) + entry.data.Len()
	if payloadLen == 0 {
		// Zero-length MESSAGE — most commonly a client-side half-close
		// (cs.Write(nil, nil, {Last:true})). No flow-control bytes to
		// reserve; emit a single H2 DATA frame with empty payload
		// carrying END_STREAM (when isLast) or MORE.
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
		entry.doneCh <- err
		return
	}
	cur := &vecCursor{lpmHdr: entry.hdr, data: entry.data}
	d := &deferredMessage{
		ctx:       entry.ctx,
		streamPtr: entry.streamPtr,
		fh:        entry.fh,
		cur:       cur,
		remaining: payloadLen,
		isLast:    entry.isLast,
		doneCh:    entry.doneCh,
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
		if d.streamPtr.getState() == streamDone {
			select {
			case d.doneCh <- errStreamDone:
			default:
			}
			delete(w.deferred, streamID)
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
		if !d.streamPtr.sendQuota.CompareAndSwap(streamQ, streamQ-grant) {
			continue // CAS lost — retry
		}
		if !w.connQuota.CompareAndSwap(connQ, connQ-grant) {
			d.streamPtr.sendQuota.Add(grant) // rollback
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
		if err := emitH2DataFromCursor(d.ctx, w.tx, streamID, d.cur, int(grant), h2f); err != nil {
			// Refund the reserved quota — these bytes were not
			// delivered and the stream is about to error out.
			d.streamPtr.sendQuota.Add(grant)
			w.connQuota.Add(grant)
			d.doneCh <- err
			delete(w.deferred, streamID)
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
}

// retryDeferred is called by the writeLoop on every wuRetryWake.
// It walks the deferred map and attempts to make progress on each
// stalled message. Runs under inlineMu.
//
// Iteration order is map-random — for fairness under high
// concurrency we don't try to preserve FIFO. A starving stream will
// eventually be reached as WU credits accumulate; in practice the
// receiver-side WU emitter drives wuRetryWake at sub-millisecond
// cadence so the latency cost of map-random is negligible.
func (w *shmFrameWriter) retryDeferred() {
	if len(w.deferred) == 0 {
		return
	}
	for sid, d := range w.deferred {
		// advanceDeferred may delete sid from the map; Go's spec
		// guarantees this is safe during a range loop (the iterator
		// observes the new state going forward).
		w.advanceDeferred(sid, d)
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

// enqueueOrInline writes the frame inline if the writer goroutine is idle
// (inlineMu available), otherwise enqueues to the channel for asynchronous
// processing. The caller does not block waiting for completion either way.
//
// Used for fire-and-forget control frames (WINDOW_UPDATE in particular) that
// callers do not need to acknowledge but where avoiding the writer-goroutine
// wakeup matters for latency. Under fair-default flow control (65535 B
// HTTP/2 window) the receiver emits a WINDOW_UPDATE roughly every DATA frame,
// and the round-trip cost of "enqueue -> wake writer goroutine -> write
// frame -> futexWake peer" is the dominant stall in the producer's send
// loop. Writing the WU inline collapses that to "write frame -> futexWake".
//
// Returns nil on success (inline or queued); ErrConnClosing if closed.
func (w *shmFrameWriter) enqueueOrInline(entry frameEntry) error {
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
	// Writer goroutine is busy; fall back to async enqueue. The caller
	// does not need synchronous completion, so doneCh stays nil.
	if !w.trySend(entry) {
		return ErrConnClosing
	}
	return nil
}

// errFrameWriterFull signals that an enqueue attempted via
// enqueueOrInlineNonBlocking could not complete because the writer was
// neither idle nor able to accept an entry on its async channel without
// blocking. Callers MUST treat this as recoverable (e.g., re-buffer the
// pending state) rather than dropping the work; the frame writer will
// catch up and a future enqueue attempt will succeed.
var errFrameWriterFull = errors.New("shm frame writer: channel full, would block")

// enqueueOrInlineNonBlocking is the strictly non-blocking variant of
// enqueueOrInline. It is the ONLY safe enqueue path for callers that
// MUST NOT block — most importantly the SHM reader goroutine, which
// is responsible for committing inbound ring bytes and waking peers.
//
// If the reader were to block on the outbound writer (which is what
// the blocking trySend in enqueueOrInline can cause), it would create
// a transport-level deadlock: the outbound ring fills because the
// peer reader is blocked the same way; the writer can't drain its
// channel because its ring writes block; the reader can't enqueue
// the WINDOW_UPDATE that would unblock the peer.
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
// (onDataFrameReceived, onMessageStart). Use the blocking
// enqueueOrInline only from app goroutines that can tolerate
// blocking (e.g., sender Write paths).
func (w *shmFrameWriter) enqueueOrInlineNonBlocking(entry frameEntry) error {
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
	// inlineMu busy; try non-blocking channel send.
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
	entry.doneCh = make(chan error, 1)
	if !w.trySend(entry) {
		return ErrConnClosing
	}
	return <-entry.doneCh
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
// streamPtr MUST be &cs.Stream or &ss.Stream — the *Stream embedded
// in the caller's ClientStream / ServerStream so WL can CAS-deduct
// from streamPtr.sendQuota.
//
// isLast indicates whether the LAST chunk emitted will carry the
// HTTP/2 END_STREAM flag (vs MORE). For streaming requests that
// will be followed by another MESSAGE on this stream, pass false;
// for the final MESSAGE that closes the request half, pass true.
func (w *shmFrameWriter) enqueueMessageAndWait(ctx context.Context, streamPtr *Stream, hdr []byte, data mem.BufferSlice, isLast bool) error {
	if w.closed.Load() {
		return ErrConnClosing
	}
	if streamPtr == nil {
		return errStreamDone
	}
	fh := FrameHeader{StreamID: streamPtr.id, Type: FrameTypeMESSAGE}
	doneCh := make(chan error, 1)
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
	if !w.trySend(entry) {
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
	select {
	case err := <-doneCh:
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
