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

// Package shm provides a shared memory transport for gRPC over shared memory
// segments, enabling high-performance inter-process communication (IPC) for
// local gRPC clients and servers.
package shm

import (
	"fmt"
	"os"
	"sync/atomic"
	"unsafe"
)

// Memory layout constants
const (
	// Magic bytes for segment identification
	SegmentMagic = "GRPCSHM\x00"

	// Current protocol version
	SegmentVersion = uint32(1)

	// Segment header size (aligned to 128 bytes)
	SegmentHeaderSize = 128

	// Ring header size (aligned to 64 bytes)
	RingHeaderSize = 64

	// Minimum ring capacity (4KB)
	MinRingCapacity = 4096

	// Default ring capacity (64KB)
	DefaultRingCapacity = 65536

	// Default sizes for shared memory segments and rings
	DefaultSegmentSize = 4 * 1024 * 1024 // 4MB total segment
	DefaultRingASize   = 1024 * 1024     // 1MB for client->server
	DefaultRingBSize   = 1024 * 1024     // 1MB for server->client
)

// Platform-specific functions (implemented in platform-specific files)
var (
	// unmapMemory unmaps a memory-mapped region
	unmapMemory func([]byte) error
)

// SegmentHeader represents the shared memory segment header.
// Layout follows the specification with 128-byte alignment.
type SegmentHeader struct {
	magic       [8]byte  // 0x00: "GRPCSHM\0"
	version     uint32   // 0x08: protocol version
	flags       uint32   // 0x0C: reserved flags
	totalSize   uint64   // 0x10: total segment size
	ringAOff    uint64   // 0x18: offset to ring A header
	ringACap    uint64   // 0x20: ring A capacity (power of 2)
	ringBOff    uint64   // 0x28: offset to ring B header
	ringBCap    uint64   // 0x30: ring B capacity (power of 2)
	serverPID   uint32   // 0x38: server process ID
	clientPID   uint32   // 0x3C: client process ID
	serverReady uint32   // 0x40: server ready flag (0->1)
	clientReady uint32   // 0x44: client mapped flag (0->1)
	closed      uint32   // 0x48: closed flag (0 open, 1 closed)
	pad         uint32   // 0x4C: padding
	reserved    [48]byte // 0x50-0x7F: reserved/padding to 128B
}

// SegmentHeader atomic access methods

// Magic returns the magic bytes
func (h *SegmentHeader) Magic() [8]byte {
	return h.magic
}

// SetMagic sets the magic bytes
func (h *SegmentHeader) SetMagic(magic [8]byte) {
	h.magic = magic
}

// Version returns the protocol version
func (h *SegmentHeader) Version() uint32 {
	return atomic.LoadUint32(&h.version)
}

// SetVersion sets the protocol version
func (h *SegmentHeader) SetVersion(version uint32) {
	atomic.StoreUint32(&h.version, version)
}

// TotalSize returns the total segment size
func (h *SegmentHeader) TotalSize() uint64 {
	return atomic.LoadUint64(&h.totalSize)
}

// SetTotalSize sets the total segment size
func (h *SegmentHeader) SetTotalSize(size uint64) {
	atomic.StoreUint64(&h.totalSize, size)
}

// RingAOffset returns the offset to ring A header
func (h *SegmentHeader) RingAOffset() uint64 {
	return atomic.LoadUint64(&h.ringAOff)
}

// SetRingAOffset sets the offset to ring A header
func (h *SegmentHeader) SetRingAOffset(offset uint64) {
	atomic.StoreUint64(&h.ringAOff, offset)
}

// RingACapacity returns ring A capacity
func (h *SegmentHeader) RingACapacity() uint64 {
	return atomic.LoadUint64(&h.ringACap)
}

// SetRingACapacity sets ring A capacity
func (h *SegmentHeader) SetRingACapacity(capacity uint64) {
	atomic.StoreUint64(&h.ringACap, capacity)
}

// RingBOffset returns the offset to ring B header
func (h *SegmentHeader) RingBOffset() uint64 {
	return atomic.LoadUint64(&h.ringBOff)
}

// SetRingBOffset sets the offset to ring B header
func (h *SegmentHeader) SetRingBOffset(offset uint64) {
	atomic.StoreUint64(&h.ringBOff, offset)
}

// RingBCapacity returns ring B capacity
func (h *SegmentHeader) RingBCapacity() uint64 {
	return atomic.LoadUint64(&h.ringBCap)
}

// SetRingBCapacity sets ring B capacity
func (h *SegmentHeader) SetRingBCapacity(capacity uint64) {
	atomic.StoreUint64(&h.ringBCap, capacity)
}

// ServerPID returns the server process ID
func (h *SegmentHeader) ServerPID() uint32 {
	return atomic.LoadUint32(&h.serverPID)
}

// SetServerPID sets the server process ID
func (h *SegmentHeader) SetServerPID(pid uint32) {
	atomic.StoreUint32(&h.serverPID, pid)
}

// ClientPID returns the client process ID
func (h *SegmentHeader) ClientPID() uint32 {
	return atomic.LoadUint32(&h.clientPID)
}

// SetClientPID sets the client process ID
func (h *SegmentHeader) SetClientPID(pid uint32) {
	atomic.StoreUint32(&h.clientPID, pid)
}

// ServerReady returns the server ready flag
func (h *SegmentHeader) ServerReady() bool {
	return atomic.LoadUint32(&h.serverReady) != 0
}

// SetServerReady sets the server ready flag
func (h *SegmentHeader) SetServerReady(ready bool) {
	var val uint32
	if ready {
		val = 1
	}
	atomic.StoreUint32(&h.serverReady, val)
}

// ClientReady returns the client ready flag
func (h *SegmentHeader) ClientReady() bool {
	return atomic.LoadUint32(&h.clientReady) != 0
}

// SetClientReady sets the client ready flag
func (h *SegmentHeader) SetClientReady(ready bool) {
	var val uint32
	if ready {
		val = 1
	}
	atomic.StoreUint32(&h.clientReady, val)
}

// Closed returns the closed flag
func (h *SegmentHeader) Closed() bool {
	return atomic.LoadUint32(&h.closed) != 0
}

// SetClosed sets the closed flag
func (h *SegmentHeader) SetClosed(closed bool) {
	var val uint32
	if closed {
		val = 1
	}
	atomic.StoreUint32(&h.closed, val)
}

// RingHeader represents a ring buffer header with atomic access fields.
// Layout follows the specification with 64-byte alignment.
type RingHeader struct {
	capacity uint64   // 0x00: power-of-two capacity in bytes
	widx     uint64   // 0x08: monotonic write index (producer)
	ridx     uint64   // 0x10: monotonic read index (consumer)
	dataSeq  uint32   // 0x18: data sequence for futex (producer increments)
	spaceSeq uint32   // 0x1C: space sequence for futex (consumer increments)
	closed   uint32   // 0x20: closed flag (producer sets to 1)
	pad      uint32   // 0x24: padding
	reserved [24]byte // 0x28-0x3F: reserved/padding to 64B
	// data area starts at offset 0x40
}

// RingHeader atomic access methods

// Capacity returns the ring capacity
func (r *RingHeader) Capacity() uint64 {
	return atomic.LoadUint64(&r.capacity)
}

// SetCapacity sets the ring capacity
func (r *RingHeader) SetCapacity(capacity uint64) {
	atomic.StoreUint64(&r.capacity, capacity)
}

// WriteIndex returns the monotonic write index (producer)
func (r *RingHeader) WriteIndex() uint64 {
	return atomic.LoadUint64(&r.widx)
}

// SetWriteIndex sets the monotonic write index (producer)
func (r *RingHeader) SetWriteIndex(idx uint64) {
	atomic.StoreUint64(&r.widx, idx)
}

// ReadIndex returns the monotonic read index (consumer)
func (r *RingHeader) ReadIndex() uint64 {
	return atomic.LoadUint64(&r.ridx)
}

// SetReadIndex sets the monotonic read index (consumer)
func (r *RingHeader) SetReadIndex(idx uint64) {
	atomic.StoreUint64(&r.ridx, idx)
}

// DataSequence returns the data sequence number for futex
func (r *RingHeader) DataSequence() uint32 {
	return atomic.LoadUint32(&r.dataSeq)
}

// IncrementDataSequence atomically increments the data sequence
func (r *RingHeader) IncrementDataSequence() uint32 {
	return atomic.AddUint32(&r.dataSeq, 1)
}

// SpaceSequence returns the space sequence number for futex
func (r *RingHeader) SpaceSequence() uint32 {
	return atomic.LoadUint32(&r.spaceSeq)
}

// IncrementSpaceSequence atomically increments the space sequence
func (r *RingHeader) IncrementSpaceSequence() uint32 {
	return atomic.AddUint32(&r.spaceSeq, 1)
}

// Closed returns the closed flag
func (r *RingHeader) Closed() bool {
	return atomic.LoadUint32(&r.closed) != 0
}

// SetClosed sets the closed flag
func (r *RingHeader) SetClosed(closed bool) {
	var val uint32
	if closed {
		val = 1
	}
	atomic.StoreUint32(&r.closed, val)
}

// DataArea returns a pointer to the ring's data area
func (r *RingHeader) DataArea() unsafe.Pointer {
	return unsafe.Pointer(uintptr(unsafe.Pointer(r)) + RingHeaderSize)
}

// Ring invariant calculation helpers

// Used returns the number of bytes currently used in the ring
func (r *RingHeader) Used() uint64 {
	// Use atomic loads to ensure consistency
	w := atomic.LoadUint64(&r.widx)
	rd := atomic.LoadUint64(&r.ridx)
	return w - rd // uint64 arithmetic handles wrap-around
}

// Available returns the number of bytes available for writing
func (r *RingHeader) Available() uint64 {
	capacity := atomic.LoadUint64(&r.capacity)
	used := r.Used()
	return capacity - used
}

// Offset converts a monotonic index to a ring buffer offset
func (r *RingHeader) Offset(index uint64) uint64 {
	capacity := atomic.LoadUint64(&r.capacity)
	return index & (capacity - 1) // fast masked wrap for power-of-2
}

// IsEmpty returns true if the ring is empty
func (r *RingHeader) IsEmpty() bool {
	return r.Used() == 0
}

// IsFull returns true if the ring is full
func (r *RingHeader) IsFull() bool {
	return r.Available() == 0
}

// CanWrite returns true if at least n bytes can be written
func (r *RingHeader) CanWrite(n uint64) bool {
	return r.Available() >= n
}

// CanRead returns true if at least n bytes can be read
func (r *RingHeader) CanRead(n uint64) bool {
	return r.Used() >= n
}

// Layout calculation and validation helpers

// IsPowerOfTwo returns true if n is a power of two
func IsPowerOfTwo(n uint64) bool {
	return n > 0 && (n&(n-1)) == 0
}

// NextPowerOfTwo returns the next power of two >= n
func NextPowerOfTwo(n uint64) uint64 {
	if n == 0 {
		return 1
	}
	if IsPowerOfTwo(n) {
		return n
	}

	// Find the highest set bit and shift left by 1
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n |= n >> 32
	n++
	return n
}

// CalculateSegmentLayout calculates the memory layout for a segment with given ring capacities
func CalculateSegmentLayout(ringACapacity, ringBCapacity uint64) (totalSize, ringAOffset, ringBOffset uint64, err error) {
	// Validate capacities are powers of two
	if !IsPowerOfTwo(ringACapacity) {
		return 0, 0, 0, fmt.Errorf("ring A capacity %d is not a power of two", ringACapacity)
	}
	if !IsPowerOfTwo(ringBCapacity) {
		return 0, 0, 0, fmt.Errorf("ring B capacity %d is not a power of two", ringBCapacity)
	}

	// Validate minimum capacity
	if ringACapacity < MinRingCapacity {
		return 0, 0, 0, fmt.Errorf("ring A capacity %d is below minimum %d", ringACapacity, MinRingCapacity)
	}
	if ringBCapacity < MinRingCapacity {
		return 0, 0, 0, fmt.Errorf("ring B capacity %d is below minimum %d", ringBCapacity, MinRingCapacity)
	}

	// Calculate offsets (aligned to 64-byte boundaries)
	ringAOffset = alignTo64(SegmentHeaderSize)
	ringBOffset = alignTo64(ringAOffset + RingHeaderSize + ringACapacity)
	totalSize = alignTo64(ringBOffset + RingHeaderSize + ringBCapacity)

	return totalSize, ringAOffset, ringBOffset, nil
}

// alignTo64 aligns a size to 64-byte boundary
func alignTo64(size uint64) uint64 {
	return (size + 63) &^ 63
}

// ValidateSegmentHeader validates a segment header for consistency
func ValidateSegmentHeader(h *SegmentHeader) error {
	// Check magic
	if h.Magic() != [8]byte{'G', 'R', 'P', 'C', 'S', 'H', 'M', 0} {
		return fmt.Errorf("invalid magic bytes")
	}

	// Check version
	if h.Version() != SegmentVersion {
		return fmt.Errorf("unsupported version %d, expected %d", h.Version(), SegmentVersion)
	}

	// Check ring capacities are powers of two
	if !IsPowerOfTwo(h.RingACapacity()) {
		return fmt.Errorf("ring A capacity %d is not a power of two", h.RingACapacity())
	}
	if !IsPowerOfTwo(h.RingBCapacity()) {
		return fmt.Errorf("ring B capacity %d is not a power of two", h.RingBCapacity())
	}

	// Check minimum capacities
	if h.RingACapacity() < MinRingCapacity {
		return fmt.Errorf("ring A capacity %d is below minimum %d", h.RingACapacity(), MinRingCapacity)
	}
	if h.RingBCapacity() < MinRingCapacity {
		return fmt.Errorf("ring B capacity %d is below minimum %d", h.RingBCapacity(), MinRingCapacity)
	}

	// Validate offsets and total size
	expectedTotal, expectedRingAOff, expectedRingBOff, err := CalculateSegmentLayout(h.RingACapacity(), h.RingBCapacity())
	if err != nil {
		return fmt.Errorf("layout calculation failed: %w", err)
	}

	if h.TotalSize() != expectedTotal {
		return fmt.Errorf("total size mismatch: got %d, expected %d", h.TotalSize(), expectedTotal)
	}
	if h.RingAOffset() != expectedRingAOff {
		return fmt.Errorf("ring A offset mismatch: got %d, expected %d", h.RingAOffset(), expectedRingAOff)
	}
	if h.RingBOffset() != expectedRingBOff {
		return fmt.Errorf("ring B offset mismatch: got %d, expected %d", h.RingBOffset(), expectedRingBOff)
	}

	return nil
}

// Segment represents a mapped shared memory segment
type Segment struct {
	File *os.File  // File descriptor for the shared memory file
	Mem  []byte    // Memory-mapped region
	H    *hdrView  // Typed view of the segment header
	A    *ringView // Typed view of ring A
	B    *ringView // Typed view of ring B
	Path string    // File path
}

// hdrView provides typed access to the segment header via pointer arithmetic
type hdrView struct {
	basePtr unsafe.Pointer // Base pointer to the memory region
}

// ringView provides typed access to a ring header and data via pointer arithmetic
type ringView struct {
	basePtr unsafe.Pointer // Base pointer to the memory region
	offset  uint64         // Offset to the ring header within the segment
}

// Close unmaps the memory and closes the file
func (s *Segment) Close() error {
	var firstErr error

	// Unmap the memory
	if s.Mem != nil {
		if err := unmapMemory(s.Mem); err != nil && firstErr == nil {
			firstErr = err
		}
		s.Mem = nil
	}

	// Close the file
	if s.File != nil {
		if err := s.File.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		s.File = nil
	}

	return firstErr
}

// hdrView methods - provide typed access to the segment header

// header returns a pointer to the SegmentHeader
func (h *hdrView) header() *SegmentHeader {
	return (*SegmentHeader)(h.basePtr)
}

// Magic returns the magic bytes
func (h *hdrView) Magic() [8]byte {
	return h.header().Magic()
}

// SetMagic sets the magic bytes
func (h *hdrView) SetMagic(magic [8]byte) {
	h.header().SetMagic(magic)
}

// Version returns the protocol version
func (h *hdrView) Version() uint32 {
	return h.header().Version()
}

// SetVersion sets the protocol version
func (h *hdrView) SetVersion(version uint32) {
	h.header().SetVersion(version)
}

// TotalSize returns the total segment size
func (h *hdrView) TotalSize() uint64 {
	return h.header().TotalSize()
}

// SetTotalSize sets the total segment size
func (h *hdrView) SetTotalSize(size uint64) {
	h.header().SetTotalSize(size)
}

// RingAOffset returns the offset to ring A header
func (h *hdrView) RingAOffset() uint64 {
	return h.header().RingAOffset()
}

// SetRingAOffset sets the offset to ring A header
func (h *hdrView) SetRingAOffset(offset uint64) {
	h.header().SetRingAOffset(offset)
}

// RingACapacity returns ring A capacity
func (h *hdrView) RingACapacity() uint64 {
	return h.header().RingACapacity()
}

// SetRingACapacity sets ring A capacity
func (h *hdrView) SetRingACapacity(capacity uint64) {
	h.header().SetRingACapacity(capacity)
}

// RingBOffset returns the offset to ring B header
func (h *hdrView) RingBOffset() uint64 {
	return h.header().RingBOffset()
}

// SetRingBOffset sets the offset to ring B header
func (h *hdrView) SetRingBOffset(offset uint64) {
	h.header().SetRingBOffset(offset)
}

// RingBCapacity returns ring B capacity
func (h *hdrView) RingBCapacity() uint64 {
	return h.header().RingBCapacity()
}

// SetRingBCapacity sets ring B capacity
func (h *hdrView) SetRingBCapacity(capacity uint64) {
	h.header().SetRingBCapacity(capacity)
}

// ServerPID returns the server process ID
func (h *hdrView) ServerPID() uint32 {
	return h.header().ServerPID()
}

// SetServerPID sets the server process ID
func (h *hdrView) SetServerPID(pid uint32) {
	h.header().SetServerPID(pid)
}

// ClientPID returns the client process ID
func (h *hdrView) ClientPID() uint32 {
	return h.header().ClientPID()
}

// SetClientPID sets the client process ID
func (h *hdrView) SetClientPID(pid uint32) {
	h.header().SetClientPID(pid)
}

// ServerReady returns the server ready flag
func (h *hdrView) ServerReady() bool {
	return h.header().ServerReady()
}

// SetServerReady sets the server ready flag
func (h *hdrView) SetServerReady(ready bool) {
	h.header().SetServerReady(ready)
}

// IsValidSharedMemorySegment checks if this segment has valid magic numbers and structure
func (h *hdrView) IsValidSharedMemorySegment() bool {
	magic := h.header().Magic()
	version := h.header().Version()
	return string(magic[:]) == SegmentMagic && version == SegmentVersion
}

// ClientReady returns the client ready flag
func (h *hdrView) ClientReady() bool {
	return h.header().ClientReady()
}

// SetClientReady sets the client ready flag
func (h *hdrView) SetClientReady(ready bool) {
	h.header().SetClientReady(ready)
}

// Closed returns the closed flag
func (h *hdrView) Closed() bool {
	return h.header().Closed()
}

// SetClosed sets the closed flag
func (h *hdrView) SetClosed(closed bool) {
	h.header().SetClosed(closed)
}

// ringView methods - provide typed access to ring headers

// header returns a pointer to the RingHeader
func (r *ringView) header() *RingHeader {
	return (*RingHeader)(unsafe.Pointer(uintptr(r.basePtr) + uintptr(r.offset)))
}

// DataArea returns a pointer to the ring's data area
func (r *ringView) DataArea() unsafe.Pointer {
	return unsafe.Pointer(uintptr(r.basePtr) + uintptr(r.offset) + RingHeaderSize)
}

// Capacity returns the ring capacity
func (r *ringView) Capacity() uint64 {
	return r.header().Capacity()
}

// SetCapacity sets the ring capacity
func (r *ringView) SetCapacity(capacity uint64) {
	r.header().SetCapacity(capacity)
}

// WriteIndex returns the monotonic write index
func (r *ringView) WriteIndex() uint64 {
	return r.header().WriteIndex()
}

// SetWriteIndex sets the monotonic write index
func (r *ringView) SetWriteIndex(idx uint64) {
	r.header().SetWriteIndex(idx)
}

// ReadIndex returns the monotonic read index
func (r *ringView) ReadIndex() uint64 {
	return r.header().ReadIndex()
}

// SetReadIndex sets the monotonic read index
func (r *ringView) SetReadIndex(idx uint64) {
	r.header().SetReadIndex(idx)
}

// DataSequence returns the data sequence number for futex
func (r *ringView) DataSequence() uint32 {
	return r.header().DataSequence()
}

// IncrementDataSequence atomically increments the data sequence
func (r *ringView) IncrementDataSequence() uint32 {
	return r.header().IncrementDataSequence()
}

// SpaceSequence returns the space sequence number for futex
func (r *ringView) SpaceSequence() uint32 {
	return r.header().SpaceSequence()
}

// IncrementSpaceSequence atomically increments the space sequence
func (r *ringView) IncrementSpaceSequence() uint32 {
	return r.header().IncrementSpaceSequence()
}

// Closed returns the closed flag
func (r *ringView) Closed() bool {
	return r.header().Closed()
}

// SetClosed sets the closed flag
func (r *ringView) SetClosed(closed bool) {
	r.header().SetClosed(closed)
}

// Ring invariant calculation methods

// Used returns the number of bytes currently used in the ring
func (r *ringView) Used() uint64 {
	return r.header().Used()
}

// Available returns the number of bytes available for writing
func (r *ringView) Available() uint64 {
	return r.header().Available()
}

// Offset converts a monotonic index to a ring buffer offset
func (r *ringView) Offset(index uint64) uint64 {
	return r.header().Offset(index)
}

// IsEmpty returns true if the ring is empty
func (r *ringView) IsEmpty() bool {
	return r.header().IsEmpty()
}

// IsFull returns true if the ring is full
func (r *ringView) IsFull() bool {
	return r.header().IsFull()
}

// CanWrite returns true if at least n bytes can be written
func (r *ringView) CanWrite(n uint64) bool {
	return r.header().CanWrite(n)
}

// CanRead returns true if at least n bytes can be read
func (r *ringView) CanRead(n uint64) bool {
	return r.header().CanRead(n)
}

// Utility functions

// RemoveSegment removes a shared memory segment file
func RemoveSegment(name string) error {
	// Try both possible paths
	paths := []string{
		"/dev/shm/grpc_shm_" + name,
		os.TempDir() + "/grpc_shm_" + name,
	}

	var lastErr error
	for _, path := range paths {
		if err := os.Remove(path); err == nil {
			return nil // Successfully removed
		} else if !os.IsNotExist(err) {
			lastErr = err // Keep track of non-NotExist errors
		}
	}

	// If we get here, the file wasn't found in either location
	if lastErr != nil {
		return lastErr
	}
	return os.ErrNotExist
}

// SegmentExists checks if a shared memory segment exists
func SegmentExists(name string) bool {
	paths := []string{
		"/dev/shm/grpc_shm_" + name,
		os.TempDir() + "/grpc_shm_" + name,
	}

	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}
