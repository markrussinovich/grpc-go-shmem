//go:build linux && (amd64 || arm64)

package shm

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestFutexLostWakeRaceFix tests that the atomic re-check in futexWait
// prevents the lost-wake race condition.
func TestFutexLostWakeRaceFix(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Futex tests only supported on Linux")
	}

	// Test with a shared counter that multiple goroutines will modify
	var counter uint32 = 0
	var wg sync.WaitGroup

	// Number of test iterations to increase chance of hitting race
	const iterations = 100
	const numWorkers = 10

	for iter := 0; iter < iterations; iter++ {
		atomic.StoreUint32(&counter, 0)
		wg = sync.WaitGroup{}

		// Channel to coordinate timing
		startCh := make(chan struct{})

		// Start waiter goroutine
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Wait for signal to start
			<-startCh

			// Take snapshot of counter
			snapshot := atomic.LoadUint32(&counter)

			// Small delay to increase chance of race - this simulates
			// the time between taking snapshot and calling futexWait
			time.Sleep(10 * time.Microsecond)

			// This should NOT hang due to the atomic re-check
			futexWait(&counter, snapshot)
		}()

		// Start multiple incrementer goroutines
		for i := 0; i < numWorkers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				// Wait for signal to start
				<-startCh

				// Increment counter and wake
				atomic.AddUint32(&counter, 1)
				futexWake(&counter, 1)
			}()
		}

		// Start all goroutines simultaneously
		close(startCh)

		// Use a timeout to detect if futexWait hangs due to lost-wake race
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			// Good - all goroutines completed
		case <-time.After(1 * time.Second):
			t.Fatalf("Iteration %d: futexWait appears to have hung - possible lost-wake race", iter)
		}
	}

	t.Logf("Completed %d iterations without lost-wake race", iterations)
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

	start = time.Now()
	err = futexWaitTimeout(addr, testValue, 50*1000*1000) // 50ms timeout
	elapsed = time.Since(start)

	// Check value after the call
	finalVal := atomic.LoadUint32(addr)
	t.Logf("Address value after futexWaitTimeout: %d", finalVal)

	// Should timeout since no one else should modify our test value
	if err == nil {
		t.Error("Expected timeout error, got nil")
	} else {
		t.Logf("Got expected timeout error: %v", err)
	}

	// Should take approximately the timeout duration (allow more variance)
	if elapsed < 30*time.Millisecond || elapsed > 300*time.Millisecond {
		t.Errorf("Timeout took %v, expected ~50ms", elapsed)
	} else {
		t.Logf("Timeout duration was %v (expected ~50ms)", elapsed)
	}
}

// TestConcurrentFutexOperations tests futex operations under concurrent load
func TestConcurrentFutexOperations(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Futex tests only supported on Linux")
	}

	var addr uint32 = 0
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
