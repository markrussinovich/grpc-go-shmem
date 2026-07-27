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

import "sync"

// zcMultiAnchorReleasePool implements mem.BufferPool for the multi-
// anchor single-frame ZC fast path. Each ZC buffer wraps the ring
// slice in a mem.Buffer backed by a fresh pool instance that captures
// the corresponding anchor's seq and the ring slice itself.
// Buffer.Free → pool.Put → ring.ReleaseMultiAnchor(seq) triggers the
// prefix-walk that advances header.ReadIdx.
//
// Allocation accounting (per ZC frame, steady state):
//
//   - zcMultiAnchorReleasePool struct       — pooled via sync.Pool
//   - ringSlice []byte (24-byte header)     — INLINED INTO THE POOL
//                                             STRUCT; mem.NewBuffer
//                                             takes &pool.ringSlice
//                                             which lives in the
//                                             pooled struct (not in
//                                             a per-call escape).
//   - Anchor identifier                     — plain uint64 seq, no
//                                             heap object (previously
//                                             a *MultiAnchor, now
//                                             gone).
//
// → steady-state heap allocations per ZC fire: ZERO when the
// sync.Pool is warm. Cold start pays one pool struct allocation,
// which is reused for the lifetime of the workload.
//
// Lifetime safety: mem.NewBuffer stores &pool.ringSlice; the pool
// struct is only returned to the sync.Pool inside Put() (i.e., after
// mem.Buffer.Free fires). The Buffer's internal pointer therefore
// never dangles while the Buffer is alive.
//
// Put MUST be idempotent — gRPC's mem.Buffer.Free is supposed to be
// called exactly once but we guard against double-free by zeroing
// ring before recycling. A second Put observes ring==nil and no-ops.

type zcMultiAnchorReleasePool struct {
	ring      *ShmRing
	seq       uint64
	ringSlice []byte // slice header lives here so mem.NewBuffer(&pool.ringSlice, pool) has no per-call escape
}

var zcMultiAnchorReleasePoolSync = sync.Pool{
	New: func() any { return &zcMultiAnchorReleasePool{} },
}

// newZcMultiAnchorReleasePool returns a pool-backed Release wrapper
// for one in-flight ZC frame. ringMem is the ring-backed slice the
// caller will hand off to mem.NewBuffer via &pool.ringSlice.
func newZcMultiAnchorReleasePool(ring *ShmRing, seq uint64, ringMem []byte) *zcMultiAnchorReleasePool {
	p := zcMultiAnchorReleasePoolSync.Get().(*zcMultiAnchorReleasePool)
	p.ring = ring
	p.seq = seq
	p.ringSlice = ringMem
	return p
}

func (p *zcMultiAnchorReleasePool) Get(n int) *[]byte {
	buf := make([]byte, n)
	return &buf
}

func (p *zcMultiAnchorReleasePool) Put(_ *[]byte) {
	if p == nil || p.ring == nil {
		return
	}
	ring := p.ring
	seq := p.seq
	p.ring = nil
	p.ringSlice = nil
	// Recycle the pool struct AFTER nilling our copies of the fields so
	// a racing double-Put can't observe a partially-recycled pool.
	defer zcMultiAnchorReleasePoolSync.Put(p)
	if isRingClosed(ring) {
		// Ring closed: skip the actual release call (segment may be
		// unmapped). Slot state remains "released" but the anchor never
		// gets prefix-walked; that is acceptable at shutdown.
		return
	}
	ring.ReleaseMultiAnchor(seq)
}
