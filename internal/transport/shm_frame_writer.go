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
	// (fire-and-forget enqueue, used by the SHM_NO_WU=1 client MESSAGE
	// path and by HEADERS / GOAWAY senders elsewhere) fails to write to
	// the ring. Without this hook the error would be silently dropped
	// after data.Free(), leaving the peer waiting forever for bytes that
	// were never sent. The callback is fired at most once per writer to
	// avoid amplifying a single ring failure into N close attempts when a
	// queue full of doomed entries drains. Set via setAsyncErrorHandler
	// after construction; nil callback means "swallow" (legacy behaviour).
	onAsyncError func(error)
	errReported  atomic.Bool
}

// frameEntry represents a single frame to be written to the ring.
type frameEntry struct {
	ctx     context.Context
	fh      FrameHeader
	payload []byte          // simple payload (HEADERS, TRAILERS, CANCEL, etc.)
	hdr     []byte          // optional header prefix for BufferSlice payloads
	data    mem.BufferSlice // zero-copy payload (MESSAGE)
	doneCh  chan error      // if non-nil, writer sends result and caller waits
	// freeData, when true, causes the writer goroutine to invoke data.Free()
	// after the frame has been written to the ring. This is used by the
	// async fire-and-forget path (v3.4 P1a-async): the caller has already
	// invoked data.Ref() to hand ownership of the buffer slice to the writer,
	// so writer must Free() after writing to balance the Ref.
	freeData bool
}

const (
	// frameWriterQueueSize is the channel buffer size. Large enough to absorb
	// bursts without blocking callers, small enough to bound memory.
	// At N=1000 concurrent streams, 256 is too small — senders block on
	// channel full. Raised to 2048 for the v3.4 P1a-async path.
	frameWriterQueueSize = 2048
)

// newShmFrameWriter creates and starts a frame writer for the given ring.
func newShmFrameWriter(tx *ShmRing) *shmFrameWriter {
	w := &shmFrameWriter{
		tx: tx,
		ch: make(chan frameEntry, frameWriterQueueSize),
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
		select {
		case entry, ok = <-w.ch:
			if !ok {
				return
			}
		default:
			// Channel empty — yield briefly to let sender goroutine run,
			// then block. This mirrors .NET's WriterLoop Phase 2.5 yield.
			runtime.Gosched()
			entry, ok = <-w.ch
			if !ok {
				return
			}
		}

		w.inlineMu.Lock()
		// Check if more frames are queued behind this one.
		pending := len(w.ch)
		if pending > 0 {
			signalBatchBytes := int(w.tx.Capacity() / 8)
			batchBytes := 0
			w.tx.BeginBatch()
			// entryBytes MUST be computed BEFORE processEntry:
			// processEntry frees entry.data on the SHM_NO_WU=1
			// fire-and-forget path, after which entry.data.Len()
			// panics with "read freed buffer".
			eb := entryBytes(entry)
			w.processEntry(entry)
			batchBytes += eb
			for i := 0; i < pending; i++ {
				next, ok := <-w.ch
				if !ok {
					break
				}
				neb := entryBytes(next)
				w.processEntry(next)
				batchBytes += neb
				// Periodically release the batch so the reader gets a
				// wake mid-burst and can drain in parallel with the
				// next group's writes, instead of waiting for the
				// entire pending queue to complete.
				if batchBytes >= signalBatchBytes && i < pending-1 {
					w.tx.EndBatch()
					w.tx.BeginBatch()
					batchBytes = 0
				}
			}
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
	if e.data != nil {
		return len(e.hdr) + e.data.Len()
	}
	return len(e.payload)
}

// processEntry writes a single frame entry to the ring and signals
// completion to the caller if doneCh is set. If entry.freeData is true,
// the writer Free()s the buffer slice after writing (async-write path).
//
// Fire-and-forget entries (doneCh == nil) have no caller waiting for
// the result. If the ring write failed, we surface the error via
// onAsyncError (fired at most once per writer) so the owning transport
// can tear down rather than silently drop bytes.
func (w *shmFrameWriter) processEntry(entry frameEntry) {
	var err error
	if entry.data != nil {
		err = writeFrameBuffers(entry.ctx, w.tx, entry.fh, entry.hdr, entry.data)
	} else {
		err = writeFrame(entry.ctx, w.tx, entry.fh, entry.payload)
	}
	if entry.freeData && entry.data != nil {
		entry.data.Free()
	}
	if entry.doneCh != nil {
		entry.doneCh <- err
		return
	}
	if err != nil && w.onAsyncError != nil && w.errReported.CompareAndSwap(false, true) {
		w.onAsyncError(err)
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

// emitMessageInlineVec emits `length` bytes from cur as one MESSAGE
// (chunked into H2 DATA frames per shmMaxFrameSize) under inlineMu,
// blocking the caller until the write completes. fh.Flags is
// translated to H2 flags once and applied to the FINAL chunk only.
//
// Used by the chunked client write slow path
// (shm_client_transport.go) so each per-window iteration emits
// straight from the source (hdr || data BufferSlice) cursor, skipping
// the contiguous materialise step that the legacy
// frameEntry{payload: buf[off:end]} path required. Saves one
// payload-size memcpy on the producer hot path for fair-default
// LargeUnary (16 MB → ~3 ms latency reduction).
//
// The cursor advances by exactly `length` bytes on success. Callers
// keep the cursor alive across iterations to walk the entire logical
// (hdr || data) stream without re-indexing.
func (w *shmFrameWriter) emitMessageInlineVec(ctx context.Context, fh FrameHeader, cur *vecCursor, length int) error {
	w.closeMu.RLock()
	defer w.closeMu.RUnlock()
	if w.closed.Load() {
		return ErrConnClosing
	}
	_, h2f := translateCustomToH2(fh)
	w.inlineMu.Lock()
	defer w.inlineMu.Unlock()
	return emitH2DataFromCursor(ctx, w.tx, fh.StreamID, cur, length, h2f)
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
}

// drainInline waits for any in-flight inline writer to release inlineMu.
// It exists as a separate method so the Lock/Unlock pair isn't flagged
// by static analysis as an empty critical section — the empty body is
// the intended barrier semantics.
func (w *shmFrameWriter) drainInline() {
	w.inlineMu.Lock()
	defer w.inlineMu.Unlock()
}
