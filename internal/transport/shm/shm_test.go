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
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

func TestSegmentHeaderSize(t *testing.T) {
	// Verify SegmentHeader is exactly 128 bytes as specified
	size := unsafe.Sizeof(SegmentHeader{})
	if size != SegmentHeaderSize {
		t.Errorf("SegmentHeader size = %d, want %d", size, SegmentHeaderSize)
	}
}

func TestRingHeaderSize(t *testing.T) {
	// Verify RingHeader is exactly 64 bytes as specified
	size := unsafe.Sizeof(RingHeader{})
	if size != RingHeaderSize {
		t.Errorf("RingHeader size = %d, want %d", size, RingHeaderSize)
	}
}

func TestSegmentHeaderFieldOffsets(t *testing.T) {
	h := &SegmentHeader{}

	// Test field offsets match specification
	tests := []struct {
		name   string
		offset uintptr
		want   uintptr
	}{
		{"magic", unsafe.Offsetof(h.magic), 0x00},
		{"version", unsafe.Offsetof(h.version), 0x08},
		{"flags", unsafe.Offsetof(h.flags), 0x0C},
		{"totalSize", unsafe.Offsetof(h.totalSize), 0x10},
		{"ringAOff", unsafe.Offsetof(h.ringAOff), 0x18},
		{"ringACap", unsafe.Offsetof(h.ringACap), 0x20},
		{"ringBOff", unsafe.Offsetof(h.ringBOff), 0x28},
		{"ringBCap", unsafe.Offsetof(h.ringBCap), 0x30},
		{"serverPID", unsafe.Offsetof(h.serverPID), 0x38},
		{"clientPID", unsafe.Offsetof(h.clientPID), 0x3C},
		{"serverReady", unsafe.Offsetof(h.serverReady), 0x40},
		{"clientReady", unsafe.Offsetof(h.clientReady), 0x44},
		{"closed", unsafe.Offsetof(h.closed), 0x48},
		{"pad", unsafe.Offsetof(h.pad), 0x4C},
		{"reserved", unsafe.Offsetof(h.reserved), 0x50},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.offset != tt.want {
				t.Errorf("offset of %s = 0x%02X, want 0x%02X", tt.name, uint64(tt.offset), uint64(tt.want))
			}
		})
	}
}

func TestRingHeaderFieldOffsets(t *testing.T) {
	r := &RingHeader{}

	// Test field offsets match specification
	tests := []struct {
		name   string
		offset uintptr
		want   uintptr
	}{
		{"capacity", unsafe.Offsetof(r.capacity), 0x00},
		{"widx", unsafe.Offsetof(r.widx), 0x08},
		{"ridx", unsafe.Offsetof(r.ridx), 0x10},
		{"dataSeq", unsafe.Offsetof(r.dataSeq), 0x18},
		{"spaceSeq", unsafe.Offsetof(r.spaceSeq), 0x1C},
		{"closed", unsafe.Offsetof(r.closed), 0x20},
		{"pad", unsafe.Offsetof(r.pad), 0x24},
		{"reserved", unsafe.Offsetof(r.reserved), 0x28},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.offset != tt.want {
				t.Errorf("offset of %s = 0x%02X, want 0x%02X", tt.name, uint64(tt.offset), uint64(tt.want))
			}
		})
	}
}

func TestIsPowerOfTwo(t *testing.T) {
	tests := []struct {
		n    uint64
		want bool
	}{
		{0, false},
		{1, true},
		{2, true},
		{3, false},
		{4, true},
		{5, false},
		{8, true},
		{16, true},
		{1024, true},
		{1023, false},
		{4096, true},
		{65536, true},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			if got := IsPowerOfTwo(tt.n); got != tt.want {
				t.Errorf("IsPowerOfTwo(%d) = %v, want %v", tt.n, got, tt.want)
			}
		})
	}
}

func TestNextPowerOfTwo(t *testing.T) {
	tests := []struct {
		n    uint64
		want uint64
	}{
		{0, 1},
		{1, 1},
		{2, 2},
		{3, 4},
		{4, 4},
		{5, 8},
		{8, 8},
		{9, 16},
		{1023, 1024},
		{1024, 1024},
		{1025, 2048},
		{4095, 4096},
		{4096, 4096},
		{65535, 65536},
		{65536, 65536},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			if got := NextPowerOfTwo(tt.n); got != tt.want {
				t.Errorf("NextPowerOfTwo(%d) = %d, want %d", tt.n, got, tt.want)
			}
		})
	}
}

func TestCalculateSegmentLayout(t *testing.T) {
	tests := []struct {
		name          string
		ringACapacity uint64
		ringBCapacity uint64
		wantErr       bool
		wantTotalSize uint64
		wantRingAOff  uint64
		wantRingBOff  uint64
	}{
		{
			name:          "default capacities",
			ringACapacity: DefaultRingCapacity,
			ringBCapacity: DefaultRingCapacity,
			wantErr:       false,
			wantTotalSize: 131328, // Calculated: 128 + 64 + 65536 + 64 + 65536 = 131328
			wantRingAOff:  128,    // Aligned segment header
			wantRingBOff:  65728,  // Aligned after ring A: 128 + 64 + 65536 = 65728
		},
		{
			name:          "minimum capacities",
			ringACapacity: MinRingCapacity,
			ringBCapacity: MinRingCapacity,
			wantErr:       false,
		},
		{
			name:          "non-power-of-two ring A",
			ringACapacity: 1000,
			ringBCapacity: 4096,
			wantErr:       true,
		},
		{
			name:          "non-power-of-two ring B",
			ringACapacity: 4096,
			ringBCapacity: 1000,
			wantErr:       true,
		},
		{
			name:          "below minimum ring A",
			ringACapacity: 1024,
			ringBCapacity: 4096,
			wantErr:       true,
		},
		{
			name:          "below minimum ring B",
			ringACapacity: 4096,
			ringBCapacity: 1024,
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			totalSize, ringAOff, ringBOff, err := CalculateSegmentLayout(tt.ringACapacity, tt.ringBCapacity)

			if (err != nil) != tt.wantErr {
				t.Errorf("CalculateSegmentLayout() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if tt.wantTotalSize != 0 && totalSize != tt.wantTotalSize {
					t.Errorf("CalculateSegmentLayout() totalSize = %d, want %d", totalSize, tt.wantTotalSize)
				}
				if tt.wantRingAOff != 0 && ringAOff != tt.wantRingAOff {
					t.Errorf("CalculateSegmentLayout() ringAOff = %d, want %d", ringAOff, tt.wantRingAOff)
				}
				if tt.wantRingBOff != 0 && ringBOff != tt.wantRingBOff {
					t.Errorf("CalculateSegmentLayout() ringBOff = %d, want %d", ringBOff, tt.wantRingBOff)
				}
			}
		})
	}
}

func TestRingInvariants(t *testing.T) {
	r := &RingHeader{}
	r.SetCapacity(4096)
	r.SetWriteIndex(0)
	r.SetReadIndex(0)

	// Test empty ring
	if !r.IsEmpty() {
		t.Error("IsEmpty() = false, want true for new ring")
	}
	if r.IsFull() {
		t.Error("IsFull() = true, want false for new ring")
	}
	if r.Used() != 0 {
		t.Errorf("Used() = %d, want 0 for new ring", r.Used())
	}
	if r.Available() != 4096 {
		t.Errorf("Available() = %d, want 4096 for new ring", r.Available())
	}

	// Test after writing some data
	r.SetWriteIndex(100)
	if r.IsEmpty() {
		t.Error("IsEmpty() = true, want false after writing")
	}
	if r.Used() != 100 {
		t.Errorf("Used() = %d, want 100 after writing 100 bytes", r.Used())
	}
	if r.Available() != 3996 {
		t.Errorf("Available() = %d, want 3996 after writing 100 bytes", r.Available())
	}

	// Test offset calculation
	offset := r.Offset(4200) // > capacity
	if offset != 104 {       // 4200 & (4096-1) = 104
		t.Errorf("Offset(4200) = %d, want 104", offset)
	}
}

func TestSegmentHeaderAtomicAccess(t *testing.T) {
	h := &SegmentHeader{}

	// Test magic bytes
	magic := [8]byte{'G', 'R', 'P', 'C', 'S', 'H', 'M', 0}
	h.SetMagic(magic)
	if h.Magic() != magic {
		t.Errorf("Magic() = %v, want %v", h.Magic(), magic)
	}

	// Test version
	h.SetVersion(SegmentVersion)
	if h.Version() != SegmentVersion {
		t.Errorf("Version() = %d, want %d", h.Version(), SegmentVersion)
	}

	// Test flags
	h.SetServerReady(true)
	if !h.ServerReady() {
		t.Error("ServerReady() = false, want true")
	}

	h.SetClientReady(true)
	if !h.ClientReady() {
		t.Error("ClientReady() = false, want true")
	}

	h.SetClosed(true)
	if !h.Closed() {
		t.Error("Closed() = false, want true")
	}
}

func TestCreateAndOpenSegment(t *testing.T) {
	// Skip test on non-Linux platforms
	if !isLinuxPlatform() {
		t.Skip("Segment tests only supported on Linux")
	}

	name := "test_segment_create_open"
	ringCapA := uint64(4096)
	ringCapB := uint64(8192)

	// Ensure clean state
	RemoveSegment(name)
	defer RemoveSegment(name)

	// Test CreateSegment
	segment, err := CreateSegment(name, ringCapA, ringCapB)
	if err != nil {
		t.Fatalf("CreateSegment() error = %v", err)
	}
	defer segment.Close()

	// Verify segment properties
	if segment.File == nil {
		t.Error("segment.File is nil")
	}
	if segment.Mem == nil {
		t.Error("segment.Mem is nil")
	}
	if segment.H == nil {
		t.Error("segment.H is nil")
	}
	if segment.A == nil {
		t.Error("segment.A is nil")
	}
	if segment.B == nil {
		t.Error("segment.B is nil")
	}

	// Verify header values
	magic := [8]byte{'G', 'R', 'P', 'C', 'S', 'H', 'M', 0}
	if segment.H.Magic() != magic {
		t.Errorf("segment.H.Magic() = %v, want %v", segment.H.Magic(), magic)
	}
	if segment.H.Version() != SegmentVersion {
		t.Errorf("segment.H.Version() = %d, want %d", segment.H.Version(), SegmentVersion)
	}
	if segment.H.RingACapacity() != ringCapA {
		t.Errorf("segment.H.RingACapacity() = %d, want %d", segment.H.RingACapacity(), ringCapA)
	}
	if segment.H.RingBCapacity() != ringCapB {
		t.Errorf("segment.H.RingBCapacity() = %d, want %d", segment.H.RingBCapacity(), ringCapB)
	}

	// Simulate server marking itself ready (since we removed auto-setting)
	segment.H.SetServerReady(true)

	if !segment.H.ServerReady() {
		t.Error("segment.H.ServerReady() = false, want true")
	}

	// Verify ring configurations
	if segment.A.Capacity() != ringCapA {
		t.Errorf("segment.A.Capacity() = %d, want %d", segment.A.Capacity(), ringCapA)
	}
	if segment.B.Capacity() != ringCapB {
		t.Errorf("segment.B.Capacity() = %d, want %d", segment.B.Capacity(), ringCapB)
	}

	// Test that rings are initially empty
	if !segment.A.IsEmpty() {
		t.Error("segment.A.IsEmpty() = false, want true for new ring")
	}
	if !segment.B.IsEmpty() {
		t.Error("segment.B.IsEmpty() = false, want true for new ring")
	}

	// Test OpenSegment from another "process" (same process for testing)
	clientSegment, err := OpenSegment(name)
	if err != nil {
		t.Fatalf("OpenSegment() error = %v", err)
	}
	defer clientSegment.Close()

	// Verify client segment can read server data
	if clientSegment.H.Magic() != magic {
		t.Errorf("clientSegment.H.Magic() = %v, want %v", clientSegment.H.Magic(), magic)
	}
	if clientSegment.H.Version() != SegmentVersion {
		t.Errorf("clientSegment.H.Version() = %d, want %d", clientSegment.H.Version(), SegmentVersion)
	}
	if clientSegment.H.RingACapacity() != ringCapA {
		t.Errorf("clientSegment.H.RingACapacity() = %d, want %d", clientSegment.H.RingACapacity(), ringCapA)
	}
	if clientSegment.H.RingBCapacity() != ringCapB {
		t.Errorf("clientSegment.H.RingBCapacity() = %d, want %d", clientSegment.H.RingBCapacity(), ringCapB)
	}
	// Note: ServerReady is already set by the server above, so client can see it
	if !clientSegment.H.ServerReady() {
		t.Error("clientSegment.H.ServerReady() = false, want true")
	}
	// Simulate client marking itself ready
	clientSegment.H.SetClientReady(true)
	if !clientSegment.H.ClientReady() {
		t.Error("clientSegment.H.ClientReady() = false, want true after OpenSegment")
	}
}

func TestCreateSegmentAlreadyExists(t *testing.T) {
	// Skip test on non-Linux platforms
	if !isLinuxPlatform() {
		t.Skip("Segment tests only supported on Linux")
	}

	name := "test_segment_exists"

	// Ensure clean state
	RemoveSegment(name)
	defer RemoveSegment(name)

	// Create first segment
	segment1, err := CreateSegment(name, 4096, 4096)
	if err != nil {
		t.Fatalf("CreateSegment() error = %v", err)
	}
	defer segment1.Close()

	// Try to create another segment with same name (should fail)
	segment2, err := CreateSegment(name, 4096, 4096)
	if err == nil {
		segment2.Close()
		t.Fatal("CreateSegment() should fail when segment already exists")
	}
}

func TestOpenSegmentNotExists(t *testing.T) {
	// Skip test on non-Linux platforms
	if !isLinuxPlatform() {
		t.Skip("Segment tests only supported on Linux")
	}

	name := "test_segment_not_exists"

	// Ensure segment doesn't exist
	RemoveSegment(name)

	// Try to open non-existent segment
	segment, err := OpenSegment(name)
	if err == nil {
		segment.Close()
		t.Fatal("OpenSegment() should fail when segment doesn't exist")
	}
}

func TestSegmentUtilities(t *testing.T) {
	name := "test_segment_utilities"

	// Test SegmentExists with non-existent segment
	if SegmentExists(name) {
		t.Error("SegmentExists() = true for non-existent segment")
	}

	// Skip remaining tests on non-Linux platforms
	if !isLinuxPlatform() {
		t.Skip("Remaining segment tests only supported on Linux")
	}

	// Ensure clean state
	RemoveSegment(name)
	defer RemoveSegment(name)

	// Create segment
	segment, err := CreateSegment(name, 4096, 4096)
	if err != nil {
		t.Fatalf("CreateSegment() error = %v", err)
	}
	defer segment.Close()

	// Test SegmentExists with existing segment
	if !SegmentExists(name) {
		t.Error("SegmentExists() = false for existing segment")
	}

	// Close and remove segment
	segment.Close()
	err = RemoveSegment(name)
	if err != nil {
		t.Errorf("RemoveSegment() error = %v", err)
	}

	// Test SegmentExists after removal
	if SegmentExists(name) {
		t.Error("SegmentExists() = true after removal")
	}
}

func TestRingViewOperations(t *testing.T) {
	// Skip test on non-Linux platforms
	if !isLinuxPlatform() {
		t.Skip("Segment tests only supported on Linux")
	}

	name := "test_ring_operations"
	ringCap := uint64(4096)

	// Ensure clean state
	RemoveSegment(name)
	defer RemoveSegment(name)

	// Create segment
	segment, err := CreateSegment(name, ringCap, ringCap)
	if err != nil {
		t.Fatalf("CreateSegment() error = %v", err)
	}
	defer segment.Close()

	ring := segment.A

	// Test initial state
	if ring.WriteIndex() != 0 {
		t.Errorf("WriteIndex() = %d, want 0", ring.WriteIndex())
	}
	if ring.ReadIndex() != 0 {
		t.Errorf("ReadIndex() = %d, want 0", ring.ReadIndex())
	}
	if !ring.IsEmpty() {
		t.Error("IsEmpty() = false, want true")
	}
	if ring.IsFull() {
		t.Error("IsFull() = true, want false")
	}
	if ring.Used() != 0 {
		t.Errorf("Used() = %d, want 0", ring.Used())
	}
	if ring.Available() != ringCap {
		t.Errorf("Available() = %d, want %d", ring.Available(), ringCap)
	}

	// Test write operations
	ring.SetWriteIndex(100)
	if ring.WriteIndex() != 100 {
		t.Errorf("WriteIndex() = %d, want 100", ring.WriteIndex())
	}
	if ring.Used() != 100 {
		t.Errorf("Used() = %d, want 100", ring.Used())
	}
	if ring.Available() != ringCap-100 {
		t.Errorf("Available() = %d, want %d", ring.Available(), ringCap-100)
	}
	if ring.IsEmpty() {
		t.Error("IsEmpty() = true, want false after writing")
	}

	// Test offset calculation
	offset := ring.Offset(4200) // > capacity
	expectedOffset := uint64(4200) & (ringCap - 1)
	if offset != expectedOffset {
		t.Errorf("Offset(4200) = %d, want %d", offset, expectedOffset)
	}

	// Test sequence operations
	seq1 := ring.IncrementDataSequence()
	seq2 := ring.IncrementDataSequence()
	if seq2 != seq1+1 {
		t.Errorf("IncrementDataSequence() not incrementing correctly: %d, %d", seq1, seq2)
	}

	// Test closed flag
	if ring.Closed() {
		t.Error("Closed() = true, want false initially")
	}
	ring.SetClosed(true)
	if !ring.Closed() {
		t.Error("Closed() = false, want true after SetClosed(true)")
	}
}

// isLinuxPlatform returns true if we're running on a supported Linux platform
func isLinuxPlatform() bool {
	// This will be true only when the Linux-specific files are compiled
	segment, err := CreateSegment("__test_platform_check__", 4096, 4096)
	if err != nil {
		return false
	}
	segment.Close()
	RemoveSegment("__test_platform_check__")
	return true
}

func TestFutexBasic(t *testing.T) {
	// Test basic futex operations
	var addr uint32 = 42

	// Test wake on address with no waiters - should return 0
	n, err := futexWake(&addr, 1)
	if !isLinuxPlatform() {
		// On non-Linux platforms, should return ErrUnsupported
		if err == nil {
			t.Error("futexWake should return error on non-Linux platforms")
		}
		return
	}

	if err != nil {
		t.Fatalf("futexWake failed: %v", err)
	}
	if n != 0 {
		t.Errorf("futexWake returned %d, want 0 (no waiters)", n)
	}

	// Test wait with wrong value - should return immediately
	addr = 42
	err = futexWait(&addr, 100) // wrong value
	if err != nil {
		t.Errorf("futexWait with wrong value failed: %v", err)
	}
}

func TestFutexWaitWake(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Futex tests only supported on Linux")
	}

	var addr uint32 = 0
	waitDone := make(chan bool, 1)
	wakeCount := make(chan int, 1)

	// Start a goroutine that waits
	go func() {
		err := futexWait(&addr, 0) // wait while addr == 0
		if err != nil {
			t.Errorf("futexWait failed: %v", err)
		}
		waitDone <- true
	}()

	// Give the waiter a chance to start waiting
	time.Sleep(10 * time.Millisecond)

	// Wake the waiter
	go func() {
		n, err := futexWake(&addr, 1)
		if err != nil {
			t.Errorf("futexWake failed: %v", err)
		}
		wakeCount <- n
	}()

	// Wait for both operations to complete
	select {
	case <-waitDone:
		// Good - waiter was woken
	case <-time.After(1 * time.Second):
		t.Fatal("futexWait did not return within timeout")
	}

	select {
	case n := <-wakeCount:
		if n != 1 {
			t.Errorf("futexWake returned %d, want 1", n)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("futexWake did not complete within timeout")
	}
}

func TestFutexMultipleWaiters(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Futex tests only supported on Linux")
	}

	var addr uint32 = 42
	const numWaiters = 3
	waitDone := make(chan bool, numWaiters)

	// Start multiple waiters
	for i := 0; i < numWaiters; i++ {
		go func() {
			err := futexWait(&addr, 42) // wait while addr == 42
			if err != nil {
				t.Errorf("futexWait failed: %v", err)
			}
			waitDone <- true
		}()
	}

	// Give waiters a chance to start waiting
	time.Sleep(10 * time.Millisecond)

	// Wake all waiters
	n, err := futexWake(&addr, numWaiters)
	if err != nil {
		t.Fatalf("futexWake failed: %v", err)
	}

	// Should wake all waiters (though some might not have been waiting yet)
	if n > numWaiters {
		t.Errorf("futexWake returned %d, want <= %d", n, numWaiters)
	}

	// Wait for all waiters to complete
	for i := 0; i < numWaiters; i++ {
		select {
		case <-waitDone:
			// Good - waiter was woken
		case <-time.After(1 * time.Second):
			t.Fatalf("Waiter %d did not return within timeout", i)
		}
	}
}

func TestFutexValueChange(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Futex tests only supported on Linux")
	}

	var addr uint32 = 0
	waitDone := make(chan bool, 1)

	// Start a waiter
	go func() {
		err := futexWait(&addr, 0) // wait while addr == 0
		if err != nil {
			t.Errorf("futexWait failed: %v", err)
		}
		waitDone <- true
	}()

	// Give the waiter a chance to start waiting
	time.Sleep(10 * time.Millisecond)

	// Change the value (this alone won't wake the waiter in our implementation)
	atomic.StoreUint32(&addr, 1)

	// Now wake the waiter
	n, err := futexWake(&addr, 1)
	if err != nil {
		t.Fatalf("futexWake failed: %v", err)
	}

	// Wait for the waiter to complete
	select {
	case <-waitDone:
		// Good - waiter was woken
	case <-time.After(1 * time.Second):
		t.Fatal("futexWait did not return within timeout")
	}

	t.Logf("futexWake woke %d waiters", n)
}

func TestFutexSpuriousWake(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Futex tests only supported on Linux")
	}

	var addr uint32 = 100
	done := make(chan bool, 1)

	// Test the typical usage pattern in a separate goroutine
	go func() {
		waitCount := 0
		maxWaits := 5

		for waitCount < maxWaits {
			// Typical usage pattern: check condition, then wait only if condition unmet
			currentVal := atomic.LoadUint32(&addr)
			if currentVal != 100 {
				break // Condition met - value changed
			}

			// Condition not met, wait for change
			err := futexWait(&addr, 100)
			if err != nil {
				t.Errorf("futexWait failed: %v", err)
				break
			}

			waitCount++

			// After waking, check condition again (handles spurious wakes)
			currentVal = atomic.LoadUint32(&addr)
			if currentVal != 100 {
				break // Condition actually changed
			}

			// For this test, we'll break after a few iterations
			if waitCount >= 3 {
				break
			}
		}

		done <- true
	}()

	// Give the waiter a chance to start
	time.Sleep(10 * time.Millisecond)

	// Change the value and wake the waiter
	atomic.StoreUint32(&addr, 200)
	n, err := futexWake(&addr, 1)
	if err != nil {
		t.Errorf("futexWake failed: %v", err)
	}

	// Wait for completion
	select {
	case <-done:
		t.Logf("Test completed successfully, futexWake woke %d waiters", n)
	case <-time.After(2 * time.Second):
		t.Fatal("Test did not complete within timeout")
	}
}

func TestFutexWithSharedMemory(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Futex tests only supported on Linux")
	}

	name := "test_futex_shared"
	defer RemoveSegment(name)

	// Create a shared memory segment
	segment, err := CreateSegment(name, 4096, 4096)
	if err != nil {
		t.Fatalf("CreateSegment failed: %v", err)
	}
	defer segment.Close()

	// Test futex operations on shared memory
	// Use the dataSeq field from ring A as our futex target
	ring := segment.A

	// Test initial state
	if ring.DataSequence() != 0 {
		t.Errorf("Initial DataSequence = %d, want 0", ring.DataSequence())
	}

	// Test increment and futex wake
	oldSeq := ring.IncrementDataSequence()
	newSeq := ring.DataSequence()
	if newSeq != oldSeq {
		t.Errorf("DataSequence after increment = %d, want %d", newSeq, oldSeq)
	}

	// Get pointer to the sequence field for futex operations
	seqPtr := (*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(ring.header())) + unsafe.Offsetof(ring.header().dataSeq)))

	// Test futex wake on the sequence field
	n, err := futexWake(seqPtr, 1)
	if err != nil {
		t.Errorf("futexWake on shared memory failed: %v", err)
	}
	if n != 0 {
		t.Logf("futexWake woke %d waiters (expected 0)", n)
	}

	// Test async wait/wake pattern
	waitDone := make(chan bool, 1)
	currentSeq := ring.DataSequence()

	go func() {
		// Wait for sequence to change
		err := futexWait(seqPtr, currentSeq)
		if err != nil {
			t.Errorf("futexWait on shared memory failed: %v", err)
		}
		waitDone <- true
	}()

	// Give waiter time to start
	time.Sleep(10 * time.Millisecond)

	// Increment sequence and wake
	ring.IncrementDataSequence()
	n, err = futexWake(seqPtr, 1)
	if err != nil {
		t.Errorf("futexWake failed: %v", err)
	}

	// Wait for completion
	select {
	case <-waitDone:
		t.Logf("Shared memory futex test completed successfully, woke %d waiters", n)
	case <-time.After(1 * time.Second):
		t.Fatal("Futex wait on shared memory did not complete within timeout")
	}
}
