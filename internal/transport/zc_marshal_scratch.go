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

package transport

import "sync"

// zcMarshalScratchPool returns a heap buffer sized to hold the full
// (H2 header + LPM header + proto body) for the rare wrap-case in
// writeProtoToRingH2Blocking. The pool is sized for the H2 ceiling
// (16 MiB-1); larger requests bypass the pool. Returned slices are
// reused across calls via sync.Pool so steady-state allocations
// approach zero when wrap events come in bursts.
//
// Used ONLY by the writeLoop wrap path. The contiguous fast path
// marshals directly into ring memory and does not touch this pool.
var zcMarshalScratchPool = sync.Pool{
	New: func() any {
		// Default capacity sized for the bench's typical 64 KiB + headers
		// payload; pool returns this when no larger buf is cached.
		b := make([]byte, 0, 65536)
		return &b
	},
}

// getZcMarshalScratch returns a []byte with cap >= size and len = 0.
// If a pooled buffer is too small, this allocates a fresh one (the
// pool will be repopulated with the larger size on putZcMarshalScratch).
func getZcMarshalScratch(size int) []byte {
	pb := zcMarshalScratchPool.Get().(*[]byte)
	b := *pb
	if cap(b) < size {
		b = make([]byte, 0, size)
	}
	return b[:0]
}

// putZcMarshalScratch returns the scratch buffer to the pool.
// Discards buffers > 16 MiB to avoid pinning large allocations after
// occasional jumbo frames.
func putZcMarshalScratch(b []byte) {
	const maxRetained = 16 * 1024 * 1024
	if cap(b) > maxRetained {
		return
	}
	pb := &b
	zcMarshalScratchPool.Put(pb)
}

// asyncProtoBufPool holds buffers that carry the PRE-MARSHALLED proto
// body for the async writeProto fallback. The sender marshals the
// proto.Message into one of these on its own SendMsg goroutine (so the
// live message is never retained across SendMsg, which would race an
// application that legally reuses the message), hands the bytes to the
// writer goroutine, which copies them into the ring and returns the
// buffer here. Get on the sender, Put on the writer — sync.Pool is safe
// across goroutines.
var asyncProtoBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 65536)
		return &b
	},
}

// getAsyncProtoBuf returns a []byte with cap >= size and len 0.
func getAsyncProtoBuf(size int) []byte {
	pb := asyncProtoBufPool.Get().(*[]byte)
	b := *pb
	if cap(b) < size {
		b = make([]byte, 0, size)
	}
	return b[:0]
}

// putAsyncProtoBuf returns a buffer to the async proto pool. Buffers
// larger than 16 MiB are dropped to avoid pinning jumbo allocations.
// A nil buffer is a no-op so terminal-path releases can be unconditional.
func putAsyncProtoBuf(b []byte) {
	if b == nil {
		return
	}
	const maxRetained = 16 * 1024 * 1024
	if cap(b) > maxRetained {
		return
	}
	b = b[:0]
	pb := &b
	asyncProtoBufPool.Put(pb)
}
