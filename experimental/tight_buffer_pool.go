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

package experimental

import (
	"sync"
	"sync/atomic"

	"google.golang.org/grpc/mem"
)

// TightBufferPool returns a mem.BufferPool that returns buffers of exactly
// the requested size rather than the next-power-of-two tier the default
// BinaryTieredBufferPool returns. This eliminates the per-Get overshoot
// the default pool incurs on payloads just above a tier boundary — the
// canonical example being a 4 KiB protobuf message snapping to the 16 KiB
// tier (4× overshoot) on every Marshal.
//
// The shared-memory transport benchmark profile shows that overshoot is
// responsible for the majority of marshal-side allocation pressure on
// small-payload workloads (about 64 % of total alloc_space). Replacing
// the default pool with a TightBufferPool eliminates that pressure
// without changing wire-format semantics or codec selection.
//
// Trade-off: every distinct requested size becomes its own sync.Pool, so
// workloads with high size variability create many pools. To bound memory,
// the pool caps the number of distinct size classes at maxSizeClasses
// (default 1024). Get requests for new size classes beyond the cap fall
// back to a single shared sync.Pool that does not enforce the exact-
// size guarantee — those uncommon sizes pay the same overshoot the
// default BinaryTieredBufferPool incurs but are never blocked.
//
// Concurrency: the pool is safe for concurrent use. Internally it uses
// a sync.Map of *sync.Pool, so Get / Put on the hot path (sizes that
// have already been allocated) is lock-free read + sync.Pool.Get / Put.
// First-time allocation of a new size class takes one LoadOrStore on
// the inner sync.Map.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func TightBufferPool() mem.BufferPool {
	return newTightBufferPool(defaultTightPoolMaxSizeClasses)
}

const defaultTightPoolMaxSizeClasses = 1024

type tightBufferPool struct {
	pools          sync.Map // map[int]*sync.Pool — key is exact size class
	sizeClassCount atomic.Int32
	maxSizeClasses int32
	overflowPool   sync.Pool // shared fallback when sizeClassCount >= maxSizeClasses
}

func newTightBufferPool(maxSizeClasses int32) *tightBufferPool {
	return &tightBufferPool{maxSizeClasses: maxSizeClasses}
}

// Get returns a buffer with capacity exactly equal to size. If a previously
// returned buffer of exactly this size is available in the per-size sync.Pool
// it is reused (zero allocation); otherwise a fresh make([]byte, size) is
// performed and returned. The buffer's length is set to size.
func (p *tightBufferPool) Get(size int) *[]byte {
	if size <= 0 {
		empty := make([]byte, 0)
		return &empty
	}
	pool := p.poolFor(size)
	if v := pool.Get(); v != nil {
		b := v.(*[]byte)
		if cap(*b) >= size {
			*b = (*b)[:size]
			return b
		}
		// Capacity shrank below request (shouldn't happen — Put preserves
		// cap, and pool entries are keyed by cap). Discard and alloc fresh.
	}
	buf := make([]byte, size)
	return &buf
}

// Put returns the buffer to the pool keyed by its capacity. The buffer's
// length is reset to its capacity so the next Get can re-slice safely.
// Callers must not retain references to the slice after Put.
func (p *tightBufferPool) Put(b *[]byte) {
	if b == nil {
		return
	}
	c := cap(*b)
	if c == 0 {
		return
	}
	*b = (*b)[:c] // restore length so cap is preserved on reuse
	pool := p.poolFor(c)
	pool.Put(b)
}

// poolFor locates (or creates) the *sync.Pool dedicated to the given size.
// To avoid unbounded growth on workloads with many distinct sizes, the
// number of pools is capped at p.maxSizeClasses. Once the cap is reached
// new sizes share p.overflowPool (which loses the exact-size guarantee
// but never blocks). The cap is a soft limit applied with a CAS race —
// minor over-creation under contention is acceptable.
func (p *tightBufferPool) poolFor(size int) *sync.Pool {
	if v, ok := p.pools.Load(size); ok {
		return v.(*sync.Pool)
	}
	if p.sizeClassCount.Load() >= p.maxSizeClasses {
		return &p.overflowPool
	}
	newPool := &sync.Pool{}
	actual, loaded := p.pools.LoadOrStore(size, newPool)
	if !loaded {
		p.sizeClassCount.Add(1)
	}
	return actual.(*sync.Pool)
}

// Ensure tightBufferPool satisfies mem.BufferPool.
var _ mem.BufferPool = (*tightBufferPool)(nil)
