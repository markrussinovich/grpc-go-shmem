//go:build linux || windows

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

package engine

import "sync/atomic"

// SHM spin-wait runtime configuration.
//
// Background: the SHM ring's reader and writer both block (eventually)
// on platform futex / WaitOnAddress when the ring is empty / full.
// Pure-futex behaviour pays ~25–50 µs per wake/wait cycle on Linux,
// which on a small-message ping-pong RPC turns into ~120 µs of round-
// trip latency — defeating SHM's main selling point of avoiding
// syscalls in the data path.
//
// An adaptive spin-then-block (gRFC G3 §"Adaptive Spin-Then-Block")
// can absorb the wake within a few microseconds of user-space work
// AND let the writer skip its FUTEX_WAKE syscall when the reader is
// still in its spin window (because dataWaiters == 0 there). When
// the spin succeeds, both sides pay ZERO syscalls — that's how SHM
// reaches sub-µs RPC latency.
//
// The cost: spin burns CPU. Reviewer (Doug) specifically asked us to
// measure the SHM transport without lock-spinning so the comparison
// against UDS / TCP isn't skewed by spinning CPU that those transports
// don't incur.
//
// Resolution: the spin is OFF BY DEFAULT (cutoffs initialised to 0,
// so the spin loop iterates zero times and falls straight through to
// the futex re-check + block path). Operators that want low-latency
// SHM call ConfigureShmSpinIterations(n) before constructing any
// transports — n is the maximum spin cutoff the per-ring adaptive
// algorithm can grow to. n == 0 keeps the default (no spin). n is
// clamped to the platform's spinIterationsLimit (see shm_spin_*.go).
//
// Tuning guidance (Linux):
//
//   - n = 0         — no spin (default). Matches UDS-style behaviour.
//     Best for latency-insensitive throughput-oriented
//     workloads and shared / oversubscribed hosts.
//   - n = 500–2000  — light spin (3.5–14 µs). Good middle ground:
//     catches most local hand-offs without burning a
//     measurable fraction of a core on idle rings.
//   - n = up to spinIterationsLimit — aggressive spin. Hot streaming
//     workloads on dedicated cores; idle CPU per ring
//     can be visible.
//
// The values are package-global rather than per-connection because
// they're effectively a deployment tuning choice (matches the pattern
// of ConfigureShmFlowControlForBench). A per-dial option can be added
// later if needed.
var (
	// shmSpinDefault is the initial dataSpinCutoff / spaceSpinCutoff
	// value used when constructing a new ShmRing.
	shmSpinDefault uint32

	// shmSpinMin is the floor the adaptive EMA decays toward on
	// repeated spin misses.
	shmSpinMin uint32

	// shmSpinMax is the cap the adaptive EMA grows toward on
	// repeated spin hits.
	shmSpinMax uint32
)

// ConfigureShmSpinIterations sets the runtime spin upper bound for
// SHM rings constructed AFTER this call. n = 0 disables spinning
// entirely (the reader / writer fall straight through to a futex
// re-check + block, which is the default). n > 0 enables adaptive
// spin with that value as the per-ring maximum cutoff; the floor is
// 0 and the starting value is n/4.
//
// n is clamped to the platform's spinIterationsLimit (defined in
// shm_spin_{linux,windows,stub}.go) so a misconfigured deployment
// can't burn an arbitrary fraction of a core.
//
// MUST be called before any ShmClientTransport or ShmServerTransport
// is constructed. The values are captured at ring construction time;
// later calls do not affect already-running connections.
//
// Safe to call from package init or test setup; not safe to call from
// the data plane.
func ConfigureShmSpinIterations(n int) {
	if n < 0 {
		n = 0
	}
	if n > spinIterationsLimit {
		n = spinIterationsLimit
	}
	atomic.StoreUint32(&shmSpinMax, uint32(n))
	// Start optimistically at max. The adaptive EMA shrinks the
	// cutoff on every spin miss, so an idle connection naturally
	// decays back toward the floor; a hot connection stays pinned
	// at max where it actually catches the producer hand-off.
	// Starting at a smaller value (e.g. n/4) means a workload whose
	// wake gap is ~70 µs but cap is 30 µs will keep missing as the
	// EMA further shrinks the cutoff, eventually settling near 0
	// even though n would have caught the gap.
	atomic.StoreUint32(&shmSpinDefault, uint32(n))
	// Floor stays at 0 so idle rings decay all the way to "no spin"
	// and stop costing CPU once their workload goes quiet.
	atomic.StoreUint32(&shmSpinMin, 0)
}

// ResetShmSpinIterationsForBench restores the no-spin default. Tests
// and benchmarks that call ConfigureShmSpinIterations should defer
// this so subsequent tests in the same `go test` invocation don't
// inherit the override.
func ResetShmSpinIterationsForBench() {
	atomic.StoreUint32(&shmSpinMax, 0)
	atomic.StoreUint32(&shmSpinDefault, 0)
	atomic.StoreUint32(&shmSpinMin, 0)
}

// loadShmSpinDefault is called from ShmRing construction.
func loadShmSpinDefault() uint32 { return atomic.LoadUint32(&shmSpinDefault) }

// loadShmSpinMin is called from the adaptive shrink path in ring.go.
func loadShmSpinMin() uint32 { return atomic.LoadUint32(&shmSpinMin) }

// loadShmSpinMax is called from the adaptive grow path in ring.go.
func loadShmSpinMax() uint32 { return atomic.LoadUint32(&shmSpinMax) }
