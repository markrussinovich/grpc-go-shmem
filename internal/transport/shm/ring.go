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

package shm

import (
	"errors"
	"io"
	"unsafe"
)

// ErrRingClosed indicates that the ring has been closed for writing
var ErrRingClosed = errors.New("ring closed")

// ShmRing represents a single-producer single-consumer (SPSC) ring buffer
// operating over shared memory with event-driven blocking.
//
// This implementation provides high-performance cross-process communication
// with zero-copy operations and minimal kernel calls through futex-based
// synchronization.
type ShmRing struct {
	capMask uint64  // capacity-1 for fast masking (capacity must be power of 2)
	hdrOff  uintptr // base address of RingHeader in mmapped bytes
	dataOff uintptr // base address of data area
	mem     []byte  // the mmapped region (no copying)
	// No Go pointers into shared memory stored here; compute addresses on demand
}

// NewShmRingFromSegment creates a ShmRing from a segment's ring view.
// This provides the high-level blocking API over the low-level ring view.
func NewShmRingFromSegment(ringView *ringView, mem []byte) *ShmRing {
	capacity := ringView.Capacity()
	return &ShmRing{
		capMask: capacity - 1, // capacity is guaranteed to be power of 2
		hdrOff:  uintptr(ringView.offset),
		dataOff: uintptr(ringView.offset + RingHeaderSize),
		mem:     mem,
	}
}

// header returns a pointer to the RingHeader in shared memory
func (r *ShmRing) header() *RingHeader {
	return (*RingHeader)(unsafe.Pointer(uintptr(unsafe.Pointer(&r.mem[0])) + r.hdrOff))
}

// dataPtr returns a pointer to the data area in shared memory
func (r *ShmRing) dataPtr() unsafe.Pointer {
	return unsafe.Pointer(uintptr(unsafe.Pointer(&r.mem[0])) + r.dataOff)
}

// capacity returns the ring capacity
func (r *ShmRing) capacity() uint64 {
	return r.capMask + 1
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
	if uint64(len(data)) > r.capacity() {
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
		used := writeIdx - readIdx
		available := r.capacity() - used

		if uint64(len(data)) <= available {
			// Space available - perform the write
			writePos := writeIdx & r.capMask

			// Handle ring wrap-around
			if writePos+uint64(len(data)) <= r.capacity() {
				// Simple case: no wrap
				destPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				copy((*[1 << 30]byte)(destPtr)[:len(data)], data)
			} else {
				// Wrap case: split the write
				firstChunk := r.capacity() - writePos

				// Write first chunk at end of buffer
				destPtr1 := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				copy((*[1 << 30]byte)(destPtr1)[:firstChunk], data[:firstChunk])

				// Write second chunk at beginning of buffer
				destPtr2 := r.dataPtr()
				copy((*[1 << 30]byte)(destPtr2)[:len(data)-int(firstChunk)], data[firstChunk:])
			}

			// Advance write index
			hdr.SetWriteIndex(writeIdx + uint64(len(data)))

			// Signal data availability to reader
			hdr.IncrementDataSequence()
			futexWake(&hdr.dataSeq, 1)

			return nil
		}

		// No space available - wait for consumer to free space
		spaceSeq := hdr.SpaceSequence()
		if err := futexWait(&hdr.spaceSeq, spaceSeq); err != nil {
			// Check for closure again after wake
			if hdr.Closed() {
				return ErrRingClosed
			}
			// Continue loop for spurious wake or other wake reasons
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
		available := writeIdx - readIdx

		if available > 0 {
			// Data available - perform the read
			readPos := readIdx & r.capMask

			// Determine how much to read (up to buffer size and available data)
			toRead := uint64(len(buf))
			if toRead > available {
				toRead = available
			}

			var bytesRead int

			// Handle ring wrap-around
			if readPos+toRead <= r.capacity() {
				// Simple case: no wrap
				srcPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				bytesRead = copy(buf, (*[1 << 30]byte)(srcPtr)[:toRead])
			} else {
				// Wrap case: split the read
				firstChunk := r.capacity() - readPos

				// Read first chunk from end of buffer
				srcPtr1 := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				bytesRead = copy(buf, (*[1 << 30]byte)(srcPtr1)[:firstChunk])

				// Read second chunk from beginning of buffer
				srcPtr2 := r.dataPtr()
				bytesRead += copy(buf[bytesRead:], (*[1 << 30]byte)(srcPtr2)[:toRead-firstChunk])
			}

			// Advance read index
			hdr.SetReadIndex(readIdx + uint64(bytesRead))

			// Signal space availability to writer
			hdr.IncrementSpaceSequence()
			futexWake(&hdr.spaceSeq, 1)

			return bytesRead, nil
		}

		// No data available and not closed - wait for producer
		if !hdr.Closed() {
			dataSeq := hdr.DataSequence()
			if err := futexWait(&hdr.dataSeq, dataSeq); err != nil {
				// Continue loop for spurious wake or other wake reasons
			}
		} else {
			// Closed and no data - return EOF
			return 0, io.EOF
		}
	}
}

// Close closes the ring for writing. Readers can still read remaining data.
func (r *ShmRing) Close() error {
	hdr := r.header()
	hdr.SetClosed(true)

	// Wake up any waiting readers and writers
	futexWake(&hdr.dataSeq, 1)
	futexWake(&hdr.spaceSeq, 1)

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
