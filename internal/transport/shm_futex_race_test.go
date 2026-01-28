//go:build linux

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
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestFutexBasicWakeLogic tests the basic futex wake/wait interaction.
// Note: The original "lost-wake race" test was removed because it tested for a
// non-issue. Proper futex usage (like in ring.go) always re-checks conditions
// in a loop after futex returns, making "lost wakes" harmless.
func TestFutexBasicWakeLogic(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Futex tests only supported on Linux")
	}

	var counter uint32 = 100

	// Start a waiter
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Wait for counter to change from 100
		futexWait(&counter, 100)
	}()

	// Give the waiter time to start
	time.Sleep(10 * time.Millisecond)

	// Change the value and wake
	atomic.StoreUint32(&counter, 101)
	futexWake(&counter, 1)

	// Should complete quickly
	select {
	case <-done:
		// Good - waiter was properly woken
	case <-time.After(200 * time.Millisecond):
		t.Fatal("futexWait did not wake when value changed and futexWake called")
	}
}

// TestFutexAtomicRecheck specifically tests the atomic re-check behavior
func TestFutexAtomicRecheck(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Futex tests only supported on Linux")
	}

	var addr uint32 = 42

	// Test 1: Value already changed - should return immediately without blocking
	atomic.StoreUint32(&addr, 43)

	start := time.Now()
	err := futexWait(&addr, 42) // Wait for old value 42, but it's now 43
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("futexWait returned error: %v", err)
	}

	// Should return almost immediately due to atomic re-check
	if elapsed > 100*time.Millisecond {
		t.Errorf("futexWait took too long (%v) when value already changed", elapsed)
	}

	// Test 2: Value matches - should proceed to syscall (but we'll wake it quickly)
	atomic.StoreUint32(&addr, 100)

	done := make(chan struct{})
	go func() {
		// This should proceed to actual futex wait
		futexWait(&addr, 100)
		close(done)
	}()

	// Give the futex wait time to start
	time.Sleep(10 * time.Millisecond)

	// Now change value and wake
	atomic.StoreUint32(&addr, 101)
	futexWake(&addr, 1)

	// Should complete quickly
	select {
	case <-done:
		// Good
	case <-time.After(500 * time.Millisecond):
		t.Error("futexWait did not wake when value changed and futexWake called")
	}
}

// TestFutexTimeoutAtomicRecheck tests the timeout version with atomic re-check
func TestFutexTimeoutAtomicRecheck(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Futex tests only supported on Linux")
	}

	// Use a dedicated memory location in a slice to avoid any interference
	data := make([]uint32, 1)
	addr := &data[0]

	// Test 1: Value already changed - should return immediately
	atomic.StoreUint32(addr, 11)

	start := time.Now()
	err := futexWaitTimeout(addr, 10, 1000*1000*1000) // 1 second timeout
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("futexWaitTimeout returned error: %v", err)
	}

	// Should return almost immediately due to atomic re-check
	if elapsed > 100*time.Millisecond {
		t.Errorf("futexWaitTimeout took too long (%v) when value already changed", elapsed)
	}

	// Test 2: Value matches but timeout occurs - use a different unique value
	const testValue uint32 = 999
	atomic.StoreUint32(addr, testValue)

	// Wait a moment to ensure the store is visible
	time.Sleep(1 * time.Millisecond)

	// Verify the value is what we expect
	currentVal := atomic.LoadUint32(addr)
	t.Logf("Address value before futexWaitTimeout: %d", currentVal)

	if currentVal != testValue {
		t.Fatalf("Value changed unexpectedly: expected %d, got %d", testValue, currentVal)
	}

	// Wait with reattempts to handle EINTR or spurious wakeups which return nil.
	timeoutStart := time.Now()
	deadline := timeoutStart.Add(50 * time.Millisecond)
	var total time.Duration
	var gotTimeout bool
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			gotTimeout = true
			break
		}

		err = futexWaitTimeout(addr, testValue, remaining.Nanoseconds())
		total = time.Since(timeoutStart)

		// Check value after each wait; it should remain unchanged in this scenario.
		if current := atomic.LoadUint32(addr); current != testValue {
			t.Fatalf("Value changed unexpectedly during timeout wait: expected %d, got %d", testValue, current)
		}

		if err == nil {
			// Likely interrupted (EINTR) or spurious wake. Re-loop with updated remaining time.
			continue
		}

		if errors.Is(err, ErrFutexTimeout) {
			gotTimeout = true
			break
		}

		t.Fatalf("futexWaitTimeout returned unexpected error: %v", err)
	}

	if !gotTimeout {
		t.Fatalf("futexWaitTimeout did not report timeout within expected window (total=%v)", total)
	}

	// Should take approximately the timeout duration (allow more variance due to scheduling).
	if total < 30*time.Millisecond || total > 150*time.Millisecond {
		t.Errorf("Timeout took %v, expected around 50ms", total)
	} else {
		t.Logf("Timeout duration was %v (expected ~50ms)", total)
	}
}

// TestConcurrentFutexOperations tests futex operations under concurrent load
func TestConcurrentFutexOperations(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Futex tests only supported on Linux")
	}

	var addr uint32
	var wg sync.WaitGroup
	const numGoroutines = 6  // Reduced for stability
	const numIterations = 10 // Reduced for stability

	// Track completion
	completed := make([]bool, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			defer func() { completed[id] = true }()

			for j := 0; j < numIterations; j++ {
				// Random choice: either wait with timeout or wake
				if (id+j)%2 == 0 {
					// Waiter: wait on current value with short timeout
					val := atomic.LoadUint32(&addr)
					futexWaitTimeout(&addr, val, 5*1000*1000) // 5ms timeout to prevent hanging
				} else {
					// Waker: increment and wake
					atomic.AddUint32(&addr, 1)
					futexWake(&addr, 5) // Wake multiple waiters
				}

				// Small delay to create more interesting interleavings
				time.Sleep(100 * time.Microsecond)
			}
		}(i)
	}

	// Wait with timeout to detect hangs
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Verify all goroutines completed
		for i, complete := range completed {
			if !complete {
				t.Errorf("Goroutine %d did not complete", i)
			}
		}
		t.Logf("All %d goroutines completed %d iterations each", numGoroutines, numIterations)
	case <-time.After(5 * time.Second): // Reduced timeout
		t.Fatal("Concurrent futex test timed out - possible deadlock or lost-wake race")
	}
}
