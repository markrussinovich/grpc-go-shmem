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
}

// frameEntry represents a single frame to be written to the ring.
type frameEntry struct {
	ctx     context.Context
	fh      FrameHeader
	payload []byte          // simple payload (HEADERS, TRAILERS, CANCEL, etc.)
	hdr     []byte          // optional header prefix for BufferSlice payloads
	data    mem.BufferSlice // zero-copy payload (MESSAGE)
	doneCh  chan error      // if non-nil, writer sends result and caller waits
}

const (
	// frameWriterQueueSize is the channel buffer size. Large enough to absorb
	// bursts without blocking callers, small enough to bound memory.
	frameWriterQueueSize = 256
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

// writeLoop is the single writer goroutine. It drains the channel and writes
// frames to the ring sequentially, eliminating the need for writeMu.
// When multiple frames are pending, it uses batch mode to suppress per-frame
// signals and issue a single reader wakeup after the batch.
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
			w.tx.BeginBatch()
			w.processEntry(entry)
			for i := 0; i < pending; i++ {
				next, ok := <-w.ch
				if !ok {
					break
				}
				w.processEntry(next)
			}
			w.tx.EndBatch()
		} else {
			w.processEntry(entry)
		}
		w.inlineMu.Unlock()
	}
}

// processEntry writes a single frame entry to the ring and signals
// completion to the caller if doneCh is set.
func (w *shmFrameWriter) processEntry(entry frameEntry) {
	var err error
	if entry.data != nil {
		err = writeFrameBuffers(entry.ctx, w.tx, entry.fh, entry.hdr, entry.data)
	} else {
		err = writeFrame(entry.ctx, w.tx, entry.fh, entry.payload)
	}
	if entry.doneCh != nil {
		entry.doneCh <- err
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
