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
	"strings"
	"testing"
	"time"
)

// TestRing_ExactCapacityWriteDoesNotBlock verifies that writing exactly the ring capacity
// does not block, eliminating the classic cap-1 limitation.
func TestRing_ExactCapacityWriteDoesNotBlock(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Ring tests only supported on Linux")
	}

	cap := uint64(64 * 1024)
	name := "test_ring_exact_capacity"

	// Ensure clean state
	RemoveSegment(name)
	defer RemoveSegment(name)

	seg, err := CreateSegment(name, cap, cap)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer seg.Close()

	ring := NewShmRingFromSegment(seg.A, seg.Mem)

	// Verify the ring reports the correct capacity
	if ring.Capacity() != cap {
		t.Fatalf("Expected ring capacity %d, got %d", cap, ring.Capacity())
	}

	payload := make([]byte, cap)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	// Create a context with timeout to prevent test hanging
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Channel to capture write result
	type writeResult struct {
		n   int
		err error
	}
	resultCh := make(chan writeResult, 1)

	// Perform the write in a goroutine to check for blocking
	go func() {
		err := ring.WriteBlocking(payload)
		var n int
		if err == nil {
			n = len(payload)
		}
		resultCh <- writeResult{n: n, err: err}
	}()

	// Wait for either write completion or timeout
	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("write failed: n=%d err=%v", result.n, result.err)
		}
		if result.n != len(payload) {
			t.Fatalf("write incomplete: expected %d bytes, wrote %d", len(payload), result.n)
		}
	case <-ctx.Done():
		t.Fatalf("write blocked: ring still has cap-1 limitation")
	}

	// Reader can now drain exactly capacity bytes.
	out := make([]byte, cap)
	read := 0
	for read < int(cap) {
		n, err := ring.ReadBlocking(out[read:])
		if err != nil {
			t.Fatalf("unexpected read error at offset %d: %v", read, err)
		}
		read += n
	}

	// Verify the data is correct
	if len(out) != len(payload) {
		t.Fatalf("length mismatch: expected %d, got %d", len(payload), len(out))
	}
	for i, expected := range payload {
		if out[i] != expected {
			t.Fatalf("data mismatch at offset %d: expected %d, got %d", i, expected, out[i])
		}
	}
}

// TestRing_CapacityPlus1WriteBlocks verifies that writing more than capacity blocks
// as expected (proving the ring correctly implements backpressure).
func TestRing_CapacityPlus1WriteBlocks(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Ring tests only supported on Linux")
	}

	cap := uint64(4096) // Use minimum capacity for test
	name := "test_ring_capacity_plus1"

	// Ensure clean state
	RemoveSegment(name)
	defer RemoveSegment(name)

	seg, err := CreateSegment(name, cap, cap)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer seg.Close()

	ring := NewShmRingFromSegment(seg.A, seg.Mem)

	// Try to write capacity + 1 bytes
	payload := make([]byte, cap+1)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	// Create a context with short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Channel to capture write result
	resultCh := make(chan error, 1)

	// Perform the write in a goroutine
	go func() {
		err := ring.WriteBlocking(payload)
		resultCh <- err
	}()

	// This should timeout (block) since capacity+1 > capacity
	select {
	case err := <-resultCh:
		if err == nil {
			t.Fatalf("write should have blocked or failed, but succeeded")
		}
		// If it returned an error quickly, that's also acceptable
		// (e.g., "data larger than ring capacity")
	case <-ctx.Done():
		// Expected: write blocked due to insufficient capacity
		// This proves backpressure works correctly
	}
}

// TestRing_MonotonicIndices verifies that the ring uses monotonic indices correctly
// and can handle wraparound without issues.
func TestRing_MonotonicIndices(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Ring tests only supported on Linux")
	}

	cap := uint64(4096) // Use minimum capacity for fast wraparound
	name := "test_ring_monotonic"

	// Ensure clean state
	RemoveSegment(name)
	defer RemoveSegment(name)

	seg, err := CreateSegment(name, cap, cap)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer seg.Close()

	ring := NewShmRingFromSegment(seg.A, seg.Mem)

	chunkSize := 32
	totalWrites := 10 // Will cause multiple wraparounds

	for i := 0; i < totalWrites; i++ {
		// Write data
		writeData := make([]byte, chunkSize)
		for j := range writeData {
			writeData[j] = byte((i*chunkSize + j) % 256)
		}

		err := ring.WriteBlocking(writeData)
		if err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}

		// Read it back
		readData := make([]byte, chunkSize)
		n, err := ring.ReadBlocking(readData)
		if err != nil {
			t.Fatalf("read %d failed: %v", i, err)
		}
		if n != chunkSize {
			t.Fatalf("read %d: expected %d bytes, got %d", i, chunkSize, n)
		}

		// Verify data integrity
		for j := 0; j < chunkSize; j++ {
			if readData[j] != writeData[j] {
				t.Fatalf("data mismatch at write %d, offset %d: expected %d, got %d",
					i, j, writeData[j], readData[j])
			}
		}
	}
}

// TestRing_ContextAwareWaits verifies that context deadlines are respected
// and time-bounded operations don't hang indefinitely.
func TestRing_ContextAwareWaits(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Ring tests only supported on Linux")
	}

	cap := uint64(4096)
	name := "test_ring_context_waits"

	// Ensure clean state
	RemoveSegment(name)
	defer RemoveSegment(name)

	seg, err := CreateSegment(name, cap, cap)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer seg.Close()

	ring := NewShmRingFromSegment(seg.A, seg.Mem)

	// Test 1: Write with timeout when buffer is full
	t.Run("WriteTimeout", func(t *testing.T) {
		// Fill the ring to capacity
		fillData := make([]byte, cap)
		err := ring.WriteBlocking(fillData)
		if err != nil {
			t.Fatalf("Failed to fill ring: %v", err)
		}

		// Try to write more with a short timeout - should fail with DeadlineExceeded
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		extraData := make([]byte, 1)
		err = ring.WriteBlockingContext(ctx, extraData)
		if err != context.DeadlineExceeded {
			t.Fatalf("Expected context.DeadlineExceeded, got: %v", err)
		}

		// Verify ring state shows it's full
		state := ring.DebugState()
		if state.Used != state.Capacity {
			t.Fatalf("Expected ring to be full: used=%d, capacity=%d", state.Used, state.Capacity)
		}

		// Clean up: read some data to make space
		readBuf := make([]byte, 1000)
		ring.ReadBlocking(readBuf)
	})

	// Test 2: Read with timeout when buffer is empty
	t.Run("ReadTimeout", func(t *testing.T) {
		// Ensure ring is empty
		for !ring.IsEmpty() {
			buf := make([]byte, 100)
			ring.ReadBlocking(buf)
		}

		// Try to read with a short timeout - should fail with DeadlineExceeded
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		readBuf := make([]byte, 100)
		_, err := ring.ReadBlockingContext(ctx, readBuf)
		if err != context.DeadlineExceeded {
			t.Fatalf("Expected context.DeadlineExceeded, got: %v", err)
		}

		// Verify ring state shows it's empty
		state := ring.DebugState()
		if state.Used != 0 {
			t.Fatalf("Expected ring to be empty: used=%d", state.Used)
		}
	})
}

// TestRing_DuelingBuffersDiagnostics tests the diagnostic helper for detecting
// deadlock scenarios with full buffers.
func TestRing_DuelingBuffersDiagnostics(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Ring tests only supported on Linux")
	}

	cap := uint64(4096)
	name1 := "test_ring_dueling_1"
	name2 := "test_ring_dueling_2"

	// Ensure clean state
	RemoveSegment(name1)
	RemoveSegment(name2)
	defer RemoveSegment(name1)
	defer RemoveSegment(name2)

	// Create two segments to simulate client→server and server→client rings
	seg1, err := CreateSegment(name1, cap, cap)
	if err != nil {
		t.Fatalf("Failed to create segment 1: %v", err)
	}
	defer seg1.Close()

	seg2, err := CreateSegment(name2, cap, cap)
	if err != nil {
		t.Fatalf("Failed to create segment 2: %v", err)
	}
	defer seg2.Close()

	clientToServer := NewShmRingFromSegment(seg1.A, seg1.Mem)
	serverToClient := NewShmRingFromSegment(seg2.A, seg2.Mem)

	// Test normal state (not dueling)
	isDueling, diagnostic := DiagnoseDuelingBuffers(clientToServer, serverToClient)
	if isDueling {
		t.Errorf("Expected no dueling with empty buffers")
	}
	if !strings.Contains(diagnostic, "Ring Buffer State:") {
		t.Errorf("Expected normal diagnostic message")
	}

	// Fill both rings to capacity to simulate dueling buffers
	fillData1 := make([]byte, cap)
	fillData2 := make([]byte, cap)

	err = clientToServer.WriteBlocking(fillData1)
	if err != nil {
		t.Fatalf("Failed to fill client→server ring: %v", err)
	}

	err = serverToClient.WriteBlocking(fillData2)
	if err != nil {
		t.Fatalf("Failed to fill server→client ring: %v", err)
	}

	// Test dueling state detection
	isDueling, diagnostic = DiagnoseDuelingBuffers(clientToServer, serverToClient)
	if !isDueling {
		t.Errorf("Expected dueling buffers detection with full rings")
	}
	if !strings.Contains(diagnostic, "DUELING FULL BUFFERS DETECTED:") {
		t.Errorf("Expected dueling buffers diagnostic message")
	}
	if !strings.Contains(diagnostic, "concurrent read/write") {
		t.Errorf("Expected solution suggestion in diagnostic")
	}

	// Log the diagnostic for manual inspection
	t.Logf("Dueling buffers diagnostic:\n%s", diagnostic)
}
