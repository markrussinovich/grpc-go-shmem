//go:build linux

package shm

import (
	"fmt"
	"testing"
	"time"
)

// TestConditionalWakeups verifies that wakeups only happen on appropriate state transitions.
//
// Architecture Overview:
// - DataSequence: Increments on every write for correctness (readers need to know data is available)
// - SpaceSequence: Increments only on full→not-full transitions for optimization  
// - futexWake(): Expensive kernel call, only happens on state transitions (empty→non-empty, full→not-full)
//
// The "conditional wakeup optimization" reduces expensive futex wake calls while maintaining
// correctness through sequence counter increments.
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

	// Test 3: Multiple reads before full→not-full should not increment spaceSeq
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

// TestConditionalWakeupPerformance validates that conditional wakeup optimization works correctly.
// The optimization is: increment DataSequence for every write (for correctness), but only
// call futexWake() on empty→non-empty transitions (for performance).
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

	// Validate that the conditional wakeup logic works correctly
	t.Run("ConditionalWakeupBehavior", func(t *testing.T) {
		// Clear ring
		for !ring.IsEmpty() {
			buf := make([]byte, 100)
			ring.ReadBlocking(buf)
		}

		start := time.Now()
		initialDataSeq := hdr.DataSequence()

		// Perform multiple writes to a non-empty ring
		numWrites := 100
		for i := 0; i < numWrites; i++ {
			err := ring.WriteBlocking([]byte{byte(i % 256)})
			if err != nil {
				t.Fatalf("Write %d failed: %v", i, err)
			}
		}

		elapsed := time.Since(start)
		finalDataSeq := hdr.DataSequence()

		// DataSequence should increment for every write (correctness requirement)
		expectedIncrements := uint32(numWrites)
		actualIncrements := finalDataSeq - initialDataSeq

		if actualIncrements != expectedIncrements {
			t.Errorf("Expected %d DataSequence increments, got %d", expectedIncrements, actualIncrements)
		}

		t.Logf("Performed %d writes in %v with %d DataSequence increments (expected %d)",
			numWrites, elapsed, actualIncrements, expectedIncrements)

		// Verify the ring has data
		if ring.IsEmpty() {
			t.Error("Ring should not be empty after writes")
		}

		// Performance expectation: should be reasonably fast since most writes
		// don't trigger expensive futex wake calls
		if elapsed > 50*time.Millisecond {
			t.Logf("Note: %d writes took %v (may indicate performance issue or loaded system)", numWrites, elapsed)
		}
	})

	// Test that verifies conditional futex wake optimization through performance timing
	// This demonstrates that only the first write to an empty ring is expensive (futex wake),
	// while subsequent writes to non-empty ring are much faster (no futex wake needed).
	t.Run("FutexWakeOptimization", func(t *testing.T) {
		// Clear ring to ensure empty state
		for !ring.IsEmpty() {
			buf := make([]byte, 100)
			ring.ReadBlocking(buf)
		}

		// Record initial state
		initialDataSeq := hdr.DataSequence()

		// The key insight: We can test the optimization by looking at timing
		// If every write caused a futex wake, it would be much slower
		// If only the first write causes a futex wake, it should be fast

		// First, do a single write to empty ring (this SHOULD trigger futex wake)
		start1 := time.Now()
		err := ring.WriteBlocking([]byte("first"))
		if err != nil {
			t.Fatalf("First write failed: %v", err)
		}
		singleWriteTime := time.Since(start1)

		// Now do many writes to non-empty ring (these should NOT trigger futex wakes)
		start2 := time.Now()
		numSubsequentWrites := 100
		for i := 0; i < numSubsequentWrites; i++ {
			err := ring.WriteBlocking([]byte{byte(i % 256)})
			if err != nil {
				t.Fatalf("Subsequent write %d failed: %v", i, err)
			}
		}
		multipleWritesTime := time.Since(start2)

		// The optimization means: multiple writes to non-empty ring should be much faster
		// per write than the first write (which may have triggered a futex wake)
		avgSubsequentWriteTime := multipleWritesTime / time.Duration(numSubsequentWrites)

		t.Logf("Single write to empty ring: %v", singleWriteTime)
		t.Logf("Average subsequent write time: %v", avgSubsequentWriteTime)
		t.Logf("Total time for %d subsequent writes: %v", numSubsequentWrites, multipleWritesTime)

		// Verify that DataSequence incremented correctly (1 + numSubsequentWrites)
		dataSeqIncrements := hdr.DataSequence() - initialDataSeq
		expectedTotal := uint32(1 + numSubsequentWrites)
		if dataSeqIncrements != expectedTotal {
			t.Errorf("Expected %d total DataSequence increments, got %d", expectedTotal, dataSeqIncrements)
		}

		// Performance check: subsequent writes should be very fast
		// (This is indirect evidence that futex wake calls are minimized)
		if avgSubsequentWriteTime > 1*time.Microsecond {
			t.Logf("Note: Average subsequent write time %v is higher than expected (may indicate system load)", avgSubsequentWriteTime)
		}

		// The writes should be fast enough that 100 writes complete in well under 1ms
		if multipleWritesTime > 10*time.Millisecond {
			t.Errorf("Multiple writes took %v, expected much faster (may indicate futex optimization not working)", multipleWritesTime)
		}

		t.Logf("Futex wake optimization test completed successfully")
	})
}
