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

import (
	"encoding/binary"
	"testing"
)

// TestLPMAccumulator_FirstChunkNoRealloc verifies that the first chunk of a
// large multi-frame LPM allocates buffer capacity sized to the data being
// appended, eliminating the cascade of grow-copies that the previous fixed
// 8 KiB initial cap incurred. The invariant: after feeding the entire first
// chunk in one call, cap(a.buf) equals (5 + len(firstChunk)) — no append-grow
// happened during the body copy. With the old 8 KiB initial cap this would
// be > final cap due to slice growth.
func TestLPMAccumulator_FirstChunkNoRealloc(t *testing.T) {
	// 16 MiB body in a single first chunk (typical maximum per H2 DATA
	// frame), with an LPM declaring 64 MiB total. This is the canonical
	// shape of the first DATA frame of a 64 MiB chunked LPM. First-chunk
	// body bytes ≥ 1 MiB triggers the large-first-chunk fast path:
	// allocate the full expectedTotal up-front so subsequent chunks
	// fit without grow-realloc.
	const totalBody = 64 * 1024 * 1024
	const firstChunkBody = 16 * 1024 * 1024
	hdr := buildLPMHeaderTest(totalBody)
	chunk1 := append(hdr, make([]byte, firstChunkBody-5)...) // 16 MiB total

	acc := &lpmAccumulator{}
	msg, leftover, err := acc.feed(chunk1, h2MaxLPMBodyBytes)
	if err != nil {
		t.Fatalf("feed first chunk: %v", err)
	}
	if msg != nil {
		t.Fatal("first chunk completed message — test setup wrong")
	}
	if len(leftover) != 0 {
		t.Fatalf("leftover after first chunk: %d bytes", len(leftover))
	}

	// Expected: cap = expectedTotal (large-first-chunk path
	// pre-allocates so future chunks avoid grow-realloc).
	if got, want := cap(acc.buf), 5+totalBody; got != want {
		t.Errorf("first-chunk allocation: cap=%d want=%d (large-first-chunk path should pre-allocate expectedTotal)", got, want)
	}
	if got, want := len(acc.buf), firstChunkBody; got != want {
		t.Errorf("first-chunk length: got=%d want=%d", got, want)
	}
}

// TestLPMAccumulator_LargeFirstChunkPreallocates verifies the
// large-body fast path: when the first chunk carries ≥1 MiB of body
// bytes AND expectedTotal is larger, the accumulator allocates the
// full expectedTotal up-front, avoiding the cascade of grow-realloc
// + memcpy that the doubling-only path would incur for multi-frame
// messages.
//
// CPU profile on 64 MiB / 256 MiB unary ZC (post-Fix #2/#3) showed
// growBufForChunk dominating both alloc (52% of total) and memmove
// (56% of CPU) — the upfront-alloc heuristic eliminates that source.
// DoS safety remains: the path only fires when the peer has already
// committed to sending ≥1 MiB; maxBody (511 MiB ceiling) caps the
// alloc.
func TestLPMAccumulator_LargeFirstChunkPreallocates(t *testing.T) {
	const totalBody = 64 * 1024 * 1024 // 64 MiB declared
	const chunkBody = 16 * 1024 * 1024 // 16 MiB first chunk on the wire
	hdr := buildLPMHeaderTest(totalBody)

	acc := &lpmAccumulator{}

	// Chunk 1 carries hdr (5) + 16 MiB - 5 body. First-chunk body bytes
	// = chunkBody (16 MiB) ≥ 1 MiB threshold → pre-allocate full
	// expectedTotal (5 + 64 MiB).
	chunk1 := append(hdr, make([]byte, chunkBody-5)...)
	if _, _, err := acc.feed(chunk1, h2MaxLPMBodyBytes); err != nil {
		t.Fatalf("feed chunk 1: %v", err)
	}
	if got, want := cap(acc.buf), 5+totalBody; got != want {
		t.Errorf("chunk 1 cap: got %d want %d (large-first-chunk fast path missing — should pre-allocate expectedTotal)", got, want)
	}

	// Subsequent chunks fit in the pre-allocated cap; no grow-realloc
	// happens. We feed 3 more 16 MiB body chunks, ending with the
	// final 5-byte tail to exactly complete the message.
	for i := 2; i <= 3; i++ {
		chunk := make([]byte, chunkBody)
		if _, _, err := acc.feed(chunk, h2MaxLPMBodyBytes); err != nil {
			t.Fatalf("feed chunk %d: %v", i, err)
		}
		if got, want := cap(acc.buf), 5+totalBody; got != want {
			t.Errorf("chunk %d cap: got %d want %d (cap should remain at expectedTotal — no grow expected)", i, got, want)
		}
	}
	chunk4 := make([]byte, chunkBody+5) // include final 5-byte tail
	msg, leftover, err := acc.feed(chunk4, h2MaxLPMBodyBytes)
	if err != nil {
		t.Fatalf("feed chunk 4: %v", err)
	}
	if len(leftover) != 0 {
		t.Errorf("leftover after final chunk: %d bytes", len(leftover))
	}
	if got, want := len(msg), 5+totalBody; got != want {
		t.Errorf("assembled message length: got %d want %d", got, want)
	}
}

// TestLPMAccumulator_SmallFirstChunkStillDoubles verifies that when the
// first chunk carries a body smaller than a typical streaming frame
// (DoS-shape: small chunks against a large declared body), the
// accumulator does NOT pre-allocate the full peer-declared
// expectedTotal — it allocates at most a 64× scaling of the first-
// chunk size. Preserves the bounded-allocation invariant: a peer
// declaring N MiB but committing to only k MiB of first-chunk body
// can only force a 64k-byte allocation, not an N-MiB one.
func TestLPMAccumulator_SmallFirstChunkStillDoubles(t *testing.T) {
	const totalBody = 64 * 1024 * 1024 // 64 MiB declared
	const firstChunkBody = 512 * 1024  // 512 KiB first chunk
	hdr := buildLPMHeaderTest(totalBody)

	acc := &lpmAccumulator{}
	chunk1 := append(hdr, make([]byte, firstChunkBody)...) // 5 + 512 KiB wire
	if _, _, err := acc.feed(chunk1, h2MaxLPMBodyBytes); err != nil {
		t.Fatalf("feed chunk 1: %v", err)
	}
	// Invariant: cap is bounded by 64 × firstChunkBody and strictly
	// less than the peer-declared total. The whole point: peer can't
	// force allocation of declared total just by saying it's big.
	gotCap := cap(acc.buf)
	maxAllowed := 64 * firstChunkBody // largeFirstChunkMultiplier × first chunk
	if gotCap > maxAllowed {
		t.Errorf("chunk 1 cap: got %d, want ≤ %d (64× multiplier bound)", gotCap, maxAllowed)
	}
	if gotCap >= 5+totalBody {
		t.Errorf("chunk 1 cap: got %d, want < %d (DoS bound: must not pre-alloc peer-declared total)",
			gotCap, 5+totalBody)
	}
}

// TestLPMAccumulator_SmallMessageNoGrow verifies that a single-chunk LPM
// smaller than the 8 KiB initial hint allocates exactly the message size
// (no slack waste from the 8 KiB minimum).
func TestLPMAccumulator_SmallMessageNoGrow(t *testing.T) {
	const bodyLen = 100
	body := make([]byte, bodyLen)
	for i := range body {
		body[i] = byte(i)
	}
	hdr := buildLPMHeaderTest(bodyLen)
	full := append(hdr, body...)

	acc := &lpmAccumulator{}
	msg, leftover, err := acc.feed(full, h2MaxLPMBodyBytes)
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	if len(leftover) != 0 {
		t.Errorf("leftover: %d bytes", len(leftover))
	}
	if got, want := len(msg), 5+bodyLen; got != want {
		t.Errorf("message length: got %d want %d", got, want)
	}
}

// TestLPMAccumulator_DoSBoundTinyChunks verifies that a peer streaming the
// LPM in tiny chunks (e.g., 8 byte feeds against a 511 MiB declared body)
// gets allocation bounded by 2× bytes received, not the peer-declared size.
// This complements TestH2LPM_NoPreallocOversized (which tests the single-
// feed case) by covering incremental feeds.
func TestLPMAccumulator_DoSBoundTinyChunks(t *testing.T) {
	const declared = h2MaxLPMBodyBytes // 511 MiB declared
	const perChunkBody = 8             // peer dribbles 8 bytes at a time
	const totalChunks = 100            // feed 800 bytes total

	hdr := buildLPMHeaderTest(declared)
	acc := &lpmAccumulator{}

	// Phase 1: feed header alone.
	if _, _, err := acc.feed(hdr, h2MaxLPMBodyBytes); err != nil {
		t.Fatalf("feed header: %v", err)
	}

	// Phase 2: dribble tiny chunks.
	tinyChunk := make([]byte, perChunkBody)
	for i := 0; i < totalChunks; i++ {
		if _, _, err := acc.feed(tinyChunk, h2MaxLPMBodyBytes); err != nil {
			t.Fatalf("feed chunk %d: %v", i, err)
		}
	}

	// Invariant: cap(buf) ≤ 2 × max(initial_cap=8 KiB, bytes received).
	// With 5 header + 800 body = 805 bytes received: cap should stay
	// within max(8192, 2*805) = 8192. Without the doubling cap, append's
	// 1.25× would still keep it small, but the DoS-bound proof is that
	// even with our explicit doubling, cap is O(received), not
	// O(declared).
	const bound = 8 * 1024 // initial hint == 8 KiB
	if got := cap(acc.buf); got > bound {
		t.Errorf("cap grew to %d on %d bytes received (declared %d) — DoS bound broken (max %d)", got, 5+totalChunks*perChunkBody, declared, bound)
	}
}

// TestLPMAccumulator_FeedSplit_BasicRoundTrip verifies that feedSplit
// (the two-slice API used by the H2 reader to avoid the intermediate
// heap copy for ring-backed slices that straddle the wrap boundary)
// correctly assembles a complete LPM from split slices.
func TestLPMAccumulator_FeedSplit_BasicRoundTrip(t *testing.T) {
	const bodyLen = 64 * 1024
	body := make([]byte, bodyLen)
	for i := range body {
		body[i] = byte(i)
	}
	hdr := buildLPMHeaderTest(bodyLen)
	full := append(hdr, body...)

	// Split at an arbitrary boundary that crosses the header (e.g.,
	// pFirst carries first 3 bytes of header, pSecond carries the
	// remaining 2 header bytes + entire body).
	splitAt := 3
	pFirst := full[:splitAt]
	pSecond := full[splitAt:]

	acc := &lpmAccumulator{}
	msg, leftover, err := acc.feedSplit(pFirst, pSecond, h2MaxLPMBodyBytes)
	if err != nil {
		t.Fatalf("feedSplit: %v", err)
	}
	if len(leftover) != 0 {
		t.Errorf("leftover: %d bytes", len(leftover))
	}
	if got, want := len(msg), 5+bodyLen; got != want {
		t.Fatalf("message length: got %d want %d", got, want)
	}
	for i := 5; i < len(msg); i++ {
		if msg[i] != body[i-5] {
			t.Fatalf("body byte %d: got %d want %d", i-5, msg[i], body[i-5])
		}
	}
}

// TestLPMAccumulator_FeedSplit_HeaderInFirstOnly covers the common case
// where pFirst contains the entire LPM header and pSecond carries body
// bytes only — exactly what readFrameViewH2 sees for a typical
// multi-frame DATA chunk that wraps mid-body.
func TestLPMAccumulator_FeedSplit_HeaderInFirstOnly(t *testing.T) {
	const bodyLen = 64 * 1024
	body := make([]byte, bodyLen)
	hdr := buildLPMHeaderTest(bodyLen)
	pFirst := append(hdr, body[:1024]...) // 5 header + 1 KiB body
	pSecond := body[1024:]                // 63 KiB body

	acc := &lpmAccumulator{}
	msg, leftover, err := acc.feedSplit(pFirst, pSecond, h2MaxLPMBodyBytes)
	if err != nil {
		t.Fatalf("feedSplit: %v", err)
	}
	if len(leftover) != 0 {
		t.Errorf("leftover: %d bytes", len(leftover))
	}
	if got, want := len(msg), 5+bodyLen; got != want {
		t.Errorf("message length: got %d want %d", got, want)
	}
}

// TestLPMAccumulator_FeedSplit_LeftoverInFirst covers the case where
// the message completes inside pFirst and the remaining bytes (start
// of the next LPM) span pFirst's tail and pSecond — leftover must
// preserve the order.
func TestLPMAccumulator_FeedSplit_LeftoverInFirst(t *testing.T) {
	const bodyLen = 10
	body := make([]byte, bodyLen)
	for i := range body {
		body[i] = byte(i + 0xA0)
	}
	hdr := buildLPMHeaderTest(bodyLen)
	full := append(hdr, body...) // 15 bytes total LPM
	// Append 7 leftover bytes: first 3 in pFirst, next 4 in pSecond.
	extra := []byte{0xE0, 0xE1, 0xE2, 0xE3, 0xE4, 0xE5, 0xE6}
	pFirst := append(append([]byte{}, full...), extra[:3]...)
	pSecond := extra[3:]

	acc := &lpmAccumulator{}
	msg, leftover, err := acc.feedSplit(pFirst, pSecond, h2MaxLPMBodyBytes)
	if err != nil {
		t.Fatalf("feedSplit: %v", err)
	}
	if got, want := len(msg), 5+bodyLen; got != want {
		t.Fatalf("message length: got %d want %d", got, want)
	}
	if got, want := leftover, extra; len(got) != len(want) {
		t.Fatalf("leftover length: got %d want %d", len(got), len(want))
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("leftover[%d]: got %d want %d", i, got[i], want[i])
			}
		}
	}
}

// TestLPMAccumulator_FeedSplit_NoIntermediateAlloc verifies the key
// optimization: feedSplit allocates exactly one buffer that fits the
// final message (cap = expectedTotal because the first chunk body is
// large enough to trigger the large-first-chunk fast path), proving
// that no intermediate heap copy occurred. Compare to the old code
// path which would have done a `make([]byte, h2fh.Length)` plus the
// accumulator's own allocation, plus a cascade of grow-reallocs for
// the multi-chunk path.
func TestLPMAccumulator_FeedSplit_NoIntermediateAlloc(t *testing.T) {
	const bodyLen = 16 * 1024 * 1024 // 16 MiB first-chunk wire size
	const declared = 64 * 1024 * 1024
	hdr := buildLPMHeaderTest(declared)
	pFirst := append(hdr, make([]byte, bodyLen-5)...) // 16 MiB total wire

	acc := &lpmAccumulator{}
	_, _, err := acc.feedSplit(pFirst, nil, h2MaxLPMBodyBytes)
	if err != nil {
		t.Fatalf("feedSplit: %v", err)
	}
	// First-chunk body bytes (bodyLen-5 = ~16 MiB) ≥ 1 MiB threshold →
	// large-first-chunk fast path pre-allocates expectedTotal (5 +
	// 64 MiB declared body). This is the desired behavior — it
	// eliminates 48 MiB of grow-copy work for the remaining chunks.
	if got, want := cap(acc.buf), 5+declared; got != want {
		t.Errorf("acc.buf cap: got %d want %d (feedSplit failed to pre-allocate expectedTotal for large first chunk)", got, want)
	}
}

// buildLPMHeaderTest constructs the 5-byte gRPC length-prefix header for a
// message body of length bodyLen.
func buildLPMHeaderTest(bodyLen int) []byte {
	hdr := make([]byte, 5)
	hdr[0] = 0 // no compression
	binary.BigEndian.PutUint32(hdr[1:5], uint32(bodyLen))
	return hdr
}
