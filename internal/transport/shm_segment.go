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
	"fmt"
	"os"
	"strings"
	"sync"
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

	// Default ring capacity (64 MiB) to keep large payloads in a single write.
	DefaultRingCapacity = 64 * 1024 * 1024

	// Default sizes for shared memory segments and rings
	DefaultSegmentSize = 136 * 1024 * 1024 // Sized to cover two 64 MiB rings plus headers
	DefaultRingASize   = 64 * 1024 * 1024  // 64 MiB for client->server
	DefaultRingBSize   = 64 * 1024 * 1024  // 64 MiB for server->client
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
	maxStreams  uint32   // 0x50: max concurrent streams (0 means unlimited)
	reserved    [44]byte // 0x54-0x7F: reserved/padding to 128B
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

// MaxStreams returns the max concurrent streams.
// A return value of 0 indicates no limit.
func (h *SegmentHeader) MaxStreams() uint32 {
	return atomic.LoadUint32(&h.maxStreams)
}

// SetMaxStreams sets the max concurrent streams.
// A value of 0 indicates no limit.
func (h *SegmentHeader) SetMaxStreams(max uint32) {
	atomic.StoreUint32(&h.maxStreams, max)
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

	// Wake up any waiters when setting to ready
	if ready {
		futexWake(&h.serverReady, 1) // Ignore error - wake is best effort
	}
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

	// Wake up any waiters when setting to ready
	if ready {
		futexWake(&h.clientReady, 1) // Ignore error - wake is best effort
	}
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
	capacity uint64 // 0x00: power-of-two capacity in bytes
	widx     uint64 // 0x08: monotonic write index (producer)
	ridx     uint64 // 0x10: monotonic read index (consumer)
	dataSeq  uint32 // 0x18: data sequence for futex (producer increments)
	spaceSeq uint32 // 0x1C: space sequence for futex (consumer increments)
	closed   uint32 // 0x20: closed flag (producer sets to 1)
	pad      uint32 // 0x24: padding
	// 0x28-0x3F: synchronization fields
	contigSeq     uint32 // 0x28: contiguity sequence (consumer increments on every read commit)
	spaceWaiters  uint32 // 0x2C: number of writers waiting on space
	contigWaiters uint32 // 0x30: number of writers waiting on contiguity
	dataWaiters   uint32 // 0x34: number of readers waiting for data
	// speculativeReserved is the number of bytes speculatively committed by
	// the reader (readIdx advanced) but still referenced by zero-copy buffers.
	// Writers deduct this from available space so they cannot overwrite ring
	// memory still in use by the reader. Accessed atomically.
	speculativeReserved int64 // 0x38-0x3F
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

// CompareAndSwapReadIndex atomically sets r.ridx to new if it currently
// equals old. Used by the deferred-publish ZC protocol to ensure
// header.ReadIdx never regresses when multiple sources (deferred Commit,
// EndZcReservation publish) race.
func (r *RingHeader) CompareAndSwapReadIndex(old, new uint64) bool {
	return atomic.CompareAndSwapUint64(&r.ridx, old, new)
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

// ContigSequence returns the contiguity sequence number for futex
func (r *RingHeader) ContigSequence() uint32 {
	return atomic.LoadUint32(&r.contigSeq)
}

// IncrementContigSequence atomically increments the contiguity sequence
func (r *RingHeader) IncrementContigSequence() uint32 {
	return atomic.AddUint32(&r.contigSeq, 1)
}

// IncSpaceWaiters increments the space waiters counter
func (r *RingHeader) IncSpaceWaiters() uint32 {
	return atomic.AddUint32(&r.spaceWaiters, 1)
}

// DecSpaceWaiters decrements the space waiters counter
func (r *RingHeader) DecSpaceWaiters() uint32 {
	return atomic.AddUint32(&r.spaceWaiters, ^uint32(0))
}

// SpaceWaiters returns the current number of writers waiting for space
func (r *RingHeader) SpaceWaiters() uint32 {
	return atomic.LoadUint32(&r.spaceWaiters)
}

// IncContigWaiters increments the contiguity waiters counter
func (r *RingHeader) IncContigWaiters() uint32 {
	return atomic.AddUint32(&r.contigWaiters, 1)
}

// DecContigWaiters decrements the contiguity waiters counter
func (r *RingHeader) DecContigWaiters() uint32 {
	return atomic.AddUint32(&r.contigWaiters, ^uint32(0))
}

// ContigWaiters returns the current number of writers waiting for contiguity
func (r *RingHeader) ContigWaiters() uint32 {
	return atomic.LoadUint32(&r.contigWaiters)
}

// IncDataWaiters increments the data waiters counter
func (r *RingHeader) IncDataWaiters() uint32 {
	return atomic.AddUint32(&r.dataWaiters, 1)
}

// DecDataWaiters decrements the data waiters counter
func (r *RingHeader) DecDataWaiters() uint32 {
	return atomic.AddUint32(&r.dataWaiters, ^uint32(0))
}

// DataWaiters returns the current number of readers waiting for data
func (r *RingHeader) DataWaiters() uint32 {
	return atomic.LoadUint32(&r.dataWaiters)
}

// SpeculativeReserved returns the bytes speculatively reserved by the reader.
func (r *RingHeader) SpeculativeReserved() int64 {
	return atomic.LoadInt64(&r.speculativeReserved)
}

// AddSpeculativeReserved atomically adds n to speculativeReserved.
func (r *RingHeader) AddSpeculativeReserved(n int64) {
	atomic.AddInt64(&r.speculativeReserved, n)
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

	closed atomic.Bool

	// rings tracks ShmRing structs that wrap this segment's mmap.
	// Segment.Close() walks this list and sets each ring's local
	// closed flag BEFORE unmapping memory, so a reader / writer
	// returning from a wake call observes localClosed=1 and skips
	// the header access that would otherwise touch unmapped memory.
	//
	// Registered via Segment.RegisterRing; safe for concurrent
	// append (registration happens during transport construction,
	// before any goroutine is touching the ring).
	ringsMu sync.Mutex
	rings   []*ShmRing

	// dataSegWaker is the per-data-segment per-direction eventfd
	// waker for the SHM_DATASEG_WAKE=1 fast path. Set by
	// CreateSegment (on the listener side) or OpenSegment (claimed
	// from stash on the dialer side) ONLY for non-control segments.
	// Nil for control segments (which still use the per-address
	// eventfd registry — see shm_inproc_wake_linux.go) and when
	// SHM_DATASEG_WAKE is not set.
	//
	// Propagated to every Ring registered via RegisterRing so the
	// ring's waitFor* / signal* call sites can route through the
	// segment's waker without a per-call lookup.
	dataSegWaker *shmDataSegWaker
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

// RegisterRing records r as wrapping this segment's mmap. On
// Segment.Close, every registered ring has its local closed flag
// set BEFORE the segment is unmapped, so any reader / writer
// returning from a wake call observes r.closed=1 and skips the
// header access that would otherwise touch unmapped memory.
//
// Idempotent; safe to call from transport / dialer / listener
// construction paths regardless of how many of them happen to wrap
// the same segment.
func (s *Segment) RegisterRing(r *ShmRing) {
	if r == nil || s == nil {
		return
	}
	s.ringsMu.Lock()
	for _, existing := range s.rings {
		if existing == r {
			s.ringsMu.Unlock()
			return
		}
	}
	s.rings = append(s.rings, r)
	// Propagate the segment's per-data-segment eventfd waker (if
	// any) to this ring. Rings registered against a non-data segment
	// or before the wake mode is enabled inherit nil and fall through
	// to the per-address eventfd registry / futex path.
	if s.dataSegWaker != nil {
		r.SetDataSegWaker(s.dataSegWaker)
	}
	s.ringsMu.Unlock()
}

// SetDataSegWaker installs a per-data-segment per-direction eventfd
// waker as the segment's wake channel. Idempotent in the sense that
// all future RegisterRing calls propagate this waker; rings already
// registered are NOT retroactively updated (they keep whatever they
// had at register time). Wire-up callers (CreateSegment /
// OpenSegment) must therefore set the waker BEFORE the first
// RegisterRing.
func (s *Segment) SetDataSegWaker(w *shmDataSegWaker) {
	s.dataSegWaker = w
}

// UnblockSameSideParkers closes the per-data-segment eventfd so any
// goroutine blocked in shmDataSegWaker.WaitForChange returns
// immediately (the eventfd file's Read syscall returns EBADF once
// the underlying *os.File is closed; WaitForChange maps that to
// ErrRingClosed). Idempotent via the waker's sync.Once. No-op when
// SHM_DATASEG_WAKE is off (waker is nil).
//
// Transport-level Close paths (ShmClientTransport.Close,
// ShmServerTransport.Close) MUST call this BEFORE wg.Wait()-ing on
// the reader/writer goroutines. The reader, parked on this side's
// recv eventfd via Go netpoll, can only exit on Read error; without
// this call wg.Wait deadlocks. Segment.Close (which runs later in
// the teardown sequence) also calls the waker's Close, but by then
// we're already past wg.Wait so it would be too late.
//
// This is a stop-signal, not a fan-out wake: the eventfd is
// permanently closed, not just signalled. Subsequent Wake/Wait
// calls become no-ops. Safe because the transport is being torn
// down and will not produce further data on this segment.
func (s *Segment) UnblockSameSideParkers() {
	if s.dataSegWaker != nil {
		s.dataSegWaker.Close()
	}
}

// setupDataSegWakeForCreator allocates a fresh pair of per-direction
// eventfds and binds one side to this segment, stashing the peer
// side for the matching OpenSegment call to claim. Called by
// CreateSegment on platforms where the primitive is supported and
// the SHM_DATASEG_WAKE env var is set; no-op for control segments
// (whose name ends in shmControlSuffix) since their long-lived
// listener side never has a matching opener-creator pair within the
// same process.
//
// Falls through silently if the eventfd syscalls fail: the
// segment's rings keep their nil dataSegWaker and the existing per-
// address eventfd / futex path handles wakes.
func setupDataSegWakeForCreator(seg *Segment) {
	if seg == nil || !shmDataSegWakeEnabled {
		return
	}
	if strings.HasSuffix(seg.Path, shmControlSuffix) {
		return
	}
	a, b, err := newShmDataSegWakerPair()
	if err != nil || a == nil || b == nil {
		return
	}
	seg.SetDataSegWaker(a)
	stashShmDataSegWakerForOpener(seg.Path, b)
}

// setupDataSegWakeForOpener claims the stashed peer endpoint from
// the matching CreateSegment call. No-op for control segments and
// when the wake mode is disabled or when there is no stashed entry
// (the cross-process case, which Phase 2 will replace with
// SCM_RIGHTS).
func setupDataSegWakeForOpener(seg *Segment) {
	if seg == nil || !shmDataSegWakeEnabled {
		return
	}
	if strings.HasSuffix(seg.Path, shmControlSuffix) {
		return
	}
	if w := claimShmDataSegWakerForOpener(seg.Path); w != nil {
		seg.SetDataSegWaker(w)
	}
}

// Close unmaps the memory and closes the file
func (s *Segment) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}

	// Set the local closed flag on every Ring that wraps this segment
	// BEFORE unmapping. Readers / writers parked in waitForData /
	// waitForSpace re-check `r.closed` after wake and skip any
	// further header access if set, avoiding use-after-unmap when
	// the segment tears down with a wait outstanding.
	s.ringsMu.Lock()
	for _, r := range s.rings {
		atomic.StoreUint32(&r.closed, 1)
	}
	registered := s.rings
	s.rings = nil
	s.ringsMu.Unlock()

	// Wake any waiters so they return from waitFor* and observe the
	// localClosed flag set above. Use the abstracted wake APIs so the
	// inproc-wake path (Go channels) and the futex / events path both
	// drain correctly.
	for _, r := range registered {
		hdr := r.header()
		r.signalData(&hdr.dataSeq)
		r.signalSpace(&hdr.spaceSeq)
		r.signalContig(&hdr.contigSeq)
	}

	// NOTE: We intentionally do NOT call dataSegWaker.RewakeLocal()
	// here. ring.Close() already does it for each registered ring
	// during the transport-level teardown which happens BEFORE
	// Segment.Close. Doing it again here would create a race window:
	// (1) we wake a same-side parker, (2) the parker's outer loop
	// re-enters waitFor*/header access, (3) the unmap below frees
	// the header memory mid-access. The dataSegWaker.Close() further
	// down closes the eventfd which will return ErrClosed to any
	// final parker through the read syscall path -- that is the
	// correct teardown route at the segment level.

	// Release any same-process wake channels registered against this
	// segment so subsequent tests / connections don't reuse stale
	// entries pointing into the about-to-unmap region. No-op on the
	// futex path (registry is empty).
	dropInprocWakersForSegment(s.Path)

	// Release the per-data-segment socketpair endpoint (if any).
	// Closing the *os.File causes the peer's parked Read to return
	// io.EOF -- our shmDataSegWaker.Wait maps that to ErrRingClosed
	// so the peer's ring loop exits cleanly. Also drains any
	// unclaimed stash entry (in case CreateSegment ran but the
	// matching OpenSegment never happened).
	if s.dataSegWaker != nil {
		s.dataSegWaker.Close()
		s.dataSegWaker = nil
	}
	dropShmDataSegWakerStash(s.Path)

	var firstErr error

	// IMPORTANT: Do not implicitly close rings here.
	//
	// Segment.Close() is used by multiple processes which may share the same
	// underlying shared memory object (e.g. the listener control segment). Closing
	// a ring mutates shared state (sets the closed flag and wakes futex waiters)
	// and must be done explicitly by the logical owner of that communication
	// channel. Segment.Close() only tears down this process's mapping.

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

// SetServerReadyAndSignal sets the server ready flag and signals any waiters.
// This is the preferred method to use instead of s.H.SetServerReady() as it
// handles cross-process signaling on Windows via named events.
func (s *Segment) SetServerReadyAndSignal(ready bool) {
	s.H.SetServerReady(ready)
	if ready {
		segmentName := extractSegmentName(s.Path)
		SignalServerReady(segmentName)
	}
}

// SetClientReadyAndSignal sets the client ready flag and signals any waiters.
// This is the preferred method to use instead of s.H.SetClientReady() as it
// handles cross-process signaling on Windows via named events.
func (s *Segment) SetClientReadyAndSignal(ready bool) {
	s.H.SetClientReady(ready)
	if ready {
		segmentName := extractSegmentName(s.Path)
		SignalClientReady(segmentName)
	}
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

// MaxStreams returns the max concurrent streams.
// A return value of 0 indicates no limit.
func (h *hdrView) MaxStreams() uint32 {
	return h.header().MaxStreams()
}

// SetMaxStreams sets the max concurrent streams.
// A value of 0 indicates no limit.
func (h *hdrView) SetMaxStreams(max uint32) {
	h.header().SetMaxStreams(max)
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
