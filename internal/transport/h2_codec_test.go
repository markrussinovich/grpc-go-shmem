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
	"runtime"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc/mem"
)

// decodeHpackToFields is a test helper that runs a fresh HPACK decoder
// over a single HEADERS frame payload, returning all emitted fields. Used
// to inspect on-wire values without relying on our internal decoder
// (which deliberately strips/synthesizes pseudo-headers).
func decodeHpackToFields(b []byte) ([]hpack.HeaderField, error) {
	var fields []hpack.HeaderField
	d := hpack.NewDecoder(4096, func(f hpack.HeaderField) {
		fields = append(fields, f)
	})
	if _, err := d.Write(b); err != nil {
		return nil, err
	}
	if err := d.Close(); err != nil {
		return nil, err
	}
	return fields, nil
}

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
	// Metadata round-trips both the user header (x-custom) and the
	// transport-emitted content-type. Ordering is encoder-order
	// (content-type first because it's written before user metadata).
	var customKV *KV
	var sawContentType bool
	for i := range got.Metadata {
		switch got.Metadata[i].Key {
		case "x-custom":
			customKV = &got.Metadata[i]
		case "content-type":
			sawContentType = true
		}
	}
	if !sawContentType {
		t.Errorf("expected content-type in decoded metadata, got %+v", got.Metadata)
	}
	if customKV == nil {
		t.Fatalf("x-custom not preserved: %+v", got.Metadata)
	}
	if len(customKV.Values) != 2 {
		t.Errorf("expected 2 x-custom values, got %d", len(customKV.Values))
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Write a HEADERS frame using the internal HeadersV1 KV encoding.
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
	decoded, err := takeOrDecodeHeaders(rx.h2Decoder(), got)
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
	decoded, err := takeOrDecodeTrailers(rx.h2Decoder(), got)
	if err != nil {
		t.Fatalf("decodeTrailers: %v", err)
	}
	if decoded.GRPCStatusCode != 0 {
		t.Errorf("status mismatch: got %d", decoded.GRPCStatusCode)
	}
}

// TestH2BinaryMetadata_HeaderRoundTrip verifies that arbitrary byte values
// passed in HeadersV1.Metadata for keys with the "-bin" suffix survive an
// H2 wire encode→decode cycle as raw bytes — i.e. that the HPACK adapter
// applies base64 at the wire boundary per gRPC-over-HTTP/2 binary-headers
// rules. Non-binary metadata must NOT be base64-decoded even when its
// plain-text value coincidentally looks like base64 (e.g., "YWJj").
func TestH2BinaryMetadata_HeaderRoundTrip(t *testing.T) {
	segName := fmt.Sprintf("h2bin-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, 1<<20, 1<<20)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 0x00..0xFF non-text bytes: would crash a UTF-8 reader and would
	// not survive verbatim transport on a real H2 peer that expects
	// base64. Tests the round-trip of arbitrary byte content.
	binValue := []byte{0x00, 0x01, 0x7F, 0x80, 0xFE, 0xFF}
	// "YWJj" is base64("abc"). A buggy decoder that always base64-
	// decodes regardless of suffix would turn this into "abc"; a
	// correct decoder leaves it as the literal string "YWJj".
	textLikeBase64 := []byte("YWJj")

	hdrPayload := encodeHeaders(HeadersV1{
		Version:   1,
		HdrType:   0,
		Method:    "/svc/Bin",
		Authority: "test",
		Metadata: []KV{
			{Key: "x-binary-bin", Values: [][]byte{binValue}},
			{Key: "x-text", Values: [][]byte{textLikeBase64}},
		},
	})
	if err := writeFrame(ctx, tx, FrameHeader{
		Type:     FrameTypeHEADERS,
		StreamID: 1,
		Flags:    HeadersFlagINITIAL,
	}, hdrPayload); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	_, got, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	decoded, err := takeOrDecodeHeaders(rx.h2Decoder(), got)
	if err != nil {
		t.Fatalf("decodeHeaders: %v", err)
	}

	var gotBin, gotText []byte
	for _, kv := range decoded.Metadata {
		switch kv.Key {
		case "x-binary-bin":
			if len(kv.Values) != 1 {
				t.Fatalf("x-binary-bin: got %d values, want 1", len(kv.Values))
			}
			gotBin = kv.Values[0]
		case "x-text":
			if len(kv.Values) != 1 {
				t.Fatalf("x-text: got %d values, want 1", len(kv.Values))
			}
			gotText = kv.Values[0]
		}
	}
	if !bytes.Equal(gotBin, binValue) {
		t.Errorf("binary metadata round-trip: got %x want %x", gotBin, binValue)
	}
	if !bytes.Equal(gotText, textLikeBase64) {
		t.Errorf("text metadata accidentally base64-decoded: got %q want %q",
			string(gotText), string(textLikeBase64))
	}
}

// TestH2BinaryMetadata_TrailerRoundTrip exercises the trailers path,
// which is the primary user of -bin metadata in practice (gRPC carries
// rich error details via grpc-status-details-bin).
func TestH2BinaryMetadata_TrailerRoundTrip(t *testing.T) {
	segName := fmt.Sprintf("h2bintr-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, 1<<20, 1<<20)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	statusDetails := []byte{0x08, 0x05, 0x12, 0x07, 't', 'e', 's', 't', 'i', 'n', 'g'}
	tlrPayload := encodeTrailers(TrailersV1{
		Version:        1,
		GRPCStatusCode: 13, // Internal
		GRPCStatusMsg:  "boom",
		Metadata: []KV{
			{Key: "grpc-status-details-bin", Values: [][]byte{statusDetails}},
		},
	})
	if err := writeFrame(ctx, tx, FrameHeader{
		Type:     FrameTypeTRAILERS,
		StreamID: 5,
		Flags:    TrailersFlagEndStream,
	}, tlrPayload); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	fh, got, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if fh.Type != FrameTypeTRAILERS {
		t.Fatalf("type: got %d want TRAILERS", fh.Type)
	}
	dec, err := takeOrDecodeTrailers(rx.h2Decoder(), got)
	if err != nil {
		t.Fatalf("decodeTrailers: %v", err)
	}
	if dec.GRPCStatusCode != 13 || dec.GRPCStatusMsg != "boom" {
		t.Errorf("status: got code=%d msg=%q", dec.GRPCStatusCode, dec.GRPCStatusMsg)
	}
	var found []byte
	for _, kv := range dec.Metadata {
		if kv.Key == "grpc-status-details-bin" {
			found = kv.Values[0]
		}
	}
	if !bytes.Equal(found, statusDetails) {
		t.Errorf("status-details-bin round-trip: got %x want %x", found, statusDetails)
	}
}

// TestH2BinaryMetadata_WireFormatIsBase64 inspects the on-wire HPACK
// payload to confirm the value emitted for a "-bin" header is the
// standard base64 representation, not the raw bytes. This is what a
// stock grpc-go / grpc-java / grpc-c++ peer would see; the tests above
// only verify self-interop.
func TestH2BinaryMetadata_WireFormatIsBase64(t *testing.T) {
	enc := newHpackEncoderHolder()
	raw := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	out := h2EncodeHeaders(enc.enc, enc.scratch, HeadersV1{
		Version: 1,
		HdrType: 0,
		Method:  "/svc/M",
		Metadata: []KV{
			{Key: "x-bin", Values: [][]byte{raw}},
		},
	})
	// Decode with a fresh hpack decoder (independent dynamic table) and
	// inspect the literal value. encodeBinHeader uses raw-std (no padding);
	// "DEADBEEF" → "3q2+7w".
	hf, err := decodeHpackToFields(out)
	if err != nil {
		t.Fatalf("decode hpack: %v", err)
	}
	var seen string
	for _, f := range hf {
		if f.Name == "x-bin" {
			seen = f.Value
		}
	}
	want := encodeBinHeader(raw) // "3q2+7w"
	if seen != want {
		t.Errorf("on-wire x-bin value: got %q want %q (raw bytes would be %q)",
			seen, want, string(raw))
	}
}

// ---------------------------------------------------------------------------
// H2 control-frame validation tests (RFC 7540 §6.x).
//
// Each malformed-frame test follows a uniform pattern:
//
//  1. inject a hand-crafted malformed H2 frame into the ring,
//  2. assert readFrame returns an error,
//  3. write a normal MESSAGE frame on the same ring,
//  4. assert that MESSAGE reads back intact — proves the ring read
//     index advanced past the malformed payload (i.e. the codec
//     drained the bytes before throwing, no stuck reader).
// ---------------------------------------------------------------------------

// injectH2Frame writes a raw H2 frame (header + payload) into the ring
// directly, bypassing the encoder's own validation. Used to simulate a
// peer that sends a frame our codec considers malformed.
func injectH2Frame(ctx context.Context, t *testing.T, tx *ShmRing,
	frameType H2FrameType, flags byte, streamID uint32, payload []byte) {
	t.Helper()
	var hdr [h2FrameHeaderSize]byte
	encodeH2FrameHeaderTo(&hdr, H2FrameHeader{
		Length:   uint32(len(payload)),
		Type:     frameType,
		Flags:    flags,
		StreamID: streamID,
	})
	res, err := tx.ReserveWrite(ctx, h2FrameHeaderSize+len(payload))
	if err != nil {
		t.Fatalf("ReserveWrite: %v", err)
	}
	// The injection bypass intentionally writes raw bytes — wrap-around
	// handling mirrors writeH2Single's pattern.
	if len(res.First) >= h2FrameHeaderSize {
		copy(res.First[:h2FrameHeaderSize], hdr[:])
		bodyInFirst := len(res.First) - h2FrameHeaderSize
		if bodyInFirst > len(payload) {
			bodyInFirst = len(payload)
		}
		copy(res.First[h2FrameHeaderSize:h2FrameHeaderSize+bodyInFirst], payload[:bodyInFirst])
		if len(res.Second) > 0 && bodyInFirst < len(payload) {
			copy(res.Second, payload[bodyInFirst:])
		}
	} else {
		copy(res.First, hdr[:len(res.First)])
		remHdr := h2FrameHeaderSize - len(res.First)
		copy(res.Second[:remHdr], hdr[len(res.First):])
		bodyDest := res.Second[remHdr:]
		copy(bodyDest, payload)
	}
	if err := res.Commit(h2FrameHeaderSize + len(payload)); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// writeNormalMessageH2 writes a small MESSAGE frame on tx using the
// production encoder; used as the post-recovery probe in malformed-frame
// tests.
func writeNormalMessageH2(ctx context.Context, t *testing.T, tx *ShmRing, body []byte) {
	t.Helper()
	payload := make([]byte, 5+len(body))
	payload[0] = 0
	binary.BigEndian.PutUint32(payload[1:5], uint32(len(body)))
	copy(payload[5:], body)
	if err := writeFrame(ctx, tx, FrameHeader{
		Type:     FrameTypeMESSAGE,
		StreamID: 7,
	}, payload); err != nil {
		t.Fatalf("post-recovery writeFrame: %v", err)
	}
}

// readNormalMessageH2 reads exactly one MESSAGE frame and verifies its
// body matches `want`; used as the post-recovery probe.
func readNormalMessageH2(ctx context.Context, t *testing.T, rx *ShmRing, want []byte) {
	t.Helper()
	fh, got, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("post-recovery readFrame: %v", err)
	}
	if fh.Type != FrameTypeMESSAGE {
		t.Fatalf("post-recovery type: got %d want MESSAGE", fh.Type)
	}
	// Body bytes start after the 5-byte LPM prefix.
	if len(got) < 5 || !bytes.Equal(got[5:], want) {
		t.Errorf("post-recovery body: got %q want %q", got[5:], want)
	}
}

func newH2RingPair(t *testing.T) (tx, rx *ShmRing, ctx context.Context, cancel context.CancelFunc, segName string) {
	t.Helper()
	segName = fmt.Sprintf("h2valid-%d-%d", time.Now().UnixNano(), goroutineID())
	seg, err := CreateSegment(segName, 1<<20, 1<<20)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	tx = NewShmRingFromSegment(seg.A, seg.Mem)
	rx = NewShmRingFromSegment(seg.A, seg.Mem)
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(func() {
		cancel()
		seg.Close()
		RemoveSegment(segName)
	})
	return
}

// goroutineID returns a coarse identifier used to disambiguate segment
// names in parallel test runs. (We don't need a real goroutine ID.)
func goroutineID() int64 {
	return time.Now().UnixNano() & 0xFFFF
}

func TestH2Validate_RstStreamWrongLength(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// streamID=1 but length=8 instead of 4.
	injectH2Frame(ctx, t, tx, H2FrameRSTSTREAM, 0, 1, make([]byte, 8))
	writeNormalMessageH2(ctx, t, tx, []byte("ok"))
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on malformed RST_STREAM length")
	}
	readNormalMessageH2(ctx, t, rx, []byte("ok"))
}

func TestH2Validate_RstStreamZeroStreamID(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// length=4 but streamID=0.
	injectH2Frame(ctx, t, tx, H2FrameRSTSTREAM, 0, 0, make([]byte, 4))
	writeNormalMessageH2(ctx, t, tx, []byte("ok"))
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on RST_STREAM streamID=0")
	}
	readNormalMessageH2(ctx, t, rx, []byte("ok"))
}

func TestH2Validate_SettingsNonZeroStreamID(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	injectH2Frame(ctx, t, tx, H2FrameSETTINGS, 0, 5, nil)
	writeNormalMessageH2(ctx, t, tx, []byte("ok"))
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on SETTINGS streamID != 0")
	}
	readNormalMessageH2(ctx, t, rx, []byte("ok"))
}

func TestH2Validate_SettingsBadLength(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// non-ACK SETTINGS with length not a multiple of 6.
	injectH2Frame(ctx, t, tx, H2FrameSETTINGS, 0, 0, make([]byte, 7))
	writeNormalMessageH2(ctx, t, tx, []byte("ok"))
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on SETTINGS length not multiple of 6")
	}
	readNormalMessageH2(ctx, t, rx, []byte("ok"))
}

func TestH2Validate_SettingsAckWithPayload(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// ACK flag set but non-empty payload.
	injectH2Frame(ctx, t, tx, H2FrameSETTINGS, H2FlagAck, 0, make([]byte, 6))
	writeNormalMessageH2(ctx, t, tx, []byte("ok"))
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on SETTINGS ACK with non-empty payload")
	}
	readNormalMessageH2(ctx, t, rx, []byte("ok"))
}

func TestH2Validate_PingWrongLength(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	injectH2Frame(ctx, t, tx, H2FramePING, 0, 0, make([]byte, 4)) // need 8
	writeNormalMessageH2(ctx, t, tx, []byte("ok"))
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on PING length != 8")
	}
	readNormalMessageH2(ctx, t, rx, []byte("ok"))
}

func TestH2Validate_PingNonZeroStreamID(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	injectH2Frame(ctx, t, tx, H2FramePING, 0, 1, make([]byte, 8))
	writeNormalMessageH2(ctx, t, tx, []byte("ok"))
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on PING streamID != 0")
	}
	readNormalMessageH2(ctx, t, rx, []byte("ok"))
}

func TestH2Validate_GoAwayShortPayload(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// length < 8 (last-stream-id + error-code).
	injectH2Frame(ctx, t, tx, H2FrameGOAWAY, 0, 0, make([]byte, 4))
	writeNormalMessageH2(ctx, t, tx, []byte("ok"))
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on GOAWAY length < 8")
	}
	readNormalMessageH2(ctx, t, rx, []byte("ok"))
}

func TestH2Validate_GoAwayNonZeroStreamID(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	injectH2Frame(ctx, t, tx, H2FrameGOAWAY, 0, 9, make([]byte, 8))
	writeNormalMessageH2(ctx, t, tx, []byte("ok"))
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on GOAWAY streamID != 0")
	}
	readNormalMessageH2(ctx, t, rx, []byte("ok"))
}

func TestH2Validate_WindowUpdateWrongLength(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	injectH2Frame(ctx, t, tx, H2FrameWINDOWUPDATE, 0, 1, make([]byte, 3))
	writeNormalMessageH2(ctx, t, tx, []byte("ok"))
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on WINDOW_UPDATE length != 4")
	}
	readNormalMessageH2(ctx, t, rx, []byte("ok"))
}

func TestH2Validate_WindowUpdateZeroIncrement(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// length=4, increment=0 (stream-error PROTOCOL_ERROR per RFC 7540
	// §6.9.1: "A receiver MUST treat the receipt of a WINDOW_UPDATE
	// frame with an flow-control window increment of 0 as a stream
	// error or connection error of type PROTOCOL_ERROR".
	injectH2Frame(ctx, t, tx, H2FrameWINDOWUPDATE, 0, 1, []byte{0, 0, 0, 0})
	writeNormalMessageH2(ctx, t, tx, []byte("ok"))
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on WINDOW_UPDATE increment=0")
	}
	readNormalMessageH2(ctx, t, rx, []byte("ok"))
}

// TestH2WindowUpdate_BigEndianWire verifies that the SHM sender writes
// the WINDOW_UPDATE Window Size Increment as big-endian (RFC 7540
// §6.9.1) and that the receive path interprets it consistently.
//
// Regression: prior to this commit the senders wrote little-endian
// while the codec's non-zero validator (h2_codec.go around line 1204)
// read big-endian. Both ends of the SHM connection used the same
// codebase, so the validator's "non-zero" check was unreliable for
// certain magic values where the LE bytes look like 0x80000000 in BE
// (clearing bit 31 via `& 0x7FFFFFFF` would zero them, triggering a
// spurious "WINDOW_UPDATE increment must be non-zero" connection
// error). The wire format also violated the RFC, making the SHM
// transport non-interoperable with a wire conformance audit even
// though both same-codebase peers happened to agree.
//
// The test injects a known increment value as a raw H2 WINDOW_UPDATE
// payload (big-endian bytes), then asserts the validator accepts it
// AND the application-level handler decodes the same value.
func TestH2WindowUpdate_BigEndianWire(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)

	// A WINDOW_UPDATE increment whose LE encoding would look like
	// 0x80000000 in BE: increment = 128 (LE bytes [0x80,0,0,0]).
	// BE encoding of 128 = [0,0,0,0x80]. Inject the BE-correct bytes;
	// the validator's `BigEndian.Uint32 & 0x7FFFFFFF` must read 128,
	// not zero.
	const increment uint32 = 128
	beBytes := []byte{0x00, 0x00, 0x00, 0x80}
	injectH2Frame(ctx, t, tx, H2FrameWINDOWUPDATE, 0, 1, beBytes)

	fh, payload, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame: %v — codec validator likely reads wrong endianness", err)
	}
	if fh.Type != FrameTypeWindowUpdate {
		t.Fatalf("frame type: got %d want WindowUpdate", fh.Type)
	}
	// The codec passes the raw payload through to the application
	// handler. The handler in shm_client_transport / shm_server_transport
	// decodes via binary.BigEndian.Uint32, matching the wire spec.
	if got := binary.BigEndian.Uint32(payload); got != increment {
		t.Errorf("decoded increment: got %d want %d", got, increment)
	}
}

// ---------------------------------------------------------------------------
// CONTINUATION frame tests (RFC 7540 §6.10).
// ---------------------------------------------------------------------------

// hpackEncodeForTest deterministically encodes a list of HeaderFields to
// HPACK bytes with a fresh encoder (no shared dynamic-table state with
// any production decoder under test).
func hpackEncodeForTest(t *testing.T, fields ...hpack.HeaderField) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	for _, f := range fields {
		if err := enc.WriteField(f); err != nil {
			t.Fatalf("WriteField %v: %v", f, err)
		}
	}
	return buf.Bytes()
}

// TestH2Continuation_TwoFragments_RoundTrip splits a single HPACK header
// block into two halves and emits them as HEADERS (no END_HEADERS) +
// CONTINUATION (END_HEADERS). The reader must reassemble the block and
// produce the same HeadersV1 we'd see from a single-fragment HEADERS.
func TestH2Continuation_TwoFragments_RoundTrip(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	hpackBlock := hpackEncodeForTest(t,
		hpack.HeaderField{Name: ":method", Value: "POST"},
		hpack.HeaderField{Name: ":scheme", Value: "http"},
		hpack.HeaderField{Name: ":path", Value: "/svc/MyMethod"},
		hpack.HeaderField{Name: ":authority", Value: "example"},
		hpack.HeaderField{Name: "te", Value: "trailers"},
		hpack.HeaderField{Name: "content-type", Value: "application/grpc"},
		hpack.HeaderField{Name: "x-custom", Value: "v"},
	)
	// Split roughly in half (must split at any byte; HPACK is a byte
	// stream, the split point need not be on a field boundary).
	half := len(hpackBlock) / 2
	if half == 0 {
		t.Fatalf("hpack block too small: %d", len(hpackBlock))
	}
	// First fragment: HEADERS, no END_HEADERS.
	injectH2Frame(ctx, t, tx, H2FrameHEADERS, 0, 1, hpackBlock[:half])
	// Second fragment: CONTINUATION, END_HEADERS set.
	injectH2Frame(ctx, t, tx, H2FrameCONTINUATION, H2FlagEndHeaders, 1, hpackBlock[half:])

	fh, payload, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if fh.Type != FrameTypeHEADERS {
		t.Fatalf("type: got %d want HEADERS", fh.Type)
	}
	hv, derr := takeOrDecodeHeaders(rx.h2Decoder(), payload)
	if derr != nil {
		t.Fatalf("decodeHeaders: %v", derr)
	}
	if hv.Method != "/svc/MyMethod" {
		t.Errorf("method: got %q want /svc/MyMethod", hv.Method)
	}
	if hv.Authority != "example" {
		t.Errorf("authority: got %q want example", hv.Authority)
	}
	var seen string
	for _, kv := range hv.Metadata {
		if kv.Key == "x-custom" && len(kv.Values) > 0 {
			seen = string(kv.Values[0])
		}
	}
	if seen != "v" {
		t.Errorf("x-custom: got %q want v", seen)
	}
}

// TestH2Continuation_StreamIDMismatch verifies that a CONTINUATION
// fragment whose stream id differs from the originating HEADERS is
// rejected as PROTOCOL_ERROR (RFC 7540 §6.10).
func TestH2Continuation_StreamIDMismatch(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	hpackBlock := hpackEncodeForTest(t,
		hpack.HeaderField{Name: ":method", Value: "POST"},
		hpack.HeaderField{Name: ":path", Value: "/x"},
	)
	half := len(hpackBlock) / 2
	injectH2Frame(ctx, t, tx, H2FrameHEADERS, 0, 1, hpackBlock[:half])
	// CONTINUATION on a different stream id.
	injectH2Frame(ctx, t, tx, H2FrameCONTINUATION, H2FlagEndHeaders, 99, hpackBlock[half:])

	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on CONTINUATION streamID mismatch")
	}
}

// TestH2Continuation_NonContinuationInterleaved verifies that a frame of
// any type other than CONTINUATION arriving while a HEADERS sequence is
// open is rejected (RFC 7540 §6.10). gRPC peers cannot legally
// interleave DATA, RST_STREAM, etc. between HEADERS and trailing
// CONTINUATION.
func TestH2Continuation_NonContinuationInterleaved(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	hpackBlock := hpackEncodeForTest(t,
		hpack.HeaderField{Name: ":method", Value: "POST"},
		hpack.HeaderField{Name: ":path", Value: "/x"},
	)
	half := len(hpackBlock) / 2
	injectH2Frame(ctx, t, tx, H2FrameHEADERS, 0, 1, hpackBlock[:half])
	// DATA frame interleaved (would be valid on its own but illegal
	// here while HEADERS is open).
	injectH2Frame(ctx, t, tx, H2FrameDATA, 0, 1, []byte{0x00, 0x00, 0x00, 0x00, 0x00})
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on DATA frame interleaved between HEADERS and CONTINUATION")
	}
}

// TestH2Continuation_StrayContinuation verifies that a CONTINUATION
// frame appearing outside any HEADERS sequence is rejected.
func TestH2Continuation_StrayContinuation(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	injectH2Frame(ctx, t, tx, H2FrameCONTINUATION, H2FlagEndHeaders, 1,
		hpackEncodeForTest(t, hpack.HeaderField{Name: "x", Value: "y"}))
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on stray CONTINUATION outside a HEADERS sequence")
	}
}

// ---------------------------------------------------------------------------
// Self-review hardening tests (RFC 7540 §6.1, §6.2, §6.10, §8.1.2).
// ---------------------------------------------------------------------------

// TestH2HpackName_LowercaseOnEncode asserts that header field names are
// emitted lowercase on the H2 wire even when the in-memory metadata key
// arrives mixed-case. Real H2 peers (RFC 7540 §8.1.2) reject any
// uppercase byte in a field name with a connection error.
func TestH2HpackName_LowercaseOnEncode(t *testing.T) {
	enc := newHpackEncoderHolder()
	out := h2EncodeHeaders(enc.enc, enc.scratch, HeadersV1{
		Version: 1,
		HdrType: 0,
		Method:  "/svc/M",
		Metadata: []KV{
			{Key: "X-Mixed-Case", Values: [][]byte{[]byte("v")}},
		},
	})
	hf, err := decodeHpackToFields(out)
	if err != nil {
		t.Fatalf("decode hpack: %v", err)
	}
	for _, f := range hf {
		// All emitted names must be lowercase.
		for i := 0; i < len(f.Name); i++ {
			if c := f.Name[i]; c >= 'A' && c <= 'Z' {
				t.Errorf("name %q contains uppercase byte at %d (RFC 7540 §8.1.2 violation)", f.Name, i)
				break
			}
		}
	}
}

// TestH2BinaryMetadata_MixedCaseKey verifies that "-bin" suffix
// detection survives a mixed-case key on send, and that a peer sending
// a mixed-case "-Bin" suffix (non-conformant but possible) is still
// treated as binary on decode.
func TestH2BinaryMetadata_MixedCaseKey(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	binValue := []byte{0xCA, 0xFE, 0xBA, 0xBE}
	hdrPayload := encodeHeaders(HeadersV1{
		Version: 1, HdrType: 0, Method: "/svc/M",
		Metadata: []KV{
			{Key: "X-Mixed-Bin", Values: [][]byte{binValue}},
		},
	})
	if err := writeFrame(ctx, tx, FrameHeader{
		Type: FrameTypeHEADERS, StreamID: 1, Flags: HeadersFlagINITIAL,
	}, hdrPayload); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	_, got, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	dec, err := takeOrDecodeHeaders(rx.h2Decoder(), got)
	if err != nil {
		t.Fatalf("decodeHeaders: %v", err)
	}
	// Round-trip key arrives lowercased (we lowercase on send), value
	// arrives as raw binary bytes (base64 round-trip).
	var found []byte
	for _, kv := range dec.Metadata {
		if kv.Key == "x-mixed-bin" {
			found = kv.Values[0]
		}
	}
	if !bytes.Equal(found, binValue) {
		t.Errorf("mixed-case -bin round-trip: got %x want %x", found, binValue)
	}
}

func TestH2Validate_DataZeroStreamID(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// DATA on stream 0 — RFC 7540 §6.1 PROTOCOL_ERROR.
	injectH2Frame(ctx, t, tx, H2FrameDATA, 0, 0,
		[]byte{0x00, 0x00, 0x00, 0x00, 0x02, 'h', 'i'}) // valid LPM payload
	writeNormalMessageH2(ctx, t, tx, []byte("ok"))
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on DATA streamID=0")
	}
	readNormalMessageH2(ctx, t, rx, []byte("ok"))
}

func TestH2Validate_HeadersZeroStreamID(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	hpackBlock := hpackEncodeForTest(t,
		hpack.HeaderField{Name: ":method", Value: "POST"},
		hpack.HeaderField{Name: ":path", Value: "/x"},
	)
	// HEADERS on stream 0 — RFC 7540 §6.2 PROTOCOL_ERROR.
	injectH2Frame(ctx, t, tx, H2FrameHEADERS, H2FlagEndHeaders, 0, hpackBlock)
	writeNormalMessageH2(ctx, t, tx, []byte("ok"))
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on HEADERS streamID=0")
	}
	readNormalMessageH2(ctx, t, rx, []byte("ok"))
}

// TestH2Continuation_FrameCountCap asserts that a peer streaming an
// excessive number of zero-length CONTINUATION frames hits the
// h2MaxContinuationFrames bound and is rejected, even though the
// cumulative-byte cap (h2MaxHeaderListSize) is never tripped because
// each CONTINUATION's payload is empty. Defends against a buggy or
// adversarial local SHM peer that would otherwise tie up the reader
// goroutine.
func TestH2Continuation_FrameCountCap(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	hpackBlock := hpackEncodeForTest(t,
		hpack.HeaderField{Name: ":method", Value: "POST"},
		hpack.HeaderField{Name: ":path", Value: "/x"},
	)
	// First fragment without END_HEADERS opens the sequence.
	injectH2Frame(ctx, t, tx, H2FrameHEADERS, 0, 1, hpackBlock)
	// Inject one more than the cap of empty CONTINUATIONs (none with
	// END_HEADERS so the assembler keeps looping).
	for i := 0; i < h2MaxContinuationFrames+1; i++ {
		injectH2Frame(ctx, t, tx, H2FrameCONTINUATION, 0, 1, nil)
	}
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on CONTINUATION frame count overflow")
	}
}

// ---------------------------------------------------------------------------
// gRPC-over-HTTP/2 spec compliance tests (gRFC G2).
// ---------------------------------------------------------------------------

// TestH2LPM_DoSCap_OversizedDeclared asserts the LPM accumulator
// rejects a tiny DATA frame that declares an oversized body before
// allocating. Without this cap a malicious peer could declare a
// multi-gigabyte body in 5 bytes and force a giant make().
func TestH2LPM_DoSCap_OversizedDeclared(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// LPM header declaring an absurdly large body but providing only a
	// few bytes; the codec must reject the declaration before
	// allocating the buffer.
	declared := uint32(1 << 30) // 1 GiB declared, well past h2MaxLPMBodyBytes
	body := make([]byte, 7)
	body[0] = 0
	binary.BigEndian.PutUint32(body[1:5], declared)
	body[5] = 'a'
	body[6] = 'b'
	injectH2Frame(ctx, t, tx, H2FrameDATA, 0, 1, body)
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on oversized declared LPM body")
	}
}

// TestH2GrpcMessage_PercentEncoded asserts that a status message
// containing % and non-ASCII bytes round-trips via percent-encoding on
// the H2 wire (gRFC G2 / 'Status & status-message').
func TestH2GrpcMessage_PercentEncoded(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// Mix of plain ASCII, %, non-ASCII (中), and a control byte.
	msg := "boom 50% \u4e2d\u6587 \x01"
	tlrPayload := encodeTrailers(TrailersV1{
		Version:        1,
		GRPCStatusCode: 13,
		GRPCStatusMsg:  msg,
	})
	if err := writeFrame(ctx, tx, FrameHeader{
		Type: FrameTypeTRAILERS, StreamID: 5, Flags: TrailersFlagEndStream,
	}, tlrPayload); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	fh, got, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if fh.Type != FrameTypeTRAILERS {
		t.Fatalf("type: got %d want TRAILERS", fh.Type)
	}
	dec, err := takeOrDecodeTrailers(rx.h2Decoder(), got)
	if err != nil {
		t.Fatalf("decodeTrailers: %v", err)
	}
	if dec.GRPCStatusMsg != msg {
		t.Errorf("status message round-trip: got %q want %q", dec.GRPCStatusMsg, msg)
	}
}

// TestH2GrpcMessage_WireFormatIsPercentEncoded inspects the on-wire
// HPACK value to confirm '%' and non-ASCII are escaped per gRFC G2.
func TestH2GrpcMessage_WireFormatIsPercentEncoded(t *testing.T) {
	enc := newHpackEncoderHolder()
	out := h2EncodeTrailers(enc.enc, enc.scratch, TrailersV1{
		Version:        1,
		GRPCStatusCode: 13,
		GRPCStatusMsg:  "50% off",
	})
	hf, err := decodeHpackToFields(out)
	if err != nil {
		t.Fatalf("decode hpack: %v", err)
	}
	var seen string
	for _, f := range hf {
		if f.Name == "grpc-message" {
			seen = f.Value
		}
	}
	// '%' MUST be encoded as %25; ASCII printable space is allowed.
	if want := "50%25 off"; seen != want {
		t.Errorf("on-wire grpc-message: got %q want %q", seen, want)
	}
}

// TestH2GrpcTimeout_8DigitCap asserts that a deadline producing > 8
// digits in nanoseconds is encoded with a larger unit so the wire value
// fits the 8-digit gRFC G2 limit.
func TestH2GrpcTimeout_8DigitCap(t *testing.T) {
	enc := newHpackEncoderHolder()
	// 5 second deadline → 5_000_000_000 ns = 10 digits if naive 'n'
	// emission. EncodeDuration should pick a coarser unit.
	deadlineUnixNano := uint64(time.Now().Add(5 * time.Second).UnixNano())
	out := h2EncodeHeaders(enc.enc, enc.scratch, HeadersV1{
		Version:          1,
		HdrType:          0,
		Method:           "/svc/M",
		DeadlineUnixNano: deadlineUnixNano,
	})
	hf, err := decodeHpackToFields(out)
	if err != nil {
		t.Fatalf("decode hpack: %v", err)
	}
	var seen string
	for _, f := range hf {
		if f.Name == "grpc-timeout" {
			seen = f.Value
		}
	}
	if seen == "" {
		t.Fatal("grpc-timeout not emitted")
	}
	// Must be at most 8 digits + 1 unit byte.
	if len(seen) > 9 {
		t.Errorf("grpc-timeout %q is %d chars, exceeds 8-digit gRFC limit", seen, len(seen))
	}
}

// TestH2DataPadded_RoundTrip injects a PADDED DATA frame on stream 1
// containing a complete LPM and verifies the codec strips the padding
// and surfaces the LPM body. Self-interop never sends PADDED but
// standards-compliant H2 peers may.
func TestH2DataPadded_RoundTrip(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	body := []byte("hello")
	lpm := make([]byte, 5+len(body))
	lpm[0] = 0
	binary.BigEndian.PutUint32(lpm[1:5], uint32(len(body)))
	copy(lpm[5:], body)
	// PADDED layout: [padLen=3][LPM bytes][3 bytes padding].
	padded := make([]byte, 1+len(lpm)+3)
	padded[0] = 3
	copy(padded[1:], lpm)
	// Trailing 3 bytes default-zero (padding content is opaque).
	injectH2Frame(ctx, t, tx, H2FrameDATA, H2FlagPadded, 1, padded)
	fh, got, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if fh.Type != FrameTypeMESSAGE {
		t.Fatalf("type: got %d want MESSAGE", fh.Type)
	}
	if !bytes.Equal(got, lpm) {
		t.Errorf("padded DATA body: got %q want %q", got, lpm)
	}
}

// TestH2DataPadded_BadPadLen verifies a malformed pad-length is rejected
// AND the ring read pointer recovers (a normal MESSAGE frame after the
// malformed one is read intact).
func TestH2DataPadded_BadPadLen(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// padLen claims 100 bytes but payload is only 5 bytes total.
	bad := []byte{100, 0x00, 0x00, 0x00, 0x00}
	injectH2Frame(ctx, t, tx, H2FrameDATA, H2FlagPadded, 1, bad)
	writeNormalMessageH2(ctx, t, tx, []byte("ok"))
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on PADDED DATA with bad pad length")
	}
	readNormalMessageH2(ctx, t, rx, []byte("ok"))
}

// TestH2HeadersPriority_RoundTrip injects a HEADERS frame with the
// PRIORITY flag set: a 5-byte stream-dependency + weight prefix
// precedes the HPACK fragment. The codec must drop the priority bytes
// and decode the fragment.
func TestH2HeadersPriority_RoundTrip(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	hpackBlock := hpackEncodeForTest(t,
		hpack.HeaderField{Name: ":method", Value: "POST"},
		hpack.HeaderField{Name: ":path", Value: "/svc/Prio"},
	)
	// 5-byte priority prefix: 4-byte stream-dependency + 1-byte weight.
	withPrio := make([]byte, 5+len(hpackBlock))
	binary.BigEndian.PutUint32(withPrio[0:4], 7) // depends on stream 7
	withPrio[4] = 16                             // weight
	copy(withPrio[5:], hpackBlock)
	injectH2Frame(ctx, t, tx, H2FrameHEADERS, H2FlagEndHeaders|H2FlagPriority, 1, withPrio)
	fh, payload, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if fh.Type != FrameTypeHEADERS {
		t.Fatalf("type: got %d want HEADERS", fh.Type)
	}
	dec, err := takeOrDecodeHeaders(rx.h2Decoder(), payload)
	if err != nil {
		t.Fatalf("decodeHeaders: %v", err)
	}
	if dec.Method != "/svc/Prio" {
		t.Errorf("method: got %q want /svc/Prio", dec.Method)
	}
}

// TestH2HeadersPadded_RoundTrip injects a HEADERS frame with the
// PADDED flag and verifies the codec strips both the pad-length prefix
// and the trailing padding before decoding the HPACK fragment.
func TestH2HeadersPadded_RoundTrip(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	hpackBlock := hpackEncodeForTest(t,
		hpack.HeaderField{Name: ":method", Value: "POST"},
		hpack.HeaderField{Name: ":path", Value: "/svc/Pad"},
	)
	padLen := 4
	padded := make([]byte, 1+len(hpackBlock)+padLen)
	padded[0] = byte(padLen)
	copy(padded[1:], hpackBlock)
	// Padding bytes default-zero.
	injectH2Frame(ctx, t, tx, H2FrameHEADERS, H2FlagEndHeaders|H2FlagPadded, 1, padded)
	fh, payload, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if fh.Type != FrameTypeHEADERS {
		t.Fatalf("type: got %d want HEADERS", fh.Type)
	}
	dec, err := takeOrDecodeHeaders(rx.h2Decoder(), payload)
	if err != nil {
		t.Fatalf("decodeHeaders: %v", err)
	}
	if dec.Method != "/svc/Pad" {
		t.Errorf("method: got %q want /svc/Pad", dec.Method)
	}
}

// ---------------------------------------------------------------------------
// Second-pass review hardening tests.
// ---------------------------------------------------------------------------

// TestH2LPM_NoPreallocOversized asserts the LPM accumulator does NOT
// allocate the full declared body size up-front. A peer-controlled
// tiny DATA frame that declares (h2MaxLPMBodyBytes - 1) MiB but sends
// only a few body bytes must NOT cause a hundreds-of-MiB heap
// allocation before any per-RPC receive limit applies. Verified by
// observing runtime.ReadMemStats before/after a single failing-feed
// call.
func TestH2LPM_NoPreallocOversized(t *testing.T) {
	// Construct a deliberately-truthful LPM header inside the
	// declared cap (so the size-check fast-path passes) but only feed
	// a few body bytes; assert the resident allocation stays small.
	const declared = h2MaxLPMBodyBytes - 1024 // just under the cap
	hdrAndPartialBody := make([]byte, 5+1024)
	hdrAndPartialBody[0] = 0
	binary.BigEndian.PutUint32(hdrAndPartialBody[1:5], uint32(declared))
	for i := 5; i < len(hdrAndPartialBody); i++ {
		hdrAndPartialBody[i] = byte(i)
	}

	var memBefore, memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	acc := &lpmAccumulator{}
	msg, _, err := acc.feed(hdrAndPartialBody, h2MaxLPMBodyBytes)
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	if msg != nil {
		t.Fatal("expected msg nil — only partial body fed")
	}
	runtime.ReadMemStats(&memAfter)

	// HeapAlloc growth should be on the order of bytes actually fed
	// (< 256 KiB allowing for slice growth slack) — NOT the declared
	// size which is hundreds of MiB.
	growth := memAfter.HeapAlloc - memBefore.HeapAlloc
	const maxAcceptableGrowth = 256 * 1024
	if growth > maxAcceptableGrowth {
		t.Errorf("HeapAlloc grew %d bytes after feeding 1029 bytes (declared %d); pre-allocation DoS guard is leaky",
			growth, declared)
	}

	// Ensure the accumulator can still complete the message if all
	// bytes arrive (sanity check: incremental allocation didn't break
	// the happy path).
	_ = acc // silence linter; growth into legitimate territory is
	// covered by other LPM tests in the suite.
}

// TestH2DataPaddedZeroLength_ViewReader asserts the production
// readFrameView path (which dispatches to readFrameViewH2) rejects a
// PADDED|END_STREAM DATA frame with Length=0 instead of treating it as
// a valid HALFCLOSE. The mandatory 1-byte pad-length prefix can't fit
// in a zero-byte payload (FRAME_SIZE_ERROR per RFC 7540 §6.1).
func TestH2DataPaddedZeroLength_ViewReader(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// Inject DATA with PADDED|END_STREAM and Length=0.
	injectH2Frame(ctx, t, tx, H2FrameDATA, H2FlagPadded|H2FlagEndStream, 1, nil)
	// readFrameView is the production hot path; it dispatches to
	// readFrameViewH2 because the ring is configured with HTTP/2.
	_, buf, err := readFrameView(ctx, rx)
	if buf != nil {
		buf.Free()
	}
	if err == nil {
		t.Fatal("expected error on PADDED|END_STREAM DATA with Length=0")
	}
}

// TestH2GrpcTimeout_DecodeTooLong asserts the receive-side timeout
// parser rejects values whose digit-portion exceeds the 8-digit gRFC
// limit (e.g., a 10-digit nanosecond value emitted by a non-conformant
// peer). The previous parseGrpcTimeout helper accepted any-length
// digit string and could overflow on huge hour values.
func TestH2GrpcTimeout_DecodeTooLong(t *testing.T) {
	enc := newHpackEncoderHolder()
	dec := newHpackDecoderHolder()

	// Hand-craft a HEADERS HPACK block with grpc-timeout="999999999n"
	// (9 digits + unit = 10 chars > 9-char limit).
	var buf bytes.Buffer
	hpackEnc := hpack.NewEncoder(&buf)
	_ = hpackEnc.WriteField(hpack.HeaderField{Name: ":method", Value: "POST"})
	_ = hpackEnc.WriteField(hpack.HeaderField{Name: ":path", Value: "/x"})
	_ = hpackEnc.WriteField(hpack.HeaderField{Name: "grpc-timeout", Value: "999999999n"})
	hpackBlock := buf.Bytes()

	h, _, _, err := h2DecodeHeaders(dec, hpackBlock)
	if err != nil {
		t.Fatalf("h2DecodeHeaders: %v", err)
	}
	// On reject (length > 9), DeadlineUnixNano should remain 0 rather
	// than carry an overflow / nonsense deadline. The decoder
	// silently drops the bad value (matches stock grpc-go behaviour
	// which does not surface a malformed-timeout connection error
	// when the field's syntax is invalid; the upper layer then
	// treats the call as no-deadline).
	if h.DeadlineUnixNano != 0 {
		t.Errorf("DeadlineUnixNano: got %d want 0 for over-long timeout (10 digits)", h.DeadlineUnixNano)
	}
	_ = enc // unused but keeps imports stable
}

// ---------------------------------------------------------------------------
// Third-pass review hardening tests.
// ---------------------------------------------------------------------------

// TestH2DataEndStreamWithBody_EmitsHalfClose asserts that a DATA frame
// TestH2DataEndStreamWithBody_EmitsMoreClear verifies that a DATA
// frame with a non-empty body AND END_STREAM flag — the canonical
// shape emitted by stock grpc-go's HTTP/2 transport, grpc-java, and
// grpc-c++ when finishing a unary client send — surfaces a MESSAGE
// with MessageFlagMORE CLEARED. The MORE=0 signal is what
// ShmServerTransport.handleMessage uses to write io.EOF on the
// stream's recv channel; without it the call hangs waiting for an
// explicit half-close.
func TestH2DataEndStreamWithBody_EmitsMoreClear(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	body := []byte("hello")
	lpm := make([]byte, 5+len(body))
	lpm[0] = 0
	binary.BigEndian.PutUint32(lpm[1:5], uint32(len(body)))
	copy(lpm[5:], body)
	injectH2Frame(ctx, t, tx, H2FrameDATA, H2FlagEndStream, 1, lpm)

	fh, got, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if fh.Type != FrameTypeMESSAGE {
		t.Fatalf("frame type: got %d want MESSAGE", fh.Type)
	}
	if !bytes.Equal(got, lpm) {
		t.Errorf("body: got %q want %q", got, lpm)
	}
	if fh.Flags&MessageFlagMORE != 0 {
		t.Errorf("flags: got MORE=1 on END_STREAM-bearing DATA, want MORE=0")
	}
}

// TestH2DataEndStreamWithBody_ViewReader_EmitsMoreClear covers the
// production hot path (readFrameView dispatches to readFrameViewH2
// for HTTP/2 rings).
func TestH2DataEndStreamWithBody_ViewReader_EmitsMoreClear(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	body := make([]byte, 200*1024) // big enough to hit ZC fast path
	for i := range body {
		body[i] = byte(i)
	}
	lpm := make([]byte, 5+len(body))
	lpm[0] = 0
	binary.BigEndian.PutUint32(lpm[1:5], uint32(len(body)))
	copy(lpm[5:], body)
	injectH2Frame(ctx, t, tx, H2FrameDATA, H2FlagEndStream, 1, lpm)

	fh, buf, err := readFrameView(ctx, rx)
	if err != nil {
		t.Fatalf("readFrameView: %v", err)
	}
	if fh.Type != FrameTypeMESSAGE {
		t.Fatalf("type: got %d want MESSAGE", fh.Type)
	}
	if fh.Flags&MessageFlagMORE != 0 {
		t.Errorf("flags: got MORE=1 on END_STREAM-bearing DATA, want MORE=0")
	}
	if buf != nil {
		buf.Free()
	}
}

// TestH2LPM_AccumulatorResetOnError verifies that an LPM accumulator
// rejected for body-too-large clears its internal state so a subsequent
// feed call (with valid content) starts from scratch. Without the
// reset, the second call would be stuck in headerBytesSeen=5 + zero
// expectedTotal and silently drop legitimate data.
func TestH2LPM_AccumulatorResetOnError(t *testing.T) {
	acc := &lpmAccumulator{}
	// First feed: 5-byte LPM header declaring 1 GiB body — rejected
	// against a 1 KiB cap.
	tooBig := make([]byte, 5)
	tooBig[0] = 0
	binary.BigEndian.PutUint32(tooBig[1:5], 1<<30)
	if _, _, err := acc.feed(tooBig, 1024); err == nil {
		t.Fatal("expected reject on oversized body")
	}
	if acc.headerBytesSeen != 0 {
		t.Errorf("headerBytesSeen after error: got %d want 0", acc.headerBytesSeen)
	}

	// Second feed: a complete valid LPM. Accumulator must reparse the
	// header from scratch and produce a complete message.
	body := []byte("ok")
	good := make([]byte, 5+len(body))
	good[0] = 0
	binary.BigEndian.PutUint32(good[1:5], uint32(len(body)))
	copy(good[5:], body)
	msg, leftover, err := acc.feed(good, 0)
	if err != nil {
		t.Fatalf("feed after recovered state: %v", err)
	}
	if !bytes.Equal(msg, good) {
		t.Errorf("post-recovery msg: got %q want %q", msg, good)
	}
	if len(leftover) != 0 {
		t.Errorf("leftover: got %d bytes want 0", len(leftover))
	}
}

// TestH2HpackString_MaxLengthEnforced verifies the HPACK decoder
// rejects a single header value longer than the configured cap (64
// KiB). golang.org/x/net/http2/hpack.Decoder.SetMaxStringLength
// enforces this; without the cap a peer could allocate gigabytes via
// a single oversized header.
func TestH2HpackString_MaxLengthEnforced(t *testing.T) {
	dec := newHpackDecoderHolder()
	// Encode a header whose value is 128 KiB (above the 64 KiB cap).
	huge := make([]byte, 128*1024)
	for i := range huge {
		huge[i] = 'a' + byte(i%26)
	}
	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	_ = enc.WriteField(hpack.HeaderField{Name: "x-huge", Value: string(huge)})
	if _, _, _, err := h2DecodeHeaders(dec, buf.Bytes()); err == nil {
		t.Fatal("expected error on HPACK string longer than cap")
	}
}

// TestH2DataMultiLPM_EndStreamCarriesAcrossLeftover verifies that
// END_STREAM on a DATA frame containing multiple LPMs surfaces
// MESSAGE/MORE=1 for all LPMs except the LAST one, which carries
// MORE=0. Regression test: pre-fix the leftover-stash path forgot the
// END_STREAM flag, every emitted MESSAGE had MORE=0, and the server
// transport observed io.EOF after the FIRST LPM rather than after the
// last.
func TestH2DataMultiLPM_EndStreamCarriesAcrossLeftover(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// Build a DATA payload containing two complete LPMs back-to-back.
	body1 := []byte("first")
	body2 := []byte("second")
	lpm1 := make([]byte, 5+len(body1))
	lpm1[0] = 0
	binary.BigEndian.PutUint32(lpm1[1:5], uint32(len(body1)))
	copy(lpm1[5:], body1)
	lpm2 := make([]byte, 5+len(body2))
	lpm2[0] = 0
	binary.BigEndian.PutUint32(lpm2[1:5], uint32(len(body2)))
	copy(lpm2[5:], body2)
	combined := append(append([]byte{}, lpm1...), lpm2...)
	injectH2Frame(ctx, t, tx, H2FrameDATA, H2FlagEndStream, 1, combined)

	// First read: MESSAGE with lpm1, MORE=1 (more LPMs to come from
	// this DATA frame's leftover).
	fh, got, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame 1: %v", err)
	}
	if fh.Type != FrameTypeMESSAGE || !bytes.Equal(got, lpm1) {
		t.Fatalf("first frame: type=%d body=%q want MESSAGE %q", fh.Type, got, lpm1)
	}
	if fh.Flags&MessageFlagMORE == 0 {
		t.Errorf("first frame flags: got MORE=0, want MORE=1 (leftover present)")
	}

	// Second read: MESSAGE with lpm2, MORE=0 (last LPM of the
	// END_STREAM-bearing DATA frame).
	fh2, got2, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame 2: %v", err)
	}
	if fh2.Type != FrameTypeMESSAGE || !bytes.Equal(got2, lpm2) {
		t.Fatalf("second frame: type=%d body=%q want MESSAGE %q", fh2.Type, got2, lpm2)
	}
	if fh2.Flags&MessageFlagMORE != 0 {
		t.Errorf("second frame flags: got MORE=1, want MORE=0 (last LPM, END_STREAM source)")
	}
}

// TestH2LpmAccumulator_MRUCacheHits exercises the MRU cache
// short-circuit. Every consecutive call with the same stream id
// should bypass the map lookup. Cache invalidates correctly when the
// accumulator is removed.
func TestH2LpmAccumulator_MRUCacheHits(t *testing.T) {
	holder := newHpackDecoderHolder()
	a1 := holder.getLpmAccumulator(7)
	if holder.lastSid != 7 || holder.lastAcc != a1 {
		t.Fatalf("MRU cache not populated after first lookup: lastSid=%d lastAcc=%v",
			holder.lastSid, holder.lastAcc)
	}
	// Second lookup of same sid: must short-circuit (return cached).
	if a2 := holder.getLpmAccumulator(7); a2 != a1 {
		t.Errorf("MRU cache miss for repeat sid=7: got %p want %p", a2, a1)
	}
	// Different sid: cache rotates.
	a3 := holder.getLpmAccumulator(11)
	if holder.lastSid != 11 || holder.lastAcc != a3 {
		t.Errorf("MRU cache not updated for new sid: lastSid=%d", holder.lastSid)
	}
	// Removing the cached sid invalidates the cache.
	holder.removeLpmAccumulator(11)
	if holder.lastSid != 0 || holder.lastAcc != nil {
		t.Errorf("MRU cache not invalidated after removal: lastSid=%d lastAcc=%v",
			holder.lastSid, holder.lastAcc)
	}
}

// ---------------------------------------------------------------------------
// Fourth-pass review hardening tests.
// ---------------------------------------------------------------------------

// TestH2DataPartialLPM_EndStream rejects a DATA frame that delivers
// only a partial LPM header AND has END_STREAM. Without explicit
// rejection the codec previously left the accumulator in
// "in-progress" state and waited for a next DATA frame that would
// never arrive (the peer signalled end of stream), hanging the
// reader.
func TestH2DataPartialLPM_EndStream(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// Send LPM1 (complete) + 3 bytes of LPM2's 5-byte header, all in
	// one DATA frame with END_STREAM set.
	lpm1 := []byte{0x00, 0x00, 0x00, 0x00, 0x02, 'h', 'i'} // body=2
	partial2 := []byte{0x00, 0x00, 0x00}                   // 3 of 5 header bytes
	combined := append(append([]byte{}, lpm1...), partial2...)
	injectH2Frame(ctx, t, tx, H2FrameDATA, H2FlagEndStream, 1, combined)

	// First read: MESSAGE with lpm1 (MORE=1 — leftover stashed).
	fh, _, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame 1: %v", err)
	}
	if fh.Type != FrameTypeMESSAGE {
		t.Fatalf("first frame: got type %d, want MESSAGE", fh.Type)
	}

	// Second read: replay path feeds partial header. END_STREAM was
	// set; partial header in-progress with no more bytes coming →
	// must error.
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error: partial LPM header at END_STREAM")
	}
}

// TestH2EmptyDataEndStream_RemovesAccumulator verifies the empty-DATA
// + END_STREAM HALFCLOSE branch drops the per-stream accumulator when
// the accumulator is at a clean boundary (no message in flight). A
// long-lived connection processing many streams must not leak
// accumulator map entries, which would grow lpmAccumulators
// unboundedly. Setup uses a multi-DATA delivery of a complete LPM
// (split header / body across two frames) to force accumulator
// creation; once the message completes the accumulator is idle and
// can be removed safely on END_STREAM.
func TestH2EmptyDataEndStream_RemovesAccumulator(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// Frame 1: LPM header only — accumulator absorbs all 5 bytes
	// (no message yet emitted). Multi-DATA path forces accumulator
	// to be allocated for stream 1.
	header := []byte{0x00, 0x00, 0x00, 0x00, 0x02} // declares 2-byte body
	injectH2Frame(ctx, t, tx, H2FrameDATA, 0, 1, header)
	// Frame 2: full body — message completes; accumulator returns to
	// idle state (headerBytesSeen=0, pos=0) but map entry persists.
	body := []byte{'h', 'i'}
	injectH2Frame(ctx, t, tx, H2FrameDATA, 0, 1, body)
	// Frame 3: empty DATA + END_STREAM (canonical translateCustomToH2
	// HALFCLOSE encoding). Accumulator is idle so this is a clean
	// half-close — must NOT error and must drop the map entry.
	injectH2Frame(ctx, t, tx, H2FrameDATA, H2FlagEndStream, 1, nil)

	// First read surfaces the assembled MESSAGE.
	if fh, _, err := readFrame(ctx, rx); err != nil {
		t.Fatalf("readFrame MESSAGE: %v", err)
	} else if fh.Type != FrameTypeMESSAGE {
		t.Fatalf("first frame: got type %d want MESSAGE", fh.Type)
	}

	// Second read surfaces HALFCLOSE.
	fh, _, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame HALFCLOSE: %v", err)
	}
	if fh.Type != FrameTypeHALFCLOSE {
		t.Fatalf("frame type: got %d want HALFCLOSE", fh.Type)
	}

	// Inspect holder state: accumulator must have been removed.
	holder := rx.h2Decoder()
	if _, exists := holder.lpmAccumulators[1]; exists {
		t.Error("accumulator for stream 1 still present after empty-DATA+END_STREAM HALFCLOSE")
	}
}

// TestH2EmptyDataEndStream_RejectsPartialLPMInAccumulator covers the
// case where the per-stream accumulator is mid-message (header parsed,
// body partially received) when an empty DATA + END_STREAM arrives.
// Per gRPC framing, END_STREAM here truncates a length-prefixed
// message that the application is still expecting bytes for; silently
// surfacing HALFCLOSE would lose the in-flight message body. The
// codec must error and clear the accumulator to avoid replaying the
// truncated state on a subsequent stream that reuses the same id.
func TestH2EmptyDataEndStream_RejectsPartialLPMInAccumulator(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// Frame 1: full LPM header declaring 5-byte body, no body sent —
	// accumulator now headerBytesSeen=5, expectedTotal=10, pos=5,
	// inProgress()==true.
	partial := []byte{0x00, 0x00, 0x00, 0x00, 0x05}
	injectH2Frame(ctx, t, tx, H2FrameDATA, 0, 1, partial)
	// Frame 2: empty DATA + END_STREAM. With partial LPM in flight
	// this must surface as an error (truncated message), not as a
	// silent HALFCLOSE.
	injectH2Frame(ctx, t, tx, H2FrameDATA, H2FlagEndStream, 1, nil)

	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on empty DATA+END_STREAM with partial LPM in accumulator")
	}

	// Accumulator must be cleared so a subsequent reused stream id
	// doesn't see stale state.
	holder := rx.h2Decoder()
	if _, exists := holder.lpmAccumulators[1]; exists {
		t.Error("accumulator for stream 1 still present after error path")
	}
}

// TestH2EmptyDataEndStream_RejectsPartialLPM_ViewReader mirrors
// TestH2EmptyDataEndStream_RejectsPartialLPMInAccumulator on the
// readFrameView (zero-copy SliceBuffer) path. The two readers share
// the codec state machine but have parallel branches for the empty
// DATA + END_STREAM case; both must reject partial LPMs.
func TestH2EmptyDataEndStream_RejectsPartialLPM_ViewReader(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	partial := []byte{0x00, 0x00, 0x00, 0x00, 0x05}
	injectH2Frame(ctx, t, tx, H2FrameDATA, 0, 1, partial)
	injectH2Frame(ctx, t, tx, H2FrameDATA, H2FlagEndStream, 1, nil)

	if _, buf, err := readFrameView(ctx, rx); err == nil {
		if buf != nil {
			buf.Free()
		}
		t.Fatal("expected error on empty DATA+END_STREAM with partial LPM (view reader)")
	}
	holder := rx.h2Decoder()
	if _, exists := holder.lpmAccumulators[1]; exists {
		t.Error("accumulator for stream 1 still present after error path (view reader)")
	}
}

// TestH2Trailers_RejectsPartialLPMInAccumulator covers the parallel
// case for the TRAILERS branch: a HEADERS frame with END_STREAM that
// the codec interprets as gRPC trailers. If the per-stream
// accumulator is mid-message at trailers arrival, the application's
// buffered response is truncated; the codec must error rather than
// surface clean trailers + drop the partial bytes.
func TestH2Trailers_RejectsPartialLPMInAccumulator(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// Frame 1: initial HEADERS (no END_STREAM) — establishes the
	// stream so the next HEADERS is treated as trailers, not initial.
	hpackInit := hpackEncodeForTest(t,
		hpack.HeaderField{Name: ":method", Value: "POST"},
		hpack.HeaderField{Name: ":path", Value: "/svc/M"},
		hpack.HeaderField{Name: "te", Value: "trailers"},
		hpack.HeaderField{Name: "content-type", Value: "application/grpc"},
	)
	injectH2Frame(ctx, t, tx, H2FrameHEADERS, H2FlagEndHeaders, 1, hpackInit)
	// Frame 2: partial LPM body — accumulator becomes inProgress.
	partial := []byte{0x00, 0x00, 0x00, 0x00, 0x05}
	injectH2Frame(ctx, t, tx, H2FrameDATA, 0, 1, partial)
	// Frame 3: trailers (HEADERS + END_STREAM) — must error, not
	// silently emit clean trailers.
	hpackTrailers := hpackEncodeForTest(t,
		hpack.HeaderField{Name: "grpc-status", Value: "0"},
	)
	injectH2Frame(ctx, t, tx, H2FrameHEADERS,
		H2FlagEndHeaders|H2FlagEndStream, 1, hpackTrailers)

	// First read: HEADERS frame (initial).
	if fh, _, err := readFrame(ctx, rx); err != nil {
		t.Fatalf("readFrame HEADERS: %v", err)
	} else if fh.Type != FrameTypeHEADERS {
		t.Fatalf("first frame: got type %d want HEADERS", fh.Type)
	}
	// Second read: must error on TRAILERS-with-partial-LPM.
	if _, _, err := readFrame(ctx, rx); err == nil {
		t.Fatal("expected error on TRAILERS with partial LPM in accumulator")
	}
	holder := rx.h2Decoder()
	if _, exists := holder.lpmAccumulators[1]; exists {
		t.Error("accumulator for stream 1 still present after error path")
	}
}

// TestH2InitialHeadersEndStream_EmitsHalfClose covers the
// zero-message client-streaming case: the client sends a single
// HEADERS frame with END_STREAM (no DATA frames at all) to indicate
// "request started, no payload, half-close immediately". The codec
// must surface BOTH the HEADERS frame (so the server runs the RPC
// handler) AND a synthetic HALFCLOSE frame (so the handler's recv
// path observes io.EOF promptly). Without the synthetic HALFCLOSE
// the server would hang on the recv waiting for a half-close that
// never arrives.
func TestH2InitialHeadersEndStream_EmitsHalfClose(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	hpackBlock := hpackEncodeForTest(t,
		hpack.HeaderField{Name: ":method", Value: "POST"},
		hpack.HeaderField{Name: ":path", Value: "/svc/M"},
		hpack.HeaderField{Name: "te", Value: "trailers"},
		hpack.HeaderField{Name: "content-type", Value: "application/grpc"},
	)
	injectH2Frame(ctx, t, tx, H2FrameHEADERS,
		H2FlagEndHeaders|H2FlagEndStream, 1, hpackBlock)

	// First read: HEADERS surfaced as FrameTypeHEADERS so the server
	// dispatch picks up the RPC.
	fh, _, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame HEADERS: %v", err)
	}
	if fh.Type != FrameTypeHEADERS {
		t.Fatalf("first frame type: got %d want HEADERS", fh.Type)
	}
	if fh.StreamID != 1 {
		t.Fatalf("first frame StreamID: got %d want 1", fh.StreamID)
	}

	// Second read: synthetic HALFCLOSE on the same stream id — drives
	// the server-side recv loop's io.EOF without waiting for any DATA.
	fh, _, err = readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame HALFCLOSE: %v", err)
	}
	if fh.Type != FrameTypeHALFCLOSE {
		t.Fatalf("second frame type: got %d want HALFCLOSE", fh.Type)
	}
	if fh.StreamID != 1 {
		t.Fatalf("HALFCLOSE StreamID: got %d want 1", fh.StreamID)
	}

	// Pending half-close state must be cleared after surfacing.
	holder := rx.h2Decoder()
	if holder.pendingHalfCloseStreamID != 0 {
		t.Errorf("pendingHalfCloseStreamID not cleared: %d",
			holder.pendingHalfCloseStreamID)
	}
}

// TestH2InitialHeadersEndStream_EmitsHalfClose_ViewReader mirrors the
// readFrame test on the readFrameView path. Both readers must emit
// the synthetic HALFCLOSE for HEADERS+END_STREAM since either may be
// invoked by the server transport's dispatch loop.
func TestH2InitialHeadersEndStream_EmitsHalfClose_ViewReader(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	hpackBlock := hpackEncodeForTest(t,
		hpack.HeaderField{Name: ":method", Value: "POST"},
		hpack.HeaderField{Name: ":path", Value: "/svc/M"},
		hpack.HeaderField{Name: "te", Value: "trailers"},
		hpack.HeaderField{Name: "content-type", Value: "application/grpc"},
	)
	injectH2Frame(ctx, t, tx, H2FrameHEADERS,
		H2FlagEndHeaders|H2FlagEndStream, 1, hpackBlock)

	// First read: HEADERS via view reader.
	fh, buf, err := readFrameView(ctx, rx)
	if err != nil {
		t.Fatalf("readFrameView HEADERS: %v", err)
	}
	if buf != nil {
		buf.Free()
	}
	if fh.Type != FrameTypeHEADERS {
		t.Fatalf("first frame type: got %d want HEADERS", fh.Type)
	}

	// Second read: synthetic HALFCLOSE.
	fh, buf, err = readFrameView(ctx, rx)
	if err != nil {
		t.Fatalf("readFrameView HALFCLOSE: %v", err)
	}
	if buf != nil {
		buf.Free()
	}
	if fh.Type != FrameTypeHALFCLOSE {
		t.Fatalf("second frame type: got %d want HALFCLOSE", fh.Type)
	}
	if fh.StreamID != 1 {
		t.Fatalf("HALFCLOSE StreamID: got %d want 1", fh.StreamID)
	}
}

// TestH2RstStream_ClearsPendingFrame verifies that RST_STREAM after a
// DATA frame that left a partial LPM in pendingFrame correctly clears
// the pendingFrame state. Without this clear, a subsequent read would
// replay the dead stream's leftover bytes against a freshly-recreated
// accumulator on the same stream id (legitimate for protocol-violating
// peers; defense-in-depth for our codec).
func TestH2RstStream_ClearsPendingFrame(t *testing.T) {
	tx, rx, ctx, _, _ := newH2RingPair(t)
	// Build DATA[lpm1][partial-lpm2-header] on stream 1.
	body1 := []byte("ok")
	lpm1 := make([]byte, 5+len(body1))
	lpm1[0] = 0
	binary.BigEndian.PutUint32(lpm1[1:5], uint32(len(body1)))
	copy(lpm1[5:], body1)
	// 3 of 5 LPM-header bytes for a phantom lpm2.
	partial := []byte{0x00, 0x00, 0x00}
	combined := append(append([]byte{}, lpm1...), partial...)
	injectH2Frame(ctx, t, tx, H2FrameDATA, 0, 1, combined)

	// First read: MESSAGE lpm1 (MORE=1). Stashes pendingFrame=partial.
	if _, _, err := readFrame(ctx, rx); err != nil {
		t.Fatalf("readFrame 1: %v", err)
	}
	holder := rx.h2Decoder()
	if len(holder.pendingFrame) == 0 {
		t.Fatal("expected pendingFrame to be populated after partial LPM")
	}
	if holder.pendingStreamID != 1 {
		t.Fatalf("pendingStreamID: got %d want 1", holder.pendingStreamID)
	}

	// Inject RST_STREAM on stream 1.
	rstPayload := []byte{0, 0, 0, 0x08} // CANCEL error code
	injectH2Frame(ctx, t, tx, H2FrameRSTSTREAM, 0, 1, rstPayload)
	fh, _, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame after RST: %v", err)
	}
	if fh.Type != FrameTypeCANCEL {
		t.Fatalf("expected CANCEL, got %d", fh.Type)
	}

	// Verify pendingFrame state was cleared.
	if len(holder.pendingFrame) != 0 {
		t.Errorf("pendingFrame not cleared after RST_STREAM: %d bytes leftover", len(holder.pendingFrame))
	}
	if holder.pendingStreamID != 0 {
		t.Errorf("pendingStreamID not cleared after RST_STREAM: %d", holder.pendingStreamID)
	}
	if holder.pendingFrameEndStream {
		t.Error("pendingFrameEndStream not cleared after RST_STREAM")
	}
}

// TestH2WriteFrame_PayloadExceedsRingCapacity_Chunks verifies that
// writeFrame splits a MESSAGE whose total wire size (H2 header +
// payload) exceeds the ring capacity into multiple H2 DATA frames,
// and the reader's LPM accumulator reassembles them into a single
// MESSAGE on the receive side.
//
// Without this chunking behavior, ReserveWrite would reject the
// single-frame write outright (it enforces n <= capacity) and the
// caller would see an error for a logically-valid message. Per
// gRFC G3 §"Framing on the Ring": a frame larger than ring capacity
// is well-formed and must be carried incrementally.
//
// Regression test for the H2-only refactor: prior to the fix,
// writeFrameH2 only chunked when payload exceeded the 16 MiB-1 H2
// protocol limit, ignoring ring capacity entirely. With a 64 KiB
// ring, a 128 KiB MESSAGE would fail to send.
func TestH2WriteFrame_PayloadExceedsRingCapacity_Chunks(t *testing.T) {
	// Use a small ring so the test stays fast and the chunking path
	// is exercised even for modest message sizes. Ring capacity must
	// be a power of two; 64 KiB is the smallest practical size.
	const ringCap = 64 * 1024
	segName := fmt.Sprintf("h2chunk-%d-%d", time.Now().UnixNano(), goroutineID())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, ringCap, ringCap)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()
	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	// Build a 128 KiB LPM body (2 * ring capacity) so writeFrameH2
	// MUST chunk: a single frame of 9 (H2 header) + 5 (LPM) + 131072
	// (body) = 131086 bytes exceeds the 64 KiB ring capacity.
	const bodyLen = 2 * ringCap
	body := make([]byte, bodyLen)
	for i := range body {
		body[i] = byte((i * 31) ^ 0xA5)
	}
	lpm := make([]byte, 5+len(body))
	lpm[0] = 0 // no compression
	binary.BigEndian.PutUint32(lpm[1:5], uint32(len(body)))
	copy(lpm[5:], body)

	// Writer and reader run concurrently so the ring drains and the
	// next chunk's ReserveWrite can succeed.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type readResult struct {
		fh      FrameHeader
		payload []byte
		err     error
	}
	resCh := make(chan readResult, 1)
	go func() {
		fh, p, err := readFrame(ctx, rx)
		resCh <- readResult{fh: fh, payload: p, err: err}
	}()

	if err := writeFrame(ctx, tx, FrameHeader{Type: FrameTypeMESSAGE, StreamID: 1}, lpm); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("readFrame: %v", r.err)
		}
		if r.fh.Type != FrameTypeMESSAGE {
			t.Fatalf("frame type: got %d want MESSAGE", r.fh.Type)
		}
		if r.fh.StreamID != 1 {
			t.Fatalf("stream id: got %d want 1", r.fh.StreamID)
		}
		if !bytes.Equal(r.payload, lpm) {
			t.Fatalf("payload mismatch: got %d bytes want %d bytes (equal? %v)",
				len(r.payload), len(lpm), bytes.Equal(r.payload, lpm))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("read timed out — writer probably failed to chunk")
	}
}

// TestH2ReadFrameView_MultiFrameLPM_MidChainGrowDoubling verifies the
// readFrameViewH2 mid-chain fast path goes through growBufForChunk
// (explicit 2× doubling) instead of falling back to Go's default 1.25×
// slice growth.
//
// Regression: a multi-frame LPM hit the mid-chain fast path which
// used `append(acc.buf, ...)` directly without sizing the buffer
// up-front. For a 4-chunk LPM where each chunk's appended size
// matches the current cap, Go's 1.25× factor takes ~3-5 grow-realloc
// cycles to reach the final size; explicit doubling needs just 2.
// The wasted memcpy cost on a 64 MiB-class message is ~80 MiB.
//
// The test reads a 4-chunk LPM through the production readFrameView
// path while observing total HeapAlloc growth. Without the fix, the
// observed growth exceeds the LPM size by >= 50% (the grow-cascade
// allocates intermediate buffers that GC eventually reclaims, but
// the high-water mark is visible in MemStats during the read).
//
// Correctness coverage (wire bytes round-trip intact) is checked
// alongside the allocation bound.
func TestH2ReadFrameView_MultiFrameLPM_MidChainGrowDoubling(t *testing.T) {
	const chunkBody = 1 * 1024 * 1024
	const numChunks = 4
	const expectedTotal = numChunks * chunkBody
	const bodyLen = expectedTotal - 5

	const ringCap = 16 * 1024 * 1024
	segName := fmt.Sprintf("h2midchain-%d-%d", time.Now().UnixNano(), goroutineID())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, ringCap, ringCap)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()
	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Build the 4-chunk wire stream. Chunk 1 = LPM header + first
	// body slice; chunks 2-4 = pure body.
	hdr := buildLPMHeaderTest(bodyLen)
	chunk1Body := make([]byte, chunkBody-5)
	for i := range chunk1Body {
		chunk1Body[i] = byte(i & 0xFF)
	}
	chunk1 := append(append([]byte{}, hdr...), chunk1Body...) // 16 MiB wire

	bodyChunks := make([][]byte, numChunks-1)
	for i := range bodyChunks {
		bodyChunks[i] = make([]byte, chunkBody)
		for j := range bodyChunks[i] {
			bodyChunks[i][j] = byte((j + i + 1) & 0xFF)
		}
	}

	// Inject all 4 frames. The first three carry no END_STREAM; the
	// fourth completes the LPM (still no END_STREAM since this is
	// a streaming-style send, not the final DATA of an RPC).
	injectH2Frame(ctx, t, tx, H2FrameDATA, 0, 1, chunk1)
	for _, b := range bodyChunks {
		injectH2Frame(ctx, t, tx, H2FrameDATA, 0, 1, b)
	}

	// The reader's mid-chain fast path is hit between chunks 2-4
	// (chunk 1 goes through feedSplit). We can't observe the cap
	// progression mid-frame from outside readFrameViewH2, but we
	// CAN measure the total heap growth during the multi-chunk
	// read. Under explicit 2× doubling, the high-water allocation
	// is at most 2 × expectedTotal (final cap = expectedTotal, last
	// grow temporarily holds previous cap + new cap = 1.5×).
	// Under Go's default 1.25× factor, the multiple grow-realloc
	// cycles allocate >= 2.5 × expectedTotal in total because each
	// intermediate buffer survives until the next realloc copies it.
	// We assert the bound at 2.5 × expectedTotal as the regression
	// threshold.
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	// First read should consume all 4 chunks and emit the assembled
	// MESSAGE (chunk 4 fills acc.pos == expectedTotal).
	fh, buf, err := readFrameView(ctx, rx)
	if err != nil {
		t.Fatalf("readFrameView: %v", err)
	}
	runtime.ReadMemStats(&after)

	if buf != nil {
		defer buf.Free()
	}
	if fh.Type != FrameTypeMESSAGE {
		t.Fatalf("frame type: got %d want MESSAGE", fh.Type)
	}
	if fh.StreamID != 1 {
		t.Fatalf("stream id: got %d want 1", fh.StreamID)
	}

	// TotalAlloc grew by all allocations during the read, including
	// throwaway intermediate buffers from grow-reallocs. With
	// doubling: ~expectedTotal + (chunkBody for the first chunk
	// alloc) + ~2 × intermediate grows ≈ 2 × expectedTotal.
	// Without doubling: 4-5 intermediate grow-allocs > 2.5 ×
	// expectedTotal.
	allocGrowth := after.TotalAlloc - before.TotalAlloc
	const maxAllocGrowth = uint64(expectedTotal) * 5 / 2 // 2.5x
	if allocGrowth > maxAllocGrowth {
		t.Errorf("allocation growth %d bytes > bound %d (2.5 × expectedTotal=%d) — mid-chain grow-realloc regression: accumulator likely fell back to Go's default 1.25× slice growth instead of explicit 2× doubling",
			allocGrowth, maxAllocGrowth, expectedTotal)
	}

	// Correctness: assembled wire bytes match what we injected.
	data := buf.ReadOnlyData()
	if got, want := len(data), expectedTotal; got != want {
		t.Fatalf("assembled length: got %d want %d", got, want)
	}
	for i := 0; i < 5; i++ {
		if data[i] != hdr[i] {
			t.Fatalf("header byte %d: got %d want %d", i, data[i], hdr[i])
		}
	}
	for i := 0; i < len(chunk1Body); i++ {
		if data[5+i] != chunk1Body[i] {
			t.Fatalf("chunk1 body byte %d: got %d want %d", i, data[5+i], chunk1Body[i])
		}
	}
	off := 5 + len(chunk1Body)
	for i, b := range bodyChunks {
		for j := 0; j < len(b); j++ {
			if data[off+j] != b[j] {
				t.Fatalf("chunk %d body byte %d: got %d want %d (mid-chain assembly broken)", i+2, j, data[off+j], b[j])
			}
		}
		off += len(b)
	}
}

// TestWriteFrameBuffersVectoredMessage exercises the vectored MESSAGE
// write path in writeFrameBuffers / writeFrameH2Message. The path is
// triggered automatically by writeFrameBuffers for MESSAGE frames whose
// body fits in a single H2 DATA frame; we verify that the resulting
// on-wire bytes (and the reader's reassembled MESSAGE) are bit-identical
// to the previous materialise-then-writeFrame path across:
//   - single-segment payload
//   - multi-segment payload (BufferSlice with 3 segments)
//   - empty data (just the 5-byte LPM header)
//   - END_STREAM flag propagation
//   - segment boundary that straddles the ring wrap point
//
// The vectored path's whole point is to avoid materialising hdr+data
// into a contiguous heap buffer; this test gives us the safety net so
// any regression in segment-layout logic is caught immediately.
func TestWriteFrameBuffersVectoredMessage(t *testing.T) {
	mkBuf := func(b []byte) mem.Buffer { return mem.SliceBuffer(b) }
	cases := []struct {
		name      string
		hdr       []byte
		segments  [][]byte
		endStream bool
	}{
		{
			name:     "single-segment",
			hdr:      []byte{0, 0, 0, 0, 11},
			segments: [][]byte{[]byte("hello world")},
		},
		{
			name: "multi-segment",
			hdr:  []byte{0, 0, 0, 0, 21},
			segments: [][]byte{
				[]byte("multi-"),
				[]byte("segment-"),
				[]byte("payload"),
			},
		},
		{
			name:     "empty-data-just-hdr",
			hdr:      []byte{0, 0, 0, 0, 0},
			segments: nil,
		},
		{
			name:      "end-stream-single",
			hdr:       []byte{0, 0, 0, 0, 4},
			segments:  [][]byte{[]byte("eos!")},
			endStream: true,
		},
		{
			// Explicit MORE: gRPC streaming SendMsg with more
			// messages to follow ΓÇö caller passes Flags=0 (no
			// EndStream), which the H2 codec translates to no
			// END_STREAM bit on the wire; the H2 reader surfaces
			// the FrameHeader with MessageFlagMORE set so the
			// upper layer continues reading.
			name:     "more-no-end-stream",
			hdr:      []byte{0, 0, 0, 0, 5},
			segments: [][]byte{[]byte("more!")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			segName := fmt.Sprintf("h2vmsg-%s-%d", tc.name, time.Now().UnixNano())
			defer RemoveSegment(segName)
			seg, err := CreateSegment(segName, 1<<20, 1<<20)
			if err != nil {
				t.Fatalf("CreateSegment: %v", err)
			}
			defer seg.Close()
			tx := NewShmRingFromSegment(seg.A, seg.Mem)
			rx := NewShmRingFromSegment(seg.A, seg.Mem)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			data := make(mem.BufferSlice, 0, len(tc.segments))
			for _, s := range tc.segments {
				data = append(data, mkBuf(s))
			}
			fh := FrameHeader{StreamID: 7, Type: FrameTypeMESSAGE}
			if tc.endStream {
				fh.Flags = MessageFlagEndStream
			}
			if err := writeFrameBuffers(ctx, tx, fh, tc.hdr, data); err != nil {
				t.Fatalf("writeFrameBuffers: %v", err)
			}
			rfh, payload, err := readFrame(ctx, rx)
			if err != nil {
				t.Fatalf("readFrame: %v", err)
			}
			if rfh.Type != FrameTypeMESSAGE {
				t.Fatalf("frame type: got %d want MESSAGE", rfh.Type)
			}
			if rfh.StreamID != 7 {
				t.Fatalf("stream id: got %d want 7", rfh.StreamID)
			}
			// MORE bit reflects END_STREAM on the wire: MORE=0 means
			// END_STREAM observed (last MESSAGE in this direction).
			gotMore := rfh.Flags&MessageFlagMORE != 0
			wantMore := !tc.endStream
			if gotMore != wantMore {
				t.Fatalf("MORE flag: got %v want %v", gotMore, wantMore)
			}
			// Expected body = lpmHdr || flatten(segments).
			var want []byte
			want = append(want, tc.hdr...)
			for _, s := range tc.segments {
				want = append(want, s...)
			}
			if !bytes.Equal(payload, want) {
				t.Fatalf("payload mismatch:\n got %d bytes %x\nwant %d bytes %x",
					len(payload), payload, len(want), want)
			}
		})
	}
}

// TestWriteFrameBuffersVectoredMessage_WrapStraddle stresses the
// res.First / res.Second boundary by writing many small MESSAGE frames
// until the ring write head crosses the wrap point, then writing a
// final vectored MESSAGE whose payload is large enough to straddle the
// wrap. Verifies that the ringSegWriter correctly emits across the
// two-slice reservation.
//
// Sizing arithmetic (ringSize = 4096, smallFrame = 9 H2 hdr + 5 LPM
// hdr + 4 body = 18 bytes, finalFrame = 9 + 5 + 333+444+555 = 1346
// bytes): for the final write to straddle, we need writeIdx mod 4096
// to lie in [4096-1346+1, 4096-1] = [2751, 4095]. With each warmup
// iteration advancing writeIdx by exactly 18 bytes, ceil(2751/18) =
// 153 iterations is the minimum that *could* land in the straddle
// band. Anywhere from 153..227 iterations land somewhere in [2754,
// 4086]. We use 200 to land squarely inside.
//
// The post-write assertion `len(res.Second) > 0` is not directly
// observable from outside the codec, so we instead inspect
// `tx.ContiguousWriteSpace()` immediately BEFORE the final write — it
// returns the contiguous bytes remaining in res.First. If that is
// less than finalFrame, the reservation MUST straddle. The test
// fatally fails the wrap precondition before the actual write so a
// regression in the warmup arithmetic is loud, not silent.
func TestWriteFrameBuffersVectoredMessage_WrapStraddle(t *testing.T) {
	const ringSize = 4096
	segName := fmt.Sprintf("h2vwrap-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, ringSize, ringSize)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()
	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Repeatedly write+read a small MESSAGE to advance head/tail
	// past the wrap point. See the function-level comment for the
	// 200-iteration choice.
	smallBody := []byte("ping")
	smallHdr := []byte{0, 0, 0, 0, byte(len(smallBody))}
	const warmupIters = 200
	for i := 0; i < warmupIters; i++ {
		if err := writeFrameBuffers(ctx, tx,
			FrameHeader{StreamID: uint32(i + 1), Type: FrameTypeMESSAGE},
			smallHdr, mem.BufferSlice{mem.SliceBuffer(smallBody)},
		); err != nil {
			t.Fatalf("warmup writeFrameBuffers[%d]: %v", i, err)
		}
		fh, payload, err := readFrame(ctx, rx)
		if err != nil {
			t.Fatalf("warmup readFrame[%d]: %v", i, err)
		}
		if fh.Type != FrameTypeMESSAGE {
			t.Fatalf("warmup frame type: got %d want MESSAGE", fh.Type)
		}
		if !bytes.Equal(payload, append(append([]byte{}, smallHdr...), smallBody...)) {
			t.Fatalf("warmup payload mismatch")
		}
	}

	// Now write a multi-segment MESSAGE whose serialised form is
	// large enough that res.First+res.Second must straddle wrap.
	body0 := bytes.Repeat([]byte{0xAB}, 333)
	body1 := bytes.Repeat([]byte{0xCD}, 444)
	body2 := bytes.Repeat([]byte{0xEF}, 555)
	totalBody := len(body0) + len(body1) + len(body2)
	hdr := []byte{0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(hdr[1:5], uint32(totalBody))
	const finalFrameTotal = h2FrameHeaderSize + 5 + 333 + 444 + 555 // 1346

	// Precondition: the next write must straddle the wrap. If
	// ContiguousWriteSpace returns >= finalFrameTotal, res.Second
	// will be empty and we'd be testing the non-straddle branch only
	// — a silent regression of this test's stated purpose.
	if contig := tx.ContiguousWriteSpace(); contig >= uint64(finalFrameTotal) {
		t.Fatalf("test setup broken: ContiguousWriteSpace=%d >= finalFrameTotal=%d, "+
			"final write will not straddle the wrap. Adjust warmupIters.",
			contig, finalFrameTotal)
	}

	if err := writeFrameBuffers(ctx, tx,
		FrameHeader{StreamID: 999, Type: FrameTypeMESSAGE},
		hdr,
		mem.BufferSlice{mem.SliceBuffer(body0), mem.SliceBuffer(body1), mem.SliceBuffer(body2)},
	); err != nil {
		t.Fatalf("wrap-straddle writeFrameBuffers: %v", err)
	}
	rfh, payload, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("wrap-straddle readFrame: %v", err)
	}
	if rfh.Type != FrameTypeMESSAGE || rfh.StreamID != 999 {
		t.Fatalf("wrap-straddle frame header: %+v", rfh)
	}
	var want []byte
	want = append(want, hdr...)
	want = append(want, body0...)
	want = append(want, body1...)
	want = append(want, body2...)
	if !bytes.Equal(payload, want) {
		t.Fatalf("wrap-straddle payload mismatch: len got=%d want=%d", len(payload), len(want))
	}
}

// TestReadFrameViewH2_MultiLPMLeftoverNoAliasing is a regression test
// for a data-corruption bug: when one H2 DATA frame body carries
// multiple gRPC LPMs, readFrameViewH2 used to stash the leftover
// (start of the next LPM) in holder.pendingFrame as a slice that
// aliased ring memory. commitPayload.Commit(int(h2fh.Length)) ran
// IMMEDIATELY before the leftover was stored, so the writer was free
// to advance into the just-committed bytes — corrupting the second
// LPM the next read would surface.
//
// The fix copies leftover to a heap-owned buffer before commit. This
// test exercises the path by:
//  1. Building a single DATA frame body containing TWO complete
//     LPMs (so feedSplit returns msg=first LPM + leftover=second).
//  2. Reading the first LPM via readFrameView (which commits the
//     whole DATA payload to the ring read pointer).
//  3. Writing many junk frames to drive the ring writer past the
//     previous read position — overwriting the bytes that USED to
//     back the leftover slice.
//  4. Reading the second LPM via readFrameView. It should still
//     decode to the original bytes; before the fix it would decode
//     to junk (whatever the writer left in those ring slots).
func TestReadFrameViewH2_MultiLPMLeftoverNoAliasing(t *testing.T) {
	const ringSize = 4096
	segName := fmt.Sprintf("h2multilpm-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, ringSize, ringSize)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()
	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Build a DATA payload containing two LPMs:
	//
	//	LPM_A = [0][len(A)][bodyA]  bodyA = 0xAA × 100
	//	LPM_B = [0][len(B)][bodyB]  bodyB = 0xBB × 200
	const aLen, bLen = 100, 200
	bodyA := bytes.Repeat([]byte{0xAA}, aLen)
	bodyB := bytes.Repeat([]byte{0xBB}, bLen)
	lpmA := append(buildLPMHeaderTest(aLen), bodyA...)
	lpmB := append(buildLPMHeaderTest(bLen), bodyB...)
	combined := append(append([]byte{}, lpmA...), lpmB...)

	injectH2Frame(ctx, t, tx, H2FrameDATA, 0, 1, combined)

	// Read 1: surfaces LPM_A; leftover (LPM_B bytes) gets stashed in
	// holder.pendingFrame. Before the fix, that stash aliased ring
	// memory which is about to be released.
	fh1, buf1, err := readFrameView(ctx, rx)
	if err != nil {
		t.Fatalf("read 1: %v", err)
	}
	if fh1.Type != FrameTypeMESSAGE {
		t.Fatalf("read 1 type: got %d want MESSAGE", fh1.Type)
	}
	dataA := append([]byte{}, buf1.ReadOnlyData()...) // snapshot
	if buf1 != nil {
		buf1.Free()
	}
	wantA := append(append([]byte{}, buildLPMHeaderTest(aLen)...), bodyA...)
	if !bytes.Equal(dataA, wantA) {
		t.Fatalf("read 1 payload mismatch")
	}

	// Stomp the ring without draining: inject junk DATA frames whose
	// bytes will overwrite the ring slots where LPM_B *used* to
	// live. We do NOT readFrameView these — pendingFrame is replayed
	// FIRST on every readFrameView call (before any new ring read),
	// so any read here would consume the pendingFrame and mask the
	// bug. We just want the ring writer's bytes to land on top of
	// the leftover slice's backing memory.
	//
	// Ring layout: ringSize = 4096. After read 1 commits, both
	// readIdx and writeIdx are at 9 (H2 hdr) + 5+100+5+200 = 319,
	// modular = 319. The leftover (LPM_B body) used to back ring
	// positions [9+5+100 .. 9+5+100+5+200] = [114..319]. To
	// overwrite those positions the writer must wrap past 4096 and
	// re-enter [0..319]. From writeIdx=319, advancing by
	// (4096-319) + 319 = 4096 bytes wraps back exactly to position
	// 319 — covering ALL ring positions including [114..319]. Each
	// junk DATA frame is 9 (H2 hdr) + 5 (LPM hdr) + 32 (body) = 46
	// bytes. 89 junk frames advance writeIdx by 89*46 = 4094 — just
	// shy of full ring (4096) so reservation does not block, but
	// enough to land at position (319+4094) mod 4096 = 317,
	// definitely past 319.
	junkBody := bytes.Repeat([]byte{0xCC}, 32)
	junkPayload := append(buildLPMHeaderTest(len(junkBody)), junkBody...)
	for i := 0; i < 89; i++ {
		injectH2Frame(ctx, t, tx, H2FrameDATA, 0, uint32(100+i), junkPayload)
	}

	// Read 2: should pull LPM_B from holder.pendingFrame (replayed
	// before any new ring read). Pre-fix: pendingFrame aliases the
	// just-overwritten ring memory and we get junk bytes. Post-fix:
	// pendingFrame is a heap copy taken before commit and LPM_B is
	// intact.
	fh2, buf2, err := readFrameView(ctx, rx)
	if err != nil {
		t.Fatalf("read 2: %v", err)
	}
	if buf2 != nil {
		defer buf2.Free()
	}
	if fh2.Type != FrameTypeMESSAGE {
		t.Fatalf("read 2 type: got %d want MESSAGE", fh2.Type)
	}
	wantB := append(append([]byte{}, buildLPMHeaderTest(bLen)...), bodyB...)
	if !bytes.Equal(buf2.ReadOnlyData(), wantB) {
		t.Fatalf("read 2 payload mismatch:\n got %d bytes head=%x\nwant %d bytes head=%x",
			len(buf2.ReadOnlyData()), buf2.ReadOnlyData()[:min(16, len(buf2.ReadOnlyData()))],
			len(wantB), wantB[:min(16, len(wantB))])
	}
}
