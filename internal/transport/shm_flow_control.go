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

package transport

import (
	"sync"
	"sync/atomic"
	"time"
)

// SHM-specific flow control constants.
// Unlike HTTP/2 over TCP, shared memory is local with near-zero RTT, so we use
// much larger initial windows to avoid stalling on WindowUpdate round-trips.
const (
	// shmInitialWindowSize is the initial per-stream flow control window for
	// the shared memory transport. Set to 32 MB to allow large local RPCs
	// without waiting for BDP ramp-up.
	shmInitialWindowSize = 32 * 1024 * 1024 // 32 MB

	// shmBDPLimit is the maximum BDP window size for SHM. This is 4x the
	// HTTP/2 limit (16 MB) because local memory bandwidth is much higher.
	shmBDPLimit = 64 * 1024 * 1024 // 64 MB

	// shmWindowUpdateThreshold is the minimum accumulated bytes before a
	// WindowUpdate frame is sent. Batching reduces frame write overhead.
	shmWindowUpdateThreshold = shmInitialWindowSize / 4 // 8 MB
)

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
