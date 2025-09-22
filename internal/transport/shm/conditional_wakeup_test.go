//go:build linux

package shm

import (
	"fmt"
	"testing"
	"time"
)

// TestConditionalWakeups verifies that wakeups only happen on appropriate state transitions
func TestConditionalWakeups(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Conditional wakeup tests only supported on Linux")
	}

	cap := uint64(4096)
	name := fmt.Sprintf("test-conditional-wakeups-%d", time.Now().UnixNano())

	// Ensure clean state
	RemoveSegment(name)
	defer RemoveSegment(name)

	seg, err := CreateSegment(name, cap, cap)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer seg.Close()

	ring := NewShmRingFromSegment(seg.A, seg.Mem)
	hdr := ring.header()

	// Test 1: Every positive write should increment dataSeq (new semantics)
	t.Run("EveryWriteIncrementsDataSeqBasic", func(t *testing.T) {
		// Ensure ring is empty
		for !ring.IsEmpty() {
			buf := make([]byte, 100)
			ring.ReadBlocking(buf)
		}

		initialDataSeq := hdr.DataSequence()

		// First write: should increment dataSeq (every positive write increments)
		err := ring.WriteBlocking([]byte("first"))
		if err != nil {
			t.Fatalf("First write failed: %v", err)
		}

		newDataSeq := hdr.DataSequence()
		if newDataSeq != initialDataSeq+1 {
			t.Errorf("First write should increment dataSeq: expected %d, got %d", initialDataSeq+1, newDataSeq)
		}

		// Second write: should also increment dataSeq (every positive write increments)
		prevDataSeq := newDataSeq
		err = ring.WriteBlocking([]byte("second"))
		if err != nil {
			t.Fatalf("Second write failed: %v", err)
		}

		finalDataSeq := hdr.DataSequence()
		if finalDataSeq != prevDataSeq+1 {
			t.Errorf("Second write should increment dataSeq: expected %d, got %d", prevDataSeq+1, finalDataSeq)
		}
	})

	// Test 2: Reader should wake writer only on full→not-full transition
	t.Run("ReaderWakesWriterOnlyOnFullToNotFull", func(t *testing.T) {
		// Fill the ring to capacity
		largeData := make([]byte, cap)
		for i := range largeData {
			largeData[i] = byte(i % 256)
		}

		// Clear ring first
		for !ring.IsEmpty() {
			buf := make([]byte, 100)
			ring.ReadBlocking(buf)
		}

		// Fill to capacity
		err := ring.WriteBlocking(largeData)
		if err != nil {
			t.Fatalf("Failed to fill ring: %v", err)
		}

		// Verify it's full
		if ring.Used() != cap {
			t.Fatalf("Ring should be full: used=%d, capacity=%d", ring.Used(), cap)
		}

		initialSpaceSeq := hdr.SpaceSequence()

		// First read: should increment spaceSeq (full→not-full)
		buf1 := make([]byte, 100)
		_, err = ring.ReadBlocking(buf1)
		if err != nil {
			t.Fatalf("First read failed: %v", err)
		}

		newSpaceSeq := hdr.SpaceSequence()
		if newSpaceSeq != initialSpaceSeq+1 {
			t.Errorf("First read should increment spaceSeq: expected %d, got %d", initialSpaceSeq+1, newSpaceSeq)
		}

		// Second read: should NOT increment spaceSeq (not-full→not-full)
		prevSpaceSeq := newSpaceSeq
		buf2 := make([]byte, 100)
		_, err = ring.ReadBlocking(buf2)
		if err != nil {
			t.Fatalf("Second read failed: %v", err)
		}

		finalSpaceSeq := hdr.SpaceSequence()
		if finalSpaceSeq != prevSpaceSeq {
			t.Errorf("Second read should NOT increment spaceSeq: expected %d, got %d", prevSpaceSeq, finalSpaceSeq)
		}
	})

	// Test 3: Every positive write should increment dataSeq (new semantics)
	t.Run("EveryWriteIncrementsDataSeq", func(t *testing.T) {
		// Ensure ring is empty
		for !ring.IsEmpty() {
			buf := make([]byte, 100)
			ring.ReadBlocking(buf)
		}

		// Every write should increment dataSeq
		for i := 1; i <= 5; i++ {
			prevSeq := hdr.DataSequence()
			ring.WriteBlocking([]byte{byte(i)})
			newSeq := hdr.DataSequence()
			if newSeq != prevSeq+1 {
				t.Errorf("Write %d should increment dataSeq: expected %d, got %d", i, prevSeq+1, newSeq)
			}
		}
	})

	// Test 4: Multiple reads before full→not-full should not increment spaceSeq
	t.Run("MultipleReadsBeforeFullDoNotWake", func(t *testing.T) {
		// Fill ring completely
		for !ring.IsEmpty() {
			buf := make([]byte, 100)
			ring.ReadBlocking(buf)
		}

		fillData := make([]byte, cap)
		ring.WriteBlocking(fillData)

		// First read from full should wake
		initialSpaceSeq := hdr.SpaceSequence()
		buf := make([]byte, 100)
		ring.ReadBlocking(buf)

		firstSeq := hdr.SpaceSequence()
		if firstSeq != initialSpaceSeq+1 {
			t.Errorf("First read from full should wake: expected %d, got %d", initialSpaceSeq+1, firstSeq)
		}

		// Subsequent reads should NOT wake
		for i := 0; i < 3; i++ {
			prevSeq := hdr.SpaceSequence()
			buf := make([]byte, 50)
			ring.ReadBlocking(buf)
			newSeq := hdr.SpaceSequence()
			if newSeq != prevSeq {
				t.Errorf("Read %d should NOT wake: expected %d, got %d", i+2, prevSeq, newSeq)
			}
		}
	})
}

// TestConditionalWakeupPerformance demonstrates the performance improvement
func TestConditionalWakeupPerformance(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Performance tests only supported on Linux")
	}

	cap := uint64(4096)
	name := fmt.Sprintf("test-wakeup-performance-%d", time.Now().UnixNano())

	// Ensure clean state
	RemoveSegment(name)
	defer RemoveSegment(name)

	seg, err := CreateSegment(name, cap, cap)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer seg.Close()

	ring := NewShmRingFromSegment(seg.A, seg.Mem)
	hdr := ring.header()

	// Measure time for many writes with conditional wakeups
	t.Run("ConditionalWakeupBenchmark", func(t *testing.T) {
		// Clear ring
		for !ring.IsEmpty() {
			buf := make([]byte, 100)
			ring.ReadBlocking(buf)
		}

		start := time.Now()
		initialDataSeq := hdr.DataSequence()

		// Perform many small writes - only first should cause wakeup
		numWrites := 1000
		for i := 0; i < numWrites; i++ {
			err := ring.WriteBlocking([]byte{byte(i % 256)})
			if err != nil {
				t.Fatalf("Write %d failed: %v", i, err)
			}
		}

		elapsed := time.Since(start)
		finalDataSeq := hdr.DataSequence()

		// Only one wakeup should have occurred (first write: empty→non-empty)
		expectedWakeups := uint32(1)
		actualWakeups := finalDataSeq - initialDataSeq

		if actualWakeups != expectedWakeups {
			t.Errorf("Expected %d wakeups, got %d", expectedWakeups, actualWakeups)
		}

		t.Logf("Performed %d writes in %v with %d wakeups (should be 1)",
			numWrites, elapsed, actualWakeups)

		// With conditional wakeups, this should be much faster than unconditional
		// (though we can't easily test the old behavior for comparison)
		if elapsed > 100*time.Millisecond {
			t.Logf("Warning: %d writes took %v, may indicate performance issue", numWrites, elapsed)
		}
	})
}
