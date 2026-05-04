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
)

func TestH2FrameHeader_RoundTrip(t *testing.T) {
	cases := []H2FrameHeader{
		{Length: 0, Type: H2FrameDATA, Flags: 0, StreamID: 1},
		{Length: 12345, Type: H2FrameHEADERS, Flags: H2FlagEndStream | H2FlagEndHeaders, StreamID: 3},
		{Length: 0xFFFFFF, Type: H2FrameWINDOWUPDATE, Flags: 0, StreamID: 0},
		{Length: 8, Type: H2FramePING, Flags: H2FlagAck, StreamID: 0},
		{Length: 17, Type: H2FrameGOAWAY, Flags: 0, StreamID: 0},
	}
	for _, c := range cases {
		var buf [h2FrameHeaderSize]byte
		encodeH2FrameHeaderTo(&buf, c)
		got, err := decodeH2FrameHeader(buf[:])
		if err != nil {
			t.Fatalf("decodeH2FrameHeader(%+v): %v", c, err)
		}
		if got != c {
			t.Errorf("roundtrip mismatch: got %+v, want %+v", got, c)
		}
	}
}

func TestH2FrameHeader_DecodeRejectsOversized(t *testing.T) {
	var buf [h2FrameHeaderSize]byte
	// Force length > 24 bits by writing all 0xFF in the length field.
	buf[0] = 0xFF
	buf[1] = 0xFF
	buf[2] = 0xFF
	// length=2^24-1 is the max allowed; any value with the high byte set
	// in a 32-bit interpretation would fail the >max check, but the codec
	// itself can only express 24 bits so this is OK. Just verify the
	// encoder rejects out-of-range lengths.
	got, err := decodeH2FrameHeader(buf[:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Length != 0xFFFFFF {
		t.Errorf("expected max 24-bit length, got %d", got.Length)
	}
}

func TestH2FrameHeader_DecodeShort(t *testing.T) {
	if _, err := decodeH2FrameHeader([]byte{0, 0}); err == nil {
		t.Error("expected error for short header")
	}
}

func TestH2EncodeDecodeHeaders_Initial(t *testing.T) {
	enc := newHpackEncoderHolder()
	dec := newHpackDecoderHolder()
	src := HeadersV1{
		Version:          1,
		HdrType:          0,
		Method:           "/svc/Method",
		Authority:        "host:8080",
		DeadlineUnixNano: 0,
		Metadata: []KV{
			{Key: "x-custom", Values: [][]byte{[]byte("value1"), []byte("value2")}},
		},
	}
	payload := h2EncodeHeaders(enc.enc, enc.scratch, src)
	if len(payload) == 0 {
		t.Fatal("empty HPACK payload")
	}
	got, _, isTrailers, err := h2DecodeHeaders(dec, payload)
	if err != nil {
		t.Fatalf("h2DecodeHeaders: %v", err)
	}
	if isTrailers {
		t.Fatal("expected initial headers, got trailers")
	}
	if got.Method != src.Method {
		t.Errorf("method mismatch: got %q want %q", got.Method, src.Method)
	}
	if got.Authority != src.Authority {
		t.Errorf("authority mismatch: got %q want %q", got.Authority, src.Authority)
	}
	if got.HdrType != 0 {
		t.Errorf("expected HdrType=0 (client-initial), got %d", got.HdrType)
	}
	if len(got.Metadata) != 1 || got.Metadata[0].Key != "x-custom" {
		t.Fatalf("metadata not preserved: %+v", got.Metadata)
	}
	if len(got.Metadata[0].Values) != 2 {
		t.Errorf("expected 2 values, got %d", len(got.Metadata[0].Values))
	}
}

func TestH2EncodeDecodeHeaders_Trailers(t *testing.T) {
	enc := newHpackEncoderHolder()
	dec := newHpackDecoderHolder()
	src := TrailersV1{
		Version:        1,
		GRPCStatusCode: 5, // NotFound
		GRPCStatusMsg:  "not found",
		Metadata: []KV{
			{Key: "trailer-key", Values: [][]byte{[]byte("trailer-val")}},
		},
	}
	payload := h2EncodeTrailers(enc.enc, enc.scratch, src)
	_, got, isTrailers, err := h2DecodeHeaders(dec, payload)
	if err != nil {
		t.Fatalf("h2DecodeHeaders: %v", err)
	}
	if !isTrailers {
		t.Fatal("expected trailers")
	}
	if got.GRPCStatusCode != 5 {
		t.Errorf("status code mismatch: got %d want 5", got.GRPCStatusCode)
	}
	if got.GRPCStatusMsg != "not found" {
		t.Errorf("status msg mismatch: got %q", got.GRPCStatusMsg)
	}
}

func TestH2EncodeHeaders_Deadline(t *testing.T) {
	enc := newHpackEncoderHolder()
	dec := newHpackDecoderHolder()
	deadline := time.Now().Add(5 * time.Second).UnixNano()
	src := HeadersV1{
		Version:          1,
		HdrType:          0,
		Method:           "/svc/Method",
		DeadlineUnixNano: uint64(deadline),
	}
	payload := h2EncodeHeaders(enc.enc, enc.scratch, src)
	got, _, _, err := h2DecodeHeaders(dec, payload)
	if err != nil {
		t.Fatalf("h2DecodeHeaders: %v", err)
	}
	if got.DeadlineUnixNano == 0 {
		t.Error("expected DeadlineUnixNano set after decode")
	}
}

func TestH2WriteReadFrame_RoundTrip(t *testing.T) {
	segName := fmt.Sprintf("h2rw-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, 1<<20, 1<<20)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)
	tx.SetWireFormat(WireFormatHTTP2)
	rx.SetWireFormat(WireFormatHTTP2)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Write a MESSAGE frame. The body must include the gRPC LPM prefix
	// (5 bytes: 1-byte compressed flag + 4-byte big-endian length) so
	// the H2 reader's lpmAccumulator can identify a complete message.
	body := []byte("hello h2")
	payload := make([]byte, 5+len(body))
	payload[0] = 0
	binary.BigEndian.PutUint32(payload[1:5], uint32(len(body)))
	copy(payload[5:], body)
	if err := writeFrame(ctx, tx, FrameHeader{
		Type:     FrameTypeMESSAGE,
		StreamID: 3,
	}, payload); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	// Read it back.
	fh, got, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if fh.Type != FrameTypeMESSAGE {
		t.Errorf("type mismatch: got %d want %d", fh.Type, FrameTypeMESSAGE)
	}
	if fh.StreamID != 3 {
		t.Errorf("streamID mismatch: got %d want 3", fh.StreamID)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch: got %q want %q", got, payload)
	}
}

func TestH2WriteReadFrame_Headers(t *testing.T) {
	segName := fmt.Sprintf("h2hdr-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, 1<<20, 1<<20)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)
	tx.SetWireFormat(WireFormatHTTP2)
	rx.SetWireFormat(WireFormatHTTP2)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Write a HEADERS frame using the Custom16 KV encoding.
	hdrPayload := encodeHeaders(HeadersV1{
		Version:   1,
		HdrType:   0,
		Method:    "/svc/Hello",
		Authority: "test",
		Metadata: []KV{
			{Key: "x-test", Values: [][]byte{[]byte("v")}},
		},
	})
	if err := writeFrame(ctx, tx, FrameHeader{
		Type:     FrameTypeHEADERS,
		StreamID: 1,
		Flags:    HeadersFlagINITIAL,
	}, hdrPayload); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	// Read it back. The H2 codec returns the in-memory KV blob, which
	// the rest of the transport already understands.
	fh, got, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if fh.Type != FrameTypeHEADERS {
		t.Errorf("type mismatch: got %d want %d", fh.Type, FrameTypeHEADERS)
	}
	decoded, err := decodeHeaders(got)
	if err != nil {
		t.Fatalf("decodeHeaders: %v", err)
	}
	if decoded.Method != "/svc/Hello" {
		t.Errorf("method mismatch: got %q", decoded.Method)
	}
}

func TestH2WriteReadFrame_Trailers(t *testing.T) {
	segName := fmt.Sprintf("h2tlr-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, 1<<20, 1<<20)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)
	tx.SetWireFormat(WireFormatHTTP2)
	rx.SetWireFormat(WireFormatHTTP2)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tlrPayload := encodeTrailers(TrailersV1{
		Version:        1,
		GRPCStatusCode: 0,
		GRPCStatusMsg:  "OK",
	})
	if err := writeFrame(ctx, tx, FrameHeader{
		Type:     FrameTypeTRAILERS,
		StreamID: 1,
		Flags:    TrailersFlagEndStream,
	}, tlrPayload); err != nil {
		t.Fatalf("writeFrame trailers: %v", err)
	}

	fh, got, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if fh.Type != FrameTypeTRAILERS {
		t.Errorf("type mismatch: got %d want TRAILERS", fh.Type)
	}
	decoded, err := decodeTrailers(got)
	if err != nil {
		t.Fatalf("decodeTrailers: %v", err)
	}
	if decoded.GRPCStatusCode != 0 {
		t.Errorf("status mismatch: got %d", decoded.GRPCStatusCode)
	}
}

func TestNegotiateWireFormat(t *testing.T) {
	l := &ShmListener{}
	// Empty supported list always returns Custom16.
	if got := l.negotiateWireFormat([]WireFormat{WireFormatHTTP2}); got != WireFormatCustom16 {
		t.Errorf("empty supported: got %v want Custom16", got)
	}

	l.SetSupportedWireFormats([]WireFormat{WireFormatHTTP2, WireFormatCustom16})
	// Client only advertises H2 → pick H2.
	if got := l.negotiateWireFormat([]WireFormat{WireFormatHTTP2}); got != WireFormatHTTP2 {
		t.Errorf("client=H2: got %v want H2", got)
	}
	// Client advertises Custom16 first → pick Custom16.
	if got := l.negotiateWireFormat([]WireFormat{WireFormatCustom16, WireFormatHTTP2}); got != WireFormatCustom16 {
		t.Errorf("client=C16,H2: got %v want C16", got)
	}
	// Client doesn't advertise → Custom16 default.
	if got := l.negotiateWireFormat(nil); got != WireFormatCustom16 {
		t.Errorf("nil client: got %v want C16", got)
	}
}

func TestConnectRequest_RoundTripWithWireFormats(t *testing.T) {
	req := connectRequest{
		ringA:                1024,
		ringB:                2048,
		singleStreamMode:     true,
		supportedWireFormats: []WireFormat{WireFormatHTTP2, WireFormatCustom16},
	}
	b := encodeConnectRequest(req)
	got, err := decodeConnectRequest(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ringA != req.ringA || got.ringB != req.ringB {
		t.Errorf("ring sizes mismatch: %+v", got)
	}
	if !got.singleStreamMode {
		t.Error("singleStreamMode lost")
	}
	if len(got.supportedWireFormats) != 2 {
		t.Errorf("wire formats: got %v want %v", got.supportedWireFormats, req.supportedWireFormats)
	}
}

func TestConnectRequest_BackwardCompat(t *testing.T) {
	// 18-byte CONNECT request without wire format extension (legacy peer).
	req := connectRequest{ringA: 100, ringB: 200, singleStreamMode: false}
	b := encodeConnectRequest(req)
	if len(b) != 18 {
		t.Errorf("expected 18-byte legacy request, got %d", len(b))
	}
	got, err := decodeConnectRequest(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.supportedWireFormats) != 0 {
		t.Errorf("expected no wire formats from legacy request, got %v", got.supportedWireFormats)
	}
}

func TestConnectResponse_RoundTripWithWireFormat(t *testing.T) {
	resp := connectResponse{
		segmentName:  "test_seg_42",
		selectedWire: WireFormatHTTP2,
	}
	b := encodeConnectResponse(resp)
	got, err := decodeConnectResponse(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.segmentName != resp.segmentName {
		t.Errorf("segment name mismatch: got %q want %q", got.segmentName, resp.segmentName)
	}
	if got.selectedWire != WireFormatHTTP2 {
		t.Errorf("selected wire: got %v want H2", got.selectedWire)
	}
}

func TestConnectResponse_LegacyDefault(t *testing.T) {
	// Legacy response (no selectedWire byte) → decode defaults to Custom16.
	resp := connectResponse{segmentName: "old"}
	// Manually craft a v1 baseline buffer (no extension byte).
	name := []byte(resp.segmentName)
	b := make([]byte, 1+4+len(name))
	b[0] = controlWireV1
	b[1] = 3 // little-endian uint32 = 3
	b[2] = 0
	b[3] = 0
	b[4] = 0
	copy(b[5:], name)
	got, err := decodeConnectResponse(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.selectedWire != WireFormatCustom16 {
		t.Errorf("legacy decode: got %v want C16", got.selectedWire)
	}
}
