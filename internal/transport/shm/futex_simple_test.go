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
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestFutexSimpleTimeout(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Futex tests only supported on Linux")
	}

	// Create a fresh memory location
	data := make([]uint32, 1)
	addr := &data[0]

	// Set a known value
	atomic.StoreUint32(addr, 42)

	// Wait for this value with a timeout - should ultimately timeout since no one will wake us.
	totalTimeout := 100 * time.Millisecond
	start := time.Now()
	deadline := start.Add(totalTimeout)
	var total time.Duration
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		err := futexWaitTimeout(addr, 42, remaining.Nanoseconds())
		total = time.Since(start)

		if err == nil {
			// Likely interrupted (EINTR) or spurious wake; retry with the remaining timeout.
			continue
		}

		if errors.Is(err, ErrFutexTimeout) {
			break
		}

		t.Fatalf("futexWaitTimeout returned unexpected error: %v", err)
	}

	if total < 80*time.Millisecond || total > 200*time.Millisecond {
		t.Errorf("Timeout took %v, expected ~100ms", total)
	} else {
		t.Logf("futexWaitTimeout timed out after %v", total)
	}
}

func TestFutexWakeFromAnotherGoroutine(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Futex tests only supported on Linux")
	}

	data := make([]uint32, 1)
	addr := &data[0]

	atomic.StoreUint32(addr, 100)

	done := make(chan struct{})

	// Start a goroutine that will wake us after a short delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		atomic.StoreUint32(addr, 101) // Change the value
		futexWake(addr, 1)            // Wake the waiter
		close(done)
	}()

	// Wait on the original value, retrying in the face of EINTR.
	start := time.Now()
	deadline := start.Add(1 * time.Second)
	var elapsed time.Duration
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatal("futexWaitTimeout did not wake before deadline")
		}

		err := futexWaitTimeout(addr, 100, remaining.Nanoseconds())
		elapsed = time.Since(start)

		current := atomic.LoadUint32(addr)
		if current != 100 {
			if err != nil && !errors.Is(err, ErrFutexTimeout) {
				t.Fatalf("Unexpected error after wake: %v", err)
			}
			break
		}

		if err == nil {
			// Spurious wake or EINTR; continue waiting.
			continue
		}

		if errors.Is(err, ErrFutexTimeout) {
			t.Fatal("futexWaitTimeout reached timeout before wake occurred")
		}

		t.Fatalf("futexWaitTimeout returned unexpected error: %v", err)
	}

	// Wait for the goroutine to complete to avoid leaking it.
	<-done

	if elapsed < 30*time.Millisecond || elapsed > 150*time.Millisecond {
		t.Errorf("Wake took %v, expected around 50ms", elapsed)
	} else {
		t.Logf("futexWaitTimeout woke up after %v (expected ~50ms)", elapsed)
	}
}
