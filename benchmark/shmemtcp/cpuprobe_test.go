//go:build linux || windows

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

package main

import (
	"os"
	"runtime/metrics"
	"testing"
	"time"
)

// CPU usage probe for the SHM / TCP / UDS bench harness.
//
// Reviewer (Doug) asked for CPU utilisation alongside throughput numbers
// so latency / throughput wins aren't read in isolation. A transport
// that's 2x faster but burns 4x the CPU is a different proposition
// from one that's 2x faster on the same CPU budget.
//
// Implementation: read Go's `runtime/metrics` process-CPU counters
// before and after the timed loop. The Go runtime exposes
// `/cpu/classes/total:cpu-seconds` (since Go 1.20) which sums user +
// system CPU charged to the process across all OS threads. Two
// metrics are reported per benchmark iteration when the
// SHM_BENCH_CPU=1 env var is set:
//
//   - cpu-ns/op:  total process CPU time per iteration, in nanoseconds.
//                 An RPC that takes 14us wall time but burns 50us of CPU
//                 (because both client and server goroutines run in
//                 parallel on separate cores) would report ~50000.
//
//   - %cpu:       cpu-time / wall-time as a percentage. 100% = one core
//                 fully busy on average; 400% = four cores fully busy.
//
// We don't capture user vs sys split because the boundary is fuzzy on
// Windows; total is what matters for the "how much machine does this
// burn" question. For a deeper breakdown reviewers can re-run a
// single bench under `/usr/bin/time -v` or perf stat.
//
// The probe is no-op unless SHM_BENCH_CPU=1 to keep default bench
// output unchanged (and to avoid the small overhead of reading
// metrics on every iteration of a fast bench).

const cpuMetricTotal = "/cpu/classes/total:cpu-seconds"

// readProcessCPU returns the cumulative process CPU time (user + sys,
// all OS threads). Zero if the metric is unavailable (older Go).
func readProcessCPU() time.Duration {
	samples := []metrics.Sample{{Name: cpuMetricTotal}}
	metrics.Read(samples)
	if samples[0].Value.Kind() != metrics.KindFloat64 {
		return 0
	}
	return time.Duration(samples[0].Value.Float64() * float64(time.Second))
}

// cpuProbeEnabled reports whether SHM_BENCH_CPU=1 in the environment.
// Read once at package init to avoid per-call getenv overhead.
var cpuProbeEnabled = os.Getenv("SHM_BENCH_CPU") == "1"

// startCPUProbe captures the start-of-measurement CPU snapshot. The
// returned closure, when invoked, computes the delta and reports
// cpu-ns/op and %cpu via b.ReportMetric.
//
// Callers should invoke startCPUProbe AFTER b.ResetTimer() (so the
// snapshot doesn't include warm-up CPU) and call the returned closure
// AFTER b.StopTimer() (so the delta covers exactly the timed loop).
// Both are no-ops when cpuProbeEnabled is false.
func startCPUProbe(b *testing.B) func() {
	if !cpuProbeEnabled {
		return func() {}
	}
	startCPU := readProcessCPU()
	startWall := time.Now()
	return func() {
		endWall := time.Now()
		endCPU := readProcessCPU()
		dCPU := endCPU - startCPU
		dWall := endWall.Sub(startWall)
		if b.N <= 0 || dWall <= 0 {
			return
		}
		b.ReportMetric(float64(dCPU)/float64(b.N), "cpu-ns/op")
		b.ReportMetric(100*float64(dCPU)/float64(dWall), "%cpu")
	}
}
