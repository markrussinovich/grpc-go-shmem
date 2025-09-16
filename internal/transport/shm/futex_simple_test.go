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

	// Wait for this value with a timeout - should timeout since no one will wake us
	start := time.Now()
	err := futexWaitTimeout(addr, 42, 100*1000*1000) // 100ms timeout
	elapsed := time.Since(start)

	t.Logf("futexWaitTimeout took %v, error: %v", elapsed, err)

	// We expect either:
	// 1. A timeout error after ~100ms
	// 2. Or immediate return with nil if futex detected value mismatch
	if err != nil {
		// Should be a timeout error and take approximately 100ms
		if elapsed < 80*time.Millisecond || elapsed > 200*time.Millisecond {
			t.Errorf("Timeout took %v, expected ~100ms", elapsed)
		}
	} else {
		// If no error, should return very quickly
		if elapsed > 10*time.Millisecond {
			t.Errorf("Non-timeout return took %v, expected immediate", elapsed)
		}
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

	// Wait on the original value
	start := time.Now()
	err := futexWaitTimeout(addr, 100, 1000*1000*1000) // 1 second timeout
	elapsed := time.Since(start)

	// Wait for the goroutine to complete
	<-done

	// Should wake up after ~50ms with no error
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if elapsed < 40*time.Millisecond || elapsed > 100*time.Millisecond {
		t.Errorf("Wake took %v, expected ~50ms", elapsed)
	}

	t.Logf("futexWaitTimeout woke up after %v (expected ~50ms)", elapsed)
}
