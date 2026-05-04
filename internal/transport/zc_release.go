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

// zcChainReleasePool implements mem.BufferPool. Each chunk emitted by
// readFrameView during a multi-frame ZC chain (or single-frame ZC, which
// is treated as a chain of size 1) wraps its ring slice in a buffer
// backed by this pool. The Put callback decrements the per-ring
// zcInFlight counter; only when the count reaches 0 AND the chain has
// been closed by the codec (chainOpen=0) does EndZcReservation fire and
// publish the deferred ReadIdx in one shot.
//
// This guarantees the cross-process writer never sees a header.ReadIdx
// pointing inside any still-held chain segment.
//
// Get returns a small heap allocation: gRPC's mem.Buffer occasionally
// asks the pool for new buffers (e.g., when growing for split). These
// allocations are independent of the ring slice and don't need to
// touch the ring.
type zcChainReleasePool struct {
	ring *ShmRing
}

func (p *zcChainReleasePool) Get(n int) *[]byte {
	buf := make([]byte, n)
	return &buf
}

func (p *zcChainReleasePool) Put(_ *[]byte) {
	if p.ring == nil {
		return
	}
	// Closed-check before touching shared memory: this Free() may be
	// called after the transport has been closed and the segment
	// unmapped (race with shutdown). The atomic load is cheap (~1ns).
	// Once closed is set to 1 it never reverts; a stale "0" just means
	// we run ReleaseChainZcBuffer on still-valid memory; a "1" means
	// we skip it (leak is acceptable at shutdown).
	if isRingClosed(p.ring) {
		return
	}
	p.ring.ReleaseChainZcBuffer()
}
