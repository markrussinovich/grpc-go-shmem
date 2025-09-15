//go:build linux && (amd64 || arm64)

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
	"fmt"
	"syscall"
	"unsafe"
)

// Linux futex constants
const (
	FUTEX_WAIT_PRIVATE = 128 // FUTEX_WAIT | FUTEX_PRIVATE_FLAG
	FUTEX_WAKE_PRIVATE = 129 // FUTEX_WAKE | FUTEX_PRIVATE_FLAG
)

// futexWait waits for the value at addr to change from val.
// It returns when either:
//   - The value at addr is no longer equal to val
//   - Another thread calls futexWake on the same address
//   - The system call is interrupted
//
// This function should only be called when the logical condition is unmet
// and *addr == val. Always re-check the condition after this returns due
// to possible spurious wakeups.
func futexWait(addr *uint32, val uint32) error {
	// Use syscall.RawSyscall6 for the futex system call
	// syscall number, uaddr, futex_op, val, timeout, uaddr2, val3
	r1, _, errno := syscall.RawSyscall6(
		syscall.SYS_FUTEX,
		uintptr(unsafe.Pointer(addr)), // uaddr - address to wait on
		FUTEX_WAIT_PRIVATE,            // futex_op - wait operation with private flag
		uintptr(val),                  // val - expected value
		0,                             // timeout - infinite (NULL)
		0,                             // uaddr2 - unused
		0,                             // val3 - unused
	)

	if errno != 0 {
		// EAGAIN means the value didn't match - this is expected and not an error
		if errno == syscall.EAGAIN {
			return nil
		}
		// EINTR means interrupted by signal - also not a real error for our purposes
		if errno == syscall.EINTR {
			return nil
		}
		return fmt.Errorf("futex wait failed: %w", errno)
	}

	// r1 == 0 means successful wait and wake
	_ = r1
	return nil
}

// futexWake wakes up to n threads waiting on addr.
// Returns the number of threads actually woken up.
func futexWake(addr *uint32, n int) (int, error) {
	// Use syscall.RawSyscall6 for the futex system call
	r1, _, errno := syscall.RawSyscall6(
		syscall.SYS_FUTEX,
		uintptr(unsafe.Pointer(addr)), // uaddr - address to wake on
		FUTEX_WAKE_PRIVATE,            // futex_op - wake operation with private flag
		uintptr(n),                    // val - number of threads to wake
		0,                             // timeout - unused for wake
		0,                             // uaddr2 - unused
		0,                             // val3 - unused
	)

	if errno != 0 {
		return 0, fmt.Errorf("futex wake failed: %w", errno)
	}

	// r1 contains the number of threads woken
	return int(r1), nil
}
