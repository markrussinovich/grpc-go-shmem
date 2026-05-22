//go:build windows

/*
 *
 * Copyright 2026 gRPC authors.
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

// Control-segment cross-process serialization lock (Windows).
//
// We use LockFileEx on a sibling lock file (analogous to Linux flock)
// rather than a named mutex. The named-mutex approach has a critical
// pitfall in Go: ownership of a Windows mutex is tracked by OS thread,
// but Go goroutines move between threads at the scheduler's
// discretion. A goroutine that acquired via WaitForSingleObject on
// thread A may be rescheduled onto thread B before calling
// ReleaseMutex; ReleaseMutex then fails with ERROR_NOT_OWNER and the
// mutex stays held forever. LockFileEx tracks ownership by HANDLE,
// not thread, so it is safe to acquire and release from different
// goroutines / threads.

package transport

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
)

// acquireControlLock acquires an exclusive byte-range lock on a
// sibling ".lock" file next to the named control segment, blocking
// until granted or ctx is cancelled. The returned release function
// MUST be invoked once the CONNECT/ACCEPT exchange completes.
func acquireControlLock(ctx context.Context, ctlName string) (func(), error) {
	lockPath := generateSegmentPath(ctlName) + ".lock"

	pathPtr, err := windows.UTF16PtrFromString(lockPath)
	if err != nil {
		return nil, fmt.Errorf("shm: control lock path %q: %w", lockPath, err)
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("shm: open control lock %q: %w", lockPath, err)
	}

	// Poll LockFileEx with LOCKFILE_FAIL_IMMEDIATELY so ctx
	// cancellation is honoured promptly. The lock covers the whole
	// file (offset 0, max range) -- we use it for the lock state
	// only, not for file content.
	const pollInterval = 5 * time.Millisecond
	for {
		var overlapped windows.Overlapped
		lockErr := windows.LockFileEx(
			handle,
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0,
			0xFFFFFFFF, 0xFFFFFFFF,
			&overlapped,
		)
		if lockErr == nil {
			break
		}
		// ERROR_LOCK_VIOLATION (33) is "the process cannot access
		// the file because another process has locked a portion of
		// the file". That is the expected "lock is held by someone
		// else" case; retry after the poll interval. ERROR_IO_PENDING
		// (997) can also surface with FAIL_IMMEDIATELY on some
		// kernel paths; treat it as transient.
		if errno, ok := lockErr.(windows.Errno); ok && (errno == windows.ERROR_LOCK_VIOLATION || errno == windows.ERROR_IO_PENDING) {
			select {
			case <-ctx.Done():
				windows.CloseHandle(handle)
				return nil, fmt.Errorf("shm: control lock %q: %w", lockPath, ctx.Err())
			case <-time.After(pollInterval):
				continue
			}
		}
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("shm: LockFileEx %q: %w", lockPath, lockErr)
	}

	var released atomic.Bool
	return func() {
		if released.Swap(true) {
			return
		}
		// Unlock covers the same range as the original lock.
		var ov windows.Overlapped
		_ = windows.UnlockFileEx(handle, 0, 0xFFFFFFFF, 0xFFFFFFFF, &ov)
		_ = windows.CloseHandle(handle)
	}, nil
}

// removeControlLock best-effort unlinks the lock file. Failures
// (e.g., file already absent or still held by another process) are
// silently ignored.
func removeControlLock(ctlName string) {
	lockPath := generateSegmentPath(ctlName) + ".lock"
	pathPtr, err := windows.UTF16PtrFromString(lockPath)
	if err != nil {
		return
	}
	_ = windows.DeleteFile(pathPtr)
}
