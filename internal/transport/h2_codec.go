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

// hpackDecoderHolder wraps a per-ring HPACK decoder and the per-stream
// LPM accumulators used to reassemble multi-frame DATA messages.
// Single-threaded (reader goroutine).
type hpackDecoderHolder struct {
	dec      *hpack.Decoder
	decState *h2DecodeState // pre-bound to dec.SetEmitFunc — avoids per-call closure alloc
	// hdrScratch is a reusable buffer for materializing HEADERS / RST_STREAM /
	// GOAWAY / PING / WINDOW_UPDATE H2 payloads from the ring before HPACK
	// decode. Avoids one allocation per non-DATA frame on the read path.
	// The buffer grows as needed; small frames stay small.
	hdrScratch []byte
	// lpmAccumulators maps stream ID → in-progress LPM reassembly state.
	// Reader is single-threaded so a plain map (no mutex) is sufficient.
	// Cleaned up when the stream's RST_STREAM / TRAILERS arrives or when
	// the accumulator emits a complete message and finds no more state.
	lpmAccumulators map[uint32]*lpmAccumulator
	// pendingFrame holds a DATA frame's leftover body bytes that contain
	// the start of the NEXT LPM. The next call to readFrameH2 must
	// continue feeding the accumulator with this slice before reading
	// any new H2 frames. nil when no leftover.
	pendingFrame    []byte
	pendingStreamID uint32
}

// scratchBytes returns a slice of length n drawn from the holder's
// reusable hdrScratch buffer. The contents are NOT zero-initialised
// (caller is expected to overwrite). The returned slice is invalidated
// on the next scratchBytes call — the holder is single-reader so this
// is safe.
func (h *hpackDecoderHolder) scratchBytes(n int) []byte {
	if cap(h.hdrScratch) < n {
		h.hdrScratch = make([]byte, n)
	} else {
		h.hdrScratch = h.hdrScratch[:n]
	}
	return h.hdrScratch
}

func newHpackDecoderHolder() *hpackDecoderHolder {
	st := &h2DecodeState{}
	st.reset()
	dec := hpack.NewDecoder(4096, nil)
	dec.SetEmitEnabled(true)
	dec.SetEmitFunc(st.emit)
	return &hpackDecoderHolder{
		dec:             dec,
		decState:        st,
		lpmAccumulators: make(map[uint32]*lpmAccumulator),
	}
}

// getLpmAccumulator returns the accumulator for stream sid, creating it
// on first use. Safe to call only from the single reader goroutine.
func (h *hpackDecoderHolder) getLpmAccumulator(sid uint32) *lpmAccumulator {
	if a, ok := h.lpmAccumulators[sid]; ok {
		return a
	}
	a := &lpmAccumulator{}
	h.lpmAccumulators[sid] = a
	return a
}

// removeLpmAccumulator drops the accumulator for stream sid (called on
// stream close: TRAILERS, RST_STREAM, or HEADERS-with-END_STREAM).
func (h *hpackDecoderHolder) removeLpmAccumulator(sid uint32) {
	delete(h.lpmAccumulators, sid)
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

// h2DecodeState holds per-decode-call mutable state for the HPACK
// decoder's emit callback. Pre-allocated on the hpackDecoderHolder so
// the emit closure can be set once at decoder construction (capturing
// only the holder pointer) — avoiding a per-call closure allocation
// that would otherwise show up as 1 alloc + ~40 ns per HEADERS frame.
type h2DecodeState struct {
	h          HeadersV1
	t          TrailersV1
	isTrailers bool
}

// reset prepares the state for a new HEADERS decode.
func (s *h2DecodeState) reset() {
	s.h = HeadersV1{Version: 1}
	s.t = TrailersV1{Version: 1}
	s.isTrailers = false
}

// emit is the HPACK emit callback. Bound once to the decoder via
// SetEmitFunc on the holder; the holder pointer captures `s`.
func (s *h2DecodeState) emit(hf hpack.HeaderField) {
	switch hf.Name {
	case ":method":
		// POST always for gRPC; ignore.
	case ":scheme":
		// http; ignore.
	case ":path":
		s.h.Method = hf.Value
		s.h.HdrType = 0
	case ":authority":
		s.h.Authority = hf.Value
	case ":status":
		s.h.HdrType = 1
	case "te", "content-type":
		// Standard gRPC headers; ignore for in-memory model.
	case "grpc-timeout":
		if d, perr := parseGrpcTimeout(hf.Value); perr == nil {
			s.h.DeadlineUnixNano = uint64(time.Now().Add(d).UnixNano())
		}
	case "grpc-status":
		s.isTrailers = true
		if v, cerr := strconv.ParseUint(hf.Value, 10, 32); cerr == nil {
			s.t.GRPCStatusCode = uint32(v)
		}
	case "grpc-message":
		s.isTrailers = true
		s.t.GRPCStatusMsg = hf.Value
	default:
		// User metadata.
		val := append([]byte(nil), hf.Value...)
		if s.isTrailers {
			appendKV(&s.t.Metadata, hf.Name, val)
		} else {
			appendKV(&s.h.Metadata, hf.Name, val)
		}
	}
}

// h2DecodeHeaders parses an HPACK-encoded HEADERS payload into a HeadersV1.
// hdrType=0 means client-initial, 1=server-initial. trailers=true tells the
// decoder to populate a TrailersV1-like struct (caller dispatches on
// presence of grpc-status). Returns isTrailers=true when grpc-status was
// observed (HEADERS frame may be initial or trailers; the only distinction
// is the presence of grpc-status).
//
// Uses the holder's pre-bound emit callback to avoid a per-call closure
// allocation.
func h2DecodeHeaders(holder *hpackDecoderHolder, b []byte) (h HeadersV1, t TrailersV1, isTrailers bool, err error) {
	holder.decState.reset()
	if _, err = holder.dec.Write(b); err != nil {
		return HeadersV1{}, TrailersV1{}, false, err
	}
	if err = holder.dec.Close(); err != nil {
		return HeadersV1{}, TrailersV1{}, false, err
	}
	return holder.decState.h, holder.decState.t, holder.decState.isTrailers, nil
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
// readFrameH2 reads one logical SHM frame from a ring whose wire format
// is HTTP/2. Multi-frame H2 payloads (CONTINUATION, fragmented HEADERS)
// and chunked DATA are coalesced into a single FrameHeader+payload return
// via the per-stream LPM accumulator.
//
// For DATA frames: the body is a gRPC LPM byte stream that may span
// multiple H2 DATA frames. Single-frame fast path: when the accumulator
// is empty AND the body fully contains exactly one LPM, we return the
// body directly without allocating an accumulator buffer. Multi-frame
// path: feed body to the per-stream accumulator until a complete LPM
// is assembled.
//
// The returned mem.Buffer is heap-allocated (copy path); ZC for H2 DATA
// is handled by readFrameViewH2.
func readFrameH2(ctx context.Context, rx *ShmRing, holder *hpackDecoderHolder) (FrameHeader, []byte, error) {
	for {
		// First check if there's leftover data from a previous DATA
		// frame that contained the start of the next LPM.
		if len(holder.pendingFrame) > 0 {
			sid := holder.pendingStreamID
			data := holder.pendingFrame
			holder.pendingFrame = nil
			acc := holder.getLpmAccumulator(sid)
			msg, leftover, ferr := acc.feed(data, 0)
			if ferr != nil {
				return FrameHeader{}, nil, ferr
			}
			if len(leftover) > 0 {
				holder.pendingFrame = leftover
				holder.pendingStreamID = sid
			}
			if msg != nil {
				return FrameHeader{
					Type:     FrameTypeMESSAGE,
					StreamID: sid,
					Length:   uint32(len(msg)),
					Flags:    0,
				}, msg, nil
			}
			// Accumulator still in-progress (only header consumed); fall
			// through to read the next H2 DATA frame.
		}

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
			// Empty DATA + END_STREAM = HALFCLOSE.
			if len(payload) == 0 {
				if h2fh.Flags&H2FlagEndStream != 0 {
					return FrameHeader{
						Type: FrameTypeHALFCLOSE, StreamID: h2fh.StreamID, Length: 0,
					}, nil, nil
				}
				continue
			}

			// Single-frame fast path: accumulator empty AND body fully
			// contains exactly one LPM AND nothing follows. Return the
			// body directly without going through the accumulator.
			acc := holder.getLpmAccumulator(h2fh.StreamID)
			if !acc.inProgress() && len(payload) >= 5 {
				bodyLen := int(binary.BigEndian.Uint32(payload[1:5]))
				if 5+bodyLen == len(payload) {
					// Stream-end on this DATA frame implies the next
					// HEADERS/TRAILERS will arrive; clean accumulator
					// state preemptively so a stale entry can't grow.
					if h2fh.Flags&H2FlagEndStream != 0 {
						holder.removeLpmAccumulator(h2fh.StreamID)
					}
					return FrameHeader{
						Type:     FrameTypeMESSAGE,
						StreamID: h2fh.StreamID,
						Length:   uint32(len(payload)),
						Flags:    0,
					}, payload, nil
				}
			}

			// Multi-frame / multi-message path: feed the accumulator.
			data := payload
			for len(data) > 0 {
				msg, leftover, ferr := acc.feed(data, 0)
				if ferr != nil {
					return FrameHeader{}, nil, ferr
				}
				if msg != nil {
					// Stash any leftover (start of next LPM in the same
					// DATA frame) for the next iteration of the outer
					// readFrameH2 loop.
					if len(leftover) > 0 {
						holder.pendingFrame = leftover
						holder.pendingStreamID = h2fh.StreamID
					}
					return FrameHeader{
						Type:     FrameTypeMESSAGE,
						StreamID: h2fh.StreamID,
						Length:   uint32(len(msg)),
						Flags:    0,
					}, msg, nil
				}
				// feed consumed all of data into the accumulator without
				// completing a message — break out and read the next
				// H2 DATA frame.
				break
			}
			// END_STREAM arrived but accumulator still mid-message: protocol error.
			if h2fh.Flags&H2FlagEndStream != 0 && acc.inProgress() {
				return FrameHeader{}, nil, errors.New("h2: END_STREAM with incomplete LPM in accumulator")
			}
			continue
		case H2FrameHEADERS:
			// HPACK-decode and convert to Custom16 HeadersV1 / TrailersV1
			// payload format the rest of the transport expects.
			h, t, isTrailers, derr := h2DecodeHeaders(holder, payload)
			if derr != nil {
				return FrameHeader{}, nil, derr
			}
			if isTrailers {
				// Stream is ending — drop accumulator state.
				holder.removeLpmAccumulator(h2fh.StreamID)
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
		case H2FrameRSTSTREAM:
			// Stream cancelled — drop accumulator state.
			holder.removeLpmAccumulator(h2fh.StreamID)
			return FrameHeader{
				Type:     FrameTypeCANCEL,
				StreamID: h2fh.StreamID,
				Length:   uint32(len(payload)),
			}, payload, nil
		case H2FrameGOAWAY, H2FramePING, H2FrameWINDOWUPDATE:
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

// readFrameViewH2 is the H2 analogue of readFrameView. For single-frame
// DATA payloads carrying exactly one complete LPM, returns a ring-backed
// mem.Buffer using the deferred-publish ZC protocol — same model as
// Custom16's readFrameView. HEADERS, multi-frame DATA, and other small
// frames go through the copy path.
//
// ZC eligibility (mirrors Custom16):
//   - DATA frame with one complete LPM in body (body == 5+lpmLen)
//   - lpmAccumulator is empty (no in-progress chain)
//   - body slice contiguous in the ring (no wrap)
//   - rx.IsSpeculativeZCEligible (ring large enough, payload large
//     enough, at-most-one-ZC, < 75% full)
func readFrameViewH2(ctx context.Context, rx *ShmRing, holder *hpackDecoderHolder) (FrameHeader, mem.Buffer, error) {
	for {
		// Drain any pending leftover from a previous DATA frame first.
		// These bytes already lived in heap-allocated form (pendingFrame
		// is a copy, not a ring slice), so ZC isn't applicable here.
		if len(holder.pendingFrame) > 0 {
			sid := holder.pendingStreamID
			data := holder.pendingFrame
			holder.pendingFrame = nil
			acc := holder.getLpmAccumulator(sid)
			msg, leftover, ferr := acc.feed(data, 0)
			if ferr != nil {
				return FrameHeader{}, nil, ferr
			}
			if len(leftover) > 0 {
				holder.pendingFrame = leftover
				holder.pendingStreamID = sid
			}
			if msg != nil {
				buf := mem.Copy(msg, mem.DefaultBufferPool())
				return FrameHeader{
					Type:     FrameTypeMESSAGE,
					StreamID: sid,
					Length:   uint32(len(msg)),
				}, buf, nil
			}
		}

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

		// Reserve payload — but DON'T commit yet. We want to inspect
		// the body to decide between ZC and copy paths.
		var pFirst, pSecond []byte
		var commitPayload *ReadCommit
		if h2fh.Length > 0 {
			pFirst, pSecond, commitPayload, err = rx.ReadSlices(ctx, int(h2fh.Length))
			if err != nil {
				return FrameHeader{}, nil, err
			}
		}

		switch h2fh.Type {
		case H2FrameSETTINGS, H2FramePRIORITY, H2FramePUSHPROMISE:
			if commitPayload != nil {
				commitPayload.Commit(int(h2fh.Length))
			}
			continue

		case H2FrameDATA:
			if h2fh.Length == 0 {
				if h2fh.Flags&H2FlagEndStream != 0 {
					return FrameHeader{
						Type: FrameTypeHALFCLOSE, StreamID: h2fh.StreamID,
					}, nil, nil
				}
				continue
			}

			// === ZC fast path ===
			//
			// Conditions:
			//   - accumulator empty (no in-progress chain)
			//   - body contiguous (no ring wrap)
			//   - body fully contains exactly one LPM
			//   - rx.IsSpeculativeZCEligible (large enough ring/payload,
			//     at-most-one-ZC, not under back-pressure)
			//
			// The body bytes returned to the caller include the gRPC LPM
			// 5-byte prefix, matching Custom16 readFrameView.
			acc := holder.getLpmAccumulator(h2fh.StreamID)
			if !acc.inProgress() && len(pSecond) == 0 && len(pFirst) >= 5 {
				bodyLen := int(binary.BigEndian.Uint32(pFirst[1:5]))
				payloadLen := int(h2fh.Length)
				if 5+bodyLen == payloadLen && rx.IsSpeculativeZCEligible(payloadLen, true) {
					// Arm the ZC anchor with the post-frame target, then
					// don't call commitPayload.Commit — the deferred
					// target already accounts for these bytes.
					baseIdx := commitPayload.commitReadIdx
					rx.BeginSingleFrameZcCommit(baseIdx, payloadLen)

					// Pre-emptively clean accumulator state if END_STREAM
					// arrives on this DATA frame.
					if h2fh.Flags&H2FlagEndStream != 0 {
						holder.removeLpmAccumulator(h2fh.StreamID)
					}

					ringSlice := pFirst[:payloadLen:payloadLen]
					pool := &zcReleasePool{ring: rx}
					buf := mem.NewBuffer(&ringSlice, pool)
					return FrameHeader{
						Type:     FrameTypeMESSAGE,
						StreamID: h2fh.StreamID,
						Length:   uint32(payloadLen),
					}, buf, nil
				}
			}

			// === Copy path ===
			//
			// Materialize body to a heap buffer, commit, then run it
			// through the lpmAccumulator. Same as readFrameH2's logic.
			payload := make([]byte, h2fh.Length)
			cn := copy(payload, pFirst)
			if cn < int(h2fh.Length) && len(pSecond) > 0 {
				copy(payload[cn:], pSecond)
			}
			commitPayload.Commit(int(h2fh.Length))

			data := payload
			for len(data) > 0 {
				msg, leftover, ferr := acc.feed(data, 0)
				if ferr != nil {
					return FrameHeader{}, nil, ferr
				}
				if msg != nil {
					if len(leftover) > 0 {
						holder.pendingFrame = leftover
						holder.pendingStreamID = h2fh.StreamID
					}
					buf := mem.Copy(msg, mem.DefaultBufferPool())
					return FrameHeader{
						Type:     FrameTypeMESSAGE,
						StreamID: h2fh.StreamID,
						Length:   uint32(len(msg)),
					}, buf, nil
				}
				break
			}
			if h2fh.Flags&H2FlagEndStream != 0 && acc.inProgress() {
				return FrameHeader{}, nil, errors.New("h2: END_STREAM with incomplete LPM in accumulator")
			}
			continue

		case H2FrameHEADERS:
			// Materialize the H2 body into the holder's reusable scratch
			// buffer. h2DecodeHeaders consumes it synchronously so it's
			// safe to overwrite on the next frame's call to scratchBytes.
			payload := holder.scratchBytes(int(h2fh.Length))
			if h2fh.Length > 0 {
				cn := copy(payload, pFirst)
				if cn < int(h2fh.Length) && len(pSecond) > 0 {
					copy(payload[cn:], pSecond)
				}
				commitPayload.Commit(int(h2fh.Length))
			}
			h, t, isTrailers, derr := h2DecodeHeaders(holder, payload)
			if derr != nil {
				return FrameHeader{}, nil, derr
			}
			if isTrailers {
				holder.removeLpmAccumulator(h2fh.StreamID)
				out := encodeTrailers(t)
				return FrameHeader{
					Type:     FrameTypeTRAILERS,
					StreamID: h2fh.StreamID,
					Length:   uint32(len(out)),
					Flags:    TrailersFlagEndStream,
				}, mem.Copy(out, mem.DefaultBufferPool()), nil
			}
			out := encodeHeaders(h)
			return FrameHeader{
				Type:     FrameTypeHEADERS,
				StreamID: h2fh.StreamID,
				Length:   uint32(len(out)),
				Flags:    HeadersFlagINITIAL,
			}, mem.Copy(out, mem.DefaultBufferPool()), nil

		case H2FrameRSTSTREAM:
			payload := holder.scratchBytes(int(h2fh.Length))
			if h2fh.Length > 0 {
				cn := copy(payload, pFirst)
				if cn < int(h2fh.Length) && len(pSecond) > 0 {
					copy(payload[cn:], pSecond)
				}
				commitPayload.Commit(int(h2fh.Length))
			}
			holder.removeLpmAccumulator(h2fh.StreamID)
			return FrameHeader{
				Type:     FrameTypeCANCEL,
				StreamID: h2fh.StreamID,
				Length:   uint32(len(payload)),
			}, mem.Copy(payload, mem.DefaultBufferPool()), nil

		case H2FrameGOAWAY, H2FramePING, H2FrameWINDOWUPDATE:
			payload := holder.scratchBytes(int(h2fh.Length))
			if h2fh.Length > 0 {
				cn := copy(payload, pFirst)
				if cn < int(h2fh.Length) && len(pSecond) > 0 {
					copy(payload[cn:], pSecond)
				}
				commitPayload.Commit(int(h2fh.Length))
			}
			ft, fl, ok := translateH2ToCustom(h2fh.Type, h2fh.Flags)
			if !ok {
				continue
			}
			return FrameHeader{
				Type:     ft,
				StreamID: h2fh.StreamID,
				Length:   uint32(len(payload)),
				Flags:    fl,
			}, mem.Copy(payload, mem.DefaultBufferPool()), nil

		default:
			// Unknown frame type. Per RFC 7540 §4.1, ignore after consuming.
			if commitPayload != nil {
				commitPayload.Commit(int(h2fh.Length))
			}
			continue
		}
	}
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

	// MESSAGE payloads exceeding the 16MB-1 H2 frame limit are split
	// into multiple DATA frames. The reader's lpmAccumulator reassembles
	// them. This is mandatory for protocol compliance with peers that
	// honor SETTINGS_MAX_FRAME_SIZE: even a peer with the default 16 KiB
	// max frame size must be able to receive arbitrarily large messages.
	if fh.Type == FrameTypeMESSAGE && len(h2payload) > h2MaxFramePayload {
		return writeFrameH2DataChunked(ctx, tx, fh.StreamID, h2payload, h2f)
	}

	if len(h2payload) > h2MaxFramePayload {
		return fmt.Errorf("h2: payload %d exceeds 16MB max frame size for non-DATA frame type %d", len(h2payload), fh.Type)
	}

	return writeH2Single(ctx, tx, h2t, h2f, fh.StreamID, h2payload)
}

// writeH2Single writes one complete H2 frame to the ring with the given
// header fields and payload (which must fit in the 24-bit length field).
func writeH2Single(ctx context.Context, tx *ShmRing, h2t H2FrameType, h2f byte, streamID uint32, h2payload []byte) error {
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
		StreamID: streamID,
	})

	// Layout the 9-byte header at the start of the reservation.
	if len(res.First) >= h2FrameHeaderSize {
		copy(res.First[:h2FrameHeaderSize], hdr[:])
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
		bodyDest := res.Second[remHdr:]
		copy(bodyDest, h2payload)
	}
	return res.Commit(total)
}

// writeFrameH2DataChunked writes a MESSAGE whose body exceeds the 16MB-1
// H2 frame limit by splitting it into multiple DATA frames. The reader's
// lpmAccumulator reassembles the LPM stream; END_STREAM is never set on
// these intermediate frames (gRPC ends the stream via TRAILERS).
//
// Chunk size is bounded by both h2MaxFramePayload (RFC 7540 §4.2) and
// the ring capacity / 4 — the latter ensures the writer can always
// place the next chunk while the reader is still consuming the
// previous, avoiding stall under back-pressure.
func writeFrameH2DataChunked(ctx context.Context, tx *ShmRing, streamID uint32, body []byte, baseFlags byte) error {
	maxChunk := h2MaxFramePayload
	if uint64(maxChunk) > tx.Capacity()/4 {
		maxChunk = int(tx.Capacity() / 4)
	}
	if maxChunk == 0 {
		return fmt.Errorf("h2 chunk: ring capacity %d too small to chunk", tx.Capacity())
	}
	for off := 0; off < len(body); off += maxChunk {
		end := off + maxChunk
		if end > len(body) {
			end = len(body)
		}
		// Don't propagate END_STREAM to intermediate chunks — only the
		// last one (if baseFlags carries it). gRPC SHM never sets
		// END_STREAM on MESSAGE; TRAILERS ends the stream.
		flags := byte(0)
		if end == len(body) {
			flags = baseFlags
		}
		if err := writeH2Single(ctx, tx, H2FrameDATA, flags, streamID, body[off:end]); err != nil {
			return err
		}
	}
	return nil
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
