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

// zcReleasePool implements mem.BufferPool. When the wrapped ring-backed
// buffer is freed, Put calls EndZcReservation on the ring, publishing the
// deferred ReadIdx and freeing space for the writer. The buffer's data is
// not actually pooled — the byte slice points into ring memory.
//
// Get returns a small heap allocation: gRPC's mem.Buffer occasionally
// asks the pool for new buffers (e.g., when growing for split). These
// allocations are independent of the ZC ring slice and don't need to
// touch the ring.
type zcReleasePool struct {
	ring *ShmRing
}

func (p *zcReleasePool) Get(n int) *[]byte {
	buf := make([]byte, n)
	return &buf
}

func (p *zcReleasePool) Put(_ *[]byte) {
	if p.ring == nil {
		return
	}
	// Closed-check before touching shared memory: this Free() may be
	// called after the transport has been closed and the segment
	// unmapped (race with shutdown). The atomic load is cheap (~1ns).
	// Once closed is set to 1 it never reverts; a stale "0" just means
	// we run EndZcReservation on still-valid memory; a "1" means we
	// skip it (leak is acceptable at shutdown).
	if isRingClosed(p.ring) {
		return
	}
	p.ring.EndZcReservation()
}
