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

// Ring implements a single-producer/single-consumer circular byte buffer.
// Capacity is a power of two. It is safe for one writer goroutine and one reader
// goroutine concurrently. No blocking: operations return immediately with
// progress made (may be zero).
type Ring struct {
	// 64-bit monotonic counters. Writer owns w; reader owns r.
	w atomic.Uint64 // next write position (bytes since start)
	r atomic.Uint64 // next read position  (bytes since start)

	// Buffer storage and fast mask for modulo (capacity - 1).
	buf  []byte
	mask uint64 // capacity-1; valid only if capacity is power of two
	cap  uint64

	// Closed flag: 0 = open, 1 = closed.
	closed atomic.Uint32

	// Optional padding to keep w and r on separate cache lines (avoid false sharing).
	// (We will add padding later if needed; not required for correctness.)
}

// NewRing returns a new Ring with at least the requested capacity. The actual
// capacity is the next power of two >= minCap and at least 16 bytes.
// If minCap <= 0, NewRing returns an error.
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

// Capacity returns the byte capacity of the ring.
func (r *Ring) Capacity() int {
	return int(r.cap)
}

// Close marks the ring as closed. Further writes fail with ErrClosed.
// Reads of existing data continue until empty, then return 0, io.EOF-like semantics.
func (r *Ring) Close() { /* stub */ }

// Closed reports whether the ring has been closed.
func (r *Ring) Closed() bool { /* stub */ return false }

// AvailableRead returns the number of bytes currently readable (may be stale if
// called by the writer).
func (r *Ring) AvailableRead() int {
	w := r.w.Load()
	rd := r.r.Load()
	used := w - rd
	return int(used)
}

// AvailableWrite returns the number of bytes currently writable (free space).
func (r *Ring) AvailableWrite() int {
	w := r.w.Load()
	rd := r.r.Load()
	used := w - rd
	free := r.cap - used
	return int(free)
}

// Write copies up to len(p) bytes into the ring. It is non-blocking; it may
// return a short write if the ring lacks space. Returns (n, ErrClosed) if closed.
func (r *Ring) Write(p []byte) (int, error) { /* stub */ return 0, nil }

// Read copies up to len(p) bytes from the ring into p. It is non-blocking; it
// returns 0 if no data is available. Returns 0, ErrClosed only if the ring was
// closed *and* empty at the time of call.
func (r *Ring) Read(p []byte) (int, error) { /* stub */ return 0, nil }

// ReserveWrite reserves n bytes for in-place writing and returns up to two
// contiguous slices (head/tail) referencing internal storage. Caller must fill
// at most len(s1)+len(s2) bytes and then call CommitWrite(k). Returns ok=false
// if insufficient space or ring is closed.
func (r *Ring) ReserveWrite(n int) (s1, s2 []byte, ok bool) { /* stub */ return nil, nil, false }

// CommitWrite advances the write index by n bytes previously obtained via
// ReserveWrite. It is the caller's responsibility to not over-commit.
func (r *Ring) CommitWrite(n int) { /* stub */ }

// PeekRead returns up to n bytes available for reading as two contiguous slices.
// Caller must call CommitRead(k) after consuming bytes from s1/s2. Returns
// (nil,nil,false) if no data.
func (r *Ring) PeekRead(n int) (s1, s2 []byte, ok bool) { /* stub */ return nil, nil, false }

// CommitRead advances the read index by n bytes previously obtained via PeekRead.
func (r *Ring) CommitRead(n int) { /* stub */ }
