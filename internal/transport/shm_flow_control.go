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
// memory transport. It mirrors the bdpEstimator from HTTP/2 but is adapted for
// the lower-latency shared memory environment.
//
// RFC A73 Phase 5: Flow Control Alignment
// The shmem transport shares flow control settings with HTTP/2 configuration,
// using the same initial window sizes and dynamic BDP estimation algorithm.
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

// StreamPriority represents the priority of a stream for fair scheduling.
// This aligns with HTTP/2's stream priority model for multi-stream fairness.
type StreamPriority struct {
	// StreamID is the identifier of the stream.
	StreamID uint32

	// Weight is the relative weight of the stream (1-256, default 16).
	// Higher weight means more bandwidth allocation.
	Weight uint8

	// Exclusive indicates whether this stream should be the sole child
	// of its dependency.
	Exclusive bool

	// DependsOn is the stream ID that this stream depends on.
	// 0 means dependent on the connection root.
	DependsOn uint32
}

// DefaultStreamPriority returns the default priority for a new stream.
func DefaultStreamPriority(streamID uint32) StreamPriority {
	return StreamPriority{
		StreamID:  streamID,
		Weight:    16, // HTTP/2 default weight
		Exclusive: false,
		DependsOn: 0,
	}
}

// StreamScheduler provides fair scheduling for multiple concurrent streams.
// It uses weighted round-robin to ensure no single stream starves others.
type StreamScheduler struct {
	mu sync.RWMutex

	// streams maps stream ID to priority info
	streams map[uint32]*scheduledStream

	// activeList is a list of streams with pending data
	activeList []*scheduledStream

	// totalWeight is the sum of all active stream weights
	totalWeight uint32

	// roundDeficit tracks deficit for weighted fair queueing
	roundDeficit map[uint32]int64
}

type scheduledStream struct {
	priority StreamPriority
	pending  atomic.Int64 // bytes pending to be written
	active   bool
}

// NewStreamScheduler creates a new stream scheduler.
func NewStreamScheduler() *StreamScheduler {
	return &StreamScheduler{
		streams:      make(map[uint32]*scheduledStream),
		activeList:   make([]*scheduledStream, 0),
		roundDeficit: make(map[uint32]int64),
	}
}

// AddStream adds a stream with the given priority.
func (s *StreamScheduler) AddStream(priority StreamPriority) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.streams[priority.StreamID] = &scheduledStream{
		priority: priority,
		active:   false,
	}
	s.roundDeficit[priority.StreamID] = 0
}

// RemoveStream removes a stream from the scheduler.
func (s *StreamScheduler) RemoveStream(streamID uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stream, ok := s.streams[streamID]
	if !ok {
		return
	}

	if stream.active {
		s.removeFromActiveList(streamID)
		s.totalWeight -= uint32(stream.priority.Weight)
	}

	delete(s.streams, streamID)
	delete(s.roundDeficit, streamID)
}

// MarkActive marks a stream as having pending data.
func (s *StreamScheduler) MarkActive(streamID uint32, pending int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stream, ok := s.streams[streamID]
	if !ok {
		return
	}

	stream.pending.Add(pending)

	if !stream.active {
		stream.active = true
		s.activeList = append(s.activeList, stream)
		s.totalWeight += uint32(stream.priority.Weight)
	}
}

// MarkIdle marks a stream as having no pending data.
func (s *StreamScheduler) MarkIdle(streamID uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stream, ok := s.streams[streamID]
	if !ok {
		return
	}

	if stream.active {
		stream.active = false
		s.removeFromActiveList(streamID)
		s.totalWeight -= uint32(stream.priority.Weight)
	}
	stream.pending.Store(0)
}

func (s *StreamScheduler) removeFromActiveList(streamID uint32) {
	for i, stream := range s.activeList {
		if stream.priority.StreamID == streamID {
			s.activeList = append(s.activeList[:i], s.activeList[i+1:]...)
			return
		}
	}
}

// NextStream returns the next stream that should send data based on
// weighted fair queueing. Returns 0 if no active streams.
func (s *StreamScheduler) NextStream(maxBytes int64) (streamID uint32, allowedBytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.activeList) == 0 {
		return 0, 0
	}

	if s.totalWeight == 0 {
		return 0, 0
	}

	// Find the stream with the highest deficit (most deserving of bandwidth)
	var bestStream *scheduledStream
	var bestDeficit int64 = -1<<63 + 1 // min int64

	for _, stream := range s.activeList {
		deficit := s.roundDeficit[stream.priority.StreamID]
		if deficit > bestDeficit {
			bestDeficit = deficit
			bestStream = stream
		}
	}

	if bestStream == nil {
		return 0, 0
	}

	streamID = bestStream.priority.StreamID

	// Calculate the bytes to allow based on weight proportion
	weightFraction := float64(bestStream.priority.Weight) / float64(s.totalWeight)
	allowedBytes = int64(float64(maxBytes) * weightFraction)
	if allowedBytes < 1 {
		allowedBytes = 1
	}

	// Cap to pending bytes
	pending := bestStream.pending.Load()
	if pending < allowedBytes {
		allowedBytes = pending
	}

	// Update deficit using weighted fair queueing
	// Decrease this stream's deficit since it got to send
	s.roundDeficit[streamID] -= int64(bestStream.priority.Weight)

	// Increase all other streams' deficit
	for _, stream := range s.activeList {
		if stream.priority.StreamID != streamID {
			s.roundDeficit[stream.priority.StreamID] += int64(stream.priority.Weight)
		}
	}

	return streamID, allowedBytes
}

// ActiveStreamCount returns the number of active streams.
func (s *StreamScheduler) ActiveStreamCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.activeList)
}

// GetStreamPriority returns the priority of a stream.
func (s *StreamScheduler) GetStreamPriority(streamID uint32) (StreamPriority, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stream, ok := s.streams[streamID]
	if !ok {
		return StreamPriority{}, false
	}
	return stream.priority, true
}

// UpdatePriority updates the priority of an existing stream.
func (s *StreamScheduler) UpdatePriority(priority StreamPriority) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stream, ok := s.streams[priority.StreamID]
	if !ok {
		return
	}

	if stream.active {
		s.totalWeight -= uint32(stream.priority.Weight)
		s.totalWeight += uint32(priority.Weight)
	}

	stream.priority = priority
}
