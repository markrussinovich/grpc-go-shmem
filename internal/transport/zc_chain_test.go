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
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/mem"
)

// TestZcDeferredPublish_SingleFrame verifies that single-frame ZC reads
// freeze header.ReadIdx until the consumer frees the buffer.
func TestZcDeferredPublish_SingleFrame(t *testing.T) {
	ctx := context.Background()
	segName := fmt.Sprintf("zcdef-sf-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, 4*1024*1024, 4*1024*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	// Body large enough to satisfy IsSpeculativeZCEligible (>= 64 KiB on
	// 4 MiB ring). We send a MESSAGE frame whose payload is a gRPC LPM
	// (5-byte header + body).
	bodyLen := 200 * 1024
	payload := make([]byte, 5+bodyLen)
	payload[0] = 0
	binary.BigEndian.PutUint32(payload[1:5], uint32(bodyLen))
	for i := 0; i < bodyLen; i++ {
		payload[5+i] = byte(i)
	}

	if err := writeFrame(ctx, tx, FrameHeader{
		Type: FrameTypeMESSAGE, StreamID: 1,
	}, payload); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	// Capture readIdx BEFORE the read.
	hdr := rx.header()
	readIdxBefore := hdr.ReadIndex()

	fh, buf, err := readFrameView(ctx, rx)
	if err != nil {
		t.Fatalf("readFrameView: %v", err)
	}
	if fh.Type != FrameTypeMESSAGE {
		t.Fatalf("type: got %v want MESSAGE", fh.Type)
	}

	// While buf is held, header.ReadIdx must be at the frame-header
	// boundary (advanced by the 9-byte H2 frame header commit) but NOT past
	// the payload (frozen at baseZc + h2FrameHeaderSize).
	readIdxDuring := hdr.ReadIndex()
	if readIdxDuring != readIdxBefore+h2FrameHeaderSize {
		t.Errorf("readIdx during ZC: got %d want %d (= before %d + headerSize %d)",
			readIdxDuring, readIdxBefore+h2FrameHeaderSize, readIdxBefore, h2FrameHeaderSize)
	}
	if !rx.IsZcChainActive() {
		t.Error("expected zcActive=1 while ZC buffer is held")
	}

	// Verify buffer points to the message bytes.
	got := buf.ReadOnlyData()
	if len(got) != len(payload) {
		t.Errorf("buf len: got %d want %d", len(got), len(payload))
	}

	// Free the buffer — should trigger EndZcReservation, publishing
	// the deferred target to header.ReadIdx.
	buf.Free()

	if rx.IsZcChainActive() {
		t.Error("expected zcActive=0 after buffer Free")
	}
	expectedReadIdxAfter := readIdxBefore + h2FrameHeaderSize + uint64(len(payload))
	if got := hdr.ReadIndex(); got != expectedReadIdxAfter {
		t.Errorf("readIdx after Free: got %d want %d", got, expectedReadIdxAfter)
	}
}

// TestZcDeferredPublish_DeferredCommit verifies that copy-path frames
// committed during a held ZC buffer are accumulated into zcDeferredTarget
// and published only when the ZC buffer is freed.
func TestZcDeferredPublish_DeferredCommit(t *testing.T) {
	ctx := context.Background()
	segName := fmt.Sprintf("zcdef-dc-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, 4*1024*1024, 4*1024*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	// Frame 1: large MESSAGE body that goes ZC.
	bodyLen := 200 * 1024
	payload1 := make([]byte, 5+bodyLen)
	payload1[0] = 0
	binary.BigEndian.PutUint32(payload1[1:5], uint32(bodyLen))
	if err := writeFrame(ctx, tx, FrameHeader{
		Type: FrameTypeMESSAGE, StreamID: 1,
	}, payload1); err != nil {
		t.Fatalf("writeFrame 1: %v", err)
	}

	// Frame 2: small message that goes copy.
	payload2 := []byte{0, 0, 0, 0, 4, 1, 2, 3, 4} // LPM of 4 body bytes
	if err := writeFrame(ctx, tx, FrameHeader{
		Type: FrameTypeMESSAGE, StreamID: 3,
	}, payload2); err != nil {
		t.Fatalf("writeFrame 2: %v", err)
	}

	hdr := rx.header()
	readIdxBefore := hdr.ReadIndex()

	// Read frame 1 — gets ZC buffer.
	_, buf1, err := readFrameView(ctx, rx)
	if err != nil {
		t.Fatalf("readFrameView 1: %v", err)
	}
	if !rx.IsZcChainActive() {
		t.Fatal("expected zcActive=1 after first ZC read")
	}

	// Read frame 2 — copy path. Its commit goes deferred.
	_, buf2, err := readFrameView(ctx, rx)
	if err != nil {
		t.Fatalf("readFrameView 2: %v", err)
	}

	// header.ReadIdx must STILL be at the post-frame-1-header position
	// (frame 1's payload + frame 2's bytes are deferred).
	expectedFrozen := readIdxBefore + h2FrameHeaderSize
	if got := hdr.ReadIndex(); got != expectedFrozen {
		t.Errorf("readIdx during ZC hold: got %d want %d", got, expectedFrozen)
	}

	// Free the small copy buffer first — should NOT advance readIdx
	// because zcActive is still 1 and the chain isn't closed.
	buf2.Free()
	if got := hdr.ReadIndex(); got != expectedFrozen {
		t.Errorf("readIdx after copy buf Free: got %d want %d (still frozen)", got, expectedFrozen)
	}

	// Free the ZC buffer — fires EndZcReservation, publishing the
	// accumulated target which includes both frames.
	buf1.Free()

	if rx.IsZcChainActive() {
		t.Error("expected zcActive=0 after ZC buffer Free")
	}
	expectedAfter := readIdxBefore + 2*h2FrameHeaderSize + uint64(len(payload1)) + uint64(len(payload2))
	if got := hdr.ReadIndex(); got != expectedAfter {
		t.Errorf("readIdx after ZC Free: got %d want %d", got, expectedAfter)
	}
}

// TestZcChainReleasePool_DecrementOnly verifies that
// zcChainReleasePool.Put decrements zcInFlight without firing
// EndZcReservation while chainOpen=1 (mid-chain) and fires it when
// the chain is closed.
func TestZcChainReleasePool_DecrementOnly(t *testing.T) {
	ctx := context.Background()
	segName := fmt.Sprintf("zcpool-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, 4*1024*1024, 4*1024*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	// Manually open a chain and increment inflight twice.
	rx.BeginZcReservation(0)
	rx.OpenZcChain()
	rx.AddChainZcInFlight()
	rx.AddChainZcInFlight()

	pool := &zcChainReleasePool{ring: rx}

	// Free first buffer — inflight goes 2→1; chain still open → no End.
	dummy := make([]byte, 4)
	pool.Put(&dummy)
	if !rx.IsZcChainActive() {
		t.Fatal("zcActive cleared too early — first Free with chain open should not fire EndZc")
	}

	// Close the chain.
	rx.CloseZcChain()

	// Free second buffer — inflight goes 1→0; chain closed → End fires.
	pool.Put(&dummy)
	if rx.IsZcChainActive() {
		t.Fatal("zcActive should be cleared after final Free")
	}
	_ = ctx
}

// TestChainZc_BudgetReject verifies that messages exceeding
// ChainZcBudget (cap/2) fall through to the copy path.
func TestChainZc_BudgetReject(t *testing.T) {
	// The Custom16 wire used a MORE flag to express "this MESSAGE frame
	// continues a logical LPM split across multiple SHM frames", which
	// is what drives the ring-level multi-frame chain ZC machinery this
	// test exercises. Under H2-only, multi-frame LPMs are reassembled by
	// the H2 codec's lpmAccumulator (which copies into a heap buffer) and
	// the upper transport sees exactly one MESSAGE per logical LPM, so
	// the SHM-level chain-ZC budget reject path does not get activated
	// the same way. The single-frame ZC behaviour is still covered by
	// TestZcDeferredPublish_SingleFrame and TestZcDeferredPublish_DeferredCommit.
	t.Skip("chain-ZC budget reject is Custom16-MORE specific; not reachable through the H2 LPM accumulator")
	if !memBufferPoolingAvailable() {
		t.Skip("mem.Buffer pooling not available")
	}
	ctx := context.Background()
	segName := fmt.Sprintf("zcbud-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	// 4 MiB ring → ChainZcBudget = 2 MiB.
	seg, err := CreateSegment(segName, 4*1024*1024, 4*1024*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	// Two-chunk message totaling 3 MiB > 2 MiB budget. Send chunk 1
	// with MORE, chunk 2 without.
	chunkSize := 1500 * 1024
	totalBody := chunkSize * 2
	chunk1 := make([]byte, chunkSize)
	chunk1[0] = 0
	binary.BigEndian.PutUint32(chunk1[1:5], uint32(totalBody-5)) // body length excluding LPM header
	chunk2 := make([]byte, chunkSize)

	if err := writeFrame(ctx, tx, FrameHeader{
		Type: FrameTypeMESSAGE, StreamID: 1, Flags: MessageFlagMORE,
	}, chunk1); err != nil {
		t.Fatalf("writeFrame chunk1: %v", err)
	}
	if err := writeFrame(ctx, tx, FrameHeader{
		Type: FrameTypeMESSAGE, StreamID: 1,
	}, chunk2); err != nil {
		t.Fatalf("writeFrame chunk2: %v", err)
	}

	// Read both chunks. With totalMsg > budget, codec should reject
	// chain ZC and stay in chain-copy mode for the whole message.
	_, buf1, err := readFrameView(ctx, rx)
	if err != nil {
		t.Fatalf("readFrameView 1: %v", err)
	}
	if rx.IsZcChainActive() {
		t.Error("expected chain ZC rejected (over budget) — zcActive should be 0")
	}
	if !rx.ChainCopyMode() {
		t.Error("expected chainCopyMode=1 after rejection")
	}
	buf1.Free()

	_, buf2, err := readFrameView(ctx, rx)
	if err != nil {
		t.Fatalf("readFrameView 2: %v", err)
	}
	if rx.ChainCopyMode() {
		t.Error("expected chainCopyMode cleared on final chunk")
	}
	buf2.Free()
}

// memBufferPoolingAvailable reports whether mem.NewBuffer pools the
// passed-in slice or returns a SliceBuffer (if cap is below threshold).
// Used to skip tests that rely on Buffer.Free triggering pool callbacks.
func memBufferPoolingAvailable() bool {
	b := make([]byte, 4096)
	buf := mem.NewBuffer(&b, mem.DefaultBufferPool())
	_, ok := buf.(interface{ Ref() })
	buf.Free()
	return ok
}

// TestChainZc_TinyTailChunk regression-tests the scenario where a
// multi-frame chain ZC's final continuation chunk is small enough that
// mem.NewBuffer would otherwise return a no-op SliceBuffer (cap below
// the 1 KiB pooling threshold). Before the fix, that buffer's Free()
// would not invoke zcChainReleasePool.Put, leaving zcInFlight stuck
// > 0 forever and freezing header.ReadIdx — a deadlock observed at
// 16 MiB unary ZC where chunking produces 8 MiB / 8 MiB / 5 B chunks.
//
// The fix copies sub-threshold continuation chunks to a heap buffer
// (without AddChainZcInFlight) so the chain's lifecycle is driven by
// the larger chunks alone.
func TestChainZc_TinyTailChunk(t *testing.T) {
	// See TestChainZc_BudgetReject for why the multi-frame chain-ZC path
	// is not reachable from the H2-only wire.
	t.Skip("tiny-tail chunk path is Custom16-MORE specific; not reachable through the H2 LPM accumulator")
	if !memBufferPoolingAvailable() {
		t.Skip("mem.Buffer pooling not available")
	}
	ctx := context.Background()
	segName := fmt.Sprintf("zctail-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	// 4 MiB ring → ChainZcBudget = 2 MiB. Send a chain whose total
	// stays under budget but whose final chunk is sub-threshold.
	seg, err := CreateSegment(segName, 4*1024*1024, 4*1024*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	// Two chunks: 200 KiB body chunk (ZC-eligible: ≥ 64 KiB on a 4 MiB
	// ring) followed by a 5-byte tail (well below the 1 KiB pooling
	// threshold). Total = 200 KiB + 5 B < 2 MiB budget.
	bigSize := 200 * 1024
	tailSize := 5
	totalBody := bigSize + tailSize - 5 // exclude the 5-byte LPM header
	chunkBig := make([]byte, bigSize)
	chunkBig[0] = 0
	binary.BigEndian.PutUint32(chunkBig[1:5], uint32(totalBody))
	chunkTail := make([]byte, tailSize)

	if err := writeFrame(ctx, tx, FrameHeader{
		Type: FrameTypeMESSAGE, StreamID: 1, Flags: MessageFlagMORE,
	}, chunkBig); err != nil {
		t.Fatalf("writeFrame chunkBig: %v", err)
	}
	if err := writeFrame(ctx, tx, FrameHeader{
		Type: FrameTypeMESSAGE, StreamID: 1,
	}, chunkTail); err != nil {
		t.Fatalf("writeFrame chunkTail: %v", err)
	}

	hdr := rx.header()
	readIdxBefore := hdr.ReadIndex()

	// Read the first (big) chunk — opens the chain ZC anchor.
	_, bufBig, err := readFrameView(ctx, rx)
	if err != nil {
		t.Fatalf("readFrameView big: %v", err)
	}
	if !rx.IsZcChainActive() {
		t.Fatal("expected chain ZC active after first chunk")
	}
	if !rx.IsChainOpen() {
		t.Fatal("expected chain open after first chunk")
	}

	// Read the tail chunk — must take the tiny-chunk copy branch
	// (NOT a ring-backed no-op SliceBuffer).
	_, bufTail, err := readFrameView(ctx, rx)
	if err != nil {
		t.Fatalf("readFrameView tail: %v", err)
	}
	if rx.IsChainOpen() {
		t.Error("expected chain closed after final chunk")
	}

	// Free the tail buffer first. With the fix, this is a heap-backed
	// SliceBuffer that does NOT decrement zcInFlight (because the
	// codec didn't AddChainZcInFlight for it). The big chunk's
	// in-flight count is still 1.
	bufTail.Free()
	if !rx.IsZcChainActive() {
		t.Error("ZC anchor must remain held while big chunk is outstanding")
	}

	// Free the big buffer — this is the true ring-backed ZC; its
	// pool.Put decrements zcInFlight to 0; chain is already closed
	// (CloseZcChain ran on the final chunk) → EndZcReservation fires.
	bufBig.Free()
	if rx.IsZcChainActive() {
		t.Error("expected zcActive=0 after final ZC buffer Free")
	}

	// header.ReadIdx must have advanced past both chunks (2 frame
	// headers + both payloads).
	expectedAfter := readIdxBefore + 2*h2FrameHeaderSize + uint64(bigSize+tailSize)
	if got := hdr.ReadIndex(); got != expectedAfter {
		t.Errorf("readIdx after Free: got %d want %d", got, expectedAfter)
	}
}
