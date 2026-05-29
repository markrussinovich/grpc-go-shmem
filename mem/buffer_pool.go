/*
 *
 * Copyright 2024 gRPC authors.
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

package mem

import (
	"fmt"

	"google.golang.org/grpc/internal"
	"google.golang.org/grpc/internal/mem"
)

// BufferPool is a pool of buffers that can be shared and reused, resulting in
// decreased memory allocation.
type BufferPool interface {
	// Get returns a buffer with specified length from the pool.
	Get(length int) *[]byte

	// Put returns a buffer to the pool.
	//
	// The provided pointer must hold a prefix of the buffer obtained via
	// BufferPool.Get to ensure the buffer's entire capacity can be re-used.
	Put(*[]byte)
}

var (
	// defaultBufferPoolSizeExponents is the default tier set used by
	// DefaultBufferPool. Each entry is a log2 byte size; the pool
	// returns the smallest tier that fits a Get(size) request.
	//
	// The 32 KiB → 1 MiB gap in a {8, 12, 14, 15, 20} tier set causes
	// any Get(size) with 32 KiB < size ≤ 1 MiB to snap to the 1 MiB
	// tier, which the pool zeroes on every Get. A 64 KiB Marshal
	// therefore pays a 1 MiB memclr per allocation — 16× the requested
	// size. Tiers 17 (128 KiB) and 19 (512 KiB) close that gap and
	// reduce the worst-case overshoot from 16× to 2× across the
	// 32 KiB–1 MiB range. The 17/19 split was picked so neither sub-
	// range exceeds 2× overshoot while keeping the total tier count
	// (and therefore per-P sync.Pool headroom) small.
	//
	// Tradeoff: each additional tier holds its own sync.Pool per P,
	// so idle memory grows modestly with tier count × GOMAXPROCS.
	// For typical gRPC workloads — where unary and streaming sends
	// in the 32 KiB–1 MiB range are common — the reduced per-Get
	// memclr cost dominates the headroom cost.
	defaultBufferPoolSizeExponents = []uint8{
		8,
		12, // 4 KiB (Go page size)
		14, // 16 KiB (max HTTP/2 frame size used by gRPC)
		15, // 32 KiB (default buffer size for io.Copy)
		17, // 128 KiB (covers 32 KiB+1 .. 128 KiB)
		19, // 512 KiB (covers 128 KiB+1 .. 512 KiB)
		20, // 1 MiB
	}
	defaultBufferPool BufferPool
)

// DefaultBufferPoolSizeExponents returns a copy of the log2 byte-size tier
// set used by DefaultBufferPool. Callers that wrap or compose a buffer pool
// (for example a "dirty" variant that skips per-Get zeroing for a hot path
// whose buffer is fully overwritten immediately after Get) can use this to
// stay in lock-step with the default tier set instead of duplicating the
// list. The returned slice is a fresh copy and safe to retain or mutate.
func DefaultBufferPoolSizeExponents() []uint8 {
	out := make([]uint8, len(defaultBufferPoolSizeExponents))
	copy(out, defaultBufferPoolSizeExponents)
	return out
}

func init() {
	var err error
	defaultBufferPool, err = NewBinaryTieredBufferPool(defaultBufferPoolSizeExponents...)
	if err != nil {
		panic(fmt.Sprintf("Failed to create default buffer pool: %v", err))
	}

	internal.SetDefaultBufferPool = func(pool BufferPool) {
		defaultBufferPool = pool
	}

	internal.SetBufferPoolingThresholdForTesting = func(threshold int) {
		bufferPoolingThreshold = threshold
	}
}

// DefaultBufferPool returns the current default buffer pool. It is a BufferPool
// created with NewBufferPool that uses a set of default sizes optimized for
// expected workflows.
func DefaultBufferPool() BufferPool {
	return defaultBufferPool
}

// NewTieredBufferPool returns a BufferPool implementation that uses multiple
// underlying pools of the given pool sizes.
func NewTieredBufferPool(poolSizes ...int) BufferPool {
	return mem.NewTieredBufferPool(poolSizes...)
}

// NewBinaryTieredBufferPool returns a BufferPool backed by multiple sub-pools.
// This structure enables O(1) lookup time for Get and Put operations.
//
// The arguments provided are the exponents for the buffer capacities (powers
// of 2), not the raw byte sizes. For example, to create a pool of 16KB buffers
// (2^14 bytes), pass 14 as the argument.
func NewBinaryTieredBufferPool(powerOfTwoExponents ...uint8) (BufferPool, error) {
	return mem.NewBinaryTieredBufferPool(powerOfTwoExponents...)
}

// NopBufferPool is a buffer pool that returns new buffers without pooling.
type NopBufferPool struct {
	mem.NopBufferPool
}

var _ BufferPool = NopBufferPool{}
