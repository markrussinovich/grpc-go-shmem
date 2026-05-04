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
	// boundary (advanced by the 16-byte header commit) but NOT past
	// the payload (frozen at baseZc + frameHeaderSize).
	readIdxDuring := hdr.ReadIndex()
	if readIdxDuring != readIdxBefore+frameHeaderSize {
		t.Errorf("readIdx during ZC: got %d want %d (= before %d + headerSize %d)",
			readIdxDuring, readIdxBefore+frameHeaderSize, readIdxBefore, frameHeaderSize)
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
	expectedReadIdxAfter := readIdxBefore + frameHeaderSize + uint64(len(payload))
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
	expectedFrozen := readIdxBefore + frameHeaderSize
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
	expectedAfter := readIdxBefore + 2*frameHeaderSize + uint64(len(payload1)) + uint64(len(payload2))
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
