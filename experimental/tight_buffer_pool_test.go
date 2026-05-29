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
	"testing"
)

func TestTightBufferPool_ExactSizeReuse(t *testing.T) {
	p := newTightBufferPool(defaultTightPoolMaxSizeClasses)

	// First Get: pool empty, alloc fresh.
	b1 := p.Get(4109)
	if got, want := cap(*b1), 4109; got != want {
		t.Fatalf("first Get(4109): cap=%d want=%d", got, want)
	}
	if got, want := len(*b1), 4109; got != want {
		t.Fatalf("first Get(4109): len=%d want=%d", got, want)
	}
	addr1 := &(*b1)[0]

	// Return it.
	p.Put(b1)

	// Second Get of same size: should reuse the buffer (same backing array).
	b2 := p.Get(4109)
	if got, want := cap(*b2), 4109; got != want {
		t.Fatalf("reused Get(4109): cap=%d want=%d", got, want)
	}
	addr2 := &(*b2)[0]
	if addr1 != addr2 {
		t.Errorf("expected pool reuse: addr1=%p addr2=%p", addr1, addr2)
	}
	p.Put(b2)
}

func TestTightBufferPool_DistinctSizeClassesIndependent(t *testing.T) {
	p := newTightBufferPool(defaultTightPoolMaxSizeClasses)

	// 3 distinct sizes — each gets its own sync.Pool.
	b1 := p.Get(1000)
	b2 := p.Get(2000)
	b3 := p.Get(3000)
	if cap(*b1) != 1000 || cap(*b2) != 2000 || cap(*b3) != 3000 {
		t.Fatalf("caps: %d %d %d", cap(*b1), cap(*b2), cap(*b3))
	}
	p.Put(b1)
	p.Put(b2)
	p.Put(b3)

	if got, want := p.sizeClassCount.Load(), int32(3); got != want {
		t.Errorf("sizeClassCount=%d want=%d", got, want)
	}

	// Re-Get(2000) should reuse the 2000-cap buffer, not the 1000 or 3000.
	b2b := p.Get(2000)
	if got, want := cap(*b2b), 2000; got != want {
		t.Errorf("re-Get(2000) cap=%d want=%d", got, want)
	}
}

func TestTightBufferPool_PutNilSafe(t *testing.T) {
	p := newTightBufferPool(defaultTightPoolMaxSizeClasses)
	p.Put(nil)
	empty := []byte{}
	p.Put(&empty)
	// No panic, no size class created.
	if got := p.sizeClassCount.Load(); got != 0 {
		t.Errorf("sizeClassCount=%d want=0", got)
	}
}

func TestTightBufferPool_GetZeroOrNegative(t *testing.T) {
	p := newTightBufferPool(defaultTightPoolMaxSizeClasses)
	for _, n := range []int{0, -1, -100} {
		b := p.Get(n)
		if cap(*b) != 0 || len(*b) != 0 {
			t.Errorf("Get(%d) cap=%d len=%d want both 0", n, cap(*b), len(*b))
		}
	}
}

func TestTightBufferPool_OverflowFallback(t *testing.T) {
	// Tiny cap so we hit overflow after one Get/Put cycle of a new size.
	p := newTightBufferPool(1)

	// First distinct size → uses the dedicated pool.
	b1 := p.Get(100)
	p.Put(b1)
	if got, want := p.sizeClassCount.Load(), int32(1); got != want {
		t.Fatalf("after first Get/Put: sizeClassCount=%d want=%d", got, want)
	}

	// Second distinct size → overflow shared pool (sizeClassCount not incremented).
	b2 := p.Get(200)
	if cap(*b2) != 200 {
		t.Errorf("overflow Get(200) cap=%d want=200", cap(*b2))
	}
	p.Put(b2)
	if got, want := p.sizeClassCount.Load(), int32(1); got != want {
		t.Errorf("after overflow Put: sizeClassCount=%d want=%d (unchanged)", got, want)
	}
}

func TestTightBufferPool_ConcurrentGetPut(t *testing.T) {
	p := newTightBufferPool(defaultTightPoolMaxSizeClasses)
	const (
		goroutines = 64
		iters      = 1000
	)
	sizes := []int{1024, 4096, 8192, 16384, 65536}
	var wg sync.WaitGroup
	var allocCount atomic.Int64
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				size := sizes[(g+i)%len(sizes)]
				b := p.Get(size)
				if cap(*b) != size || len(*b) != size {
					t.Errorf("Get(%d): cap=%d len=%d", size, cap(*b), len(*b))
					return
				}
				// Touch the buffer to ensure it's writable.
				(*b)[0] = byte(g)
				(*b)[size-1] = byte(i)
				allocCount.Add(1)
				p.Put(b)
			}
		}(g)
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	// After heavy reuse, size class count is bounded by the distinct sizes.
	if got := p.sizeClassCount.Load(); got > int32(len(sizes)) {
		t.Errorf("sizeClassCount=%d, expected ≤ %d (distinct sizes used)", got, len(sizes))
	}
}

func TestTightBufferPool_LengthResetOnReuse(t *testing.T) {
	// After re-slicing on Put, the next Get must restore the requested length.
	p := newTightBufferPool(defaultTightPoolMaxSizeClasses)
	b := p.Get(1024)
	// Simulate the caller shrinking the slice before Put.
	short := (*b)[:5]
	p.Put(&short)
	b2 := p.Get(1024)
	if got, want := len(*b2), 1024; got != want {
		t.Errorf("after shrink+Put: re-Get len=%d want=%d", got, want)
	}
	if got, want := cap(*b2), 1024; got != want {
		t.Errorf("after shrink+Put: re-Get cap=%d want=%d", got, want)
	}
}
