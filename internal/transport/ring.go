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
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync/atomic"
	"time"
	"unsafe"
	_ "unsafe" // for go:linkname
)

// runtime_procyield yields the processor, executing a PAUSE instruction on x86.
// This is more efficient than runtime.Gosched() for short spin waits as it:
// - Hints to the CPU that we're in a spin loop
// - Reduces power consumption
// - Allows hyperthreaded sibling to make progress
// cycles is typically 1 for a single PAUSE.
//
//go:linkname runtime_procyield runtime.procyield
//revive:disable:var-naming This name must match the Go runtime internal function
func runtime_procyield(cycles uint32)

// Spin-wait constants for adaptive spinning before falling back to futex.
// Based on research from Facebook Folly's synchronization primitives.
const (
	// spinIterationsDefault is the default number of spin iterations before futex.
	// At ~7ns per PAUSE instruction, 300 iterations ≈ 2µs of spinning.
	// Folly uses 2µs as default because futex wake costs ~7-10µs.
	spinIterationsDefault = 300

	// spinIterationsMin is the minimum spin iterations for adaptive adjustment.
	spinIterationsMin = 50

	// spinIterationsMax is the maximum spin iterations to prevent excessive CPU use.
	spinIterationsMax = 2000
)

var shmDebugEnabled = os.Getenv("GRPC_SHM_DEBUG") != ""

func shmDebugf(format string, args ...any) {
	if !shmDebugEnabled {
		return
	}
	log.Printf(format, args...)
}

// ErrRingClosed indicates that the ring has been closed for writing
var ErrRingClosed = errors.New("ring closed")

// RingState represents a snapshot of ring buffer state for debugging and diagnostics
type RingState struct {
	Capacity     uint64 // Total ring capacity
	Widx         uint64 // Current write index (monotonic)
	Ridx         uint64 // Current read index (monotonic)
	Used         uint64 // Bytes currently in ring (Widx - Ridx)
	DataSeq      uint32 // Data availability sequence number
	SpaceSeq     uint32 // Space availability sequence number
	ContigSeq    uint32 // Contiguity sequence number (readSeq equivalent)
	Closed       uint32 // Ring closed flag (0 = open, 1 = closed)
	DataWaiters  uint32 // Number of readers waiting for data
	SpaceWaiters uint32 // Number of writers waiting for space
}

// ShmRing represents a single-producer single-consumer (SPSC) ring buffer
// operating over shared memory with event-driven blocking.
//
// This implementation provides high-performance cross-process communication
// with zero-copy operations and minimal kernel calls through futex-based
// synchronization.
type ShmRing struct {
	capMask  uint64  // capacity-1 for fast masking (capacity must be power of 2)
	capacity uint64  // actual data area capacity in bytes
	hdrOff   uintptr // base address of RingHeader in mmapped bytes
	dataOff  uintptr // base address of data area
	mem      []byte  // the mmapped region (no copying)
	closed   uint32  // atomic flag: 1 if this ring has been closed locally

	// pendingReadIdx tracks how far we've read (but not committed) in the ring.
	// This is process-local (not in shared memory) and allows the reader to
	// continue reading new frames while holding references to uncommitted buffers.
	// The shared readIdx only advances when buffers are freed.
	// Access via atomic operations (single reader in SPSC design).
	pendingReadIdx uint64

	// Adaptive spin state for minimizing latency on fast paths.
	// These are process-local and help tune spin duration based on workload.
	dataSpinCutoff  uint32 // Current spin iterations for waiting on data
	spaceSpinCutoff uint32 // Current spin iterations for waiting on space

	// Pre-allocated commit context for read operations (reads are single-threaded).
	readCommit ReadCommit
}

// ReadCommit holds the state needed to commit a read operation.
// This is embedded in ShmRing to avoid allocation on every ReadSlices call.
type ReadCommit struct {
	ring          *ShmRing
	commitReadIdx uint64
	maxBytes      int
}

// Commit advances the shared read index to free space for the writer.
// consumed must not exceed maxBytes.
func (rc *ReadCommit) Commit(consumed int) {
	if consumed < 0 || consumed > rc.maxBytes {
		return // Invalid consumption, ignore
	}

	hdr := rc.ring.header()

	// Advance shared read index (release-publish) - frees space for writer
	hdr.SetReadIndex(rc.commitReadIdx + uint64(consumed))

	// Contiguity: always bump after any positive read commit.
	// ContigWaiters are waiting for "more space" (not full ring), so wake on every read.
	if consumed > 0 {
		hdr.IncrementContigSequence()
		if hdr.ContigWaiters() > 0 {
			futexWake(&hdr.contigSeq, 1)
		}
	}
	// Space: always wake waiters if any are waiting.
	// This is conservative but avoids potential races in the waiter registration.
	if consumed > 0 && hdr.SpaceWaiters() > 0 {
		hdr.IncrementSpaceSequence()
		futexWake(&hdr.spaceSeq, 1)
	}
}

// SMF (Shared Memory Framing) helpers are defined in frame.go. This file uses
// ReserveFrameHeader to reserve 16-byte headers which may straddle wrap boundaries.

// NewShmRingFromSegment creates a ShmRing from a segment's ring view.
// This provides the high-level blocking API over the low-level ring view.
func NewShmRingFromSegment(ringView *ringView, mem []byte) *ShmRing {
	capacity := ringView.Capacity()
	// Enforce power-of-two capacity invariant for masked indexing
	if capacity == 0 || !IsPowerOfTwo(capacity) {
		panic("ShmRing capacity must be a power of two")
	}

	// Assert mmapped length vs offsets
	end := ringView.offset + RingHeaderSize + capacity
	if int(end) > len(mem) {
		panic("segment too small for ring")
	}

	r := &ShmRing{
		capMask:         capacity - 1, // For modulo operations: pos = idx & capMask
		hdrOff:          uintptr(ringView.offset),
		dataOff:         uintptr(ringView.offset + RingHeaderSize),
		mem:             mem,
		capacity:        capacity, // Store actual capacity separately
		dataSpinCutoff:  spinIterationsDefault,
		spaceSpinCutoff: spinIterationsDefault,
	}
	// Initialize read commit context with back-pointer (reads are single-threaded)
	r.readCommit.ring = r
	// Initialize pendingReadIdx from current shared readIdx
	atomic.StoreUint64(&r.pendingReadIdx, r.header().ReadIndex())
	shmDebugf("[DEBUG] NewShmRingFromSegment: ring=%p, hdrOff=%d, mem[0]=%p, hdr=%p, pendingReadIdx=%d", r, r.hdrOff, &mem[0], r.header(), atomic.LoadUint64(&r.pendingReadIdx))
	return r
}

// header returns a pointer to the RingHeader in shared memory
func (r *ShmRing) header() *RingHeader {
	return (*RingHeader)(unsafe.Pointer(uintptr(unsafe.Pointer(&r.mem[0])) + r.hdrOff))
}

// dataPtr returns a pointer to the data area in shared memory
func (r *ShmRing) dataPtr() unsafe.Pointer {
	return unsafe.Pointer(uintptr(unsafe.Pointer(&r.mem[0])) + r.dataOff)
}

// Capacity returns the ring capacity
func (r *ShmRing) Capacity() uint64 {
	return r.capacity
}

// DebugState returns a snapshot of the current ring state for debugging and diagnostics.
// All values are read atomically for consistent state observation.
func (r *ShmRing) DebugState() RingState {
	hdr := r.header()
	shmDebugf("DebugState: ring=%p, hdr=%p, &dataSeq=%p, &spaceSeq=%p", r, hdr, &hdr.dataSeq, &hdr.spaceSeq)

	// Read all state atomically for consistent snapshot
	widx := hdr.WriteIndex()
	ridx := hdr.ReadIndex()
	dataSeq := hdr.DataSequence()
	spaceSeq := hdr.SpaceSequence()
	contigSeq := hdr.ContigSequence()
	closed := uint32(0)
	if hdr.Closed() {
		closed = 1
	}

	return RingState{
		Capacity:     r.capacity,
		Widx:         widx,
		Ridx:         ridx,
		Used:         widx - ridx,
		DataSeq:      dataSeq,
		SpaceSeq:     spaceSeq,
		ContigSeq:    contigSeq,
		Closed:       closed,
		DataWaiters:  hdr.DataWaiters(),
		SpaceWaiters: hdr.SpaceWaiters(),
	}
}

// WriteBlocking writes data to the ring buffer using an event-driven producer algorithm.
// Blocks until space is available or the ring is closed.
//
// This implements the high-performance SPSC algorithm as specified:
// - Uses write/read indices for actual data tracking
// - Uses dataSeq/spaceSeq for futex-based event notification
// - Performs zero-copy data transfer
// - Handles spurious wakes correctly
func (r *ShmRing) WriteBlocking(data []byte) error {
	if len(data) == 0 {
		return nil // No-op for empty data
	}

	// Check if data fits in ring capacity
	if uint64(len(data)) > r.capacity {
		return errors.New("data larger than ring capacity")
	}

	hdr := r.header()

	// Producer side: write data and signal consumer
	for {
		// Check for closure first
		if hdr.Closed() {
			return ErrRingClosed
		}

		// Load current indices to check available space
		writeIdx := hdr.WriteIndex()
		readIdx := hdr.ReadIndex()

		// Calculate available space using indices
		usedBefore := writeIdx - readIdx
		available := r.capacity - usedBefore

		if uint64(len(data)) <= available {
			// Space available - perform the write
			writePos := writeIdx & r.capMask

			// Handle ring wrap-around
			if writePos+uint64(len(data)) <= r.capacity {
				// Simple case: no wrap
				destPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				copy(unsafe.Slice((*byte)(destPtr), len(data)), data)
			} else {
				// Wrap case: split the write
				firstChunk := r.capacity - writePos
				firstChunkI := int(firstChunk)

				// Write first chunk at end of buffer
				destPtr1 := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				copy(unsafe.Slice((*byte)(destPtr1), firstChunkI), data[:firstChunkI])

				// Write second chunk at beginning of buffer
				destPtr2 := r.dataPtr()
				copy(unsafe.Slice((*byte)(destPtr2), len(data)-firstChunkI), data[firstChunkI:])
			}

			// Advance write index.
			// Memory ordering rationale:
			// 1) Bytes are copied into the ring first (normal stores)
			// 2) Check emptiness right before publishing (avoids lost wake race)
			// 3) Publish the new write index with an atomic store (acts as a release)
			// 4) Wake only if we actually transitioned empty -> non-empty

			hdr.SetWriteIndex(writeIdx + uint64(len(data))) // release-publish

			// Readers may block waiting for N bytes, not just “ring is empty”, so wake
			// after any successful write commit.
			if len(data) > 0 {
				hdr.IncrementDataSequence()
				// Only wake if readers are actually blocked on futex (not spinning)
				if hdr.DataWaiters() > 0 {
					futexWake(&hdr.dataSeq, 1)
				}
			}

			return nil
		}

		// Insufficient space. Distinguish strictly full vs need-more-space.
		// Re-check under the same loop to avoid missed wake.
		writeIdx = hdr.WriteIndex()
		readIdx = hdr.ReadIndex()
		usedBefore = writeIdx - readIdx
		available = r.capacity - usedBefore
		if available == 0 {
			// Full: spin-wait then wait on spaceSeq (full→not-full)
			// Phase 1: Spin-wait before falling back to futex
			spinLimit := atomic.LoadUint32(&r.spaceSpinCutoff)
			spaceAvailable := false
			for spin := uint32(0); spin < spinLimit; spin++ {
				writeIdx = hdr.WriteIndex()
				readIdx = hdr.ReadIndex()
				if (r.capacity - (writeIdx - readIdx)) >= uint64(len(data)) {
					// Space available! Update adaptive cutoff
					if spin > 0 {
						target := min(spinIterationsMax, spin*2)
						newCutoff := (7*spinLimit + target) / 8
						atomic.StoreUint32(&r.spaceSpinCutoff, max(spinIterationsMin, newCutoff))
					}
					spaceAvailable = true
					break
				}
				if hdr.Closed() {
					return ErrRingClosed
				}
				runtime_procyield(1)
			}
			if spaceAvailable {
				continue
			}
			// Phase 2: Spin failed, reduce cutoff and fall back to futex
			newCutoff := (7*spinLimit + spinIterationsMin) / 8
			atomic.StoreUint32(&r.spaceSpinCutoff, max(spinIterationsMin, newCutoff))

			hdr.IncSpaceWaiters()
			exp := hdr.SpaceSequence()
			// Re-check condition to avoid missed wake
			writeIdx = hdr.WriteIndex()
			readIdx = hdr.ReadIndex()
			if (r.capacity - (writeIdx - readIdx)) >= uint64(len(data)) {
				hdr.DecSpaceWaiters()
				continue
			}
			_ = futexWait(&hdr.spaceSeq, exp)
			hdr.DecSpaceWaiters()
			// Re-check closure after wake to avoid infinite loop
			if hdr.Closed() {
				return ErrRingClosed
			}
			continue
		}
		// Not full but not enough: spin-wait then wait on contigSeq.
		// Phase 1: Spin-wait before falling back to futex
		spinLimit := atomic.LoadUint32(&r.spaceSpinCutoff)
		spaceAvailable := false
		for spin := uint32(0); spin < spinLimit; spin++ {
			writeIdx = hdr.WriteIndex()
			readIdx = hdr.ReadIndex()
			if (r.capacity - (writeIdx - readIdx)) >= uint64(len(data)) {
				if spin > 0 {
					target := min(spinIterationsMax, spin*2)
					newCutoff := (7*spinLimit + target) / 8
					atomic.StoreUint32(&r.spaceSpinCutoff, max(spinIterationsMin, newCutoff))
				}
				spaceAvailable = true
				break
			}
			if hdr.Closed() {
				return ErrRingClosed
			}
			runtime_procyield(1)
		}
		if spaceAvailable {
			continue
		}
		// Phase 2: Spin failed, fall back to futex
		newCutoff := (7*spinLimit + spinIterationsMin) / 8
		atomic.StoreUint32(&r.spaceSpinCutoff, max(spinIterationsMin, newCutoff))

		hdr.IncContigWaiters()
		exp := hdr.ContigSequence()
		// Re-check prior to waiting
		writeIdx = hdr.WriteIndex()
		readIdx = hdr.ReadIndex()
		if (r.capacity - (writeIdx - readIdx)) >= uint64(len(data)) {
			hdr.DecContigWaiters()
			continue
		}
		_ = futexWait(&hdr.contigSeq, exp)
		hdr.DecContigWaiters()
		// Re-check closure after wake to avoid infinite loop
		if hdr.Closed() {
			return ErrRingClosed
		}
	}
}

// ReadBlocking reads data from the ring buffer using an event-driven consumer algorithm.
// Blocks until data is available or the ring is closed.
//
// This implements the high-performance SPSC algorithm as specified:
// - Uses write/read indices for actual data tracking
// - Uses dataSeq/spaceSeq for futex-based event notification
// - Performs zero-copy data transfer
// - Handles spurious wakes correctly
func (r *ShmRing) ReadBlocking(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil // No-op for empty buffer
	}

	hdr := r.header()

	// Consumer side: read data and signal producer
	for {
		// Check for closure first
		if hdr.Closed() {
			// Check if data is still available even when closed
			writeIdx := hdr.WriteIndex()
			readIdx := hdr.ReadIndex()
			if writeIdx == readIdx {
				return 0, io.EOF
			}
			// Fall through to read remaining data
		}

		// Load current indices to check available data
		writeIdx := hdr.WriteIndex()
		readIdx := hdr.ReadIndex()

		// Calculate available data using indices
		availableBefore := writeIdx - readIdx

		if availableBefore > 0 {
			// Data available - perform the read
			readPos := readIdx & r.capMask

			// Determine how much to read (up to buffer size and available data)
			toRead := uint64(len(buf))
			if toRead > availableBefore {
				toRead = availableBefore
			}

			var bytesRead int

			// Handle ring wrap-around
			if readPos+toRead <= r.capacity {
				// Simple case: no wrap
				srcPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				toReadI := int(toRead)
				bytesRead = copy(buf, unsafe.Slice((*byte)(srcPtr), toReadI))
			} else {
				// Wrap case: split the read
				firstChunk := r.capacity - readPos
				firstChunkI := int(firstChunk)

				// Read first chunk from end of buffer
				srcPtr1 := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				bytesRead = copy(buf, unsafe.Slice((*byte)(srcPtr1), firstChunkI))

				// Read second chunk from beginning of buffer
				srcPtr2 := r.dataPtr()
				secondI := int(toRead - firstChunk)
				bytesRead += copy(buf[bytesRead:], unsafe.Slice((*byte)(srcPtr2), secondI))
			}

			// Advance read index.
			// Memory ordering rationale:
			// 1) Reader copies out bytes first
			// 2) Publish the new read index with an atomic store (acts as a release)
			// 3) Bump contigSeq always (contiguity improved), and spaceSeq only on full→not-full.
			prevUsed := availableBefore
			hdr.SetReadIndex(readIdx + uint64(bytesRead)) // release-publish

			if bytesRead > 0 {
				// Contiguity: always bump after any positive read commit
				hdr.IncrementContigSequence()
				if hdr.ContigWaiters() > 0 {
					futexWake(&hdr.contigSeq, 1)
				}
			}

			// Space became available only if we were full before this read
			if prevUsed == r.capacity {
				hdr.IncrementSpaceSequence()
				if hdr.SpaceWaiters() > 0 {
					futexWake(&hdr.spaceSeq, 1)
				}
			}

			return bytesRead, nil
		}

		// No data available - check closure and wait for producer
		if !hdr.Closed() {
			// Phase 1: Spin-wait for a short duration before falling back to futex.
			// This dramatically reduces latency in ping-pong patterns where data
			// arrives quickly, avoiding the ~10µs futex wake overhead.
			spinLimit := atomic.LoadUint32(&r.dataSpinCutoff)
			for spin := uint32(0); spin < spinLimit; spin++ {
				// Check if data arrived during spin
				if hdr.WriteIndex()-hdr.ReadIndex() > 0 {
					// Data arrived! Update adaptive cutoff (success = spin faster next time)
					if spin > 0 {
						// Exponential moving average: new = (7*old + target) / 8
						target := min(spinIterationsMax, spin*2)
						newCutoff := (7*spinLimit + target) / 8
						atomic.StoreUint32(&r.dataSpinCutoff, max(spinIterationsMin, newCutoff))
					}
					continue // Re-enter main loop to read data
				}
				// Check closure during spin
				if hdr.Closed() {
					return 0, io.EOF
				}
				// PAUSE instruction - yields to hyperthread, saves power
				runtime_procyield(1)
			}

			// Phase 2: Spin didn't succeed, fall back to futex
			// Reduce spin cutoff (timeout = spin less next time)
			newCutoff := (7*spinLimit + spinIterationsMin) / 8
			atomic.StoreUint32(&r.dataSpinCutoff, max(spinIterationsMin, newCutoff))

			hdr.IncDataWaiters()
			dataSeq := hdr.DataSequence()
			// Re-check data availability and closed state before sleeping
			writeIdx := hdr.WriteIndex()
			readIdx := hdr.ReadIndex()
			if writeIdx-readIdx > 0 {
				hdr.DecDataWaiters()
				continue
			}
			// Re-check closed flag to avoid missing a close that happened
			// after our initial check but before we entered futexWait
			if hdr.Closed() {
				hdr.DecDataWaiters()
				return 0, io.EOF
			}
			if err := futexWait(&hdr.dataSeq, dataSeq); err != nil {
				// Continue loop for spurious wake or other wake reasons
			}
			// Check if ring is still valid before decrementing - segment may
			// have been unmapped while we were blocked on futexWait
			if atomic.LoadUint32(&r.closed) == 0 {
				hdr.DecDataWaiters()
			}
		} else {
			// Closed and no data - return EOF
			return 0, io.EOF
		}
	}
}

// Close closes the ring for writing. Readers can still read remaining data.
func (r *ShmRing) Close() error {
	// Make Close idempotent. This is important because multiple higher-level
	// objects may try to close the same ring during shutdown, and a late Close
	// must not touch shared memory if the segment has already been unmapped.
	if !atomic.CompareAndSwapUint32(&r.closed, 0, 1) {
		return nil
	}

	hdr := r.header()
	hdr.SetClosed(true)

	// Wake up any waiting readers and writers; bump sequences to release waiters.
	hdr.IncrementDataSequence()
	hdr.IncrementSpaceSequence()
	hdr.IncrementContigSequence()
	futexWake(&hdr.dataSeq, 1)
	futexWake(&hdr.spaceSeq, 1)
	futexWake(&hdr.contigSeq, 1)

	return nil
}

// Available returns the number of bytes available for writing
func (r *ShmRing) Available() uint64 {
	return r.header().Available()
}

// Used returns the number of bytes currently used in the ring
func (r *ShmRing) Used() uint64 {
	return r.header().Used()
}

// IsClosed returns true if the ring is closed for writing
func (r *ShmRing) IsClosed() bool {
	return r.header().Closed()
}

// IsEmpty returns true if the ring contains no data
func (r *ShmRing) IsEmpty() bool {
	return r.header().Used() == 0
}

// IsFull returns true if the ring is completely full
func (r *ShmRing) IsFull() bool {
	return r.header().Available() == 0
}

// WriteBlockingContext writes data to the ring buffer with context deadline support.
// Blocks until space is available, the ring is closed, or context deadline exceeded.
// Returns context.DeadlineExceeded if the context deadline is exceeded.
func (r *ShmRing) WriteBlockingContext(ctx context.Context, data []byte) error {
	if len(data) == 0 {
		return nil // No-op for empty data
	}

	// Check if data fits in ring capacity
	if uint64(len(data)) > r.capacity {
		return errors.New("data larger than ring capacity")
	}

	hdr := r.header()

	// Producer side: write data and signal consumer
	for {
		// Check for closure first
		if hdr.Closed() {
			return ErrRingClosed
		}

		// Check context cancellation/deadline
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Load current indices to check available space
		writeIdx := hdr.WriteIndex()
		readIdx := hdr.ReadIndex()

		// Calculate available space using indices
		usedBefore := writeIdx - readIdx
		available := r.capacity - usedBefore

		if uint64(len(data)) <= available {
			// Space available - perform the write (same as original WriteBlocking)
			writePos := writeIdx & r.capMask

			// Handle ring wrap-around
			if writePos+uint64(len(data)) <= r.capacity {
				// Simple case: no wrap
				destPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				copy(unsafe.Slice((*byte)(destPtr), len(data)), data)
			} else {
				// Wrap case: split the write
				firstChunk := r.capacity - writePos
				firstChunkI := int(firstChunk)

				// Write first chunk at end of buffer
				destPtr1 := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				copy(unsafe.Slice((*byte)(destPtr1), firstChunkI), data[:firstChunkI])

				// Write second chunk at beginning of buffer
				destPtr2 := r.dataPtr()
				copy(unsafe.Slice((*byte)(destPtr2), len(data)-firstChunkI), data[firstChunkI:])
			}

			hdr.SetWriteIndex(writeIdx + uint64(len(data))) // release-publish

			// Readers may block waiting for N bytes, not just “ring is empty”, so wake
			// after any successful write commit.
			if len(data) > 0 {
				hdr.IncrementDataSequence()
				// Only wake if readers are actually blocked on futex (not spinning)
				if hdr.DataWaiters() > 0 {
					futexWake(&hdr.dataSeq, 1)
				}
			}

			return nil
		}

		// Need to wait for space (distinguish full vs partial)

		// Calculate timeout from context deadline
		var timeoutNs int64
		if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return context.DeadlineExceeded
			}
			timeoutNs = remaining.Nanoseconds()
		}

		// Wait for space with timeout
		var err error
		if timeoutNs > 0 {
			// Re-check and choose wait primitive
			writeIdx = hdr.WriteIndex()
			readIdx = hdr.ReadIndex()
			usedBefore = writeIdx - readIdx
			available = r.capacity - usedBefore
			if uint64(len(data)) <= available {
				continue
			}
			if available == 0 {
				hdr.IncSpaceWaiters()
				exp := hdr.SpaceSequence()
				// Re-check
				writeIdx = hdr.WriteIndex()
				readIdx = hdr.ReadIndex()
				if (r.capacity - (writeIdx - readIdx)) >= uint64(len(data)) {
					hdr.DecSpaceWaiters()
					continue
				}
				err = futexWaitTimeout(&hdr.spaceSeq, exp, timeoutNs)
				hdr.DecSpaceWaiters()
			} else {
				hdr.IncContigWaiters()
				exp := hdr.ContigSequence()
				// Re-check
				writeIdx = hdr.WriteIndex()
				readIdx = hdr.ReadIndex()
				if (r.capacity - (writeIdx - readIdx)) >= uint64(len(data)) {
					hdr.DecContigWaiters()
					continue
				}
				err = futexWaitTimeout(&hdr.contigSeq, exp, timeoutNs)
				hdr.DecContigWaiters()
			}
		} else {
			// No timeout: same logic with infinite waits
			writeIdx = hdr.WriteIndex()
			readIdx = hdr.ReadIndex()
			usedBefore = writeIdx - readIdx
			available = r.capacity - usedBefore
			if uint64(len(data)) <= available {
				continue
			}
			if available == 0 {
				hdr.IncSpaceWaiters()
				exp := hdr.SpaceSequence()
				// Re-check
				writeIdx = hdr.WriteIndex()
				readIdx = hdr.ReadIndex()
				if (r.capacity - (writeIdx - readIdx)) >= uint64(len(data)) {
					hdr.DecSpaceWaiters()
					continue
				}
				err = futexWait(&hdr.spaceSeq, exp)
				hdr.DecSpaceWaiters()
			} else {
				hdr.IncContigWaiters()
				exp := hdr.ContigSequence()
				// Re-check
				writeIdx = hdr.WriteIndex()
				readIdx = hdr.ReadIndex()
				if (r.capacity - (writeIdx - readIdx)) >= uint64(len(data)) {
					hdr.DecContigWaiters()
					continue
				}
				err = futexWait(&hdr.contigSeq, exp)
				hdr.DecContigWaiters()
			}
		}

		if err != nil {
			// Check if it's a timeout error
			if errors.Is(err, ErrFutexTimeout) {
				return context.DeadlineExceeded
			}
			return err
		}
	}
}

// ReadBlockingContext reads data from the ring buffer with context deadline support.
// Blocks until data is available, the ring is closed, or context deadline exceeded.
// Returns context.DeadlineExceeded if the context deadline is exceeded.
func (r *ShmRing) ReadBlockingContext(ctx context.Context, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil // No-op for empty buffer
	}

	hdr := r.header()

	// Consumer side: read data and signal producer
	for {
		// Check context cancellation/deadline
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}

		// Load current indices to check available data
		writeIdx := hdr.WriteIndex()
		readIdx := hdr.ReadIndex()

		usedBefore := writeIdx - readIdx

		if usedBefore > 0 {
			// Data available - perform the read
			toRead := usedBefore
			if toRead > uint64(len(buf)) {
				toRead = uint64(len(buf))
			}

			readPos := readIdx & r.capMask

			var bytesRead int

			// Handle ring wrap-around
			if readPos+toRead <= r.capacity {
				// Simple case: no wrap
				srcPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				toReadI := int(toRead)
				bytesRead = copy(buf, unsafe.Slice((*byte)(srcPtr), toReadI))
			} else {
				// Wrap case: split the read
				firstChunk := r.capacity - readPos
				firstChunkI := int(firstChunk)

				// Read first chunk from end of buffer
				srcPtr1 := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				bytesRead = copy(buf, unsafe.Slice((*byte)(srcPtr1), firstChunkI))

				// Read second chunk from beginning of buffer
				srcPtr2 := r.dataPtr()
				secondChunkI := int(toRead - firstChunk)
				bytesRead += copy(buf[bytesRead:], unsafe.Slice((*byte)(srcPtr2), secondChunkI))
			}

			// Advance read index.
			// Memory ordering rationale:
			// 1) Reader copies out bytes first
			// 2) Publish the new read index with an atomic store (acts as a release)
			// 3) Bump contigSeq always (contiguity improved), wake spaceSeq waiters if any.
			hdr.SetReadIndex(readIdx + uint64(bytesRead)) // release-publish

			if bytesRead > 0 {
				// Contiguity: always bump after any positive read commit.
				// ContigWaiters are waiting for "more space" (not full ring), so wake on every read.
				hdr.IncrementContigSequence()
				if hdr.ContigWaiters() > 0 {
					futexWake(&hdr.contigSeq, 1)
				}
			}

			// Space: always wake waiters if any are waiting.
			// This is conservative but avoids potential races in the waiter registration.
			if bytesRead > 0 && hdr.SpaceWaiters() > 0 {
				hdr.IncrementSpaceSequence()
				newSeq := hdr.SpaceSequence()
				shmDebugf("READBLOCKING_SPACE_WAKE: freed %d bytes, new spaceSeq=%d, waking waiters",
					bytesRead, newSeq)
				futexWake(&hdr.spaceSeq, 1)
			}

			return bytesRead, nil
		}

		// Check if ring is closed and no data available
		if hdr.Closed() {
			return 0, io.EOF
		}

		// Need to wait for data
		hdr.IncDataWaiters()
		dataSeq := hdr.DataSequence()
		shmDebugf("READBLOCKING_DATA_WAIT: empty ring, dataWaiters=%d, dataSeq=%d, widx=%d, ridx=%d",
			hdr.DataWaiters(), dataSeq, hdr.WriteIndex(), hdr.ReadIndex())

		// Re-check data availability before sleeping
		writeIdx = hdr.WriteIndex()
		readIdx = hdr.ReadIndex()
		if writeIdx-readIdx > 0 {
			hdr.DecDataWaiters()
			continue
		}

		// Re-check closed flag after incrementing waiters to avoid missing
		// a close that happened between our initial check and now
		if hdr.Closed() {
			hdr.DecDataWaiters()
			return 0, io.EOF
		}

		// Calculate timeout from context deadline
		var timeoutNs int64
		if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				// Check if ring is still valid before decrementing
				if atomic.LoadUint32(&r.closed) == 0 {
					hdr.DecDataWaiters()
				}
				return 0, context.DeadlineExceeded
			}
			timeoutNs = remaining.Nanoseconds()
		}

		// Wait for data with timeout
		var err error
		if timeoutNs > 0 {
			err = futexWaitTimeout(&hdr.dataSeq, dataSeq, timeoutNs)
		} else {
			err = futexWait(&hdr.dataSeq, dataSeq)
		}
		// Check if ring is still valid before decrementing - segment may
		// have been unmapped while we were blocked on futexWait
		if atomic.LoadUint32(&r.closed) == 0 {
			hdr.DecDataWaiters()
		}

		if err != nil {
			// Check if it's a timeout error
			if errors.Is(err, ErrFutexTimeout) {
				return 0, context.DeadlineExceeded
			}
			return 0, err
		}
	}
}

// DiagnoseDuelingBuffers checks if both rings in a duplex connection are full,
// indicating a potential deadlock scenario. Returns diagnostic information.
func DiagnoseDuelingBuffers(clientToServer, serverToClient *ShmRing) (bool, string) {
	csState := clientToServer.DebugState()
	scState := serverToClient.DebugState()

	// Check if both rings are full or nearly full
	csUsedPercent := float64(csState.Used) / float64(csState.Capacity) * 100
	scUsedPercent := float64(scState.Used) / float64(scState.Capacity) * 100

	isDueling := csUsedPercent >= 95.0 && scUsedPercent >= 95.0

	diagnostic := ""
	if isDueling {
		diagnostic = "DUELING FULL BUFFERS DETECTED:\n"
	} else {
		diagnostic = "Ring Buffer State:\n"
	}

	diagnostic += fmt.Sprintf("Client→Server: Used=%d/%d (%.1f%%) Widx=%d Ridx=%d DataSeq=%d SpaceSeq=%d Closed=%d\n",
		csState.Used, csState.Capacity, csUsedPercent,
		csState.Widx, csState.Ridx, csState.DataSeq, csState.SpaceSeq, csState.Closed)

	diagnostic += fmt.Sprintf("Server→Client: Used=%d/%d (%.1f%%) Widx=%d Ridx=%d DataSeq=%d SpaceSeq=%d Closed=%d\n",
		scState.Used, scState.Capacity, scUsedPercent,
		scState.Widx, scState.Ridx, scState.DataSeq, scState.SpaceSeq, scState.Closed)

	if isDueling {
		diagnostic += "This indicates both sides are blocked: client can't write (server→client full), server can't echo (client→server full).\n"
		diagnostic += "Solution: Use concurrent read/write instead of sequential operations."
	}

	return isDueling, diagnostic
}

// WriteReservation represents a reservation for writing data to the ring.
// The caller must fill exactly the reserved bytes and call Commit with the actual bytes written.
type WriteReservation struct {
	First    []byte   // First contiguous slice (from write position to end of buffer or requested size)
	Second   []byte   // Second contiguous slice (from start of buffer) - may be empty if First has enough space
	ring     *ShmRing // Ring buffer reference for commit
	writeIdx uint64   // Write index at reservation time
	maxBytes int      // Maximum bytes that can be committed
}

// Commit commits the written bytes and advances the write index.
// written must not exceed maxBytes (the reservation size).
func (wr *WriteReservation) Commit(written int) error {
	if written < 0 || written > wr.maxBytes {
		return fmt.Errorf("invalid written count %d, expected 0-%d", written, wr.maxBytes)
	}

	hdr := wr.ring.header()

	// Publish new write index.
	hdr.SetWriteIndex(wr.writeIdx + uint64(written)) // release-publish

	if written > 0 {
		hdr.IncrementDataSequence()
		newSeq := hdr.DataSequence()
		waiters := hdr.DataWaiters()
		shmDebugf("COMMIT_DATA_WAKE: written=%d, newSeq=%d, dataWaiters=%d", written, newSeq, waiters)
		// Only wake if there are waiters - avoids unnecessary syscalls
		if waiters > 0 {
			shmDebugf("COMMIT_DATA_WAKE: waking 1 waiter")
			futexWake(&hdr.dataSeq, 1)
		}
	}

	return nil
}

// ReserveWrite blocks until at least n bytes of contiguous space is available, then returns
// the writable slice(s) and a commit function. This enables zero-copy writes directly into
// the ring buffer memory. Headers may span across wrap boundaries via First+Second slices.
func (r *ShmRing) ReserveWrite(ctx context.Context, n int) (WriteReservation, error) {
	if n <= 0 {
		return WriteReservation{}, errors.New("reservation size must be positive")
	}

	if uint64(n) > r.capacity {
		return WriteReservation{}, errors.New("reservation larger than ring capacity")
	}

	hdr := r.header()

	for {
		// Check context cancellation first
		select {
		case <-ctx.Done():
			return WriteReservation{}, ctx.Err()
		default:
		}

		// Check for closure - do this after context check to avoid race with segment cleanup
		if hdr.Closed() {
			return WriteReservation{}, ErrRingClosed
		}

		// Load current indices to check available space
		writeIdx := hdr.WriteIndex()
		readIdx := hdr.ReadIndex()

		// Calculate available space
		usedBefore := writeIdx - readIdx
		available := r.capacity - usedBefore

		if uint64(n) <= available {
			// Space available - create reservation
			writePos := writeIdx & r.capMask

			var first, second []byte

			// Handle ring wrap-around - headers may straddle wrap
			if writePos+uint64(n) <= r.capacity {
				// Simple case: no wrap needed
				firstPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				first = unsafe.Slice((*byte)(firstPtr), n)
			} else {
				// Wrap case: split across end and beginning (header can straddle)
				firstLen := r.capacity - writePos
				firstPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				firstLenI := int(firstLen)
				first = unsafe.Slice((*byte)(firstPtr), firstLenI)

				secondLen := uint64(n) - firstLen
				secondPtr := r.dataPtr()
				secondLenI := int(secondLen)
				second = unsafe.Slice((*byte)(secondPtr), secondLenI)
			}

			// Return reservation with embedded commit state (no heap allocation)
			return WriteReservation{
				First:    first,
				Second:   second,
				ring:     r,
				writeIdx: writeIdx,
				maxBytes: n,
			}, nil
		}

		// Insufficient space - spin briefly before falling back to futex.
		spinCutoff := atomic.LoadUint32(&r.spaceSpinCutoff)
		spinSuccess := false
		for i := uint32(0); i < spinCutoff; i++ {
			runtime_procyield(1) // PAUSE instruction to reduce power/contention
			writeIdx = hdr.WriteIndex()
			readIdx = hdr.ReadIndex()
			if (r.capacity - (writeIdx - readIdx)) >= uint64(n) {
				spinSuccess = true
				// Adapt spin cutoff upward
				newCutoff := (7*spinCutoff + spinIterationsMax) / 8
				if newCutoff > spinIterationsMax {
					newCutoff = spinIterationsMax
				}
				atomic.StoreUint32(&r.spaceSpinCutoff, newCutoff)
				break
			}
		}
		if spinSuccess {
			continue // Loop back to reserve space
		}
		// Spin timed out - adapt cutoff downward
		newCutoff := (7*spinCutoff + spinIterationsMin) / 8
		if newCutoff < spinIterationsMin {
			newCutoff = spinIterationsMin
		}
		atomic.StoreUint32(&r.spaceSpinCutoff, newCutoff)

		if dl, ok := ctx.Deadline(); ok {
			shmDebugf("ReserveWrite: waiting with timeout=%s", time.Until(dl))
		} else {
			shmDebugf("ReserveWrite: waiting WITHOUT timeout")
		}

		// Spin failed - fall back to futex, choosing wait type based on fullness
		writeIdx = hdr.WriteIndex()
		readIdx = hdr.ReadIndex()
		free := r.capacity - (writeIdx - readIdx)
		if free == 0 {
			hdr.IncSpaceWaiters()
			exp := hdr.SpaceSequence()
			shmDebugf("RESERVE_WRITE_SPACE_WAIT: ring FULL, spaceWaiters=%d, exp=%d, widx=%d, ridx=%d",
				hdr.SpaceWaiters(), exp, writeIdx, readIdx)
			// Re-check
			writeIdx = hdr.WriteIndex()
			readIdx = hdr.ReadIndex()
			if (r.capacity - (writeIdx - readIdx)) >= uint64(n) {
				hdr.DecSpaceWaiters()
				continue
			}
			var err error
			if deadline, has := ctx.Deadline(); has {
				rem := time.Until(deadline)
				if rem <= 0 {
					hdr.DecSpaceWaiters()
					return WriteReservation{}, context.DeadlineExceeded
				}
				shmDebugf("FUTEX_ENTER: exp=%d, rem=%v", exp, rem)
				err = futexWaitTimeout(&hdr.spaceSeq, exp, rem.Nanoseconds())
				shmDebugf("FUTEX_EXIT: exp=%d, err=%v, newSeq=%d", exp, err, hdr.SpaceSequence())
			} else {
				err = futexWait(&hdr.spaceSeq, exp)
			}
			hdr.DecSpaceWaiters()
			if err != nil {
				if errors.Is(err, ErrFutexTimeout) {
					return WriteReservation{}, context.DeadlineExceeded
				}
				if ctx.Err() != nil {
					return WriteReservation{}, ctx.Err()
				}
				// else spurious wake: continue loop
			}
			// Re-check closure after wake to avoid infinite loop
			if hdr.Closed() {
				return WriteReservation{}, ErrRingClosed
			}
			continue
		}
		// Not full: contiguity-improving reads help
		hdr.IncContigWaiters()
		exp := hdr.ContigSequence()
		// Re-check
		writeIdx = hdr.WriteIndex()
		readIdx = hdr.ReadIndex()
		if (r.capacity - (writeIdx - readIdx)) >= uint64(n) {
			hdr.DecContigWaiters()
			continue
		}
		var err error
		if deadline, has := ctx.Deadline(); has {
			rem := time.Until(deadline)
			if rem <= 0 {
				hdr.DecContigWaiters()
				return WriteReservation{}, context.DeadlineExceeded
			}
			err = futexWaitTimeout(&hdr.contigSeq, exp, rem.Nanoseconds())
		} else {
			err = futexWait(&hdr.contigSeq, exp)
		}
		hdr.DecContigWaiters()
		if err != nil {
			if errors.Is(err, ErrFutexTimeout) {
				return WriteReservation{}, context.DeadlineExceeded
			}
			if ctx.Err() != nil {
				return WriteReservation{}, ctx.Err()
			}
			// else spurious wake: continue loop
		}
		// Re-check closure after wake to avoid infinite loop
		if hdr.Closed() {
			return WriteReservation{}, ErrRingClosed
		}
	}
}

// ReadSlices blocks until at least n bytes are available to read; returns slices spanning wrap.
// This enables proper reconstruction of headers that may straddle wrap boundaries.
// The caller must call commit.Commit() with the number of bytes consumed.
func (r *ShmRing) ReadSlices(ctx context.Context, n int) (first, second []byte, commit *ReadCommit, err error) {
	if n <= 0 {
		return nil, nil, nil, errors.New("read size must be positive")
	}

	hdr := r.header()

	for {
		// Check context cancellation first
		select {
		case <-ctx.Done():
			return nil, nil, nil, ctx.Err()
		default:
		}

		// Check closed state - but always allow reading remaining data first.
		// The local closed flag (r.closed) is set when this ring is closed,
		// but we should still drain any remaining data before returning EOF.
		localClosed := atomic.LoadUint32(&r.closed) != 0
		headerClosed := hdr.Closed()

		// Use pendingReadIdx for availability (allows read-ahead while buffers are held)
		pendingIdx := atomic.LoadUint64(&r.pendingReadIdx)

		if localClosed || headerClosed {
			// Check if data is still available even when closed
			writeIdx := hdr.WriteIndex()
			availableBefore := writeIdx - pendingIdx
			if availableBefore == 0 {
				return nil, nil, nil, io.EOF
			}
			// Fall through to read remaining data if available
		}

		// Load current write index to check available data
		writeIdx := hdr.WriteIndex()
		// Also get shared readIdx for the commit function (to free space for writer)
		sharedReadIdx := hdr.ReadIndex()

		// Calculate available data using pendingReadIdx (not shared readIdx)
		availableBefore := writeIdx - pendingIdx

		if availableBefore >= uint64(n) {
			// Data available - create slices using pendingIdx (local read position)
			readPos := pendingIdx & r.capMask

			var firstSlice, secondSlice []byte

			// Handle ring wrap-around - allow headers to straddle wrap
			if readPos+uint64(n) <= r.capacity {
				// Simple case: no wrap needed
				srcPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				firstSlice = unsafe.Slice((*byte)(srcPtr), n)
			} else {
				// Wrap case: split across end and beginning
				firstLen := r.capacity - readPos
				firstPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				firstLenI := int(firstLen)
				firstSlice = unsafe.Slice((*byte)(firstPtr), firstLenI)

				secondLen := uint64(n) - firstLen
				secondPtr := r.dataPtr()
				secondLenI := int(secondLen)
				secondSlice = unsafe.Slice((*byte)(secondPtr), secondLenI)
			}

			// Advance pendingReadIdx now - this allows us to read ahead
			// while the application holds the buffer
			atomic.StoreUint64(&r.pendingReadIdx, pendingIdx+uint64(n))

			// Set up pre-allocated commit context (no closure allocation)
			r.readCommit.commitReadIdx = sharedReadIdx
			r.readCommit.maxBytes = n

			return firstSlice, secondSlice, &r.readCommit, nil
		}

		// No data available - spin briefly before falling back to futex.
		// This avoids syscall overhead in the common case where data arrives quickly.
		spinCutoff := atomic.LoadUint32(&r.dataSpinCutoff)
		spinSuccess := false
		for i := uint32(0); i < spinCutoff; i++ {
			runtime_procyield(1) // PAUSE instruction to reduce power/contention
			writeIdx = hdr.WriteIndex()
			pendingIdx = atomic.LoadUint64(&r.pendingReadIdx)
			if writeIdx-pendingIdx >= uint64(n) {
				spinSuccess = true
				// Adapt spin cutoff upward (exponential moving average)
				newCutoff := (7*spinCutoff + spinIterationsMax) / 8
				if newCutoff > spinIterationsMax {
					newCutoff = spinIterationsMax
				}
				atomic.StoreUint32(&r.dataSpinCutoff, newCutoff)
				break
			}
		}
		if spinSuccess {
			continue // Loop back to return data
		}
		// Spin timed out - adapt cutoff downward
		newCutoff := (7*spinCutoff + spinIterationsMin) / 8
		if newCutoff < spinIterationsMin {
			newCutoff = spinIterationsMin
		}
		atomic.StoreUint32(&r.dataSpinCutoff, newCutoff)

		// Spin failed, fall back to futex - check context first
		select {
		case <-ctx.Done():
			return nil, nil, nil, ctx.Err()
		default:
		}

		if dl, ok := ctx.Deadline(); ok {
			shmDebugf("ReadSlices: waiting with timeout=%s", time.Until(dl))
		} else {
			shmDebugf("ReadSlices: waiting WITHOUT timeout")
		}

		// Check local closed flag before accessing header
		// If closed, re-check for data - producer may have written before closing
		localClosed = atomic.LoadUint32(&r.closed) != 0
		headerClosed = hdr.Closed()

		if localClosed || headerClosed {
			// Re-check if data appeared (race with producer)
			writeIdx = hdr.WriteIndex()
			pendingIdx = atomic.LoadUint64(&r.pendingReadIdx)
			available := writeIdx - pendingIdx
			if available == 0 {
				return nil, nil, nil, io.EOF
			}
			// If data exists but not enough to satisfy the request, and the ring
			// is closed (no more data will arrive), return EOF rather than looping.
			if available < uint64(n) {
				return nil, nil, nil, io.EOF
			}
			// Enough data appeared, loop back to read it
			continue
		}

		// Snapshot the producer's wake-up sequence, then re-check indices before
		// sleeping to avoid a lost-wake race where the producer commits data after
		// our availability check but before we enter futexWait.
		hdr.IncDataWaiters()
		dataSeq := hdr.DataSequence()
		writeIdx = hdr.WriteIndex()
		pendingIdx = atomic.LoadUint64(&r.pendingReadIdx)
		if writeIdx-pendingIdx >= uint64(n) {
			// Data became available; loop back to return slices.
			hdr.DecDataWaiters()
			continue
		}

		// Re-check closed flag after incrementing waiters and before futexWait
		// to avoid missing a close that happened between our initial check and now
		localClosed = atomic.LoadUint32(&r.closed) != 0
		headerClosed = hdr.Closed()
		if localClosed || headerClosed {
			hdr.DecDataWaiters()
			// Re-check if data appeared
			writeIdx = hdr.WriteIndex()
			pendingIdx = atomic.LoadUint64(&r.pendingReadIdx)
			if writeIdx-pendingIdx >= uint64(n) {
				continue
			}
			return nil, nil, nil, io.EOF
		}

		shmDebugf("[DEBUG] Ring read: no data available, dataSeq=%d, waiting on futex...", dataSeq)

		// If ctx has a deadline, wait with timeout; otherwise, infinite wait.
		var err error
		if deadline, has := ctx.Deadline(); has {
			rem := time.Until(deadline)
			if rem <= 0 {
				// Check if ring is still valid before decrementing
				if atomic.LoadUint32(&r.closed) == 0 {
					hdr.DecDataWaiters()
				}
				return nil, nil, nil, context.DeadlineExceeded
			}
			shmDebugf("[DEBUG] Ring read: calling futexWaitTimeout with timeout=%v", rem)
			err = futexWaitTimeout(&hdr.dataSeq, dataSeq, rem.Nanoseconds())
		} else {
			shmDebugf("[DEBUG] Ring read: calling futexWait (no timeout)")
			err = futexWait(&hdr.dataSeq, dataSeq)
		}
		// Check if ring is still valid before decrementing - the segment may have
		// been unmapped while we were blocked on futexWait
		if atomic.LoadUint32(&r.closed) == 0 {
			hdr.DecDataWaiters()
		}
		shmDebugf("[DEBUG] Ring read: futex returned, err=%v", err)

		if err != nil {
			// Translate futex timeout to context timeout; keep going on spurious wake.
			if errors.Is(err, ErrFutexTimeout) {
				return nil, nil, nil, context.DeadlineExceeded
			}
			// Other errors: fall through and reloop (spurious wake/etc.)
		}
	}
}

// WriteAll writes all bytes to the ring buffer, blocking as needed.
// This is a convenience method that handles multiple reservations if needed.
// Supports chunking when message > available space.
func (r *ShmRing) WriteAll(ctx context.Context, p []byte) error {
	if len(p) == 0 {
		return nil
	}

	remaining := p
	for len(remaining) > 0 {
		// Reserve space for as much as possible (up to remaining length)
		toWrite := len(remaining)
		if uint64(toWrite) > r.capacity {
			toWrite = int(r.capacity)
		}

		reservation, err := r.ReserveWrite(ctx, toWrite)
		if err != nil {
			return err
		}

		// Copy data into the reservation (zero-copy into ring memory)
		written := 0
		if len(reservation.First) > 0 {
			n := copy(reservation.First, remaining[written:])
			written += n
		}
		if len(reservation.Second) > 0 && written < toWrite {
			n := copy(reservation.Second, remaining[written:])
			written += n
		}

		// Commit the written bytes
		if err := reservation.Commit(written); err != nil {
			return err
		}

		remaining = remaining[written:]
	}

	return nil
}

// ReadExact reads exactly n bytes into dst, blocking as needed.
// If len(dst) >= n, it uses dst as the buffer (alloc-free).
// Otherwise, it allocates a new slice. Handles header reconstruction across wraps.
func (r *ShmRing) ReadExact(ctx context.Context, n int, dst []byte) ([]byte, error) {
	if n <= 0 {
		return nil, errors.New("read size must be positive")
	}

	// Use dst if it's large enough, otherwise allocate
	var result []byte
	if len(dst) >= n {
		result = dst[:n]
	} else {
		result = make([]byte, n)
	}

	totalRead := 0
	for totalRead < n {
		remaining := n - totalRead

		// Read slices for the remaining bytes
		first, second, commit, err := r.ReadSlices(ctx, remaining)
		if err != nil {
			return nil, err
		}

		// Copy from the slices to our result buffer (handles wrap reconstruction)
		copied := 0
		if len(first) > 0 {
			copyLen := len(first)
			if copyLen > remaining {
				copyLen = remaining
			}
			copy(result[totalRead:], first[:copyLen])
			copied += copyLen
		}
		if len(second) > 0 && copied < remaining {
			copyLen := len(second)
			if copyLen > remaining-copied {
				copyLen = remaining - copied
			}
			copy(result[totalRead+copied:], second[:copyLen])
			copied += copyLen
		}

		// Commit the read
		commit.Commit(copied)
		totalRead += copied
	}

	return result, nil
}

// ReserveFrameHeader reserves a 16-byte frame header region.
// NOTE: Header may straddle wrap boundaries. Use returned First/Second slices accordingly.
// The reader supports straddled headers via ReadSlices for proper reconstruction.
//
// Memory ordering follows the SPSC invariant:
//
//	writer: memcpy header -> atomic.Store(w,new) [release] -> AddUint32(dataSeq) -> futex_wake
//	reader: atomic.Load(w) [acquire] -> copy -> atomic.Store(r,new) [release] -> AddUint32(spaceSeq) -> futex_wake
func (r *ShmRing) ReserveFrameHeader(ctx context.Context) (WriteReservation, error) {
	return r.ReserveWrite(ctx, frameHeaderSize)
}
