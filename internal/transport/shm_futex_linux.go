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
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"syscall"
	"unsafe"
)

var futexDebugEnabled = os.Getenv("GRPC_SHM_FUTEX_DEBUG") != ""

func futexLogf(format string, args ...any) {
	if !futexDebugEnabled {
		return
	}
	log.Printf(format, args...)
}

// Linux futex constants
const (
	futexOpWait        = 0   // FUTEX_WAIT (shared, for cross-process)
	futexOpWake        = 1   // FUTEX_WAKE (shared, for cross-process)
	futexWaitPrivate   = 128 // FUTEX_WAIT | FUTEX_PRIVATE_FLAG
	futexWakePrivate   = 129 // FUTEX_WAKE | FUTEX_PRIVATE_FLAG
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
	// Critical: Re-check the value atomically before entering the syscall
	// This prevents the lost-wake race where another thread increments
	// the sequence and wakes us between our snapshot and futex entry
	if atomic.LoadUint32(addr) != val {
		return nil // Value already changed, no need to wait
	}

	futexLogf("[FUTEX] futexWait: addr=%p, val=%d", addr, val)

	// Use syscall.Syscall6 instead of RawSyscall6 to allow Go runtime scheduling
	// This is important for cross-process futex
	r1, _, errno := syscall.Syscall6(
		syscall.SYS_FUTEX,
		uintptr(unsafe.Pointer(addr)), // uaddr - address to wait on
		futexOpWait,                   // futex_op - wait operation (shared, for cross-process)
		uintptr(val),                  // val - expected value
		0,                             // timeout - infinite (NULL)
		0,                             // uaddr2 - unused
		0,                             // val3 - unused
	)

	futexLogf("[FUTEX] futexWait returned: r1=%d, errno=%d", r1, errno)

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

// futexWaitTimeout waits on addr until the value changes from val or timeout elapses.
// timeout is specified in nanoseconds. Returns an error if the wait times out.
//
// This function should only be called when the logical condition is unmet
// and *addr == val. Always re-check the condition after this returns due
// to possible spurious wakeups.
func futexWaitTimeout(addr *uint32, val uint32, timeoutNs int64) error {
	if timeoutNs <= 0 {
		return futexWait(addr, val) // No timeout, use infinite wait
	}

	// Critical: Re-check the value atomically before entering the syscall
	// This prevents the lost-wake race where another thread increments
	// the sequence and wakes us between our snapshot and futex entry
	currentVal := atomic.LoadUint32(addr)
	if currentVal != val {
		shmDebugf("FUTEX_PRECHECK_SKIP: expected=%d, current=%d, addr=%p", val, currentVal, addr)
		return nil // Value already changed, no need to wait
	}

	shmDebugf("FUTEX_ENTERING_SYSCALL: expected=%d, current=%d, addr=%p, timeout=%dns", val, currentVal, addr, timeoutNs)

	// Convert nanoseconds to timespec using the standard library helper
	// which handles architecture-specific field types (int64 on amd64, int32 on 386)
	ts := syscall.NsecToTimespec(timeoutNs)

	// Use syscall.Syscall6 instead of RawSyscall6 for cross-process futex
	r1, _, errno := syscall.Syscall6(
		syscall.SYS_FUTEX,
		uintptr(unsafe.Pointer(addr)), // uaddr - address to wait on
		futexOpWait,                   // futex_op - wait operation (shared, for cross-process)
		uintptr(val),                  // val - expected value
		uintptr(unsafe.Pointer(&ts)),  // timeout - timespec pointer
		0,                             // uaddr2 - unused
		0,                             // val3 - unused
	)

	if errno != 0 {
		// EAGAIN means the value didn't match - not an error
		if errno == syscall.EAGAIN {
			return nil
		}
		// EINTR means interrupted by signal - not an error
		if errno == syscall.EINTR {
			return nil
		}
		// ETIMEDOUT means the wait timed out
		if errno == syscall.ETIMEDOUT {
			return ErrFutexTimeout
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
	if futexDebugEnabled {
		futexLogf("[FUTEX] futexWake: addr=%p, n=%d, current_val=%d", addr, n, atomic.LoadUint32(addr))
	}

	// Use syscall.Syscall6 instead of RawSyscall6 for cross-process futex
	r1, _, errno := syscall.Syscall6(
		syscall.SYS_FUTEX,
		uintptr(unsafe.Pointer(addr)), // uaddr - address to wake on
		futexOpWake,                   // futex_op - wake operation (shared, for cross-process)
		uintptr(n),                    // val - number of threads to wake
		0,                             // timeout - unused for wake
		0,                             // uaddr2 - unused
		0,                             // val3 - unused
	)

	futexLogf("[FUTEX] futexWake returned: r1=%d (threads woken), errno=%d", r1, errno)

	if errno != 0 {
		return 0, fmt.Errorf("futex wake failed: %w", errno)
	}

	// r1 contains the number of threads woken
	return int(r1), nil
}
