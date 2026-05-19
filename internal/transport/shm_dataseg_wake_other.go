//go:build !linux

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

// Non-Linux stub for the per-data-segment per-direction eventfd
// wake primitive. Windows / macOS / other platforms keep using the
// existing per-address eventfd registry / futex / WaitOnAddress path.
//
// All exported symbols match the Linux build so call sites in
// shm_segment.go and ring.go compile identically.

package transport

import "time"

const shmDataSegWakeEnabled = false

type shmDataSegWaker struct{}

func (*shmDataSegWaker) Wake()                     {}
func (*shmDataSegWaker) Wait(time.Duration) error                              { return nil }
func (*shmDataSegWaker) WaitForChange(*uint32, uint32, time.Duration) error    { return nil }
func (*shmDataSegWaker) RewakeLocal()                                          {}
func (*shmDataSegWaker) Close()                                                {}

func newShmDataSegWakerPair() (*shmDataSegWaker, *shmDataSegWaker, error) {
	return nil, nil, nil
}

func stashShmDataSegWakerForOpener(string, *shmDataSegWaker) {}
func claimShmDataSegWakerForOpener(string) *shmDataSegWaker  { return nil }
func dropShmDataSegWakerStash(string)                        {}

// ShmDataSegWakeCounters mirrors the Linux struct so cross-platform
// callers (bench harness) compile.
type ShmDataSegWakeCounters struct {
	WakeCallsTotal    uint64
	WakeSyscalls      uint64
	WaitCallsTotal    uint64
	WaitSyscalls      uint64
	WaitReturnNil     uint64
	WaitReturnTimeout uint64
	WaitReturnClosed  uint64
	WaitReturnEOF     uint64
	WaitReturnOther   uint64
	RewakeLocal       uint64
	FanOutBailout     uint64
}

func (a ShmDataSegWakeCounters) Sub(_ ShmDataSegWakeCounters) ShmDataSegWakeCounters {
	return a
}

func LoadShmDataSegWakeCounters() ShmDataSegWakeCounters {
	return ShmDataSegWakeCounters{}
}
