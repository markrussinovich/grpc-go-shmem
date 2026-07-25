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

// This file implements flow control mechanisms for the shared memory transport.
//
// RFC A73 Phase 5: Flow Control Alignment
//
// The shared memory transport uses its own optimized flow control constants
// that differ from HTTP/2's defaults because local shared memory has near-zero
// RTT and much higher bandwidth than TCP:
//
// - Initial window size: 32 MB (vs HTTP/2's 64 KB) to avoid stalling on
//   WindowUpdate round-trips for large local RPCs.
// - Maximum BDP window: 64 MB (vs HTTP/2's 16 MB) to fully utilize local
//   memory bandwidth.
// - WindowUpdate batching: deltas are accumulated and only sent when they
//   exceed shmWindowUpdateThreshold (8 MB), reducing control frame overhead.
// - BDP estimation: same exponential moving average algorithm as HTTP/2 but
//   with the higher shmBDPLimit ceiling.
//
// Key SHM-specific constants:
//   - shmInitialWindowSize = 32 MB: initial per-stream window
//   - shmBDPLimit = 64 MB: maximum BDP window
//   - shmWindowUpdateThreshold = 8 MB: batching threshold
//
// BDP algorithm constants (shared with HTTP/2):
//   - alpha = 0.9: Smoothing factor for RTT estimation
//   - beta = 0.66: Threshold for BDP increase trigger
//   - gamma = 2: Multiplicative factor for BDP growth
//
// Note: gRPC dial/server options (WithInitialWindowSize, etc.) do NOT currently
// override the SHM-specific constants. The SHM transport always uses the
// hardcoded values above. This may change in a future revision.

package engine

import (
	"sync"
	"sync/atomic"
	"time"
)

// SHM-specific flow control. Unlike HTTP/2 over TCP, shared memory is
// local with near-zero RTT, so production tunes the per-stream window
// much higher than the 65535-byte HTTP/2 default to avoid WindowUpdate
// round-trips during bulk streaming.
//
// The size knobs below are package-level `var`s rather than `const`s
// so benchmark / test code can switch the SHM transport into a fair-
// comparison profile (matching the HTTP/2 default window used by the
// in-tree TCP and Unix-socket benchmarks). Production code MUST NOT
// mutate these from the data plane — the transport reads them once
// at construction (for `initialWindowSize` and the conn / stream
// quota initial values) and on every WindowUpdate emission (for the
// batching threshold). Use ConfigureShmFlowControlForBench from test
// setup BEFORE any transport is dialed or listened.
var (
	// shmInitialWindowSize is the initial per-stream flow-control
	// window the SHM BDP estimator starts from. Default 32 MiB allows
	// large local RPCs without waiting for BDP ramp-up.
	shmInitialWindowSize = 32 * 1024 * 1024

	// shmWindowUpdateThreshold is the minimum accumulated bytes
	// before a WindowUpdate frame is sent. Batching reduces frame
	// write overhead. Default is shmInitialWindowSize/4 = 8 MiB.
	// ConfigureShmFlowControlForBench keeps the relationship sane:
	// if the threshold ever exceeded the effective window the sender
	// would deadlock (the consumer can never accumulate enough to
	// trigger a WindowUpdate before the producer exhausts the window).
	shmWindowUpdateThreshold = shmInitialWindowSize / 4

	// shmMaxFrameSize bounds the body of a single H2 DATA frame the
	// producer emits. Defaults to the RFC 7540 ceiling (16 MiB - 1)
	// because SHM is local and per-frame overhead is negligible.
	// HTTP/2 over TCP / UDS in this codebase uses the HTTP/2 spec
	// default of 16384 bytes; bench code can match it via
	// ConfigureShmFlowControlForBench so SHM and TCP / UDS emit the
	// same number of DATA frames per write. The receiver always
	// accepts up to the RFC ceiling regardless of this knob.
	shmMaxFrameSize = h2MaxFramePayload
)

const (
	// shmBDPLimit is the maximum BDP window size for SHM. 4× HTTP/2's
	// limit (16 MiB) because local memory bandwidth is much higher.
	shmBDPLimit = 64 * 1024 * 1024 // 64 MB
)

// ConfigureShmFlowControlForBench overrides shmInitialWindowSize and
// shmWindowUpdateThreshold consistently. Intended for benchmark and
// regression-test code that wants to exercise the SHM transport under
// HTTP/2-default (65535 B) or other window sizes — both to compare
// SHM against TCP / UDS on equal footing and to drive the transport
// into code paths (multi-DATA-frame LPM under flow control) that are
// hidden by the SHM-tuned 32 MiB default.
//
// MUST be called BEFORE any ShmClientTransport or ShmServerTransport
// is constructed. The values are captured once at construction. NOT
// safe to call from the data plane.
//
// initialWindow values below 4 KiB are clamped to 4 KiB so the
// WindowUpdate threshold never collapses to zero (which would cause
// every byte received to emit a window-update frame).
func ConfigureShmFlowControlForBench(initialWindow int) {
	if initialWindow < 4*1024 {
		initialWindow = 4 * 1024
	}
	shmInitialWindowSize = initialWindow
	// Threshold = window / 4, with a 1 KiB floor (so we don't pay
	// WindowUpdate overhead per-byte under tiny windows) and a
	// window/2 ceiling (so the sender can always refill at least
	// once before exhausting the window).
	//
	// We empirically validated window/4 vs window/2: under streaming
	// the smaller threshold pipelines better — the producer receives
	// a steady trickle of small credits and never fully drains the
	// window. window/2 makes the producer wait for larger but less
	// frequent refills, which adds a full RTT block between each
	// chunk. HTTP/2 over TCP fires WUs at delta/2 because TCP's
	// per-segment overhead is high; SHM's per-WU overhead is much
	// lower so more frequent updates are net positive.
	threshold := initialWindow / 4
	if threshold < 1024 {
		threshold = 1024
	}
	if threshold >= initialWindow {
		threshold = initialWindow / 2
	}
	shmWindowUpdateThreshold = threshold
}

// computeWUThreshold returns the per-transport WindowUpdate emission
// threshold for the given effective initial window. Mirrors the
// math in ConfigureShmFlowControlForBench but as a pure helper that
// can be called per-transport after a grpc.WithInitialWindowSize
// override updates the transport's effective window.
//
// Bounds:
//   - 1 KiB floor: avoid emitting a WU per byte under tiny windows
//   - window/2 ceiling: the sender must be able to refill at least
//     once before exhausting its window, otherwise it parks forever
//   - window/4 default: matches HTTP/2 spec heuristic and pipelines
//     well under streaming
//
// initialWindow <= 0 falls back to the package-global default (set
// by ConfigureShmFlowControlForBench or the build-time constant).
// This is the correctness fix for a real deadlock: previously,
// sendWindowUpdate used `shmWindowUpdateThreshold` directly, so a
// transport dialed with grpc.WithInitialWindowSize(65535) inherited
// the 8 MiB threshold from the package global (computed from the
// 32 MiB shm-tuned default). The receiver's onRead would emit
// 16 KiB credits, sendWindowUpdate would accumulate them, but the
// 8 MiB threshold would never be reached because the sender had
// already exhausted its 64 KiB window and was parked. Result:
// permanent hang under any small-window deployment.
func computeWUThreshold(initialWindow int32) uint32 {
	if initialWindow <= 0 {
		return uint32(shmWindowUpdateThreshold)
	}
	t := initialWindow / 4
	if t < 1024 {
		t = 1024
	}
	if int64(t) >= int64(initialWindow) {
		t = initialWindow / 2
	}
	return uint32(t)
}

// ConfigureShmMaxFrameSizeForBench overrides shmMaxFrameSize so the SHM
// producer chunks H2 DATA frames at the given body size, matching the
// HTTP/2 spec default of 16384 used by TCP / UDS in this codebase
// when run under a fair-comparison bench profile. Values are clamped
// to the RFC range [2^14, 2^24-1].
//
// MUST be called BEFORE any ShmClientTransport or ShmServerTransport
// is constructed. Reset via ResetShmFlowControlForBench.
func ConfigureShmMaxFrameSizeForBench(maxFrame int) {
	const minFrame = 1 << 14 // RFC 7540 §6.5.2 SETTINGS_MAX_FRAME_SIZE lower bound
	if maxFrame < minFrame {
		maxFrame = minFrame
	}
	if maxFrame > h2MaxFramePayload {
		maxFrame = h2MaxFramePayload
	}
	shmMaxFrameSize = maxFrame
}

// ResetShmFlowControlForBench restores the SHM flow-control knobs to
// their production defaults (32 MiB window, 8 MiB threshold, RFC max
// frame size). Tests and benchmarks that call ConfigureShmFlowControlForBench
// or ConfigureShmMaxFrameSizeForBench should `defer` this so subsequent
// tests in the same `go test` invocation don't inherit the override.
func ResetShmFlowControlForBench() {
	shmInitialWindowSize = 32 * 1024 * 1024
	shmWindowUpdateThreshold = shmInitialWindowSize / 4
	shmMaxFrameSize = h2MaxFramePayload
}

// shmBDPEstimator provides bandwidth-delay product estimation for the shared
// memory transport. It uses the same exponential moving average algorithm as
// HTTP/2's bdpEstimator but with a higher ceiling (shmBDPLimit = 64 MB).
//
// Performance optimization: Uses atomic operations for the hot path (add)
// to avoid mutex contention on every message.
type shmBDPEstimator struct {
	// Fast path fields - accessed atomically without lock
	// settled is 1 when BDP estimation is complete (bdp == shmBDPLimit)
	settled atomic.Uint32
	// sample is updated atomically during measurement
	sampleAtomic atomic.Uint32
	// isSentAtomic is 1 when a BDP ping is outstanding
	isSentAtomic atomic.Uint32

	// Slow path fields - protected by mutex
	mu sync.Mutex

	// bdp is the current BDP estimate in bytes.
	bdp uint32

	// bwMax is the maximum bandwidth observed so far (bytes/sec).
	bwMax float64

	// sentAt is the time when the BDP ping was sent.
	sentAt time.Time

	// sampleCount is the number of samples taken so far.
	sampleCount uint64

	// rtt is the smoothed round-trip time in seconds.
	rtt float64

	// updateFlowControl is called when the BDP estimate changes.
	updateFlowControl func(n uint32)
}

// newShmBDPEstimator creates a new BDP estimator for the shm transport.
func newShmBDPEstimator(initialWindow uint32, updateFn func(n uint32)) *shmBDPEstimator {
	return &shmBDPEstimator{
		bdp:               initialWindow,
		updateFlowControl: updateFn,
	}
}

// add adds bytes to the current sample. Returns true if a BDP ping should be sent.
// This is the hot path - optimized to avoid mutex in common cases.
func (b *shmBDPEstimator) add(n uint32) bool {
	// Fast path: if already settled at shmBDPLimit, nothing to do
	if b.settled.Load() != 0 {
		return false
	}

	// Fast path: if a ping is already sent, just accumulate sample atomically
	if b.isSentAtomic.Load() != 0 {
		b.sampleAtomic.Add(n)
		return false
	}

	// Slow path: need to initiate a new measurement cycle
	b.mu.Lock()
	defer b.mu.Unlock()

	// Double-check after acquiring lock
	if b.bdp == shmBDPLimit {
		b.settled.Store(1)
		return false
	}

	// Check again if another goroutine already set isSent
	if b.isSentAtomic.Load() != 0 {
		b.sampleAtomic.Add(n)
		return false
	}

	// Start new measurement
	b.isSentAtomic.Store(1)
	b.sampleAtomic.Store(n)
	b.sentAt = time.Time{}
	b.sampleCount++
	return true
}

// timesnap records the time when a BDP ping is sent.
func (b *shmBDPEstimator) timesnap() {
	b.mu.Lock()
	b.sentAt = time.Now()
	b.mu.Unlock()
}

// calculate updates the BDP estimate when a BDP ping ack is received.
func (b *shmBDPEstimator) calculate() {
	b.mu.Lock()

	if b.sentAt.IsZero() {
		b.mu.Unlock()
		return
	}

	rttSample := time.Since(b.sentAt).Seconds()

	// Bootstrap RTT with an average of first 10 samples.
	if b.sampleCount < 10 {
		b.rtt += (rttSample - b.rtt) / float64(b.sampleCount)
	} else {
		// Exponential moving average for subsequent samples.
		b.rtt += (rttSample - b.rtt) * alpha
	}

	// Read and reset atomic sample
	sample := b.sampleAtomic.Swap(0)
	b.isSentAtomic.Store(0)

	// The sample is at most 1.5x the real BDP on a saturated connection.
	bwCurrent := float64(sample) / (b.rtt * 1.5)
	if bwCurrent > b.bwMax {
		b.bwMax = bwCurrent
	}

	// Update BDP if the sample suggests higher capacity.
	if float64(sample) >= beta*float64(b.bdp) && bwCurrent == b.bwMax && b.bdp != shmBDPLimit {
		sampleFloat := float64(sample)
		b.bdp = uint32(gamma * sampleFloat)
		if b.bdp > shmBDPLimit {
			b.bdp = shmBDPLimit
			b.settled.Store(1)
		}
		bdp := b.bdp
		b.mu.Unlock()
		b.updateFlowControl(bdp)
		return
	}

	b.mu.Unlock()
}
