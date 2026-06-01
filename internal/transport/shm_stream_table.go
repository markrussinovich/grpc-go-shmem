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

// PR #11 (Direct-Mapped Streams Table) — shared constants + index
// helper used by both ShmClientTransport and ShmServerTransport.
//
// The per-frame dispatch hot path resolves a streamID to its
// *ClientStream / *ServerStream. Pre-PR-#11 this went through a
// single-entry MRU cache (`cachedStream`) that only worked for the
// 1-stream-at-a-time case; with N concurrent streams the cache
// missed essentially every frame, falling through to `t.mu.RLock()`
// + `t.streams[id]`. At 1000-stream concurrency this was a measured
// hot path. PR #11 replaces the MRU with a fixed-size direct-mapped
// atomic table indexed by `streamSlotIdx(streamID)`.
//
// Key correctness points (caught by Opus 4.8 + GPT-5.5 in the
// adversarial pre-review):
//
//   - HTTP/2 client-initiated stream IDs are always ODD (RFC 7540
//     §5.1.1). The index function MUST shift out the low bit; using
//     `streamID % N` would waste half the table.
//
//   - The slot stores an `atomic.Pointer[ClientStream/ServerStream]`
//     directly — NO heap-allocated wrapper struct — to keep the
//     dispatch path allocation-free under collisions.
//
//   - The slow-path fallback (slot miss → t.mu.RLock → t.streams[id])
//     MUST publish into the slot WHILE STILL HOLDING THE RLOCK to
//     avoid a stale-publish race: between unlock and Store a close
//     could remove the stream + clear the slot, then a stale Store
//     would resurrect a dead pointer with no future close to clean
//     it. Hence the publish lives inline at the call site.
//
//   - Slot clearing on stream removal MUST be a CAS against the
//     current pointer ("clear only if still points to me") to avoid
//     wiping a newer stream that won the same slot by collision.
//
// N = 2048 provides headroom for >1024-stream cells with zero
// collisions at the 1000-stream target. 2048 pointer-sized atomics
// = 16 KiB per transport, negligible vs the per-conn ring footprint.
const shmStreamSlotCount = 2048

// streamSlotIdx maps an HTTP/2 stream ID to its direct-mapped slot
// index. Client-initiated stream IDs are odd, so we shift out the
// low bit before masking with (shmStreamSlotCount-1) so that
// consecutive streams 1, 3, 5, ... occupy slots 0, 1, 2, ...
// without leaving every other slot unreachable.
func streamSlotIdx(streamID uint32) uint32 {
	return (streamID >> 1) & (shmStreamSlotCount - 1)
}
