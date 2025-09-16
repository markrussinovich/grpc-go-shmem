/*
 * Copyright 2024 gRPC authors.
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

package shm

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestRingCloseUnblocksWaiters verifies that Close() properly unblocks waiting goroutines
func TestRingCloseUnblocksWaiters(t *testing.T) {
	// Setup cleanup - this test verifies no goroutine leaks
	t.Cleanup(func() {
		// Allow time for goroutines to complete
		time.Sleep(100 * time.Millisecond)
		runtime.GC()
	})

	// Create a ring buffer with minimum capacity (use unique timestamp)
	testName := fmt.Sprintf("grpc_shm_test-close-unblocks-%d", time.Now().UnixNano())
	seg, err := CreateSegment(testName, 4096, 4096)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer func() {
		seg.Close()
		RemoveSegment(testName)
	}()

	ringA := seg.A
	ring := NewShmRingFromSegment(ringA, seg.Mem)

	var wg sync.WaitGroup
	var readerUnblocked bool
	var readerMutex sync.Mutex

	// Test 1: ReadBlockingContext on empty ring should block, then unblock on Close()
	wg.Add(1)
	go func() {
		defer wg.Done()

		// This should block waiting for data (ring is empty)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		readBuf := make([]byte, 100)
		_, err := ring.ReadBlockingContext(ctx, readBuf)

		readerMutex.Lock()
		readerUnblocked = true
		readerMutex.Unlock()

		// Expected: error after close (ring closed or context timeout)
		if err == nil {
			t.Errorf("Expected reader to be unblocked with error, got success")
		}
	}()

	// Give goroutine time to start and block
	time.Sleep(200 * time.Millisecond)

	// Verify reader is still blocked
	readerMutex.Lock()
	if readerUnblocked {
		t.Error("Reader should still be blocked before close")
	}
	readerMutex.Unlock()

	// Close the ring - this should unblock the reader
	closeStart := time.Now()
	err = ring.Close()
	if err != nil {
		t.Fatalf("Ring close failed: %v", err)
	}

	// Wait for goroutine to complete with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Good - reader completed
		elapsed := time.Since(closeStart)
		if elapsed > 1*time.Second {
			t.Errorf("Reader took too long to unblock: %v", elapsed)
		}
		t.Logf("Reader unblocked in %v after close", elapsed)
	case <-time.After(2 * time.Second):
		t.Fatal("Reader did not unblock within 2 seconds after close - potential leak")
	}

	// Verify reader was unblocked
	readerMutex.Lock()
	if !readerUnblocked {
		t.Error("Reader was not unblocked by close")
	}
	readerMutex.Unlock()
}

// TestRingClosePreventsNewOperations verifies that operations fail after close
func TestRingClosePreventsNewOperations(t *testing.T) {
	testName := fmt.Sprintf("grpc_shm_test-close-prevents-ops-%d", time.Now().UnixNano())
	seg, err := CreateSegment(testName, 4096, 4096)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer func() {
		seg.Close()
		RemoveSegment(testName)
	}()

	ringA := seg.A
	ring := NewShmRingFromSegment(ringA, seg.Mem)

	// Close the ring
	err = ring.Close()
	if err != nil {
		t.Fatalf("Ring close failed: %v", err)
	}

	// Verify operations return quickly with appropriate errors after close
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = ring.WriteBlockingContext(ctx, []byte("should fail quickly"))
	elapsed := time.Since(start)

	if err == nil {
		t.Error("WriteBlockingContext should fail after close")
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("WriteBlockingContext took too long after close: %v", elapsed)
	}

	// Test read as well
	start = time.Now()
	readBuf := make([]byte, 100)
	_, err = ring.ReadBlockingContext(ctx, readBuf)
	elapsed = time.Since(start)

	if err == nil {
		t.Error("ReadBlockingContext should fail after close")
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("ReadBlockingContext took too long after close: %v", elapsed)
	}
}
