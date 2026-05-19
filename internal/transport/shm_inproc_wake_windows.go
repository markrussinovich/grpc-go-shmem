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

package transport

import (
	"time"
	"unsafe"
)

// Windows stub for the same-process socketpair wake mechanism.
//
// Windows does not have a direct socketpair() equivalent in
// golang.org/x/sys/windows. The intended cross-platform mechanism for
// the production implementation is AF_UNIX SOCK_STREAM (supported on
// Windows 10 1803+) with `WSADuplicateSocketW` for cross-process
// handle transfer, but we haven't wired that up yet. Until then,
// SHM_INPROC_WAKE has no effect on Windows: the standard
// WaitOnAddress / WakeByAddressSingle path is used (see
// shm_event_windows.go).
//
// All exported symbols match the Linux build so call sites compile
// identically.

const shmInprocWakeEnabled = false

type shmInprocWaker struct{}

// Wait is unreachable on Windows because getInprocWaker always returns
// nil; the method exists so call sites in ring.go compile identically
// on both platforms.
func (*shmInprocWaker) Wait(time.Duration) error { return nil }

// Wake is unreachable on Windows for the same reason as Wait above.
func (*shmInprocWaker) Wake() {}

// Close is unreachable on Windows for the same reason as Wait above.
func (*shmInprocWaker) Close() {}

func getInprocWaker(string, unsafe.Pointer, *uint32) *shmInprocWaker {
	return nil
}

func dropInprocWakersForSegment(string) {}

// ShmInprocWakeCounters is the same shape as the Linux version but
// always zero on Windows since the in-proc waker is not used here.
type ShmInprocWakeCounters struct {
	WakeCallsTotal      uint64
	WakeSyscalls        uint64
	WaitCallsTotal      uint64
	WaitSyscalls        uint64
	WaitSyscallReturned uint64
}

// Sub keeps API parity with the Linux build.
func (a ShmInprocWakeCounters) Sub(_ ShmInprocWakeCounters) ShmInprocWakeCounters {
	return a
}

// LoadShmInprocWakeCounters returns zero on Windows.
func LoadShmInprocWakeCounters() ShmInprocWakeCounters {
	return ShmInprocWakeCounters{}
}
