//go:build windows

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
	"math"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var futexDebugEnabled = os.Getenv("GRPC_SHM_FUTEX_DEBUG") != ""

var (
	modSync                 = windows.NewLazySystemDLL("api-ms-win-core-synch-l1-2-0.dll")
	procWaitOnAddress       = modSync.NewProc("WaitOnAddress")
	procWakeByAddressSingle = modSync.NewProc("WakeByAddressSingle")
	procWakeByAddressAll    = modSync.NewProc("WakeByAddressAll")
	waitProcsOnce           sync.Once
	waitProcsErr            error

	// Cached proc addresses for direct syscall, avoiding LazyProc.Call()
	// overhead (2 extra function calls + variadic slice allocations per call).
	waitOnAddressAddr       uintptr
	wakeByAddressSingleAddr uintptr
	wakeByAddressAllAddr    uintptr
)

func futexLogf(format string, args ...any) {
	if !futexDebugEnabled {
		return
	}
	log.Printf(format, args...)
}

func ensureWaitProcs() error {
	waitProcsOnce.Do(func() {
		if err := modSync.Load(); err != nil {
			waitProcsErr = err
			return
		}
		for _, p := range []*windows.LazyProc{procWaitOnAddress, procWakeByAddressSingle, procWakeByAddressAll} {
			if err := p.Find(); err != nil {
				waitProcsErr = err
				return
			}
		}
		// Cache raw addresses so we can call syscall.Syscall6 directly,
		// bypassing LazyProc.Call() → Proc.Call() → SyscallN() overhead.
		waitOnAddressAddr = procWaitOnAddress.Addr()
		wakeByAddressSingleAddr = procWakeByAddressSingle.Addr()
		wakeByAddressAllAddr = procWakeByAddressAll.Addr()
	})
	if waitProcsErr != nil {
		return fmt.Errorf("wait-on-address unavailable: %w", waitProcsErr)
	}
	return nil
}

func waitOnAddress(addr *uint32, val uint32, timeoutMs uint32) error {
	if err := ensureWaitProcs(); err != nil {
		return fmt.Errorf("%w: %v", ErrFutexNotSupported, err)
	}
	// Use syscall.Syscall6 directly with cached proc address to avoid
	// LazyProc.Call() overhead (mustFind check + 2 variadic forwardings).
	r1, _, e1 := syscall.Syscall6(waitOnAddressAddr, 4,
		uintptr(unsafe.Pointer(addr)),
		uintptr(unsafe.Pointer(&val)),
		unsafe.Sizeof(val),
		uintptr(timeoutMs),
		0, 0)
	if e1 == 0 {
		return nil
	}
	if r1 == 0 {
		if e1 == syscall.Errno(windows.ERROR_TIMEOUT) {
			return ErrFutexTimeout
		}
		return fmt.Errorf("WaitOnAddress: %w", e1)
	}
	return nil
}

func wakeByAddress(addr *uint32, wakeAll bool) error {
	if err := ensureWaitProcs(); err != nil {
		return fmt.Errorf("%w: %v", ErrFutexNotSupported, err)
	}
	// Use syscall.Syscall directly with cached proc address.
	fnAddr := wakeByAddressSingleAddr
	if wakeAll {
		fnAddr = wakeByAddressAllAddr
	}
	r1, _, e1 := syscall.Syscall(fnAddr, 1,
		uintptr(unsafe.Pointer(addr)),
		0, 0)
	if e1 == 0 {
		return nil
	}
	if r1 == 0 {
		return fmt.Errorf("WakeByAddress: %w", e1)
	}
	return nil
}

// futexWait waits for addr to change from val using Windows WaitOnAddress.
func futexWait(addr *uint32, val uint32) error {
	// Fast-path check to avoid lost wakes.
	if atomic.LoadUint32(addr) != val {
		return nil
	}

	futexLogf("[FUTEX] WaitOnAddress addr=%p val=%d", addr, val)
	return waitOnAddress(addr, val, windows.INFINITE)
}

// futexWaitTimeout waits on addr until it changes from val or the timeout elapses.
// timeoutNs is expressed in nanoseconds.
func futexWaitTimeout(addr *uint32, val uint32, timeoutNs int64) error {
	if timeoutNs <= 0 {
		return futexWait(addr, val)
	}

	// Convert to milliseconds, rounding up to avoid zero-length waits.
	d := time.Duration(timeoutNs)
	timeoutMs := uint32(1)
	if d > 0 {
		ms := (d + time.Millisecond - 1) / time.Millisecond
		if ms > 0 {
			if ms > math.MaxUint32-1 {
				timeoutMs = math.MaxUint32 - 1
			} else {
				timeoutMs = uint32(ms)
			}
		}
	}

	// Re-check before blocking to avoid lost wake.
	if atomic.LoadUint32(addr) != val {
		return nil
	}

	futexLogf("[FUTEX] WaitOnAddress addr=%p val=%d timeoutMs=%d", addr, val, timeoutMs)
	return waitOnAddress(addr, val, timeoutMs)
}

// futexWake wakes up to n waiters on addr using Windows wake primitives.
// Waiter tracking is done at the ring level (DataWaiters/SpaceWaiters/ContigWaiters)
// so we don't need redundant sync.Map tracking here.
// Returns 0 as the woken count since WakeByAddress doesn't report how many
// threads were actually woken.
func futexWake(addr *uint32, n int) (int, error) {
	if n <= 0 {
		return 0, nil
	}
	wakeAll := n > 1
	if err := wakeByAddress(addr, wakeAll); err != nil {
		return 0, err
	}
	return 0, nil
}
