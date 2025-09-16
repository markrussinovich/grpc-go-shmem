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
    "context"
    "errors"
    "fmt"
    "io"
    "time"
    "unsafe"
)

// ErrRingClosed indicates that the ring has been closed for writing
var ErrRingClosed = errors.New("ring closed")

// RingState represents a snapshot of ring buffer state for debugging and diagnostics
type RingState struct {
	Capacity uint64 // Total ri
	Widx     uint64 // Current write index (monotonic)
	Ridx     uint64 // Current read index (monotonic)
	Used     uint64 // Bytes currently in ring (Widx - Ridx)
	DataSeq  uint32 // Data availability sequence number
	SpaceSeq uint32 // Space availability sequence number
	Closed   uint32 // Ring closed flag (0 = open, 1 = closed)
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
	// No Go pointers into shared memory stored here; compute addresses on demand
}

// SMF (Shared Memory Framing) helpers are defined in frame.go. This file uses
// ReserveFrameHeader to ensure 16-byte-aligned headers and PAD behavior.

// NewShmRingFromSegment creates a ShmRing from a segment's ring view.
// This provides the high-level blocking API over the low-level ring view.
func NewShmRingFromSegment(ringView *ringView, mem []byte) *ShmRing {
	capacity := ringView.Capacity()
	return &ShmRing{
		capMask:  capacity - 1, // For modulo operations: pos = idx & capMask
		hdrOff:   uintptr(ringView.offset),
		dataOff:  uintptr(ringView.offset + RingHeaderSize),
		mem:      mem,
		capacity: capacity, // Store actual capacity separately
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

// Capacity returns the ring capacity
func (r *ShmRing) Capacity() uint64 {
	return r.capacity
}

// DebugState returns a snapshot of the current ring state for debugging and diagnostics.
// All values are read atomically for consistent state observation.
func (r *ShmRing) DebugState() RingState {
	hdr := r.header()

	// Read all state atomically for consistent snapshot
	widx := hdr.WriteIndex()
	ridx := hdr.ReadIndex()
	dataSeq := hdr.DataSequence()
	spaceSeq := hdr.SpaceSequence()
	closed := uint32(0)
	if hdr.Closed() {
		closed = 1
	}

	return RingState{
		Capacity: r.capacity,
		Widx:     widx,
		Ridx:     ridx,
		Used:     widx - ridx,
		DataSeq:  dataSeq,
		SpaceSeq: spaceSeq,
		Closed:   closed,
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
				copy((*[1 << 30]byte)(destPtr)[:len(data)], data)
			} else {
				// Wrap case: split the write
				firstChunk := r.capacity - writePos

				// Write first chunk at end of buffer
				destPtr1 := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				copy((*[1 << 30]byte)(destPtr1)[:firstChunk], data[:firstChunk])

				// Write second chunk at beginning of buffer
				destPtr2 := r.dataPtr()
				copy((*[1 << 30]byte)(destPtr2)[:len(data)-int(firstChunk)], data[firstChunk:])
			}

			// Advance write index.
			// Memory ordering rationale:
			// 1) Bytes are copied into the ring first (normal stores)
			// 2) Publish the new write index with an atomic store (acts as a release)
			// 3) Bump dataSeq and futex_wake so any waiter sees the published bytes
			hdr.SetWriteIndex(writeIdx + uint64(len(data))) // release-publish

			// Only wake readers if buffer transitioned from empty → non-empty.
			// Waiters use futex_wait on dataSeq with equality semantics; we increment
			// after publishing w to avoid lost wakeups.
			if usedBefore == 0 {
				hdr.IncrementDataSequence()
				futexWake(&hdr.dataSeq, 1)
			}

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
				bytesRead = copy(buf, (*[1 << 30]byte)(srcPtr)[:toRead])
			} else {
				// Wrap case: split the read
				firstChunk := r.capacity - readPos

				// Read first chunk from end of buffer
				srcPtr1 := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				bytesRead = copy(buf, (*[1 << 30]byte)(srcPtr1)[:firstChunk])

				// Read second chunk from beginning of buffer
				srcPtr2 := r.dataPtr()
				bytesRead += copy(buf[bytesRead:], (*[1 << 30]byte)(srcPtr2)[:toRead-firstChunk])
			}

			// Advance read index.
			// Memory ordering rationale:
			// 1) Reader copies out bytes first
			// 2) Publish the new read index with an atomic store (acts as a release)
			// 3) Bump spaceSeq and futex_wake so a blocked writer can proceed
			hdr.SetReadIndex(readIdx + uint64(bytesRead)) // release-publish

			// Only wake writers if buffer transitioned from full → not-full.
			usedBefore := writeIdx - readIdx
			if usedBefore == r.capacity {
				hdr.IncrementSpaceSequence()
				futexWake(&hdr.spaceSeq, 1)
			}

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
				copy((*[1 << 30]byte)(destPtr)[:len(data)], data)
			} else {
				// Wrap case: split the write
				firstChunk := r.capacity - writePos

				// Write first chunk at end of buffer
				destPtr1 := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				copy((*[1 << 30]byte)(destPtr1)[:firstChunk], data[:firstChunk])

				// Write second chunk at beginning of buffer
				destPtr2 := r.dataPtr()
				copy((*[1 << 30]byte)(destPtr2)[:len(data)-int(firstChunk)], data[firstChunk:])
			}

			// Advance write index (release-publish) then signal via dataSeq/futex.
			hdr.SetWriteIndex(writeIdx + uint64(len(data)))

			// Only wake readers if buffer transitioned from empty → non-empty.
			if usedBefore == 0 {
				hdr.IncrementDataSequence()
				futexWake(&hdr.dataSeq, 1)
			}

			return nil
		}

		// Need to wait for space
		spaceSeq := hdr.SpaceSequence()

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
			err = futexWaitTimeout(&hdr.spaceSeq, spaceSeq, timeoutNs)
		} else {
			err = futexWait(&hdr.spaceSeq, spaceSeq)
		}

		if err != nil {
			// Check if it's a timeout error
			if err.Error() == "futex wait timed out" {
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
				bytesRead = copy(buf, (*[1 << 30]byte)(srcPtr)[:toRead])
			} else {
				// Wrap case: split the read
				firstChunk := r.capacity - readPos

				// Read first chunk from end of buffer
				srcPtr1 := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				bytesRead = copy(buf, (*[1 << 30]byte)(srcPtr1)[:firstChunk])

				// Read second chunk from beginning of buffer
				srcPtr2 := r.dataPtr()
				secondChunk := toRead - firstChunk
				bytesRead += copy(buf[bytesRead:], (*[1 << 30]byte)(srcPtr2)[:secondChunk])
			}

			// Advance read index (release-publish) then signal via spaceSeq/futex.
			hdr.SetReadIndex(readIdx + uint64(bytesRead))

			// Only wake writers if buffer transitioned from full → not-full.
			if usedBefore == r.capacity {
				hdr.IncrementSpaceSequence()
				futexWake(&hdr.spaceSeq, 1)
			}

			return bytesRead, nil
		}

		// Check if ring is closed and no data available
		if hdr.Closed() {
			return 0, io.EOF
		}

		// Need to wait for data
		dataSeq := hdr.DataSequence()

		// Calculate timeout from context deadline
		var timeoutNs int64
		if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
			remaining := time.Until(deadline)
			if remaining <= 0 {
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

		if err != nil {
			// Check if it's a timeout error
			if err.Error() == "futex wait timed out" {
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
	First  []byte // First contiguous slice (from write position to end of buffer or requested size)
	Second []byte // Second contiguous slice (from start of buffer) - may be empty if First has enough space
	commit func(written int) error
}

// Commit commits the written bytes and advances the write index.
// written must not exceed the total capacity of First + Second slices.
func (wr *WriteReservation) Commit(written int) error {
	return wr.commit(written)
}

// ReserveWrite blocks until at least n bytes of contiguous space is available, then returns
// the writable slice(s) and a commit function. This enables zero-copy writes directly into
// the ring buffer memory. Headers may span across wrap boundaries via First+Second slices.
func (r *ShmRing) ReserveWrite(n int, ctx context.Context) (WriteReservation, error) {
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
				first = (*[1 << 30]byte)(firstPtr)[:n:n]
			} else {
				// Wrap case: split across end and beginning (header can straddle)
				firstLen := r.capacity - writePos
				firstPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				first = (*[1 << 30]byte)(firstPtr)[:firstLen:firstLen]

				secondLen := uint64(n) - firstLen
				secondPtr := r.dataPtr()
				second = (*[1 << 30]byte)(secondPtr)[:secondLen:secondLen]
			}

			// Create commit function that captures the current write state.
			// Memory ordering rationale: the caller copies into First/Second slices
			// first, then Commit performs a release store of the new w index, then
			// bumps dataSeq and futex_wake, which prevents lost wakeups (waiters
			// wait on equality of the sequence value).
			commitFunc := func(written int) error {
				if written < 0 || written > n {
					return fmt.Errorf("invalid written count %d, expected 0-%d", written, n)
				}

				// Publish new write index and wake any waiting readers.
				hdr.SetWriteIndex(writeIdx + uint64(written)) // release-publish

				// Wake readers if any data was written.
				if written > 0 {
					hdr.IncrementDataSequence()
					futexWake(&hdr.dataSeq, 1)
				}

				return nil
			}

			return WriteReservation{
				First:  first,
				Second: second,
				commit: commitFunc,
			}, nil
		}

		// No space available - wait for consumer to free space (event-driven, no polling)
		spaceSeq := hdr.SpaceSequence()
		if err := futexWait(&hdr.spaceSeq, spaceSeq); err != nil {
			// Check for closure again after wake
			if hdr.Closed() {
				return WriteReservation{}, ErrRingClosed
			}
			// Continue loop for spurious wake or other wake reasons
		}
	}
}

// ReadSlices blocks until at least n bytes are available to read; returns slices spanning wrap.
// This enables proper reconstruction of headers that may straddle wrap boundaries.
// The caller must call commit with the number of bytes consumed.
func (r *ShmRing) ReadSlices(n int, ctx context.Context) (first, second []byte, commit func(consumed int), err error) {
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

		// Check for closure - do this after context check to avoid race with segment cleanup
		if hdr.Closed() {
			// Check if data is still available even when closed
			writeIdx := hdr.WriteIndex()
			readIdx := hdr.ReadIndex()
			availableBefore := writeIdx - readIdx
			if availableBefore == 0 {
				return nil, nil, nil, io.EOF
			}
			// Fall through to read remaining data if available
		}
		
		// Load current indices to check available data
		writeIdx := hdr.WriteIndex()
		readIdx := hdr.ReadIndex()

		// Calculate available data
		availableBefore := writeIdx - readIdx

		if availableBefore >= uint64(n) {
			// Data available - create slices (may span wrap for header reconstruction)
			readPos := readIdx & r.capMask

			var firstSlice, secondSlice []byte

			// Handle ring wrap-around - allow headers to straddle wrap
			if readPos+uint64(n) <= r.capacity {
				// Simple case: no wrap needed
				srcPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				firstSlice = (*[1 << 30]byte)(srcPtr)[:n:n]
			} else {
				// Wrap case: split across end and beginning
				firstLen := r.capacity - readPos
				firstPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				firstSlice = (*[1 << 30]byte)(firstPtr)[:firstLen:firstLen]

				secondLen := uint64(n) - firstLen
				secondPtr := r.dataPtr()
				secondSlice = (*[1 << 30]byte)(secondPtr)[:secondLen:secondLen]
			}

			// Create commit function
			commitFunc := func(consumed int) {
				if consumed < 0 || consumed > n {
					return // Invalid consumption, ignore
				}

				// Advance read index (release-publish)
				hdr.SetReadIndex(readIdx + uint64(consumed))

				// Only wake writers if buffer transitioned from full → not-full
				if availableBefore == r.capacity && consumed > 0 {
					hdr.IncrementSpaceSequence()
					futexWake(&hdr.spaceSeq, 1)
				}
			}

			return firstSlice, secondSlice, commitFunc, nil
		}

		// No data available and not closed - wait for producer (event-driven, no polling)
		if !hdr.Closed() {
			dataSeq := hdr.DataSequence()
			if err := futexWait(&hdr.dataSeq, dataSeq); err != nil {
				// Continue loop for spurious wake or other wake reasons
			}
		} else {
			// Closed and insufficient data - return EOF
			return nil, nil, nil, io.EOF
		}
	}
}

// WriteAll writes all bytes to the ring buffer, blocking as needed.
// This is a convenience method that handles multiple reservations if needed.
// Supports chunking when message > available space.
func (r *ShmRing) WriteAll(p []byte, ctx context.Context) error {
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

		reservation, err := r.ReserveWrite(toWrite, ctx)
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
func (r *ShmRing) ReadExact(n int, dst []byte, ctx context.Context) ([]byte, error) {
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
		first, second, commit, err := r.ReadSlices(remaining, ctx)
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
		commit(copied)
		totalRead += copied
	}

	return result, nil
}

// ReserveFrameHeader reserves a 16-byte, 16-byte-aligned header region that will
// never straddle the end of the ring. If fewer than 16 bytes remain before the
// end, this method emits a PAD frame (type=0) that fills the remainder to the
// end, publishes it (w += padLen + headerSize), and then returns a reservation
// for the header at the start of the ring.
//
// Memory ordering follows the SPSC invariant:
//   writer: memcpy header/pad -> atomic.Store(w,new) [release] -> AddUint32(dataSeq) -> futex_wake
//   reader: atomic.Load(w) [acquire] -> copy -> atomic.Store(r,new) [release] -> AddUint32(spaceSeq) -> futex_wake
func (r *ShmRing) ReserveFrameHeader(ctx context.Context) (WriteReservation, error) {
    hdr := r.header()

    for {
        // Respect context cancellation/deadline.
        select {
        case <-ctx.Done():
            return WriteReservation{}, ctx.Err()
        default:
        }

        if hdr.Closed() {
            return WriteReservation{}, ErrRingClosed
        }

        writeIdx := hdr.WriteIndex()
        readIdx := hdr.ReadIndex()
        used := writeIdx - readIdx
        available := r.capacity - used

        writePos := writeIdx & r.capMask
        remaining := r.capacity - writePos

        // Determine how much free space is needed to both place/emit a PAD (if any)
        // and to hold the 16B header itself.
        needed := uint64(frameHeaderSize)
        if remaining < frameHeaderSize {
            // Need to consume the tail with a PAD frame: pad payload is 'remaining',
            // plus 16B PAD header that will be written at offset 0, plus the 16B
            // actual header we are about to reserve at the start.
            needed += remaining + frameHeaderSize
        }

        if available >= needed {
            // We can proceed.
            if remaining >= frameHeaderSize {
                // Happy path: header fits contiguously; return a 16B slice at writePos.
                firstPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
                first := (*[1 << 30]byte)(firstPtr)[:frameHeaderSize:frameHeaderSize]

                commit := func(written int) error {
                    if written < 0 || written > frameHeaderSize {
                        return fmt.Errorf("invalid header write size %d", written)
                    }
                    // Publish new w and signal availability.
                    hdr.SetWriteIndex(writeIdx + uint64(written))
                    if written > 0 {
                        hdr.IncrementDataSequence()
                        futexWake(&hdr.dataSeq, 1)
                    }
                    return nil
                }

                return WriteReservation{First: first, Second: nil, commit: commit}, nil
            }

            // Need to emit PAD: write PAD header at offset 0, skip remaining tail.
            // 1) Write PAD header at offset 0 (not yet visible to reader since w not advanced)
            padHdrPtr := r.dataPtr()
            var fh FrameHeader
            fh.Length = uint32(remaining)
            fh.StreamID = 0
            fh.Type = FrameTypePAD
            fh.Flags = 0
            fh.Reserved = 0
            fh.Reserved2 = 0
            var hdrBytes [frameHeaderSize]byte
            encodeFrameHeaderTo(&hdrBytes, fh)
            copy((*[1<<30]byte)(padHdrPtr)[:frameHeaderSize], hdrBytes[:])

            // 2) Publish w to include [tail payload + PAD header]
            padAdvance := remaining + frameHeaderSize
            newW := writeIdx + padAdvance
            hdr.SetWriteIndex(newW)
            // Wake reader if needed (conditional wakeup optimization: if buffer was empty
            // before this publish, reader may be waiting on dataSeq).
            if used == 0 {
                hdr.IncrementDataSequence()
                futexWake(&hdr.dataSeq, 1)
            }

            // 3) Now return a reservation for the actual 16B header at start (offset 16)
            headerPos := uint64(frameHeaderSize) // start-of-ring after PAD header
            firstPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(headerPos))
            first := (*[1 << 30]byte)(firstPtr)[:frameHeaderSize:frameHeaderSize]

            commit := func(written int) error {
                if written < 0 || written > frameHeaderSize {
                    return fmt.Errorf("invalid header write size %d", written)
                }
                // Publish new w and signal availability.
                hdr.SetWriteIndex(newW + uint64(written))
                if written > 0 {
                    hdr.IncrementDataSequence()
                    futexWake(&hdr.dataSeq, 1)
                }
                return nil
            }

            return WriteReservation{First: first, Second: nil, commit: commit}, nil
        }

        // Not enough space: wait on spaceSeq, event-driven (no polling)
        spaceSeq := hdr.SpaceSequence()
        if err := futexWait(&hdr.spaceSeq, spaceSeq); err != nil {
            if hdr.Closed() {
                return WriteReservation{}, ErrRingClosed
            }
            // Spurious wake or EINTR; loop and re-check
        }
    }
}
