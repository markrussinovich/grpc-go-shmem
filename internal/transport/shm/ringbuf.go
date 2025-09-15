/*
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
 */

// Package shm provides shared-memory transport primitives.
package shm

import (
	"errors"
	"sync/atomic"
)

// ErrClosed indicates the ring has been closed; further I/O is disallowed.
var ErrClosed = errors.New("ring: closed")

// paddedUint64 wraps an atomic.Uint64 with cache-line padding to prevent false sharing.
//
// False sharing occurs when multiple CPU cores access different variables that
// happen to be on the same cache line. When one core modifies its variable, the
// entire cache line is invalidated on other cores, forcing them to reload the
// cache line even if they only need their own variable.
//
// In a ring buffer, the writer frequently updates 'w' while the reader frequently
// updates 'r'. Without padding, these could end up on the same cache line, causing
// unnecessary cache line bouncing between cores and degrading performance.
//
// By padding each atomic counter to its own 64-byte cache line, we ensure that
// updates to 'w' and 'r' don't interfere with each other at the cache level.
//
// Assumes 64-byte cache lines (common on x86-64). The atomic.Uint64 itself is 8 bytes,
// so we pad with 56 bytes to reach the full 64-byte cache line size.
type paddedUint64 struct {
	atomic.Uint64
	_ [56]byte // Cache line padding (64 - 8 = 56)
}

// roundUpPowerOfTwo returns the next power of two >= n, with minimum value of 16.
func roundUpPowerOfTwo(n int) uint64 {
	if n < 16 {
		return 16
	}

	// Convert to uint64 for bit manipulation
	x := uint64(n)

	// If already a power of two, return as-is
	if x&(x-1) == 0 {
		return x
	}

	// Round up to next power of two using bit manipulation
	x--
	x |= x >> 1
	x |= x >> 2
	x |= x >> 4
	x |= x >> 8
	x |= x >> 16
	x |= x >> 32
	x++

	return x
}

// Ring implements a single-producer/single-consumer (SPSC) circular byte buffer
// with non-blocking semantics and power-of-two capacity.
//
// The ring buffer is designed for high-performance communication between exactly
// one writer goroutine and one reader goroutine. All operations are lock-free
// and non-blocking, using atomic operations for synchronization.
//
// Key characteristics:
//   - SPSC: Safe for one concurrent writer and one concurrent reader
//   - Non-blocking: All operations return immediately with progress made (may be zero)
//   - Power-of-two capacity: Enables efficient modulo operations using bitwise AND
//   - Lock-free: Uses only atomic operations, no mutexes or condition variables
//   - Cache-optimized: Padded counters prevent false sharing between cores
//
// The ring buffer supports both copying I/O (Read/Write) and zero-copy I/O
// (ReserveWrite/CommitWrite and PeekRead/CommitRead) for maximum flexibility.
//
// Typical usage:
//
//	ring, err := NewRing(1024)
//	if err != nil {
//	    // handle error
//	}
//
//	// Writer goroutine
//	go func() {
//	    data := []byte("hello")
//	    n, err := ring.Write(data)
//	    // handle partial writes and errors
//	}()
//
//	// Reader goroutine
//	go func() {
//	    buf := make([]byte, 100)
//	    n, err := ring.Read(buf)
//	    // handle partial reads and errors
//	}()
type Ring struct {
	// 64-bit monotonic counters with cache-line padding to avoid false sharing.
	// Writer owns w; reader owns r.
	w paddedUint64 // next write position (bytes since start)
	r paddedUint64 // next read position  (bytes since start)

	// Buffer storage and fast mask for modulo (capacity - 1).
	buf  []byte
	mask uint64 // capacity-1; valid only if capacity is power of two
	cap  uint64

	// Closed flag: 0 = open, 1 = closed.
	closed atomic.Uint32
}

// NewRing creates a new Ring with at least the requested capacity.
//
// The actual capacity will be the next power of two >= minCap, with a minimum
// of 16 bytes. This power-of-two requirement enables efficient modulo operations
// using bitwise AND instead of expensive division.
//
// Parameters:
//   - minCap: minimum desired capacity in bytes (must be > 0)
//
// Returns:
//   - *Ring: a new ring buffer ready for SPSC operation
//   - error: non-nil if minCap <= 0
//
// Examples:
//   - NewRing(100) creates a ring with 128-byte capacity
//   - NewRing(1024) creates a ring with 1024-byte capacity
//   - NewRing(1) creates a ring with 16-byte capacity (minimum)
func NewRing(minCap int) (*Ring, error) {
	if minCap <= 0 {
		return nil, errors.New("ring: capacity must be positive")
	}

	capacity := roundUpPowerOfTwo(minCap)

	return &Ring{
		buf:  make([]byte, capacity),
		mask: capacity - 1,
		cap:  capacity,
	}, nil
}

// Capacity returns the total byte capacity of the ring buffer.
//
// The capacity is always a power of two and represents the maximum number
// of bytes that can be stored in the buffer. This value is fixed at
// construction time and never changes.
//
// Example:
//
//	ring, _ := NewRing(1000)    // Requests 1000 bytes
//	cap := ring.Capacity()      // Returns 1024 (next power of 2)
func (r *Ring) Capacity() int {
	return int(r.cap)
}

// Close marks the ring as closed for writing.
//
// After Close is called:
//   - All subsequent Write and ReserveWrite operations will return ErrClosed
//   - Read operations continue to work normally until the ring is empty
//   - Once empty, Read operations return (0, ErrClosed)
//
// This provides clean shutdown semantics similar to io.EOF, allowing readers
// to drain all remaining data before seeing the closed state.
//
// Close is safe to call multiple times and safe to call concurrently with
// Read/Write operations.
func (r *Ring) Close() {
	r.closed.Store(1)
}

// Closed reports whether the ring has been closed.
//
// Returns true if Close has been called, false otherwise.
// This method is safe to call concurrently with other operations.
//
// Note that a closed ring may still contain readable data. Use AvailableRead
// to check for remaining data, and Read operations will work normally until
// the ring is both closed and empty.
func (r *Ring) Closed() bool {
	return r.closed.Load() == 1
}

// AvailableRead returns the number of bytes currently available for reading.
//
// This method provides a snapshot of readable data at the time of the call.
// The value may become stale immediately due to concurrent operations:
//   - If called by a reader: value is accurate for immediate use
//   - If called by a writer: value may increase due to concurrent writes
//
// Use this method to check data availability before Read/PeekRead operations
// or to implement flow control logic.
//
// Example:
//
//	if ring.AvailableRead() >= messageSize {
//	    // Guaranteed that at least messageSize bytes can be read
//	    data := make([]byte, messageSize)
//	    n, err := ring.Read(data)
//	}
func (r *Ring) AvailableRead() int {
	w := r.w.Load()
	rd := r.r.Load()
	used := w - rd
	return int(used)
}

// AvailableWrite returns the number of bytes currently available for writing.
//
// This method provides a snapshot of free space at the time of the call.
// The value may become stale immediately due to concurrent operations:
//   - If called by a writer: value is accurate for immediate use
//   - If called by a reader: value may increase due to concurrent reads
//
// Use this method to check space availability before Write/ReserveWrite
// operations or to implement backpressure mechanisms.
//
// Example:
//
//	if ring.AvailableWrite() >= len(data) {
//	    // Guaranteed that data can be written without blocking
//	    n, err := ring.Write(data)
//	}
func (r *Ring) AvailableWrite() int {
	w := r.w.Load()
	rd := r.r.Load()
	used := w - rd
	free := r.cap - used
	return int(free)
}

// Write copies up to len(p) bytes into the ring buffer.
//
// Write is non-blocking and may return a short write if insufficient space
// is available. The operation returns immediately with the number of bytes
// actually written. A short write is not an error condition.
//
// Parameters:
//   - p: byte slice to write (may be empty)
//
// Returns:
//   - n: number of bytes actually written (0 <= n <= len(p))
//   - error: ErrClosed if the ring has been closed, nil otherwise
//
// Write behavior:
//   - If ring is closed: returns (0, ErrClosed) immediately
//   - If ring is full: returns (0, nil) - no error, just no progress
//   - If ring has partial space: returns (k, nil) where k < len(p)
//   - If ring has sufficient space: returns (len(p), nil)
//
// Write is safe to call concurrently with Read operations from another goroutine.
// Only one goroutine should call Write concurrently (SPSC design).
func (r *Ring) Write(p []byte) (int, error) {
	// If closed, return error immediately
	if r.closed.Load() == 1 {
		return 0, ErrClosed
	}

	// Load current indices
	w := r.w.Load()
	rd := r.r.Load()

	// Calculate available space
	used := w - rd
	free := r.cap - used
	if free == 0 {
		return 0, nil // No space available
	}

	// Determine how much we can write
	want := uint64(len(p))
	if want > free {
		want = free
	}

	// Compute write offset in buffer
	off := w & r.mask

	// Compute bytes until end of buffer
	first := want
	if first > r.cap-off {
		first = r.cap - off
	}

	// Copy first part
	copy(r.buf[off:off+first], p[:first])

	// Copy second part if needed (wrap around)
	second := want - first
	if second > 0 {
		copy(r.buf[0:second], p[first:first+second])
	}

	// Publish the write (store after bytes are visible)
	r.w.Store(w + want)

	return int(want), nil
}

// Read copies up to len(p) bytes from the ring buffer into p.
//
// Read is non-blocking and returns immediately with the number of bytes
// actually read. If no data is available, it returns (0, nil).
//
// Parameters:
//   - p: destination buffer (may be empty)
//
// Returns:
//   - n: number of bytes actually read (0 <= n <= len(p))
//   - error: ErrClosed only if ring is both closed AND empty, nil otherwise
//
// Read behavior:
//   - If ring is empty and open: returns (0, nil)
//   - If ring is empty and closed: returns (0, ErrClosed)
//   - If ring has partial data: returns (k, nil) where k < len(p)
//   - If ring has sufficient data: returns (len(p), nil)
//
// The ErrClosed return follows EOF semantics: data can still be read from
// a closed ring until it becomes empty. Only when both closed AND empty
// does Read return ErrClosed.
//
// Read is safe to call concurrently with Write operations from another goroutine.
// Only one goroutine should call Read concurrently (SPSC design).
func (r *Ring) Read(p []byte) (int, error) {
	// Load current indices
	w := r.w.Load() // acquire
	rd := r.r.Load()

	// Calculate available data
	avail := w - rd
	if avail == 0 {
		// No data available
		if r.closed.Load() == 1 {
			return 0, ErrClosed // empty and closed
		}
		return 0, nil // empty but not closed
	}

	// Determine how much we can read
	want := uint64(len(p))
	if want > avail {
		want = avail
	}

	// Compute read offset in buffer
	off := rd & r.mask

	// Compute bytes until end of buffer
	first := want
	if first > r.cap-off {
		first = r.cap - off
	}

	// Copy first part
	copy(p[:first], r.buf[off:off+first])

	// Copy second part if needed (wrap around)
	second := want - first
	if second > 0 {
		copy(p[first:first+second], r.buf[0:second])
	}

	// Publish the read (store after bytes are consumed)
	r.r.Store(rd + want)

	return int(want), nil
}

// ReserveWrite reserves n bytes for zero-copy in-place writing.
//
// This method provides direct access to the ring buffer's internal memory,
// eliminating the need to copy data. The returned slices reference the actual
// ring buffer storage where data should be written.
//
// Parameters:
//   - n: number of bytes to reserve (must be > 0)
//
// Returns:
//   - s1: first contiguous slice for writing (may be entire reservation)
//   - s2: second contiguous slice for writing (nil if no wrap-around needed)
//   - ok: true if reservation succeeded, false if insufficient space or closed
//
// Usage pattern:
//  1. Call ReserveWrite(n) to get direct memory access
//  2. Write data directly into s1 and s2 slices
//  3. Call CommitWrite(actualBytes) to publish the data
//
// The total capacity of s1+s2 equals n bytes. Due to the circular nature of
// the buffer, the reservation may be split into two contiguous segments when
// wrap-around is needed.
//
// ReserveWrite will fail (ok=false) if:
//   - n <= 0
//   - Insufficient space available
//   - Ring is closed
//
// After a successful ReserveWrite, the caller MUST call CommitWrite exactly once
// with the number of bytes actually written. Calling ReserveWrite again before
// CommitWrite results in undefined behavior.
func (r *Ring) ReserveWrite(n int) (s1, s2 []byte, ok bool) {
	// Check if closed
	if r.closed.Load() == 1 {
		return nil, nil, false
	}

	// Check for valid input
	if n <= 0 {
		return nil, nil, false
	}

	// Load current indices
	w := r.w.Load()
	rd := r.r.Load()

	// Calculate available space
	used := w - rd
	free := r.cap - used
	if uint64(n) > free {
		return nil, nil, false
	}

	// Compute write offset in buffer
	off := w & r.mask

	// Compute bytes until end of buffer
	first := uint64(n)
	if first > r.cap-off {
		first = r.cap - off
	}

	// Create first slice
	s1 = r.buf[off : off+first]

	// Create second slice if needed (wrap around)
	second := uint64(n) - first
	if second > 0 {
		s2 = r.buf[0:second]
	}

	return s1, s2, true
}

// CommitWrite advances the write index by n bytes, completing a zero-copy write
// operation started with ReserveWrite. The n parameter must not exceed the
// number of bytes previously reserved, and the caller must have written exactly
// n bytes to the slice returned by ReserveWrite.
//
// Example zero-copy write pattern:
//
//	data := []byte("hello")
//	buf, ok := ring.ReserveWrite(len(data))
//	if ok {
//	    copy(buf, data)           // Write directly to ring buffer
//	    ring.CommitWrite(len(data)) // Make data visible to readers
//	}
//
// CommitWrite is safe to call from multiple goroutines, but the caller is
// responsible for ensuring that the n value corresponds to data actually
// written to the reserved buffer slice.
func (r *Ring) CommitWrite(n int) {
	if n > 0 {
		r.w.Add(uint64(n))
	}
}

// PeekRead provides zero-copy access to up to n bytes of buffered data without
// advancing the read position. Returns up to two contiguous byte slices (s1, s2)
// that together contain the available data, and ok=true if any data is available.
// Returns (nil, nil, false) if no data is available or n <= 0.
//
// The data may span the ring buffer boundary, requiring two slices:
//   - s1: First contiguous portion of data
//   - s2: Second portion if data wraps around (may be nil)
//
// After processing the data from s1 and s2, the caller must call CommitRead
// with the number of bytes actually consumed to advance the read position.
//
// Example zero-copy read pattern:
//
//	s1, s2, ok := ring.PeekRead(1024)
//	if ok {
//	    consumed := process(s1) + process(s2)  // Process data in-place
//	    ring.CommitRead(consumed)               // Advance read position
//	}
//
// The returned slices are valid until the next call to PeekRead, CommitRead,
// or any method that might advance the read position. PeekRead is safe to call
// from multiple goroutines, but callers must coordinate to ensure proper
// CommitRead sequencing.
func (r *Ring) PeekRead(n int) (s1, s2 []byte, ok bool) {
	// Check for valid input
	if n <= 0 {
		return nil, nil, false
	}

	// Load current indices
	w := r.w.Load()
	rd := r.r.Load()

	// Calculate available data
	avail := w - rd
	if avail == 0 {
		return nil, nil, false
	}

	// Determine how much we can peek
	want := uint64(n)
	if want > avail {
		want = avail
	}

	// Compute read offset in buffer
	off := rd & r.mask

	// Compute bytes until end of buffer
	first := want
	if first > r.cap-off {
		first = r.cap - off
	}

	// Create first slice
	s1 = r.buf[off : off+first]

	// Create second slice if needed (wrap around)
	second := want - first
	if second > 0 {
		s2 = r.buf[0:second]
	}

	return s1, s2, true
}

// CommitRead advances the read index by n bytes, completing a zero-copy read
// operation started with PeekRead. The n parameter should not exceed the
// total number of bytes available in the slices returned by PeekRead, and
// represents the number of bytes actually consumed by the application.
//
// Example usage with partial consumption:
//
//	s1, s2, ok := ring.PeekRead(1024)
//	if ok {
//	    // Process only part of available data
//	    consumed := processHeader(s1[:headerSize])
//	    ring.CommitRead(consumed)  // Advance by actual consumption
//	}
//
// CommitRead is safe to call from multiple goroutines, but callers must
// ensure that the n value corresponds to data actually processed from
// the most recent PeekRead operation.
func (r *Ring) CommitRead(n int) {
	if n > 0 {
		r.r.Add(uint64(n))
	}
}
