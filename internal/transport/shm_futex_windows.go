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
	waitCounts              sync.Map
)

func trackWaiter(addr *uint32) func() {
	key := uintptr(unsafe.Pointer(addr))
	ptr, _ := waitCounts.LoadOrStore(key, new(int64))
	cnt := ptr.(*int64)
	atomic.AddInt64(cnt, 1)
	return func() {
		if atomic.AddInt64(cnt, -1) == 0 {
			waitCounts.Delete(key)
		}
	}
}

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
	r1, _, e1 := procWaitOnAddress.Call(uintptr(unsafe.Pointer(addr)), uintptr(unsafe.Pointer(&val)), unsafe.Sizeof(val), uintptr(timeoutMs))
	if errno, ok := e1.(windows.Errno); ok && errno == windows.ERROR_SUCCESS {
		return nil
	}
	if r1 == 0 {
		if errno, ok := e1.(windows.Errno); ok {
			if errno == windows.ERROR_TIMEOUT {
				return ErrFutexTimeout
			}
			return fmt.Errorf("WaitOnAddress: %w", errno)
		}
		return fmt.Errorf("WaitOnAddress: %v", e1)
	}
	return nil
}

func wakeByAddress(addr *uint32, wakeAll bool) error {
	if err := ensureWaitProcs(); err != nil {
		return fmt.Errorf("%w: %v", ErrFutexNotSupported, err)
	}
	proc := procWakeByAddressSingle
	if wakeAll {
		proc = procWakeByAddressAll
	}
	r1, _, e1 := proc.Call(uintptr(unsafe.Pointer(addr)))
	if errno, ok := e1.(windows.Errno); ok && errno == windows.ERROR_SUCCESS {
		return nil
	}
	if r1 == 0 {
		if errno, ok := e1.(windows.Errno); ok {
			return fmt.Errorf("WakeByAddress: %w", errno)
		}
		return fmt.Errorf("WakeByAddress: %v", e1)
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
	release := trackWaiter(addr)
	defer release()
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
	release := trackWaiter(addr)
	defer release()
	return waitOnAddress(addr, val, timeoutMs)
}

// futexWake wakes up to n waiters on addr using Windows wake primitives.
func futexWake(addr *uint32, n int) (int, error) {
	if n <= 0 {
		return 0, nil
	}
	key := uintptr(unsafe.Pointer(addr))
	if ptr, ok := waitCounts.Load(key); ok {
		waiters := atomic.LoadInt64(ptr.(*int64))
		if waiters == 0 {
			return 0, nil
		}
	}
	woken := 0
	if ptr, ok := waitCounts.Load(key); ok {
		waiters := atomic.LoadInt64(ptr.(*int64))
		if waiters == 0 {
			return 0, nil
		}
		woken = n
		if int64(woken) > waiters {
			woken = int(waiters)
		}
	}
	wakeAll := n > 1
	if err := wakeByAddress(addr, wakeAll); err != nil {
		return 0, err
	}
	return woken, nil
}
