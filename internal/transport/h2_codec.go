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
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/proto"
)

// h2Codec encodes and decodes the SHM transport's in-memory frame model
// (FrameHeader + payload bytes) on top of the HTTP/2 wire format.
//
// The mapping between Custom16 frame types and HTTP/2 frame types is:
//
//	Custom16 HEADERS  (initial)  → H2 HEADERS  + END_HEADERS
//	Custom16 HEADERS  (server)   → H2 HEADERS  + END_HEADERS
//	Custom16 MESSAGE  (more=0)   → H2 DATA
//	Custom16 MESSAGE  (more=1)   → H2 DATA (intermediate chunk; END_STREAM=0)
//	Custom16 TRAILERS            → H2 HEADERS  + END_HEADERS + END_STREAM
//	Custom16 CANCEL              → H2 RST_STREAM (error_code=CANCEL)
//	Custom16 GOAWAY              → H2 GOAWAY
//	Custom16 PING/PONG           → H2 PING (PONG = PING + ACK)
//	Custom16 HALFCLOSE           → H2 DATA  (empty, END_STREAM=1)
//	Custom16 WINDOW_UPDATE       → H2 WINDOW_UPDATE
//
// The HEADERS/TRAILERS payloads, which are Custom16 KV blobs in the legacy
// codec, are HPACK-encoded as H2 :pseudo-header fields plus regular gRPC
// metadata. The decoder converts back to the same in-memory HeadersV1 /
// TrailersV1 structs the rest of the transport already uses, so callers
// remain wire-format-agnostic.

// h2HpackPool reuses HPACK encoders to amortize the dynamic-table cost.
// HPACK encoders are stateful (per-connection dynamic table), so each
// ring uses its own encoder/decoder. This pool is used only as a way to
// avoid per-frame allocations of the bytes.Buffer that backs the encoder.
var h2HpackPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// hpackEncoderHolder bundles a per-ring HPACK encoder with its scratch
// buffer. The encoder is single-threaded (writer goroutine).
type hpackEncoderHolder struct {
	enc     *hpack.Encoder
	scratch *bytes.Buffer
}

func newHpackEncoderHolder() *hpackEncoderHolder {
	scratch := new(bytes.Buffer)
	enc := hpack.NewEncoder(scratch)
	return &hpackEncoderHolder{enc: enc, scratch: scratch}
}

// hpackDecoderHolder wraps a per-ring HPACK decoder. Single-threaded
// (reader goroutine).
type hpackDecoderHolder struct {
	dec *hpack.Decoder
}

func newHpackDecoderHolder() *hpackDecoderHolder {
	return &hpackDecoderHolder{dec: hpack.NewDecoder(4096, nil)}
}

// h2Encoder lazily initializes and returns the HPACK encoder for this
// ring. Must only be called when r.wire == WireFormatHTTP2.
func (r *ShmRing) h2Encoder() *hpackEncoderHolder {
	if r.h2Enc == nil {
		r.h2Enc = newHpackEncoderHolder()
	}
	return r.h2Enc
}

// h2Decoder lazily initializes and returns the HPACK decoder for this
// ring. Must only be called when r.wire == WireFormatHTTP2.
func (r *ShmRing) h2Decoder() *hpackDecoderHolder {
	if r.h2Dec == nil {
		r.h2Dec = newHpackDecoderHolder()
	}
	return r.h2Dec
}

// h2EncodeHeaders converts an in-memory HeadersV1 into HPACK-encoded bytes
// suitable for an H2 HEADERS frame payload. The encoder argument carries
// the per-ring dynamic table state.
func h2EncodeHeaders(enc *hpack.Encoder, scratch *bytes.Buffer, h HeadersV1) []byte {
	scratch.Reset()
	if h.HdrType == 0 {
		// Client-initial.
		_ = enc.WriteField(hpack.HeaderField{Name: ":method", Value: "POST"})
		_ = enc.WriteField(hpack.HeaderField{Name: ":scheme", Value: "http"})
		_ = enc.WriteField(hpack.HeaderField{Name: ":path", Value: h.Method})
		if h.Authority != "" {
			_ = enc.WriteField(hpack.HeaderField{Name: ":authority", Value: h.Authority})
		}
		_ = enc.WriteField(hpack.HeaderField{Name: "te", Value: "trailers"})
		_ = enc.WriteField(hpack.HeaderField{Name: "content-type", Value: "application/grpc"})
		if h.DeadlineUnixNano != 0 {
			ns := int64(h.DeadlineUnixNano) - time.Now().UnixNano()
			if ns < 0 {
				ns = 0
			}
			// gRPC encodes timeout as `<value><unit>`. Use nanoseconds for precision.
			_ = enc.WriteField(hpack.HeaderField{Name: "grpc-timeout", Value: strconv.FormatInt(ns, 10) + "n"})
		}
	} else {
		// Server-initial.
		_ = enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
		_ = enc.WriteField(hpack.HeaderField{Name: "content-type", Value: "application/grpc"})
	}
	for _, kv := range h.Metadata {
		for _, v := range kv.Values {
			_ = enc.WriteField(hpack.HeaderField{Name: kv.Key, Value: string(v)})
		}
	}
	return scratch.Bytes()
}

// h2EncodeTrailers HPACK-encodes a TrailersV1 into an H2 HEADERS frame
// payload (typically with END_STREAM | END_HEADERS flags).
func h2EncodeTrailers(enc *hpack.Encoder, scratch *bytes.Buffer, t TrailersV1) []byte {
	scratch.Reset()
	_ = enc.WriteField(hpack.HeaderField{Name: "grpc-status", Value: strconv.FormatUint(uint64(t.GRPCStatusCode), 10)})
	if t.GRPCStatusMsg != "" {
		_ = enc.WriteField(hpack.HeaderField{Name: "grpc-message", Value: t.GRPCStatusMsg})
	}
	for _, kv := range t.Metadata {
		for _, v := range kv.Values {
			_ = enc.WriteField(hpack.HeaderField{Name: kv.Key, Value: string(v)})
		}
	}
	return scratch.Bytes()
}

// h2DecodeHeaders parses an HPACK-encoded HEADERS payload into a HeadersV1.
// hdrType=0 means client-initial, 1=server-initial. trailers=true tells the
// decoder to populate a TrailersV1-like struct (caller dispatches on
// presence of grpc-status). Returns isTrailers=true when grpc-status was
// observed (HEADERS frame may be initial or trailers; the only distinction
// is the presence of grpc-status).
func h2DecodeHeaders(dec *hpack.Decoder, b []byte) (h HeadersV1, t TrailersV1, isTrailers bool, err error) {
	dec.SetEmitEnabled(true)
	dec.SetEmitFunc(func(hf hpack.HeaderField) {
		switch hf.Name {
		case ":method":
			// POST always for gRPC; ignore.
		case ":scheme":
			// http; ignore.
		case ":path":
			h.Method = hf.Value
			h.HdrType = 0
		case ":authority":
			h.Authority = hf.Value
		case ":status":
			h.HdrType = 1
		case "te", "content-type":
			// Standard gRPC headers; ignore for in-memory model.
		case "grpc-timeout":
			if d, perr := parseGrpcTimeout(hf.Value); perr == nil {
				h.DeadlineUnixNano = uint64(time.Now().Add(d).UnixNano())
			}
		case "grpc-status":
			isTrailers = true
			if v, cerr := strconv.ParseUint(hf.Value, 10, 32); cerr == nil {
				t.GRPCStatusCode = uint32(v)
			}
		case "grpc-message":
			isTrailers = true
			t.GRPCStatusMsg = hf.Value
		default:
			// User metadata.
			val := append([]byte(nil), hf.Value...)
			if isTrailers {
				appendKV(&t.Metadata, hf.Name, val)
			} else {
				appendKV(&h.Metadata, hf.Name, val)
			}
		}
	})
	if _, err = dec.Write(b); err != nil {
		return HeadersV1{}, TrailersV1{}, false, err
	}
	if err = dec.Close(); err != nil {
		return HeadersV1{}, TrailersV1{}, false, err
	}
	h.Version = 1
	t.Version = 1
	return h, t, isTrailers, nil
}

func appendKV(metadata *[]KV, key string, val []byte) {
	for i := range *metadata {
		if (*metadata)[i].Key == key {
			(*metadata)[i].Values = append((*metadata)[i].Values, val)
			return
		}
	}
	*metadata = append(*metadata, KV{Key: key, Values: [][]byte{val}})
}

// parseGrpcTimeout parses gRPC's `<value><unit>` timeout encoding.
func parseGrpcTimeout(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, errors.New("grpc-timeout too short")
	}
	unit := s[len(s)-1]
	num, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil {
		return 0, err
	}
	switch unit {
	case 'n':
		return time.Duration(num) * time.Nanosecond, nil
	case 'u':
		return time.Duration(num) * time.Microsecond, nil
	case 'm':
		return time.Duration(num) * time.Millisecond, nil
	case 'S':
		return time.Duration(num) * time.Second, nil
	case 'M':
		return time.Duration(num) * time.Minute, nil
	case 'H':
		return time.Duration(num) * time.Hour, nil
	default:
		return 0, fmt.Errorf("unknown grpc-timeout unit %q", unit)
	}
}

// translateCustomToH2 maps an in-memory FrameHeader (Custom16 model) to the
// equivalent H2 frame type and flags. The caller is responsible for any
// payload transformation (HPACK for HEADERS/TRAILERS, etc.).
func translateCustomToH2(fh FrameHeader) (H2FrameType, byte) {
	switch fh.Type {
	case FrameTypeMESSAGE:
		// MORE flag means "more chunks follow" — not END_STREAM.
		// Otherwise still not END_STREAM (server uses TRAILERS to end).
		return H2FrameDATA, 0
	case FrameTypeHEADERS:
		return H2FrameHEADERS, H2FlagEndHeaders
	case FrameTypeTRAILERS:
		return H2FrameHEADERS, H2FlagEndHeaders | H2FlagEndStream
	case FrameTypeCANCEL:
		return H2FrameRSTSTREAM, 0
	case FrameTypeGOAWAY:
		return H2FrameGOAWAY, 0
	case FrameTypePING:
		return H2FramePING, 0
	case FrameTypePONG:
		return H2FramePING, H2FlagAck
	case FrameTypeHALFCLOSE:
		return H2FrameDATA, H2FlagEndStream
	case FrameTypeWindowUpdate:
		return H2FrameWINDOWUPDATE, 0
	case FrameTypePAD:
		// No direct H2 analogue. Encode as a SETTINGS ack-style no-op:
		// emit nothing on the wire (caller should skip PAD frames in H2).
		return H2FrameType(0xFF), 0
	}
	return H2FrameType(0xFF), 0
}

// translateH2ToCustom maps an H2 frame to a Custom16 frame type and flags
// for delivery into the existing dispatch machinery. The caller has
// already decoded the header.
//
// HEADERS frames require examining the payload (after HPACK decode) to
// distinguish initial-headers from trailers (presence of grpc-status).
// translateH2ToCustom assumes initial-headers; the caller fixes up TRAILERS.
//
// Note on MESSAGE/MORE: H2's only stream-end signal is END_STREAM (via
// DATA with empty body or HEADERS with grpc-status). Within a stream,
// each DATA frame is a complete LPM message (no chunking — the LPM
// accumulator in the reader handles fragmented LPMs across DATA frames).
// We therefore never set MessageFlagMORE on the synthesized Custom16
// MESSAGE frame. Multi-chunk Custom16 → H2 emission would require a
// separate accumulator on the reader side; for now writeProtoToRingH2
// always emits a single DATA frame per message.
func translateH2ToCustom(t H2FrameType, flags byte) (FrameType, uint8, bool) {
	switch t {
	case H2FrameDATA:
		// MORE flag is not representable in plain H2 DATA. Caller
		// distinguishes empty DATA + END_STREAM as HALFCLOSE.
		return FrameTypeMESSAGE, 0, true
	case H2FrameHEADERS:
		if flags&H2FlagEndStream != 0 {
			return FrameTypeTRAILERS, TrailersFlagEndStream, true
		}
		return FrameTypeHEADERS, HeadersFlagINITIAL, true
	case H2FrameRSTSTREAM:
		return FrameTypeCANCEL, 0, true
	case H2FrameGOAWAY:
		return FrameTypeGOAWAY, 0, true
	case H2FramePING:
		if flags&H2FlagAck != 0 {
			return FrameTypePONG, 0, true
		}
		return FrameTypePING, 0, true
	case H2FrameWINDOWUPDATE:
		return FrameTypeWindowUpdate, 0, true
	default:
		// PRIORITY, SETTINGS, PUSH_PROMISE, CONTINUATION are skipped at
		// this layer (CONTINUATION is reassembled by the H2 reader).
		return 0, 0, false
	}
}

// rstStreamPayload encodes a CANCEL/RST_STREAM payload (4-byte error code).
func rstStreamPayload(code H2ErrorCode) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(code))
	return b[:]
}

// goawayPayloadH2 encodes a GOAWAY payload. layout: lastStreamID(4) +
// errorCode(4) + opaque debug data. We pack the Custom16 payload (a UTF-8
// debug message) into the debug-data section and use NoError as the code.
func goawayPayloadH2(custom []byte) []byte {
	out := make([]byte, 8+len(custom))
	binary.BigEndian.PutUint32(out[0:4], 0) // lastStreamID
	binary.BigEndian.PutUint32(out[4:8], uint32(H2ErrNoError))
	copy(out[8:], custom)
	return out
}

// windowUpdatePayload encodes a WINDOW_UPDATE payload (4-byte increment).
func windowUpdatePayload(increment uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], increment&0x7FFFFFFF)
	return b[:]
}

// statusCodeToH2Err maps a gRPC status code to an H2 error code for
// RST_STREAM. Most cancellations use Cancel; flow-control violations use
// FlowControlError.
func statusCodeToH2Err(c codes.Code) H2ErrorCode {
	switch c {
	case codes.Canceled:
		return H2ErrCancel
	case codes.ResourceExhausted:
		return H2ErrFlowControlError
	case codes.Unavailable:
		return H2ErrRefusedStream
	default:
		return H2ErrInternalError
	}
}

// pingPayloadH2 encodes a PING payload (8 bytes opaque).
// gRPC SHM uses the first 8 bytes of the legacy Custom16 payload.
func pingPayloadH2(opaque []byte) []byte {
	var b [8]byte
	copy(b[:], opaque)
	return b[:]
}

// extractPingOpaque pulls the 8-byte opaque PING payload (RFC 7540 §6.7).
func extractPingOpaque(payload []byte) ([]byte, error) {
	if len(payload) != 8 {
		return nil, fmt.Errorf("h2 PING payload must be 8 bytes, got %d", len(payload))
	}
	return payload, nil
}

// readFrameH2 reads one logical SHM frame from a ring whose wire format is
// HTTP/2. Multi-frame H2 payloads (CONTINUATION, fragmented HEADERS) and
// chunked DATA are coalesced into a single FrameHeader+payload return.
//
// For DATA frames carrying gRPC LPM-prefixed messages, multi-frame chunks
// are handled by the caller via the MORE flag (mirroring Custom16).
//
// The returned mem.Buffer is heap-allocated (copy path); ZC for H2 DATA
// is handled by readFrameViewH2.
func readFrameH2(ctx context.Context, rx *ShmRing, dec *hpack.Decoder) (FrameHeader, []byte, error) {
	for {
		// Read 9-byte H2 frame header.
		first, second, commitHdr, err := rx.ReadSlices(ctx, h2FrameHeaderSize)
		if err != nil {
			return FrameHeader{}, nil, err
		}
		var hb [h2FrameHeaderSize]byte
		n := copy(hb[:], first)
		if n < h2FrameHeaderSize && len(second) > 0 {
			n += copy(hb[n:], second)
		}
		if n != h2FrameHeaderSize {
			commitHdr.Commit(h2FrameHeaderSize)
			return FrameHeader{}, nil, errors.New("h2: short frame header")
		}
		h2fh, err := decodeH2FrameHeader(hb[:])
		if err != nil {
			commitHdr.Commit(h2FrameHeaderSize)
			return FrameHeader{}, nil, err
		}
		commitHdr.Commit(h2FrameHeaderSize)

		// Read payload.
		var payload []byte
		if h2fh.Length > 0 {
			pFirst, pSecond, commitPayload, perr := rx.ReadSlices(ctx, int(h2fh.Length))
			if perr != nil {
				return FrameHeader{}, nil, perr
			}
			payload = make([]byte, h2fh.Length)
			n := copy(payload, pFirst)
			if n < int(h2fh.Length) && len(pSecond) > 0 {
				copy(payload[n:], pSecond)
			}
			commitPayload.Commit(int(h2fh.Length))
		}

		// Translate H2 → Custom16 frame model.
		switch h2fh.Type {
		case H2FrameSETTINGS, H2FramePRIORITY, H2FramePUSHPROMISE:
			// Skipped at this layer.
			continue
		case H2FrameDATA:
			ft, fl, ok := translateH2ToCustom(h2fh.Type, h2fh.Flags)
			if !ok {
				continue
			}
			if len(payload) == 0 && h2fh.Flags&H2FlagEndStream != 0 {
				return FrameHeader{
					Type: FrameTypeHALFCLOSE, StreamID: h2fh.StreamID, Length: 0,
				}, nil, nil
			}
			return FrameHeader{
				Type:     ft,
				StreamID: h2fh.StreamID,
				Length:   uint32(len(payload)),
				Flags:    fl,
			}, payload, nil
		case H2FrameHEADERS:
			// HPACK-decode and convert to Custom16 HeadersV1 / TrailersV1
			// payload format the rest of the transport expects.
			h, t, isTrailers, derr := h2DecodeHeaders(dec, payload)
			if derr != nil {
				return FrameHeader{}, nil, derr
			}
			if isTrailers {
				out := encodeTrailers(t)
				return FrameHeader{
					Type:     FrameTypeTRAILERS,
					StreamID: h2fh.StreamID,
					Length:   uint32(len(out)),
					Flags:    TrailersFlagEndStream,
				}, out, nil
			}
			out := encodeHeaders(h)
			return FrameHeader{
				Type:     FrameTypeHEADERS,
				StreamID: h2fh.StreamID,
				Length:   uint32(len(out)),
				Flags:    HeadersFlagINITIAL,
			}, out, nil
		case H2FrameRSTSTREAM, H2FrameGOAWAY, H2FramePING, H2FrameWINDOWUPDATE:
			ft, fl, ok := translateH2ToCustom(h2fh.Type, h2fh.Flags)
			if !ok {
				continue
			}
			return FrameHeader{
				Type:     ft,
				StreamID: h2fh.StreamID,
				Length:   uint32(len(payload)),
				Flags:    fl,
			}, payload, nil
		default:
			// Unknown frame type. Per RFC 7540 §4.1 unknown types must be
			// ignored after their payload is consumed (already done above).
			continue
		}
	}
}

// readFrameViewH2 is the H2 analogue of readFrameView (zero-copy capable).
// Single-frame DATA payloads return a ring-backed mem.Buffer; HEADERS and
// other small frames always copy.
func readFrameViewH2(ctx context.Context, rx *ShmRing, dec *hpack.Decoder) (FrameHeader, mem.Buffer, error) {
	fh, payload, err := readFrameH2(ctx, rx, dec)
	if err != nil {
		return FrameHeader{}, nil, err
	}
	if payload == nil {
		return fh, nil, nil
	}
	// Speculative ZC for H2 DATA is not yet wired (the caller already
	// gets a heap buffer from readFrameH2). Future work: lift the ZC
	// path from Custom16 readFrameView once the deferred-commit model
	// is implemented for both wire formats.
	buf := mem.Copy(payload, mem.DefaultBufferPool())
	return fh, buf, nil
}

// writeFrameH2 writes one logical SHM frame on a ring whose wire format is
// HTTP/2. Translates the in-memory FrameHeader+payload model into the
// equivalent H2 frame(s).
func writeFrameH2(ctx context.Context, tx *ShmRing, fh FrameHeader, payload []byte, enc *hpack.Encoder, scratch *bytes.Buffer) error {
	h2t, h2f := translateCustomToH2(fh)
	if h2t == 0xFF {
		// PAD: skip silently.
		return nil
	}

	var h2payload []byte
	switch fh.Type {
	case FrameTypeHEADERS:
		hv, derr := decodeHeaders(payload)
		if derr != nil {
			return derr
		}
		h2payload = h2EncodeHeaders(enc, scratch, hv)
	case FrameTypeTRAILERS:
		tv, derr := decodeTrailers(payload)
		if derr != nil {
			return derr
		}
		h2payload = h2EncodeTrailers(enc, scratch, tv)
	case FrameTypeCANCEL:
		h2payload = rstStreamPayload(H2ErrCancel)
	case FrameTypeGOAWAY:
		h2payload = goawayPayloadH2(payload)
	case FrameTypeMESSAGE, FrameTypeWindowUpdate, FrameTypePING, FrameTypePONG:
		h2payload = payload
	case FrameTypeHALFCLOSE:
		h2payload = nil
	default:
		h2payload = payload
	}

	if len(h2payload) > h2MaxFramePayload {
		return fmt.Errorf("h2: payload %d exceeds 16MB max frame size", len(h2payload))
	}

	total := h2FrameHeaderSize + len(h2payload)
	if uint64(total) > tx.Capacity() {
		return fmt.Errorf("h2: frame %d exceeds ring capacity %d", total, tx.Capacity())
	}

	res, err := tx.ReserveWrite(ctx, total)
	if err != nil {
		return err
	}
	var hdr [h2FrameHeaderSize]byte
	encodeH2FrameHeaderTo(&hdr, H2FrameHeader{
		Length:   uint32(len(h2payload)),
		Type:     h2t,
		Flags:    h2f,
		StreamID: fh.StreamID,
	})

	// Layout the 9-byte header at the start of the reservation.
	if len(res.First) >= h2FrameHeaderSize {
		copy(res.First[:h2FrameHeaderSize], hdr[:])
		// Body follows.
		writePos := h2FrameHeaderSize
		bodyInFirst := len(res.First) - writePos
		if bodyInFirst > len(h2payload) {
			bodyInFirst = len(h2payload)
		}
		copy(res.First[writePos:writePos+bodyInFirst], h2payload[:bodyInFirst])
		if len(res.Second) > 0 && bodyInFirst < len(h2payload) {
			copy(res.Second, h2payload[bodyInFirst:])
		}
	} else {
		// Header straddles wrap: split.
		copy(res.First, hdr[:len(res.First)])
		remHdr := h2FrameHeaderSize - len(res.First)
		copy(res.Second[:remHdr], hdr[len(res.First):])
		// Body comes after header in second half.
		bodyDest := res.Second[remHdr:]
		copy(bodyDest, h2payload)
	}
	return res.Commit(total)
}

// writeProtoToRingH2 is the H2 analogue of writeProtoToRing: it marshals
// a proto.Message directly into the ring as the body of an H2 DATA frame,
// preceded by the gRPC LPM 5-byte length-prefix. Returns (true, err) when
// the ZC write was attempted (success or failure); (false, nil) when the
// caller should fall back to the copy path.
//
// Layout in the ring:
//
//	[9-byte H2 DATA header][1-byte LPM compressed=0][4-byte LPM length][proto body]
//
// Total = 9 + 5 + pSize. ZC eligibility mirrors the Custom16 path: the
// frame must fit contiguously in the ring without wrap.
func writeProtoToRingH2(ctx context.Context, tx *ShmRing, streamID uint32, msg proto.Message, pSize int, flags uint8) (bool, error) {
	total := h2FrameHeaderSize + 5 + pSize

	// Skip ZC for messages that won't fit in a single frame.
	// cap/3 mirrors the Custom16 chunking-path budget.
	if uint64(total) > tx.Capacity()/3 {
		return false, nil
	}
	if uint64(total) > h2MaxFramePayload+h2FrameHeaderSize {
		// Single H2 DATA frame can't carry more than 16MB-1 of body.
		return false, nil
	}
	// Non-blocking contiguous-space check.
	if tx.ContiguousWriteSpace() < uint64(total) {
		return false, nil
	}

	res, err := tx.ReserveWrite(ctx, total)
	if err != nil {
		return false, err
	}

	// H2 DATA frame header (9 bytes).
	var h2hdr [h2FrameHeaderSize]byte
	// MORE flag → no END_STREAM. !MORE → still no END_STREAM (server
	// uses TRAILERS to end). Same semantics as writeFrameH2 mapping.
	var h2flags byte
	if flags&MessageFlagMORE == 0 {
		// Final chunk for this message. END_STREAM stays 0 (TRAILERS ends).
		h2flags = 0
	}
	encodeH2FrameHeaderTo(&h2hdr, H2FrameHeader{
		Length:   uint32(5 + pSize),
		Type:     H2FrameDATA,
		Flags:    h2flags,
		StreamID: streamID,
	})
	copy(res.First[0:h2FrameHeaderSize], h2hdr[:])

	// gRPC 5-byte LPM header.
	res.First[h2FrameHeaderSize] = 0 // compressed flag = 0
	binary.BigEndian.PutUint32(res.First[h2FrameHeaderSize+1:h2FrameHeaderSize+5], uint32(pSize))

	// Marshal directly into the ring memory after the LPM header.
	dst := res.First[h2FrameHeaderSize+5 : h2FrameHeaderSize+5]
	out, err := protoMarshalAppend(dst, msg)
	if err != nil {
		return false, err
	}
	if len(out) != pSize {
		return false, fmt.Errorf("writeProtoToRingH2: size mismatch: %d vs %d", pSize, len(out))
	}

	return true, res.Commit(total)
}
