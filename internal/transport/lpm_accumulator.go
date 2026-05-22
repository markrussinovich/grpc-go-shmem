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

import (
	"encoding/binary"
	"errors"
	"fmt"

	imem "google.golang.org/grpc/internal/mem"
	"google.golang.org/grpc/mem"
)

// shmLpmPool backs lpmAccumulator.buf allocations across all SHM
// transports. Buffers are pooled by power-of-two size and explicitly
// NOT zeroed on Get — the accumulator immediately overwrites every
// returned slice with received DATA-frame bytes before the slice is
// handed to the gRPC layer, so the runtime.memclrNoHeapPointers cost
// that the default DefaultBufferPool() pays on every Get is pure
// waste here. Profiling fair-default 1 MiB streaming after the
// pre-alloc + cross-RPC pooling fixes showed memclr at ~20% of total
// CPU; switching to a dirty pool eliminates it.
//
// Size tiers match mem.DefaultBufferPool: 256 B, 4 KiB, 16 KiB,
// 32 KiB, 1 MiB. Requests above 1 MiB fall back to the dirty simple
// pool which does sync.Pool reuse without the size-bucketing.
var shmLpmPool = func() mem.BufferPool {
	p, err := imem.NewDirtyBinaryTieredBufferPool(8, 12, 14, 15, 20)
	if err != nil {
		// Argument list is hardcoded above; failure here implies a
		// programming error in imem itself.
		panic(fmt.Sprintf("shmLpmPool: NewDirtyBinaryTieredBufferPool failed: %v", err))
	}
	return p
}()

// lpmAccumulator reassembles a single in-progress gRPC length-prefixed
// message (LPM) across multiple HTTP/2 DATA frames. HTTP/2 DATA frames
// carry a byte stream of LPM-prefixed messages (gRFC §"Length-Prefixed-
// Message"); one DATA frame may contain a fragment of one message,
// multiple complete messages, or a mix.
//
// The accumulator emits "1 internal MESSAGE = 1 complete app message" so
// upper layers see the same model the rest of the transport uses. The LPM header (5
// bytes) is preserved at the start of the emitted body for compatibility
// with downstream readers that expect to strip it themselves.
//
// LPM wire format (gRFC):
//
//	+--------+--------+--------+--------+--------+...
//	| 1 byte | 4 byte big-endian length  | length bytes of body
//	|comp flg|  uncompressed body length |
//	+--------+---------------------------+...
//
// State machine:
//
//	state = empty:
//	  - header bytes seen 0..4 → keep collecting in headerBuf
//	  - header bytes seen == 5 → parse expected body length, allocate buffer
//	state = collecting:
//	  - append body bytes until pos == expectedTotal → emit
//
// Single-frame fast path: when the accumulator is empty AND the entire
// LPM (5+body) is contained in a single DATA frame body, the codec
// bypasses the accumulator and returns the body slice directly. This
// preserves the ZC opportunity for the common case.
type lpmAccumulator struct {
	headerBuf       [5]byte // partial LPM header bytes collected so far
	headerBytesSeen int     // 0..5 — header bytes written to headerBuf
	expectedTotal   int     // 5 + body length, set once header complete
	pos             int     // bytes written into buf (includes 5-byte header)
	buf             []byte  // accumulated frame, len == pos, cap >= expectedTotal

	// pool, when non-nil, is the source of buf allocations. Each
	// emitted message takes its backing slice from this pool;
	// downstream callers re-wrap the slice via mem.NewBuffer(&msg,
	// pool) so that Buffer.Free() returns the slice to the pool
	// instead of dropping it on the GC. Reusing the same slice across
	// RPCs eliminates the runtime.mallocgcLarge + runtime.memclr cost
	// that profiling showed as the dominant overhead under
	// fair-default streaming once growBufForChunk was handled.
	//
	// The accumulator never auto-pools allocations from the standard
	// allocator: a nil pool keeps the legacy GC-managed behaviour
	// (used by unit tests and the H2 codec read paths that don't
	// pass through SHM).
	pool mem.BufferPool
}

// inProgress reports whether the accumulator is mid-message.
func (a *lpmAccumulator) inProgress() bool {
	return a.headerBytesSeen > 0 || a.pos > 0
}

// feed consumes data from a DATA frame body. Returns:
//   - msg != nil: a complete LPM (header+body) is ready; caller should
//     emit it as a MESSAGE frame. The returned slice is a fresh allocation
//     owned by the caller.
//   - leftover: bytes from data not yet consumed (start of the next LPM
//     or partial header). The caller should re-feed leftover on the next
//     iteration if it's non-empty. (This is rare in practice — a single
//     DATA frame typically carries exactly one LPM.)
//   - err: protocol violation (e.g., LPM body length exceeds maxBody).
//
// maxBody is the maximum allowed body length (unencoded) — payload too
// large is rejected per gRPC's MaxReceiveMessageSize semantics. Pass 0
// to disable the size cap (enforced upstream).
func (a *lpmAccumulator) feed(data []byte, maxBody int) (msg []byte, leftover []byte, err error) {
	// Phase 1: complete the LPM header if not yet parsed.
	if a.headerBytesSeen < 5 {
		need := 5 - a.headerBytesSeen
		if len(data) < need {
			n := copy(a.headerBuf[a.headerBytesSeen:], data)
			a.headerBytesSeen += n
			return nil, nil, nil
		}
		copy(a.headerBuf[a.headerBytesSeen:], data[:need])
		a.headerBytesSeen = 5
		data = data[need:]

		bodyLen := int(binary.BigEndian.Uint32(a.headerBuf[1:5]))
		if bodyLen < 0 {
			// Reset header-parse state so a subsequent feed call (in
			// principle: caller treats this as connection-fatal, but
			// defense-in-depth) doesn't silently mis-parse on stale
			// state.
			a.headerBytesSeen = 0
			return nil, nil, errors.New("h2 LPM: negative body length")
		}
		if maxBody > 0 && bodyLen > maxBody {
			a.headerBytesSeen = 0
			return nil, nil, fmt.Errorf("h2 LPM: body length %d exceeds max %d", bodyLen, maxBody)
		}
		a.expectedTotal = 5 + bodyLen
		// Allocate incrementally rather than to the full declared
		// expectedTotal: a peer-controlled tiny DATA frame that
		// declares hundreds of MiB in the LPM header would otherwise
		// force an immediate huge allocation before any per-RPC
		// receive limit can reject it. Append below grows the slice
		// amortised-O(N) on actual received bytes; the cap above
		// (maxBody) provides a hard upper bound on declared size, but
		// we never trust it for peer-declared size; we DO trust it
		// for actually-received bytes (which are bounded by ring
		// reservations and the maxBody cap already applied above).
		//
		// Initial cap is min(expectedTotal, max(8 KiB, 5+len(data))):
		//   * Small messages (≤ 8 KiB total): cap = expectedTotal,
		//     no grow ever needed.
		//   * Large message arriving in one chunk (typical for
		//     ZC-eligible MESSAGE bodies up to 16 MiB-1): cap =
		//     5+body length, sized to fit the data we're about to
		//     append without any grow-realloc.
		//   * DoS attempt (peer declares 511 MiB but sends 1024
		//     bytes): cap = 8 KiB, only what was received-or-headroom
		//     can be allocated. The 256 KiB DoS-bound asserted by
		//     TestH2LPM_NoPreallocOversized is preserved.
		//     largeFirstChunkMultiplier × first-chunk body bytes is
		//     used as the pre-alloc hint: a 16 KiB first chunk grants
		//     1 MiB up-front (matches a typical fair-default 1 MiB
		//     LPM where MAX_FRAME_SIZE limits each DATA frame to
		//     16 KiB), while a 1 KiB DoS chunk only grants 64 KiB,
		//     well under the 256 KiB bound asserted by
		//     TestH2LPM_NoPreallocOversized.
		const initialBufHint = 8 * 1024
		const largeFirstChunkMultiplier = 64
		initialCap := initialBufHint
		if 5+len(data) > initialCap {
			initialCap = 5 + len(data)
		}
		if hint := largeFirstChunkMultiplier * len(data); hint > initialCap {
			initialCap = hint
		}
		if initialCap > a.expectedTotal {
			initialCap = a.expectedTotal
		}
		a.buf = a.allocBuf(initialCap)
		a.buf = append(a.buf, a.headerBuf[:]...)
		a.pos = 5
	}

	// Phase 2: append body bytes.
	//
	// For mid-message chunks that would overflow the current buffer
	// capacity, we explicitly grow to min(expectedTotal, 2*cap)
	// instead of relying on Go's default 1.25× slice growth factor.
	// This bounds total grow-copy work to ~2× the final buffer size
	// (vs. ~4× under the default factor) for large LPMs that span
	// many H2 DATA frames, materially improving 16 MiB-plus message
	// throughput on the receive side.
	//
	// DoS safety: cap can only double when the new ceiling is
	// expectedTotal or 2× the previous cap. Since the previous cap
	// was bounded by max(initialHint, 5+bytes received so far), the
	// new cap is bounded by 2× bytes received. A peer streaming
	// 1-byte chunks against a 511 MiB declared body therefore still
	// gets only O(received) allocation, not O(declared).
	remaining := a.expectedTotal - a.pos
	if remaining > len(data) {
		if a.pos+len(data) > cap(a.buf) {
			a.growBufForChunk(len(data))
		}
		a.buf = append(a.buf, data...)
		a.pos += len(data)
		return nil, nil, nil
	}

	// Complete the message.
	if a.pos+remaining > cap(a.buf) {
		a.growBufForChunk(remaining)
	}
	a.buf = append(a.buf, data[:remaining]...)
	a.pos += remaining
	leftover = data[remaining:]
	msg = a.buf

	// Reset for the next message.
	a.headerBytesSeen = 0
	a.expectedTotal = 0
	a.pos = 0
	a.buf = nil

	return msg, leftover, nil
}

// growBufForChunk grows a.buf to accommodate at least `need` additional
// bytes. The new capacity is min(expectedTotal, max(2*cap, pos+need)),
// so each grow at least doubles cap (faster convergence than the
// default 1.25× slice growth) while never exceeding the known final
// size. The 2× doubling keeps cap bounded by 2× bytes received, which
// preserves the DoS-bound asserted by TestH2LPM_NoPreallocOversized.
func (a *lpmAccumulator) growBufForChunk(need int) {
	newCap := 2 * cap(a.buf)
	if newCap < a.pos+need {
		newCap = a.pos + need
	}
	if newCap > a.expectedTotal {
		newCap = a.expectedTotal
	}
	if newCap <= cap(a.buf) {
		// No grow needed (caller already checked, but be safe).
		return
	}
	newBuf := a.allocBuf(newCap)[:len(a.buf)]
	copy(newBuf, a.buf)
	a.releaseBuf(a.buf)
	a.buf = newBuf
}

// allocBuf returns a slice with len=0 cap>=size. When a.pool is set,
// the slice is taken from the pool (avoiding the runtime.memclr that
// `make([]byte, 0, size)` would perform on a fresh allocation). The
// returned slice's cap may exceed size since the binary-tiered pool
// rounds up to the next power of two; callers MUST honour cap when
// computing further allocations.
func (a *lpmAccumulator) allocBuf(size int) []byte {
	if a.pool == nil {
		return make([]byte, 0, size)
	}
	buf := a.pool.Get(size)
	// pool returns *[]byte; len may be ≥ size depending on rounding.
	return (*buf)[:0:cap(*buf)]
}

// releaseBuf returns a slice to a.pool. No-op when pool is nil or
// when the slice was never pool-rooted (cap < pool's minimum tier).
// The pool tolerates either case via its sizedBufferPool fallback.
func (a *lpmAccumulator) releaseBuf(buf []byte) {
	if a.pool == nil || cap(buf) == 0 {
		return
	}
	b := buf[:0]
	a.pool.Put(&b)
}

// feedSplit is the two-slice analogue of feed: it consumes data from
// pFirst followed by pSecond in one accumulation pass. Used by the H2
// reader when a ring-backed payload straddles the ring's wrap boundary
// (a common case for large DATA frames near ring capacity).
//
// Why this exists: the caller's alternative is to materialize a
// contiguous heap slice via make+copy+copy before calling feed. For a
// 16 MiB first chunk of a multi-frame LPM, that intermediate
// allocation costs one extra 16 MiB heap allocation and one extra
// 16 MiB memcpy per first chunk. feedSplit copies pFirst and pSecond
// directly into a.buf, eliminating the intermediate.
//
// Semantics match feed: returns the assembled message when the LPM
// completes within (pFirst, pSecond), with leftover bytes from the
// tail of pSecond returned to the caller for next-frame replay.
func (a *lpmAccumulator) feedSplit(pFirst, pSecond []byte, maxBody int) (msg, leftover []byte, err error) {
	// Phase 1: complete the LPM header if not yet parsed. Handle the
	// uncommon case where the 5-byte header straddles pFirst/pSecond
	// or where pFirst alone is shorter than the bytes still needed.
	if a.headerBytesSeen < 5 {
		need := 5 - a.headerBytesSeen
		// Drain bytes for the header from pFirst, then pSecond.
		got := 0
		if got < need && len(pFirst) > 0 {
			n := copy(a.headerBuf[a.headerBytesSeen:], pFirst)
			a.headerBytesSeen += n
			pFirst = pFirst[n:]
			got += n
		}
		if got < need && len(pSecond) > 0 {
			n := copy(a.headerBuf[a.headerBytesSeen:], pSecond)
			a.headerBytesSeen += n
			pSecond = pSecond[n:]
			got += n
		}
		if a.headerBytesSeen < 5 {
			return nil, nil, nil
		}

		bodyLen := int(binary.BigEndian.Uint32(a.headerBuf[1:5]))
		if bodyLen < 0 {
			a.headerBytesSeen = 0
			return nil, nil, errors.New("h2 LPM: negative body length")
		}
		if maxBody > 0 && bodyLen > maxBody {
			a.headerBytesSeen = 0
			return nil, nil, fmt.Errorf("h2 LPM: body length %d exceeds max %d", bodyLen, maxBody)
		}
		a.expectedTotal = 5 + bodyLen
		// Initial cap matches feed's Fix-#2 sizing: max(8 KiB, 5+body
		// bytes in this chunk), bounded by expectedTotal. The 5
		// accounts for the LPM header that will live in a.buf; the
		// remaining capacity covers the body bytes about to be
		// appended. Bytes after header consumption represent body
		// only, so we add 5 explicitly here.
		//
		// Large-body fast path: scale the pre-alloc cap by the
		// first-chunk body size (64× multiplier, clamped to
		// expectedTotal). This skips the cascade of doubling-realloc
		// work (16 KiB → 32 KiB → … copying every time) on the
		// receive side of multi-frame messages, which the CPU
		// profile of fair-default 1 MiB streaming shows as the
		// dominant cost (lpmAccumulator.growBufForChunk ~13% of
		// total). A peer attacking with a tiny chunk (e.g., 1 KiB)
		// against a huge declared body length still hits the small-
		// allocation path and is bounded by Fix #2's "received-so-
		// far + 8 KiB" cap. maxBody (caller-supplied, typically
		// 511 MiB) is the absolute hard ceiling on expectedTotal so
		// the upfront alloc cannot exceed it.
		bodyBytes := len(pFirst) + len(pSecond)
		const initialBufHint = 8 * 1024
		const largeFirstChunkMultiplier = 64
		initialCap := initialBufHint
		if 5+bodyBytes > initialCap {
			initialCap = 5 + bodyBytes
		}
		if hint := largeFirstChunkMultiplier * bodyBytes; hint > initialCap {
			initialCap = hint
		}
		if initialCap > a.expectedTotal {
			initialCap = a.expectedTotal
		}
		a.buf = a.allocBuf(initialCap)
		a.buf = append(a.buf, a.headerBuf[:]...)
		a.pos = 5
	}

	// Phase 2: append body bytes from pFirst then pSecond. Mirror the
	// grow behavior of feed but applied across the two slices in
	// sequence so the worst-case capacity tracks bytes actually
	// observed (DoS bound preserved).
	srcs := [2][]byte{pFirst, pSecond}
	for i, src := range srcs {
		if len(src) == 0 {
			continue
		}
		remaining := a.expectedTotal - a.pos
		if remaining >= len(src) {
			if a.pos+len(src) > cap(a.buf) {
				a.growBufForChunk(len(src))
			}
			a.buf = append(a.buf, src...)
			a.pos += len(src)
			continue
		}
		// src contains the tail of the message AND leftover for the
		// next message. Take only `remaining` bytes; stash the rest.
		if a.pos+remaining > cap(a.buf) {
			a.growBufForChunk(remaining)
		}
		a.buf = append(a.buf, src[:remaining]...)
		a.pos += remaining
		tail := src[remaining:]
		// Leftover assembly: bytes left in this src plus any
		// subsequent src not yet visited.
		if i == 0 && len(srcs[1]) > 0 {
			// We were iterating pFirst; pSecond is still untouched.
			if len(tail) == 0 {
				leftover = srcs[1]
			} else {
				leftover = make([]byte, 0, len(tail)+len(srcs[1]))
				leftover = append(leftover, tail...)
				leftover = append(leftover, srcs[1]...)
			}
		} else {
			leftover = tail
		}
		msg = a.buf
		// Reset for the next message.
		a.headerBytesSeen = 0
		a.expectedTotal = 0
		a.pos = 0
		a.buf = nil
		return msg, leftover, nil
	}

	// All bytes consumed; check whether the message is complete.
	if a.pos == a.expectedTotal && a.expectedTotal > 0 {
		msg = a.buf
		a.headerBytesSeen = 0
		a.expectedTotal = 0
		a.pos = 0
		a.buf = nil
		return msg, nil, nil
	}
	return nil, nil, nil
}
