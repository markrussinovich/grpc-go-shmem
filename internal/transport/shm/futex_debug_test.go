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
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// testFutexDirect tests futex directly with manual syscalls
func TestFutexDirect(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Futex tests only supported on Linux")
	}

	data := make([]uint32, 1)
	addr := &data[0]
	atomic.StoreUint32(addr, 42)

	// Manual futex call with timeout
	var ts syscall.Timespec
	timeoutNs := int64(100 * 1000 * 1000) // 100ms
	ts.Sec = timeoutNs / 1e9
	ts.Nsec = timeoutNs % 1e9

	t.Logf("Direct futex test: addr=%p, val=42, timeout=%d.%09d", addr, ts.Sec, ts.Nsec)

	start := time.Now()
	r1, r2, errno := syscall.RawSyscall6(
		syscall.SYS_FUTEX,
		uintptr(unsafe.Pointer(addr)),
		FUTEX_WAIT_PRIVATE,
		42, // expected value
		uintptr(unsafe.Pointer(&ts)),
		0,
		0,
	)
	elapsed := time.Since(start)

	t.Logf("Direct futex result: r1=%d, r2=%d, errno=%v, elapsed=%v", r1, r2, errno, elapsed)

	if errno == syscall.ETIMEDOUT {
		t.Logf("Got expected timeout after %v", elapsed)
		if elapsed < 80*time.Millisecond || elapsed > 200*time.Millisecond {
			t.Errorf("Timeout duration %v not close to expected 100ms", elapsed)
		}
	} else if errno == syscall.EAGAIN {
		t.Logf("Got EAGAIN (value mismatch) after %v", elapsed)
		// This might happen if the value changed between store and futex
		currentVal := atomic.LoadUint32(addr)
		t.Logf("Current value: %d", currentVal)
	} else if errno == 0 {
		t.Logf("Futex returned success (woken up) after %v", elapsed)
	} else {
		t.Errorf("Unexpected errno: %v", errno)
	}
}

// testFutexWithWaker tests that futex can be woken by another goroutine
func TestFutexWithWaker(t *testing.T) {
	if !isLinuxPlatform() {
		t.Skip("Futex tests only supported on Linux")
	}

	data := make([]uint32, 1)
	addr := &data[0]
	atomic.StoreUint32(addr, 100)

	done := make(chan struct{})

	// Start waker goroutine
	go func() {
		time.Sleep(50 * time.Millisecond)
		t.Logf("Waker: changing value from 100 to 101")
		atomic.StoreUint32(addr, 101)

		// Wake the waiter
		r1, r2, errno := syscall.RawSyscall6(
			syscall.SYS_FUTEX,
			uintptr(unsafe.Pointer(addr)),
			FUTEX_WAKE_PRIVATE,
			1, // wake 1 waiter
			0,
			0,
			0,
		)
		t.Logf("Waker: futex_wake result: r1=%d, r2=%d, errno=%v", r1, r2, errno)
		close(done)
	}()

	// Wait on value 100
	t.Logf("Waiter: starting futex_wait on value 100")
	start := time.Now()
	r1, r2, errno := syscall.RawSyscall6(
		syscall.SYS_FUTEX,
		uintptr(unsafe.Pointer(addr)),
		FUTEX_WAIT_PRIVATE,
		100, // expected value
		0,   // no timeout
		0,
		0,
	)
	elapsed := time.Since(start)

	<-done // wait for waker to finish

	t.Logf("Waiter: futex_wait result: r1=%d, r2=%d, errno=%v, elapsed=%v", r1, r2, errno, elapsed)

	if errno == 0 {
		t.Logf("Successfully woken up after %v", elapsed)
		if elapsed < 40*time.Millisecond || elapsed > 100*time.Millisecond {
			t.Errorf("Wake time %v not close to expected 50ms", elapsed)
		}
	} else {
		t.Errorf("Unexpected result: errno=%v", errno)
	}
}
