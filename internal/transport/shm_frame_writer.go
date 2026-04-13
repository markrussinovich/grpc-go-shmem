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
//   - enqueue/enqueueAndWait use trySend which recovers from the panic that
//     occurs if a producer races with close(ch). This eliminates the need for
//     a mutex on the hot path while remaining safe during shutdown.
type shmFrameWriter struct {
	tx     *ShmRing
	ch     chan frameEntry // data + control frames from app goroutines
	done   chan struct{}   // closed when writer goroutine exits
	wg     sync.WaitGroup
	closed atomic.Bool
}

// frameEntry represents a single frame to be written to the ring.
type frameEntry struct {
	ctx      context.Context
	fh       FrameHeader
	payload  []byte         // simple payload (HEADERS, TRAILERS, CANCEL, etc.)
	hdr      []byte         // optional header prefix for BufferSlice payloads
	data     mem.BufferSlice // zero-copy payload (MESSAGE)
	maxChunk int            // max frame payload for chunked writes; 0 = default
	doneCh   chan error      // if non-nil, writer sends result and caller waits
}

const (
	// frameWriterQueueSize is the channel buffer size. Large enough to absorb
	// bursts without blocking callers, small enough to bound memory.
	frameWriterQueueSize = 256
)

// newShmFrameWriter creates and starts a frame writer for the given ring.
func newShmFrameWriter(tx *ShmRing) *shmFrameWriter {
	w := &shmFrameWriter{
		tx:   tx,
		ch:   make(chan frameEntry, frameWriterQueueSize),
		done: make(chan struct{}),
	}
	w.wg.Add(1)
	go w.writeLoop()
	return w
}

// writeLoop is the single writer goroutine. It drains the channel and writes
// frames to the ring sequentially, eliminating the need for writeMu.
func (w *shmFrameWriter) writeLoop() {
	defer w.wg.Done()
	defer close(w.done)

	for entry := range w.ch {
		var err error
		if entry.data != nil {
			err = writeFrameBuffersChunked(entry.ctx, w.tx, entry.fh, entry.hdr, entry.data, entry.maxChunk)
		} else {
			err = writeFrame(entry.ctx, w.tx, entry.fh, entry.payload)
		}
		if entry.doneCh != nil {
			entry.doneCh <- err
		}
	}
}

// trySend attempts to send an entry to the channel. Returns false if the
// channel is closed (recovering from the panic). This is safe because the
// only goroutine that closes w.ch is close(), and a recovered panic here
// simply means "writer is shutting down".
func (w *shmFrameWriter) trySend(entry frameEntry) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	w.ch <- entry
	return true
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

// enqueueAndWait submits a frame and blocks until the write completes,
// returning the write error (if any). Use this for frames where the caller
// must know the outcome (e.g., HEADERS in NewStream).
func (w *shmFrameWriter) enqueueAndWait(entry frameEntry) error {
	if w.closed.Load() {
		return ErrConnClosing
	}
	entry.doneCh = make(chan error, 1)
	if !w.trySend(entry) {
		return ErrConnClosing
	}
	return <-entry.doneCh
}

// close shuts down the writer goroutine. It closes the channel, causing
// writeLoop to drain remaining entries and exit. Blocks until completion.
// Any concurrent enqueue calls will get ErrConnClosing via trySend's recover.
func (w *shmFrameWriter) close() {
	if w.closed.Swap(true) {
		return // already closed
	}
	close(w.ch)
	w.wg.Wait()
}
