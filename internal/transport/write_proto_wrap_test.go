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
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestWriteTwoSegAcrossSpans verifies the ring-wrap split-copy used by
// writeProtoBytesToRingH2Blocking: writing two logical segments (the
// 14-byte preamble `a` and the payload `b`) into the concatenation
// first++second must reproduce a++b for EVERY boundary position —
// including len(first) < len(a), == len(a), and > len(a).
func TestWriteTwoSegAcrossSpans(t *testing.T) {
	aa := make([]byte, h2FrameHeaderSize+5) // 14-byte preamble
	for i := range aa {
		aa[i] = byte('A' + i)
	}
	bb := make([]byte, 40)
	for i := range bb {
		bb[i] = byte(i)
	}
	want := append(append([]byte{}, aa...), bb...)
	total := len(want)
	for firstLen := 0; firstLen <= total; firstLen++ {
		first := make([]byte, firstLen)
		second := make([]byte, total-firstLen)
		writeTwoSegAcrossSpans(first, second, aa, bb)
		got := append(append([]byte{}, first...), second...)
		if !bytes.Equal(got, want) {
			t.Fatalf("firstLen=%d: split-copy mismatch\n got=%v\nwant=%v", firstLen, got, want)
		}
	}
}

// TestWriteProtoToRingH2_Wrap reproduces the wire-format corruption
// observed on the Linux bench after enabling ZC marshal in writeLoop
// (89f65b98a). The hypothesis: when ReserveWrite returns a reservation
// that straddles the ring wrap boundary, len(res.First) < total. The
// existing writeProtoToRingH2Core writes the H2 + LPM headers into
// res.First[:14] (OK) but does `protoMarshalAppend(res.First[14:14], msg)`
// where cap(dst) = len(res.First) - 14 < pSize. proto.MarshalAppend
// internally reallocates, writes to a heap buffer, and res.First's
// body bytes remain whatever was in the ring before (garbage). After
// Commit, the reader sees garbage in the body region and proto.Unmarshal
// fails.
//
// This test directly invokes writeProtoToRingH2Blocking on a small
// ring positioned so the reservation must wrap. If the bug is present,
// reading the ring back finds non-zero bytes that don't decode as the
// proto we sent.
func TestWriteProtoToRingH2_Wrap(t *testing.T) {
	ctx := context.Background()
	const ringSize = 64 * 1024 // 64 KiB ring, easy to wrap
	segName := fmt.Sprintf("wraprepro-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, ringSize, ringSize)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()
	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	// Advance writeIdx/readIdx so that the NEXT write would wrap.
	// We need to position writeIdx (mod capacity) such that the
	// remaining contiguous space at the end is LESS than the next
	// write's total bytes (9 + 5 + 4096 = 4110).
	//
	// Strategy: write+commit a frame of size pre that puts writeIdx
	// near the wrap boundary. Then verify the next reservation will
	// straddle.
	const pre = ringSize - 1000 // leaves 1000 bytes contig at end
	{
		// Reserve "pre" bytes, fill, commit. Then read+commit the
		// same to advance readIdx past pre.
		res, err := tx.ReserveWrite(ctx, pre)
		if err != nil {
			t.Fatalf("ReserveWrite pre: %v", err)
		}
		if len(res.First) != pre {
			t.Fatalf("pre reservation not contiguous: First=%d Second=%d", len(res.First), len(res.Second))
		}
		for i := range res.First {
			res.First[i] = 0xAA // marker
		}
		if err := res.Commit(pre); err != nil {
			t.Fatalf("Commit pre: %v", err)
		}
		// Now drain via reader.
		first, second, commitRC, err := rx.ReadSlices(ctx, pre)
		if err != nil {
			t.Fatalf("ReadSlices pre drain: %v", err)
		}
		if len(first)+len(second) != pre {
			t.Fatalf("drain pre size mismatch")
		}
		commitRC.Commit(pre)
	}

	// Now ring is empty, but writeIdx is at (pre % ringSize).
	// A 4110-byte write should straddle. Confirm:
	contig := tx.ContiguousWriteSpace()
	t.Logf("after pre: writeIdx %% cap = %d, ContiguousWriteSpace = %d",
		(ringSize-1000)%ringSize, contig)
	if contig >= 4110 {
		t.Fatalf("contig %d >= 4110; ring did not wrap as planned", contig)
	}

	// Build a proto message of size ≈4 KiB (BytesValue with 4 KiB body).
	body := make([]byte, 4096)
	for i := range body {
		body[i] = byte(i & 0xFF)
	}
	msg := wrapperspb.Bytes(body)
	pSize := protoSize(msg)

	// Call the BLOCKING ZC marshal variant — the one writeLoop uses.
	// If the bug is present, this writes corrupt bytes to the ring.
	if err := writeProtoToRingH2Blocking(ctx, tx, 1, msg, pSize, 0); err != nil {
		t.Fatalf("writeProtoToRingH2Blocking: %v", err)
	}

	// Read the frame back via the H2 codec and verify the body.
	fh, buf, err := readFrameView(ctx, rx)
	if err != nil {
		t.Fatalf("readFrameView: %v", err)
	}
	if fh.Type != FrameTypeMESSAGE {
		t.Fatalf("frame type: got %v want MESSAGE", fh.Type)
	}
	got := buf.ReadOnlyData()
	want := 5 + pSize
	if len(got) != want {
		t.Errorf("buf len: got %d want %d", len(got), want)
	}
	// First byte = compressed flag = 0.
	if got[0] != 0 {
		t.Errorf("LPM compressed flag: got %d want 0", got[0])
	}
	gotPSize := int(binary.BigEndian.Uint32(got[1:5]))
	if gotPSize != pSize {
		t.Errorf("LPM body length: got %d want %d", gotPSize, pSize)
	}
	// Decode the proto and check the payload.
	gotMsg := &wrapperspb.BytesValue{}
	if err := proto.Unmarshal(got[5:], gotMsg); err != nil {
		t.Fatalf("proto.Unmarshal: %v  (first 16 body bytes: %x)", err, got[5:21])
	}
	gotBody := gotMsg.GetValue()
	if len(gotBody) != len(body) {
		t.Errorf("payload body len: got %d want %d", len(gotBody), len(body))
	}
	for i := range gotBody {
		if gotBody[i] != body[i] {
			t.Errorf("body byte %d: got %d want %d", i, gotBody[i], body[i])
			break
		}
	}
	buf.Free()
}
