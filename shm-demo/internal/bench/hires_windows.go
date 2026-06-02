// Copyright 2026 gRPC SHM Demo authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build windows

package bench

import "golang.org/x/sys/windows"
import "unsafe"

// Windows' Go monotonic clock (time.Now) updates only on the system timer tick
// (~1-15ms), which is far too coarse to time sub-100µs RPC round-trips: most
// samples read as zero. We use QueryPerformanceCounter directly, which has
// sub-microsecond resolution, for per-sample latency timing.

var (
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procQueryPerfCounter = kernel32.NewProc("QueryPerformanceCounter")
	procQueryPerfFreq    = kernel32.NewProc("QueryPerformanceFrequency")
)

var qpcFreq int64

func init() {
	procQueryPerfFreq.Call(uintptr(unsafe.Pointer(&qpcFreq)))
	if qpcFreq == 0 {
		qpcFreq = 1 // avoid div-by-zero; tickNanos will be meaningless but safe
	}
}

// nowTick returns the current high-resolution performance counter value.
func nowTick() int64 {
	var c int64
	procQueryPerfCounter.Call(uintptr(unsafe.Pointer(&c)))
	return c
}

// tickNanos converts a counter delta to nanoseconds.
func tickNanos(delta int64) int64 {
	return delta * 1_000_000_000 / qpcFreq
}
