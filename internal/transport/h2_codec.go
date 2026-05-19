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
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc/internal/grpcutil"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/proto"
)

// h2Codec encodes and decodes the SHM transport's in-memory frame model
// (FrameHeader + payload bytes) on top of the HTTP/2 wire format.
//
// The mapping between the in-memory FrameHeader types and HTTP/2 frame
// types is:
//
//	HEADERS  (initial)  → H2 HEADERS  + END_HEADERS
//	HEADERS  (server)   → H2 HEADERS  + END_HEADERS
//	MESSAGE  (more=0)   → H2 DATA
//	MESSAGE  (more=1)   → H2 DATA (intermediate chunk; END_STREAM=0)
//	TRAILERS            → H2 HEADERS  + END_HEADERS + END_STREAM
//	CANCEL              → H2 RST_STREAM (error_code=CANCEL)
//	GOAWAY              → H2 GOAWAY
//	PING/PONG           → H2 PING (PONG = PING + ACK)
//	HALFCLOSE           → H2 DATA  (empty, END_STREAM=1)
//	WINDOW_UPDATE       → H2 WINDOW_UPDATE
//
// HEADERS/TRAILERS payloads are KV blobs in the in-memory HeadersV1 /
// TrailersV1 model. They are HPACK-encoded as H2 :pseudo-header fields
// plus regular gRPC metadata. The decoder converts back to the same
// HeadersV1 / TrailersV1 structs the rest of the transport already
// uses.

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
	// pendingFrameEndStream is set when the DATA frame whose leftover
	// landed in pendingFrame had END_STREAM. The replay path must
	// honour the deferred-halfclose state once the leftover drains:
	// without this flag, a multi-LPM DATA frame ending the stream
	// (legitimate when grpc-go peer batches multiple messages with
	// END_STREAM on the last DATA) would surface the messages but
	// never produce HALFCLOSE.
	pendingFrameEndStream bool

	// lastSid / lastAcc form a single-entry MRU cache over
	// lpmAccumulators. The reader is single-goroutine and almost all
	// gRPC traffic is sticky to one stream id at a time (pipelined
	// unary RPCs, streaming) so the cache absorbs essentially every
	// DATA frame's accumulator lookup. Map miss path still works
	// (cache update only on map insert / hit). lastSid == 0 means
	// cache empty (stream id 0 is reserved per RFC 7540 §5.1.1 and
	// rejected by validateH2ControlFrame for DATA/HEADERS, so it's
	// safe as the sentinel).
	lastSid uint32
	lastAcc *lpmAccumulator

	// pendingHalfCloseStreamID, when non-zero, requests a synthetic
	// FrameTypeHALFCLOSE to be returned on the next read BEFORE
	// touching the ring. Set when an initial HEADERS frame arrived
	// with END_STREAM (a zero-message client-streaming request: rare
	// but legal per RFC 7540 §6.2 and gRFC G2). The codec returns
	// the HEADERS first so the server transport can create the
	// stream and dispatch the handler; the deferred HALFCLOSE then
	// triggers the upper layer's client-half-close path
	// (ShmServerTransport.handleHalfClose writes io.EOF to the
	// stream's recv channel) so a HEADERS-only RPC doesn't hang
	// waiting for a MESSAGE that never arrives.
	pendingHalfCloseStreamID uint32

	// onDataFrame, when non-nil, is invoked synchronously every time
	// the codec consumes the body bytes of an H2 DATA frame from the
	// ring — BEFORE those bytes are handed to the per-stream
	// lpmAccumulator. The callback runs on the reader goroutine and
	// MUST NOT block on the producer (e.g., it must not call into
	// frameWriter.enqueueAndWait); instead it queues WINDOW_UPDATE
	// frames non-blocking. The transport layer plugs this in to
	// credit HTTP/2 flow control per-DATA-frame, decoupled from LPM
	// reassembly — which mirrors http2_client.handleData's "Decouple
	// connection's flow control from application's read" design and
	// lets a producer with a small per-stream send window drain a
	// multi-DATA-frame LPM without deadlocking on a WindowUpdate that
	// never arrives because the consumer is buffering the partial
	// LPM. nil is the legacy behaviour (credit only on complete LPM
	// inside handleMessage / handleMessageBuffer); valid for callers
	// that pin sendQuota = maxWindowSize.
	onDataFrame func(streamID uint32, size uint32)
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
	// Cap any single decoded HPACK string at 64 KiB. RFC 7541 doesn't
	// enforce a per-string limit; without this guard a peer could send
	// one HEADERS field whose Huffman-decoded value is gigabytes,
	// allocating memory on the receiver per field. 64 KiB is well
	// above any realistic gRPC metadata value (path, authority, custom
	// metadata) and matches stock golang.org/x/net/http2's default for
	// the MaxHeaderListSize SETTINGS knob.
	dec.SetMaxStringLength(64 * 1024)
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
//
// Hot path optimisation: a single-entry MRU cache (lastSid/lastAcc)
// short-circuits the map lookup. gRPC traffic is overwhelmingly sticky
// to a single stream id within a sequence of DATA frames (unary RPC
// finishes its message exchange in 2-3 DATA frames on the same stream;
// streaming sends many DATA frames in a row on one stream). The
// reader is single-goroutine so the cache is race-free without
// locking.
func (h *hpackDecoderHolder) getLpmAccumulator(sid uint32) *lpmAccumulator {
	if sid == h.lastSid && h.lastAcc != nil {
		return h.lastAcc
	}
	if a, ok := h.lpmAccumulators[sid]; ok {
		h.lastSid = sid
		h.lastAcc = a
		return a
	}
	a := &lpmAccumulator{pool: shmLpmPool}
	h.lpmAccumulators[sid] = a
	h.lastSid = sid
	h.lastAcc = a
	return a
}

// notifyDataFrameConsumed invokes the holder's onDataFrame callback
// if one is set. Called at every site in the codec that has just
// committed an H2 DATA-frame body from the ring — BEFORE either the
// lpmAccumulator buffers the bytes (multi-DATA-frame LPM) or the
// ZC fast path returns a ring-backed slice (single-DATA-frame LPM).
// The transport plugs in onDataFrame to credit HTTP/2 flow control
// per DATA frame, mirroring http2_client.handleData's decoupling of
// connection flow control from message reassembly.
//
// streamID is the H2 stream id from the DATA frame header; size is
// the on-wire payload length (h2fh.Length) — this includes any
// padding bytes for PADDED frames because the bytes WERE charged
// against the peer's window on the wire even though the codec will
// later strip them.
func (h *hpackDecoderHolder) notifyDataFrameConsumed(streamID uint32, size uint32) {
	if h.onDataFrame != nil && size > 0 {
		h.onDataFrame(streamID, size)
	}
}

// removeLpmAccumulator drops the accumulator for stream sid (called on
// stream close: TRAILERS, RST_STREAM, or HEADERS-with-END_STREAM).
// Invalidates the MRU cache when the removed sid matches AND clears
// any in-flight pendingFrame state for that sid — without the latter,
// a peer that sends DATA[partial-lpm]+RST_STREAM would leave the
// pendingFrame slice from the dead stream queued; the next read would
// replay it against a freshly-recreated accumulator on the same sid.
func (h *hpackDecoderHolder) removeLpmAccumulator(sid uint32) {
	delete(h.lpmAccumulators, sid)
	if h.lastSid == sid {
		h.lastSid = 0
		h.lastAcc = nil
	}
	if h.pendingStreamID == sid {
		h.pendingFrame = nil
		h.pendingStreamID = 0
		h.pendingFrameEndStream = false
	}
}

// h2Encoder lazily initializes and returns the HPACK encoder for this
// ring. Allocated on first use.
func (r *ShmRing) h2Encoder() *hpackEncoderHolder {
	if r.h2Enc == nil {
		r.h2Enc = newHpackEncoderHolder()
	}
	return r.h2Enc
}

// h2Decoder lazily initializes and returns the HPACK decoder for this
// ring. Allocated on first use.
func (r *ShmRing) h2Decoder() *hpackDecoderHolder {
	if r.h2Dec == nil {
		r.h2Dec = newHpackDecoderHolder()
	}
	return r.h2Dec
}

// writeMetadataField HPACK-encodes one metadata key/value pair, applying
// gRPC-over-HTTP/2 binary-metadata transport rules:
//
//   - Keys ending in "-bin" carry arbitrary byte values. The internal
//     HeadersV1/TrailersV1 model holds the RAW bytes (correct for the SHM
//     boundary). On the H2 wire those values MUST be base64-encoded per
//     gRFC G2 / gRPC-over-HTTP/2 §"Binary headers". Stock gRPC peers
//     (grpc-go's own HTTP/2 transport, grpc-java, grpc-c++) base64-decode
//     such headers on receipt, and emitting raw bytes here would violate
//     the spec and confuse anyone tracing the wire (Wireshark, gRPC tracing,
//     debug proxies).
//   - HPACK header names MUST be lowercase (RFC 7540 §8.1.2). This is a
//     hard requirement for real H2 peer interop — receivers that follow
//     the spec strictly will reject any uppercase byte in the field name
//     with PROTOCOL_ERROR. We lowercase here so callers can use whatever
//     case suits them. The lowercase form is also what makes the "-bin"
//     suffix detection match keys like "X-Custom-Bin".
func writeMetadataField(enc *hpack.Encoder, name string, raw []byte) {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, binHdrSuffix) {
		_ = enc.WriteField(hpack.HeaderField{Name: lower, Value: encodeBinHeader(raw)})
		return
	}
	_ = enc.WriteField(hpack.HeaderField{Name: lower, Value: string(raw)})
}

// decodeMetadataValue materialises a metadata value from a decoded HPACK
// header value, base64-decoding when name is a binary header. Tolerates
// malformed base64 by returning the raw HPACK bytes; matches stock
// grpc-go's lenient behaviour (a strict reject here would tear down the
// connection on a single bad header).
//
// Case-insensitive on the suffix check so a non-conformant peer that
// sends an uppercase "-Bin" still gets binary semantics. Conformant
// gRPC peers always send lowercase per RFC 7540 §8.1.2 so the lowercase
// fast path covers the overwhelmingly common case.
func decodeMetadataValue(name, hpackValue string) []byte {
	if !hasBinHeaderSuffix(name) {
		return []byte(hpackValue)
	}
	if b, err := decodeBinHeader(hpackValue); err == nil {
		return b
	}
	return []byte(hpackValue)
}

// hasBinHeaderSuffix is a case-insensitive equivalent of
// strings.HasSuffix(name, binHdrSuffix). Avoids allocating a lowercased
// copy of name when the suffix doesn't match anyway.
func hasBinHeaderSuffix(name string) bool {
	if len(name) < len(binHdrSuffix) {
		return false
	}
	tail := name[len(name)-len(binHdrSuffix):]
	for i := 0; i < len(binHdrSuffix); i++ {
		c := tail[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != binHdrSuffix[i] {
			return false
		}
	}
	return true
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
		// content-type defaults to "application/grpc" but caller may
		// request a subtype (e.g. "application/grpc+proto" /
		// "+json" / "+<custom>") via the Metadata "content-type"
		// key per gRFC G2 / gRPC-over-HTTP/2.
		_ = enc.WriteField(hpack.HeaderField{Name: "content-type", Value: pickContentType(h.Metadata)})
		if h.DeadlineUnixNano != 0 {
			ns := int64(h.DeadlineUnixNano) - time.Now().UnixNano()
			if ns < 0 {
				ns = 0
			}
			// gRPC encodes timeout as `<value><unit>`. The value MUST
			// fit in 8 ASCII digits per gRFC G2 / gRPC-over-HTTP/2
			// "Timeout"; grpcutil.EncodeDuration picks the largest unit
			// that satisfies that constraint (e.g. 5s -> "5S", not
			// "5000000000n", which strict peers reject as 10 digits).
			_ = enc.WriteField(hpack.HeaderField{Name: "grpc-timeout",
				Value: grpcutil.EncodeDuration(time.Duration(ns))})
		}
	} else {
		// Server-initial.
		_ = enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
		_ = enc.WriteField(hpack.HeaderField{Name: "content-type", Value: pickContentType(h.Metadata)})
	}
	for _, kv := range h.Metadata {
		// content-type was already written above (potentially derived
		// from this metadata); skip the duplicate to avoid emitting
		// the same header twice on the wire.
		if strings.EqualFold(kv.Key, "content-type") {
			continue
		}
		for _, v := range kv.Values {
			writeMetadataField(enc, kv.Key, v)
		}
	}
	return scratch.Bytes()
}

// pickContentType returns the content-type value to write on the wire.
// If the caller supplied a "content-type" metadata key with at least one
// value, the first value is used (case-insensitive key match). Otherwise
// returns the canonical "application/grpc" default.
func pickContentType(md []KV) string {
	for _, kv := range md {
		if strings.EqualFold(kv.Key, "content-type") && len(kv.Values) > 0 {
			return string(kv.Values[0])
		}
	}
	return "application/grpc"
}

// h2EncodeTrailers HPACK-encodes a TrailersV1 into an H2 HEADERS frame
// payload (typically with END_STREAM | END_HEADERS flags).
func h2EncodeTrailers(enc *hpack.Encoder, scratch *bytes.Buffer, t TrailersV1) []byte {
	scratch.Reset()
	_ = enc.WriteField(hpack.HeaderField{Name: "grpc-status", Value: strconv.FormatUint(uint64(t.GRPCStatusCode), 10)})
	if t.GRPCStatusMsg != "" {
		// gRPC-over-HTTP/2 requires grpc-message to be percent-encoded
		// (gRFC G2 / 'Status & status-message'): only printable ASCII
		// 0x20-0x7E except '%' is allowed verbatim; all other bytes
		// must be %-escaped. encodeGrpcMessage handles the fast path
		// (no escape needed) without allocation.
		_ = enc.WriteField(hpack.HeaderField{Name: "grpc-message", Value: encodeGrpcMessage(t.GRPCStatusMsg)})
	}
	for _, kv := range t.Metadata {
		for _, v := range kv.Values {
			writeMetadataField(enc, kv.Key, v)
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
	case "te":
		// Standard gRPC header; ignore for in-memory model.
	case "content-type":
		// Surface the content-type into the metadata map so the
		// upper-layer transport can extract a non-default subtype
		// (e.g. "application/grpc+proto" → ContentSubtype "proto").
		// The encoder is symmetric: it picks the metadata
		// "content-type" value when present, defaulting to the
		// canonical "application/grpc" otherwise.
		if s.isTrailers {
			appendKV(&s.t.Metadata, "content-type", []byte(hf.Value))
		} else {
			appendKV(&s.h.Metadata, "content-type", []byte(hf.Value))
		}
	case "grpc-timeout":
		if d, perr := decodeTimeout(hf.Value); perr == nil {
			s.h.DeadlineUnixNano = uint64(time.Now().Add(d).UnixNano())
		}
	case "grpc-status":
		s.isTrailers = true
		if v, cerr := strconv.ParseUint(hf.Value, 10, 32); cerr == nil {
			s.t.GRPCStatusCode = uint32(v)
		}
	case "grpc-message":
		s.isTrailers = true
		// Reverse of encodeGrpcMessage. decodeGrpcMessage tolerates
		// malformed escapes by returning the input unchanged.
		s.t.GRPCStatusMsg = decodeGrpcMessage(hf.Value)
	default:
		// User metadata. Apply -bin base64 decode at the HPACK boundary
		// per gRPC-over-HTTP/2 binary-headers rules (mirror of
		// writeMetadataField). decodeMetadataValue returns the
		// caller-owned []byte; the emit callback's closure already
		// guarantees independence from the HPACK input slice (the
		// hpack package's decodeString deep-copies via string(u.b)).
		val := decodeMetadataValue(hf.Name, hf.Value)
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

// h2MaxHeaderListSize bounds the cumulative HPACK block size accepted
// across a HEADERS + CONTINUATION sequence. A malicious peer streaming
// gigabytes of HEADERS would otherwise OOM the receiver before any
// upper-layer rate limiter triggers. 8 MiB is well above the largest
// realistic gRPC HEADERS block (rich grpc-status-details-bin trailers
// in error responses are typically a few KiB).
const h2MaxHeaderListSize = 8 * 1024 * 1024

// h2MaxLPMBodyBytes bounds the LPM body length the codec is willing to
// allocate for in a single gRPC message reassembled from H2 DATA
// frames. Without this cap a peer could send a tiny DATA frame whose
// 5-byte LPM header declares a multi-gigabyte body, and the
// accumulator would attempt make([]byte, 0, declared) before any
// upper-layer MaxRecvMsgSize check has a chance to reject it. The cap
// is a transport-level DoS bound; per-stream limits set by
// grpc.MaxCallRecvMsgSize / grpc.MaxRecvMsgSize still apply
// independently and may further restrict acceptable sizes.
//
// 512 MiB is set well above the largest body benchmarked by the proto
// suite (256 MiB user payload, which serialises to ~256 MiB + 6 bytes
// after protobuf wrapping). Any peer requiring more than this should
// stream rather than send a single unary message.
const h2MaxLPMBodyBytes = 512 * 1024 * 1024

// h2MaxContinuationFrames bounds the number of CONTINUATION frames a
// peer may emit while assembling one logical HEADERS block. Defends
// against a peer that streams an unbounded number of zero-length
// CONTINUATION frames — the cumulative-byte cap above never trips
// because the payload contributes nothing, but each iteration still
// consumes 9 bytes of ring header and CPU. Picked well above what any
// real gRPC peer would emit (with SETTINGS_MAX_FRAME_SIZE = 16 MiB-1
// the absolute maximum is one CONTINUATION).
const h2MaxContinuationFrames = 256

// readH2HeadersWithContinuations assembles a complete HPACK header block
// for a HEADERS frame whose first fragment did not carry END_HEADERS.
// Reads subsequent H2 frames from rx and copies their payloads into a
// pooled buffer until END_HEADERS is observed.
//
// firstPayload is the (already drained from the ring) body of the
// initial HEADERS frame. The returned slice owns its own memory; the
// caller is free to retain or pass it to the HPACK decoder. Stays nil
// on error.
//
// Validation enforced inside the loop:
//
//   - the only legal frame type until END_HEADERS is CONTINUATION
//     (RFC 7540 §6.10: any other type is PROTOCOL_ERROR; a peer cannot
//     legally interleave DATA, RST_STREAM, etc. between HEADERS and
//     trailing CONTINUATION);
//   - streamID MUST match the originating HEADERS streamID;
//   - cumulative payload bounded by h2MaxHeaderListSize.
//
// Each CONTINUATION frame's payload is fully drained (committed) before
// any error is propagated, so the ring read pointer stays in sync with
// the cross-process writer's view.
func readH2HeadersWithContinuations(
	ctx context.Context, rx *ShmRing, headersStreamID uint32, firstPayload []byte,
) ([]byte, error) {
	if len(firstPayload) > h2MaxHeaderListSize {
		return nil, fmt.Errorf("h2 HEADERS first fragment %d bytes exceeds %d",
			len(firstPayload), h2MaxHeaderListSize)
	}
	// Start with a buffer sized to a typical 2-3 fragment case while
	// allowing growth up to the cap.
	initialCap := len(firstPayload) * 2
	if initialCap < 1024 {
		initialCap = 1024
	}
	if initialCap > h2MaxHeaderListSize {
		initialCap = h2MaxHeaderListSize
	}
	assembled := make([]byte, 0, initialCap)
	assembled = append(assembled, firstPayload...)

	frameCount := 0
	for {
		frameCount++
		if frameCount > h2MaxContinuationFrames {
			return nil, fmt.Errorf("h2 HEADERS+CONTINUATION exceeded %d frames (DoS guard)",
				h2MaxContinuationFrames)
		}
		// Read the next 9-byte H2 frame header.
		first, second, commitHdr, err := rx.ReadSlices(ctx, h2FrameHeaderSize)
		if err != nil {
			return nil, err
		}
		var hb [h2FrameHeaderSize]byte
		n := copy(hb[:], first)
		if n < h2FrameHeaderSize && len(second) > 0 {
			n += copy(hb[n:], second)
		}
		commitHdr.Commit(h2FrameHeaderSize)
		if n != h2FrameHeaderSize {
			return nil, errors.New("h2: short CONTINUATION frame header")
		}
		fh, err := decodeH2FrameHeader(hb[:])
		if err != nil {
			return nil, err
		}

		// Drain the frame payload into a scratch buffer regardless of
		// what we do with it — this keeps the ring read pointer in
		// sync if validation rejects the frame.
		var payload []byte
		if fh.Length > 0 {
			pFirst, pSecond, commitPayload, perr := rx.ReadSlices(ctx, int(fh.Length))
			if perr != nil {
				return nil, perr
			}
			payload = make([]byte, fh.Length)
			cn := copy(payload, pFirst)
			if cn < int(fh.Length) && len(pSecond) > 0 {
				copy(payload[cn:], pSecond)
			}
			commitPayload.Commit(int(fh.Length))
		}

		if fh.Type != H2FrameCONTINUATION {
			return nil, fmt.Errorf("h2 frame type %s interleaved between HEADERS and CONTINUATION (RFC 7540 §6.10)", fh.Type)
		}
		if fh.StreamID != headersStreamID {
			return nil, fmt.Errorf("h2 CONTINUATION streamID %d does not match originating HEADERS streamID %d",
				fh.StreamID, headersStreamID)
		}
		if int64(len(assembled))+int64(len(payload)) > h2MaxHeaderListSize {
			return nil, fmt.Errorf("h2 HEADERS+CONTINUATION cumulative payload exceeds %d bytes",
				h2MaxHeaderListSize)
		}
		assembled = append(assembled, payload...)

		if fh.Flags&H2FlagEndHeaders != 0 {
			return assembled, nil
		}
	}
}

// stripDataPadding removes the PADDED flag's prefix/suffix from a DATA
// frame body per RFC 7540 §6.1. When PADDED is not set, returns the
// input unchanged. When PADDED is set, the first byte is the pad
// length, followed by the actual data, followed by `pad length` zero
// bytes; we trim both ends and return only the payload.
//
// Returns an error if the pad length exceeds the available bytes
// (FRAME_SIZE_ERROR per RFC).
//
// gRPC peers do not normally send PADDED DATA, but a standards-
// compliant H2 sender (Kestrel, nginx, envoy) may legally do so when
// it satisfies an upper-layer alignment or DoS-mitigation policy.
func stripDataPadding(payload []byte, flags byte) ([]byte, error) {
	if flags&H2FlagPadded == 0 {
		return payload, nil
	}
	if len(payload) < 1 {
		return nil, errors.New("h2 DATA PADDED with empty payload (RFC 7540 §6.1 FRAME_SIZE_ERROR)")
	}
	padLen := int(payload[0])
	if 1+padLen > len(payload) {
		return nil, fmt.Errorf("h2 DATA pad length %d exceeds available payload %d (RFC 7540 §6.1)",
			padLen, len(payload)-1)
	}
	return payload[1 : len(payload)-padLen], nil
}

// stripHeadersPaddingAndPriority removes the PADDED flag's pad-length
// prefix + trailing padding and the PRIORITY flag's 5-byte priority
// prefix from a HEADERS frame body per RFC 7540 §6.2. Returns the
// HPACK fragment slice. The PRIORITY weight/dependency information is
// dropped — gRPC does not surface it (and RFC 9113 deprecates it).
//
// Errors on malformed lengths (FRAME_SIZE_ERROR per RFC).
func stripHeadersPaddingAndPriority(payload []byte, flags byte) ([]byte, error) {
	if flags&(H2FlagPadded|H2FlagPriority) == 0 {
		return payload, nil
	}
	out := payload
	padLen := 0
	if flags&H2FlagPadded != 0 {
		if len(out) < 1 {
			return nil, errors.New("h2 HEADERS PADDED with empty payload (RFC 7540 §6.2 FRAME_SIZE_ERROR)")
		}
		padLen = int(out[0])
		out = out[1:]
	}
	if flags&H2FlagPriority != 0 {
		// 5-byte stream-dependency + weight prefix.
		if len(out) < 5 {
			return nil, errors.New("h2 HEADERS PRIORITY prefix shorter than 5 bytes (RFC 7540 §6.2 FRAME_SIZE_ERROR)")
		}
		out = out[5:]
	}
	if padLen > len(out) {
		return nil, fmt.Errorf("h2 HEADERS pad length %d exceeds remaining payload %d (RFC 7540 §6.2)",
			padLen, len(out))
	}
	return out[:len(out)-padLen], nil
}

// validateH2ControlFrame checks the per-RFC 7540 invariants of the
// control-frame types the codec understands (RST_STREAM, SETTINGS, PING,
// GOAWAY, WINDOW_UPDATE, plus stream-id checks for DATA/HEADERS).
// Returns a non-nil error when the frame is malformed; the caller is
// responsible for ensuring the malformed payload bytes are committed to
// keep the ring read pointer in sync before propagating the error up.
//
// Validations enforced:
//
//   - DATA (§6.1): stream id MUST be non-zero. Receiving DATA on stream 0
//     is a connection error of type PROTOCOL_ERROR; silently accepting
//     it would let a buggy/malicious peer inject DATA into our stream-0
//     dispatch slot which the upper layer treats as connection-control.
//   - HEADERS (§6.2): stream id MUST be non-zero. Same reasoning as DATA;
//     additionally combines badly with our CONTINUATION assembly logic,
//     which would otherwise key the assembly state on streamID 0.
//   - RST_STREAM (§6.4): payload length MUST be exactly 4; stream id
//     MUST be non-zero. Length-tampered RST_STREAM in particular could
//     otherwise silently change call state on the receiver, since we
//     map it to internal Cancel.
//   - SETTINGS (§6.5): stream id MUST be 0; non-ACK payload length MUST
//     be a multiple of 6; ACK payload length MUST be 0.
//   - PING (§6.7): stream id MUST be 0; payload length MUST be exactly 8.
//   - GOAWAY (§6.8): stream id MUST be 0; payload length MUST be at
//     least 8 (last-stream-id + error-code, debug data optional).
//   - WINDOW_UPDATE (§6.9.1): payload length MUST be exactly 4. The
//     increment-must-be-non-zero check is enforced after payload read.
//
// Conformance with stock gRPC peers (grpc-go's HTTP/2 transport,
// grpc-java, grpc-c++) requires rejecting these forms; silently
// accepting them masks peer bugs at integration time.
func validateH2ControlFrame(h2fh H2FrameHeader) error {
	switch h2fh.Type {
	case H2FrameDATA:
		if h2fh.StreamID == 0 {
			return errors.New("h2 DATA frame must have streamID != 0 (RFC 7540 §6.1)")
		}
	case H2FrameHEADERS:
		if h2fh.StreamID == 0 {
			return errors.New("h2 HEADERS frame must have streamID != 0 (RFC 7540 §6.2)")
		}
	case H2FrameRSTSTREAM:
		if h2fh.Length != 4 || h2fh.StreamID == 0 {
			return fmt.Errorf("h2 RST_STREAM malformed (streamID=%d, length=%d; require streamID != 0 && length == 4)",
				h2fh.StreamID, h2fh.Length)
		}
	case H2FrameSETTINGS:
		if h2fh.StreamID != 0 {
			return fmt.Errorf("h2 SETTINGS frame must have streamID=0 (got %d)", h2fh.StreamID)
		}
		if h2fh.Flags&H2FlagAck != 0 {
			if h2fh.Length != 0 {
				return fmt.Errorf("h2 SETTINGS ACK must have empty payload (got length=%d)", h2fh.Length)
			}
		} else if h2fh.Length%6 != 0 {
			return fmt.Errorf("h2 SETTINGS payload length %d is not a multiple of 6", h2fh.Length)
		}
	case H2FramePING:
		if h2fh.StreamID != 0 || h2fh.Length != 8 {
			return fmt.Errorf("h2 PING malformed (streamID=%d, length=%d; require streamID == 0 && length == 8)",
				h2fh.StreamID, h2fh.Length)
		}
	case H2FrameGOAWAY:
		if h2fh.StreamID != 0 || h2fh.Length < 8 {
			return fmt.Errorf("h2 GOAWAY malformed (streamID=%d, length=%d; require streamID == 0 && length >= 8)",
				h2fh.StreamID, h2fh.Length)
		}
	case H2FrameWINDOWUPDATE:
		if h2fh.Length != 4 {
			return fmt.Errorf("h2 WINDOW_UPDATE payload length %d != 4", h2fh.Length)
		}
		// Increment-must-be-non-zero is enforced separately by the
		// caller after reading the 4-byte payload.
	}
	return nil
}

// translateCustomToH2 maps an in-memory FrameHeader to the equivalent
// H2 frame type and flags. The caller is responsible for any payload
// transformation (HPACK for HEADERS/TRAILERS, etc.).
func translateCustomToH2(fh FrameHeader) (H2FrameType, byte) {
	switch fh.Type {
	case FrameTypeMESSAGE:
		// END_STREAM is set on the wire only when the caller signalled
		// MessageFlagEndStream (logical "this is the last message
		// from MY send direction"). Set by the client transport on
		// its last request message; never set by the server. See
		// MessageFlagEndStream's docstring in frame.go.
		var f byte
		if fh.Flags&MessageFlagEndStream != 0 {
			f = H2FlagEndStream
		}
		return H2FrameDATA, f
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

// translateH2ToCustom maps an H2 frame to the in-memory FrameHeader's
// frame type and flags for delivery into the existing dispatch
// machinery. The caller has already decoded the header.
//
// HEADERS frames require examining the payload (after HPACK decode) to
// distinguish initial-headers from trailers (presence of grpc-status).
// translateH2ToCustom assumes initial-headers; the caller fixes up TRAILERS.
//
// Note on MESSAGE/MORE: H2's only stream-end signal is END_STREAM (via
// DATA with empty body or HEADERS with grpc-status). Within a stream,
// each DATA frame is a complete LPM message (no chunking — the LPM
// accumulator in the reader handles fragmented LPMs across DATA frames).
// We therefore never set MessageFlagMORE on the synthesized MESSAGE
// frame at this layer; the per-LPM MORE derivation happens in the read
// loop based on END_STREAM + leftover bytes.
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
// errorCode(4) + opaque debug data. We pack the payload (a UTF-8
// debug message) into the debug-data section and use NoError as the code.
func goawayPayloadH2(custom []byte) []byte {
	out := make([]byte, 8+len(custom))
	binary.BigEndian.PutUint32(out[0:4], 0) // lastStreamID
	binary.BigEndian.PutUint32(out[4:8], uint32(H2ErrNoError))
	copy(out[8:], custom)
	return out
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
		// Synthetic HALFCLOSE: an earlier read returned an initial
		// HEADERS that carried END_STREAM (zero-message client
		// stream). Surface the deferred half-close before touching
		// the ring so the upper-layer state machine sees HEADERS
		// then HALFCLOSE in the right order.
		if holder.pendingHalfCloseStreamID != 0 {
			sid := holder.pendingHalfCloseStreamID
			holder.pendingHalfCloseStreamID = 0
			holder.removeLpmAccumulator(sid)
			return FrameHeader{
				Type: FrameTypeHALFCLOSE, StreamID: sid, Length: 0,
			}, nil, nil
		}

		// First check if there's leftover data from a previous DATA
		// frame that contained the start of the next LPM.
		if len(holder.pendingFrame) > 0 {
			sid := holder.pendingStreamID
			data := holder.pendingFrame
			endStream := holder.pendingFrameEndStream
			holder.pendingFrame = nil
			holder.pendingFrameEndStream = false
			acc := holder.getLpmAccumulator(sid)
			msg, leftover, ferr := acc.feed(data, h2MaxLPMBodyBytes)
			if ferr != nil {
				return FrameHeader{}, nil, ferr
			}
			if len(leftover) > 0 {
				// Carry END_STREAM forward to the next replay so a
				// multi-LPM DATA frame ending the stream emits MORE=0
				// after the LAST LPM, not after the first.
				holder.pendingFrame = leftover
				holder.pendingStreamID = sid
				holder.pendingFrameEndStream = endStream
			}
			if msg != nil {
				// Set MessageFlagMORE based on END_STREAM + leftover:
				// MORE=1 when more messages follow (either more LPMs
				// queued OR more frames coming), MORE=0 only on the
				// last LPM of the END_STREAM-bearing DATA frame.
				// ShmServerTransport.handleMessage uses MORE=0 to
				// detect client half-close.
				msgFlags := MessageFlagMORE
				if endStream && len(leftover) == 0 {
					msgFlags = 0
					holder.removeLpmAccumulator(sid)
				}
				return FrameHeader{
					Type:     FrameTypeMESSAGE,
					StreamID: sid,
					Length:   uint32(len(msg)),
					Flags:    msgFlags,
				}, msg, nil
			}
			// Accumulator still in-progress (only header consumed): if
			// the source DATA frame had END_STREAM AND the current
			// feed produced no message AND there's still bytes left to
			// arrive (acc.inProgress, no leftover, no msg), the peer
			// truncated the LPM mid-message — connection-fatal per
			// gRPC framing.
			if endStream && len(leftover) == 0 && acc.inProgress() {
				return FrameHeader{}, nil, errors.New("h2: END_STREAM with incomplete LPM in pendingFrame replay")
			}
			// Otherwise fall through to read the next H2 DATA frame.
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

		// RFC 7540 §6.x control-frame validation. Payload is already
		// drained above; surfacing the error here leaves the ring read
		// pointer in sync.
		if verr := validateH2ControlFrame(h2fh); verr != nil {
			return FrameHeader{}, nil, verr
		}

		// Translate H2 → internal frame model.
		switch h2fh.Type {
		case H2FrameSETTINGS, H2FramePRIORITY, H2FramePUSHPROMISE:
			// Skipped at this layer.
			continue
		case H2FrameCONTINUATION:
			// CONTINUATION reaching the top-level switch is a stray
			// fragment outside any HEADERS sequence — RFC 7540 §6.10
			// requires this to be treated as PROTOCOL_ERROR. The
			// in-sequence case is handled inside readH2HeadersWithContinuations
			// where the per-stream HEADERS state is tracked.
			return FrameHeader{}, nil, errors.New("h2 CONTINUATION frame received outside a HEADERS sequence (RFC 7540 §6.10)")
		case H2FrameDATA:
			// Credit HTTP/2 flow control for the on-wire DATA bytes
			// at frame-receipt time, decoupled from LPM reassembly.
			// Mirrors http2_client.handleData. The bytes have already
			// been committed from the ring above; we credit BEFORE
			// branching into LPM accumulator / ZC paths so that
			// multi-DATA-frame LPMs don't starve the producer of
			// window updates while we buffer partial bytes.
			holder.notifyDataFrameConsumed(h2fh.StreamID, h2fh.Length)
			// RFC 7540 §6.1: PADDED DATA carries a 1-byte pad-length
			// prefix and trailing padding. Strip both before LPM parse.
			// gRPC peers don't normally pad, but a standards-compliant
			// H2 sender legally may.
			//
			// PADDED with Length=0 is illegal — the mandatory pad-
			// length prefix can't fit (FRAME_SIZE_ERROR per §6.1).
			// stripDataPadding's len < 1 guard catches this.
			if h2fh.Flags&H2FlagPadded != 0 {
				stripped, serr := stripDataPadding(payload, h2fh.Flags)
				if serr != nil {
					return FrameHeader{}, nil, serr
				}
				payload = stripped
			}
			// Empty DATA + END_STREAM = HALFCLOSE.
			if len(payload) == 0 {
				if h2fh.Flags&H2FlagEndStream != 0 {
					// If the per-stream accumulator is mid-message,
					// END_STREAM here truncates the LPM. Per gRPC
					// framing this is a connection-fatal protocol
					// error; silently dropping the partial bytes
					// would corrupt the application's view of the
					// stream.
					if acc, ok := holder.lpmAccumulators[h2fh.StreamID]; ok && acc.inProgress() {
						holder.removeLpmAccumulator(h2fh.StreamID)
						return FrameHeader{}, nil, errors.New("h2: END_STREAM on empty DATA with incomplete LPM in accumulator")
					}
					// Drop any per-stream accumulator so a long-lived
					// connection doesn't leak map entries when streams
					// end via empty-DATA+END_STREAM (the canonical
					// shape our own writer emits via translateCustomToH2).
					holder.removeLpmAccumulator(h2fh.StreamID)
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
					// Set MORE flag based on END_STREAM:
					// MORE=0 signals client half-close on the server
					// transport (handleMessage uses MORE=0 to write
					// io.EOF); peer-set END_STREAM on a DATA carrying
					// exactly one complete LPM is the canonical
					// "last message" shape from grpc-go HTTP/2,
					// grpc-java, grpc-c++.
					msgFlags := MessageFlagMORE
					if h2fh.Flags&H2FlagEndStream != 0 {
						msgFlags = 0
						holder.removeLpmAccumulator(h2fh.StreamID)
					}
					return FrameHeader{
						Type:     FrameTypeMESSAGE,
						StreamID: h2fh.StreamID,
						Length:   uint32(len(payload)),
						Flags:    msgFlags,
					}, payload, nil
				}
			}

			// Multi-frame / multi-message path: feed the accumulator.
			data := payload
			for len(data) > 0 {
				msg, leftover, ferr := acc.feed(data, h2MaxLPMBodyBytes)
				if ferr != nil {
					return FrameHeader{}, nil, ferr
				}
				if msg != nil {
					// Stash any leftover (start of next LPM in the same
					// DATA frame) for the next iteration of the outer
					// readFrameH2 loop. Carry END_STREAM forward to the
					// replay path so a multi-LPM DATA frame ending the
					// stream emits MORE=0 after the LAST LPM, not the
					// first one.
					msgFlags := MessageFlagMORE
					if len(leftover) > 0 {
						holder.pendingFrame = leftover
						holder.pendingStreamID = h2fh.StreamID
						holder.pendingFrameEndStream = h2fh.Flags&H2FlagEndStream != 0
					} else if h2fh.Flags&H2FlagEndStream != 0 {
						// Last LPM of the END_STREAM-bearing DATA
						// frame: signal client half-close to the
						// upper transport via MORE=0.
						msgFlags = 0
						holder.removeLpmAccumulator(h2fh.StreamID)
					}
					return FrameHeader{
						Type:     FrameTypeMESSAGE,
						StreamID: h2fh.StreamID,
						Length:   uint32(len(msg)),
						Flags:    msgFlags,
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
			// HPACK-decode and convert to the internal HeadersV1 / TrailersV1
			// payload format the rest of the transport expects.
			//
			// RFC 7540 §6.2: PADDED and PRIORITY flags add a 1-byte
			// pad-length prefix and a 5-byte priority prefix to the
			// HEADERS body before the HPACK fragment, plus trailing
			// padding bytes. Strip these before HPACK decode. gRPC
			// peers don't typically set these flags, but
			// standards-compliant senders may, and the H2 spec requires
			// receivers to handle them.
			//
			// If END_HEADERS is missing, assemble the full HPACK block
			// from subsequent CONTINUATION frames before decoding (RFC
			// 7540 §6.10). The single-fragment fast path (END_HEADERS
			// already set) is the overwhelmingly common case in gRPC;
			// the slow path triggers only when a peer's HEADERS payload
			// exceeds SETTINGS_MAX_FRAME_SIZE. Padding/priority is
			// only carried on the FIRST fragment so we strip it here
			// before passing to the CONTINUATION assembler.
			hpackBlock := payload
			if h2fh.Flags&(H2FlagPadded|H2FlagPriority) != 0 {
				stripped, serr := stripHeadersPaddingAndPriority(hpackBlock, h2fh.Flags)
				if serr != nil {
					return FrameHeader{}, nil, serr
				}
				hpackBlock = stripped
			}
			if h2fh.Flags&H2FlagEndHeaders == 0 {
				hpackBlock, err = readH2HeadersWithContinuations(ctx, rx, h2fh.StreamID, hpackBlock)
				if err != nil {
					return FrameHeader{}, nil, err
				}
			} else if len(hpackBlock) > h2MaxHeaderListSize {
				// Single-fragment HEADERS: the multi-fragment path enforces
				// this cap inside readH2HeadersWithContinuations, but a peer
				// can also fit up to (2^24-1) bytes in a single HEADERS frame
				// when END_HEADERS is set. Apply the same h2MaxHeaderListSize
				// bound here so the DoS guard documented on that constant
				// holds for both paths.
				return FrameHeader{}, nil, fmt.Errorf(
					"h2 HEADERS payload exceeds %d bytes (got %d)",
					h2MaxHeaderListSize, len(hpackBlock))
			}
			h, t, isTrailers, derr := h2DecodeHeaders(holder, hpackBlock)
			if derr != nil {
				return FrameHeader{}, nil, derr
			}
			if isTrailers {
				// TRAILERS ends the stream. If the per-stream
				// accumulator is mid-message the request body was
				// truncated mid-LPM; surface a connection-fatal
				// error rather than silently dropping the partial
				// bytes.
				if acc, ok := holder.lpmAccumulators[h2fh.StreamID]; ok && acc.inProgress() {
					holder.removeLpmAccumulator(h2fh.StreamID)
					return FrameHeader{}, nil, errors.New("h2: TRAILERS with incomplete LPM in accumulator")
				}
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
			// Initial HEADERS may carry END_STREAM (zero-message
			// client stream). Defer a synthetic HALFCLOSE so the
			// next read fires the upper-layer client-half-close
			// path; otherwise the stream would be created by
			// ShmServerTransport.handleHeaders and then hang
			// waiting for a MESSAGE that never arrives.
			if h2fh.Flags&H2FlagEndStream != 0 {
				holder.pendingHalfCloseStreamID = h2fh.StreamID
			}
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
			if h2fh.Type == H2FrameWINDOWUPDATE {
				// RFC 7540 §6.9.1: increment MUST be non-zero. The
				// 4-byte payload validation above (length == 4) guarantees
				// the slice has the bytes to read.
				inc := binary.BigEndian.Uint32(payload) & 0x7FFFFFFF
				if inc == 0 {
					return FrameHeader{}, nil, errors.New("h2 WINDOW_UPDATE increment must be non-zero")
				}
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
// the upstream readFrameView contract. HEADERS, multi-frame DATA, and other small
// frames go through the copy path.
//
// ZC eligibility:
//   - DATA frame with one complete LPM in body (body == 5+lpmLen)
//   - lpmAccumulator is empty (no in-progress chain)
//   - body slice contiguous in the ring (no wrap)
//   - rx.IsSpeculativeZCEligible (ring large enough, payload large
//     enough, at-most-one-ZC, < 75% full)
func readFrameViewH2(ctx context.Context, rx *ShmRing, holder *hpackDecoderHolder) (FrameHeader, mem.Buffer, error) {
	for {
		// Synthetic HALFCLOSE deferred from an earlier HEADERS+
		// END_STREAM read. See readFrameH2's matching block.
		if holder.pendingHalfCloseStreamID != 0 {
			sid := holder.pendingHalfCloseStreamID
			holder.pendingHalfCloseStreamID = 0
			holder.removeLpmAccumulator(sid)
			return FrameHeader{
				Type: FrameTypeHALFCLOSE, StreamID: sid,
			}, nil, nil
		}

		// Drain any pending leftover from a previous DATA frame first.
		// These bytes already lived in heap-allocated form (pendingFrame
		// is a copy, not a ring slice), so ZC isn't applicable here.
		if len(holder.pendingFrame) > 0 {
			sid := holder.pendingStreamID
			data := holder.pendingFrame
			endStream := holder.pendingFrameEndStream
			holder.pendingFrame = nil
			holder.pendingFrameEndStream = false
			acc := holder.getLpmAccumulator(sid)
			msg, leftover, ferr := acc.feed(data, h2MaxLPMBodyBytes)
			if ferr != nil {
				return FrameHeader{}, nil, ferr
			}
			if len(leftover) > 0 {
				holder.pendingFrame = leftover
				holder.pendingStreamID = sid
				holder.pendingFrameEndStream = endStream
			}
			if msg != nil {
				atomic.AddUint64(&shmAccReadFire, 1)
				// MORE flag: see readFrameH2's matching block.
				msgFlags := MessageFlagMORE
				if endStream && len(leftover) == 0 {
					msgFlags = 0
					holder.removeLpmAccumulator(sid)
				}
				// acc.buf is the exact LPM size, allocated from acc.pool;
				// wrap via mem.NewBuffer(&msg, acc.pool) so Buffer.Free()
				// returns the slice to the pool for reuse on the next RPC,
				// avoiding the runtime.memclr cost of a fresh make().
				return FrameHeader{
					Type:     FrameTypeMESSAGE,
					StreamID: sid,
					Length:   uint32(len(msg)),
					Flags:    msgFlags,
				}, mem.NewBuffer(&msg, acc.pool), nil
			}
			// Truncated LPM at end of stream: connection-fatal.
			if endStream && len(leftover) == 0 && acc.inProgress() {
				return FrameHeader{}, nil, errors.New("h2: END_STREAM with incomplete LPM in pendingFrame replay")
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

		// RFC 7540 §6.x control-frame validation. We must commit any
		// reserved payload bytes before propagating the error so the
		// ring read pointer stays in sync with the cross-process
		// writer's view.
		if verr := validateH2ControlFrame(h2fh); verr != nil {
			if commitPayload != nil {
				commitPayload.Commit(int(h2fh.Length))
			}
			return FrameHeader{}, nil, verr
		}

		switch h2fh.Type {
		case H2FrameSETTINGS, H2FramePRIORITY, H2FramePUSHPROMISE:
			if commitPayload != nil {
				commitPayload.Commit(int(h2fh.Length))
			}
			continue
		case H2FrameCONTINUATION:
			// Stray CONTINUATION outside a HEADERS sequence — RFC 7540
			// §6.10: PROTOCOL_ERROR. In-sequence CONTINUATION is
			// consumed inside readH2HeadersWithContinuations.
			if commitPayload != nil {
				commitPayload.Commit(int(h2fh.Length))
			}
			return FrameHeader{}, nil, errors.New("h2 CONTINUATION frame received outside a HEADERS sequence (RFC 7540 §6.10)")

		case H2FrameDATA:
			// Credit HTTP/2 flow control for the on-wire DATA bytes
			// at frame-receipt time, decoupled from LPM reassembly.
			// Mirrors http2_client.handleData. The bytes have not been
			// committed from the ring yet at this point (the various
			// sub-paths below commit lazily), but the peer charged
			// their send window the moment they wrote the DATA frame
			// to the wire so we MUST credit it back ASAP — otherwise a
			// multi-DATA-frame LPM whose total size exceeds the
			// peer's per-stream window deadlocks (peer waits for
			// WINDOW_UPDATE; codec waits for the rest of the LPM
			// before it would have surfaced handleMessage which is
			// where the legacy per-LPM credit happened).
			//
			// h2fh.Length is the on-wire payload size including any
			// PADDED bytes; we credit the full on-wire size because
			// that is what was charged against the peer's window.
			holder.notifyDataFrameConsumed(h2fh.StreamID, h2fh.Length)
			// PADDED with Length=0 is illegal: the mandatory 1-byte
			// pad-length prefix can't fit. Per RFC 7540 §6.1
			// FRAME_SIZE_ERROR. Check this BEFORE the empty-DATA
			// HALFCLOSE shortcut below — otherwise a malformed
			// PADDED|END_STREAM frame would surface as a valid
			// half-close.
			if h2fh.Flags&H2FlagPadded != 0 && h2fh.Length == 0 {
				return FrameHeader{}, nil, errors.New("h2 DATA PADDED with empty payload (RFC 7540 §6.1 FRAME_SIZE_ERROR)")
			}
			if h2fh.Length == 0 {
				if h2fh.Flags&H2FlagEndStream != 0 {
					if acc, ok := holder.lpmAccumulators[h2fh.StreamID]; ok && acc.inProgress() {
						holder.removeLpmAccumulator(h2fh.StreamID)
						return FrameHeader{}, nil, errors.New("h2: END_STREAM on empty DATA with incomplete LPM in accumulator")
					}
					// Drop any per-stream accumulator (see matching
					// branch in readFrameH2).
					holder.removeLpmAccumulator(h2fh.StreamID)
					return FrameHeader{
						Type: FrameTypeHALFCLOSE, StreamID: h2fh.StreamID,
					}, nil, nil
				}
				continue
			}

			acc := holder.getLpmAccumulator(h2fh.StreamID)
			payloadLen := int(h2fh.Length)

			// PADDED DATA frames carry a 1-byte pad-length prefix and
			// trailing padding (RFC 7540 §6.1). The ZC / single-frame
			// fast paths below assume the ring slice IS the LPM body;
			// with padding present the LPM length match and ring-slice
			// bounds would be wrong. When PADDED is set, fall through
			// directly to the heap-copy multi-frame path which applies
			// stripDataPadding after copy. gRPC peers don't send
			// PADDED in practice, so the fast-path skip costs nothing
			// in the common case.
			isPadded := h2fh.Flags&H2FlagPadded != 0
			if !isPadded {

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
				// 5-byte prefix.
				if !acc.inProgress() && len(pSecond) == 0 && len(pFirst) >= 5 {
					bodyLen := int(binary.BigEndian.Uint32(pFirst[1:5]))
					if 5+bodyLen == payloadLen && rx.IsSpeculativeZCEligible(payloadLen, true) {
						atomic.AddUint64(&shmZCReadFire, 1)
						// Arm the ZC anchor with the post-frame target, then
						// don't call commitPayload.Commit — the deferred
						// target already accounts for these bytes.
						baseIdx := commitPayload.commitReadIdx
						rx.BeginSingleFrameZcCommit(baseIdx, payloadLen)
						rx.AddChainZcInFlight()

						// Set MORE flag based on END_STREAM. MORE=0
						// signals client half-close to the server
						// transport (ShmServerTransport.handleMessage
						// uses MORE=0 to write io.EOF). Required for
						// cross-impl interop with grpc-go HTTP/2,
						// grpc-java, grpc-c++ which set END_STREAM on
						// the last DATA frame carrying body bytes.
						msgFlags := MessageFlagMORE
						if h2fh.Flags&H2FlagEndStream != 0 {
							msgFlags = 0
							holder.removeLpmAccumulator(h2fh.StreamID)
						}

						ringSlice := pFirst[:payloadLen:payloadLen]
						pool := &zcChainReleasePool{ring: rx}
						buf := mem.NewBuffer(&ringSlice, pool)
						return FrameHeader{
							Type:     FrameTypeMESSAGE,
							StreamID: h2fh.StreamID,
							Length:   uint32(payloadLen),
							Flags:    msgFlags,
						}, buf, nil
					}
				}

				// === Single-frame copy fast path ===
				//
				// Body fits entirely in this DATA frame and contains exactly
				// one complete LPM (5+bodyLen == payloadLen). Copy directly
				// to a pool buffer, bypassing the lpmAccumulator. Without
				// this path, the accumulator allocates `make([]byte,
				// payloadLen)` for the heap copy AND its own `acc.buf` then
				// appends into it — two allocs + two memcpys per frame.
				// Single mem.Copy gives us one alloc + one memcpy, matching
				// readFrameView parity.
				//
				// Pool choice: shmLpmPool (dirty, no memclr-on-Get) instead
				// of mem.DefaultBufferPool. The default pool zero-fills
				// every returned buffer; under 1000-stream concurrent
				// ping-pong at 64 KiB the cumulative memclr dominates the
				// CPU profile (~40% of total cycles, per WSL EPYC pprof)
				// and crushes shm-tuned single-frame throughput. The
				// accumulator path (used for chunked frames) was already
				// on the dirty pool; bringing the single-frame copy path
				// here onto the same pool gives both code paths matching
				// allocation cost.
				//
				// Reads the 5-byte LPM header from the (possibly split) ring
				// slice via a small stack array so the fast path applies
				// even when the body wraps.
				if !acc.inProgress() && len(pFirst)+len(pSecond) == payloadLen && payloadLen >= 5 {
					var hdr [5]byte
					n := copy(hdr[:], pFirst)
					if n < 5 {
						copy(hdr[n:], pSecond)
					}
					bodyLen := int(binary.BigEndian.Uint32(hdr[1:5]))
					if 5+bodyLen == payloadLen {
						atomic.AddUint64(&shmCopyReadFire, 1)
						var buf mem.Buffer
						if len(pSecond) == 0 {
							buf = mem.Copy(pFirst[:payloadLen], shmLpmPool)
						} else {
							poolBuf := shmLpmPool.Get(payloadLen)
							cn := copy(*poolBuf, pFirst)
							copy((*poolBuf)[cn:], pSecond)
							buf = mem.NewBuffer(poolBuf, shmLpmPool)
						}
						commitPayload.Commit(payloadLen)
						// MORE flag based on END_STREAM (see ZC fast
						// path above).
						msgFlags := MessageFlagMORE
						if h2fh.Flags&H2FlagEndStream != 0 {
							msgFlags = 0
							holder.removeLpmAccumulator(h2fh.StreamID)
						}
						return FrameHeader{
							Type:     FrameTypeMESSAGE,
							StreamID: h2fh.StreamID,
							Length:   uint32(payloadLen),
							Flags:    msgFlags,
						}, buf, nil
					}
				}

				// === Multi-frame / multi-LPM path ===
				//
				// Body is a fragment of an in-progress LPM, or contains
				// multiple LPMs. Two sub-paths:
				//
				//  1. Mid-chain fast path: accumulator already in progress
				//     AND this entire DATA frame fits within the remaining
				//     LPM bytes (no completion / new LPM mid-frame). Append
				//     ring slices directly into acc.buf, avoiding the
				//     intermediate `make([]byte, payloadLen) + copy`. This
				//     halves the per-chunk memcpy budget for messages
				//     chunked at cap/8 (e.g., 16 MiB body → 8/8/5 chunks).
				//
				//  2. Slow path: copy ring → heap, run accumulator (handles
				//     LPM completion + leftover bytes that start a new LPM
				//     in the same DATA frame).
				//
				// Both paths return the accumulator's heap buffer wrapped
				// via mem.NewBuffer(&msg, acc.pool); the wrapping pool is
				// the same one acc.buf was allocated from, so Buffer.Free()
				// returns the slice to the pool for reuse on the next RPC
				// and avoids the runtime.memclr cost of a fresh make().
				if acc.inProgress() && acc.expectedTotal-acc.pos >= payloadLen {
					// Mid-chain chunk: route through growBufForChunk to
					// pick up the explicit 2× doubling rather than Go's
					// default 1.25× slice growth. Without this, after
					// feedSplit sized acc.buf to the first chunk
					// exactly, every subsequent chunk would trigger a
					// realloc + memcpy of the entire accumulated body
					// (a 64 MiB / 4-chunk LPM costs ~80 MiB of wasted
					// grow-copy under default append growth).
					if acc.pos+payloadLen > cap(acc.buf) {
						acc.growBufForChunk(payloadLen)
					}
					acc.buf = append(acc.buf, pFirst...)
					if len(pSecond) > 0 {
						acc.buf = append(acc.buf, pSecond...)
					}
					acc.pos += payloadLen
					commitPayload.Commit(payloadLen)
					if acc.pos != acc.expectedTotal {
						if h2fh.Flags&H2FlagEndStream != 0 {
							return FrameHeader{}, nil, errors.New("h2: END_STREAM with incomplete LPM in accumulator")
						}
						continue
					}
					msg := acc.buf
					acc.headerBytesSeen = 0
					acc.expectedTotal = 0
					acc.pos = 0
					acc.buf = nil
					// Mid-chain accumulator complete: this chunk fully
					// finished the in-progress LPM AND fit within
					// remaining bytes (no leftover possible by
					// definition). Set MORE flag based on END_STREAM.
					msgFlags := MessageFlagMORE
					if h2fh.Flags&H2FlagEndStream != 0 {
						msgFlags = 0
						holder.removeLpmAccumulator(h2fh.StreamID)
					}
					return FrameHeader{
						Type:     FrameTypeMESSAGE,
						StreamID: h2fh.StreamID,
						Length:   uint32(len(msg)),
						Flags:    msgFlags,
					}, mem.NewBuffer(&msg, acc.pool), nil
				}
			} // end isPadded fast-path skip

			// Non-padded fast path: feed ring slices into the
			// accumulator directly, skipping the intermediate
			// `make([]byte, h2fh.Length)` allocation + ring→heap copy
			// that the padded path below still requires (padding is
			// stripped in-place on the heap copy). For large
			// multi-frame LPMs (e.g., the first 16 MiB chunk of a
			// 64 MiB MESSAGE) this saves one 16 MiB allocation and
			// one 16 MiB memcpy per first chunk — material at the
			// upper end of the size range.
			if h2fh.Flags&H2FlagPadded == 0 {
				msg, leftover, ferr := acc.feedSplit(pFirst, pSecond, h2MaxLPMBodyBytes)
				// leftover may alias ring memory (pFirst/pSecond) when
				// feedSplit consumes only one source slice. Copying to a
				// heap-owned buffer BEFORE Commit prevents the writer
				// (cross-process or another goroutine on the same ring)
				// from overwriting the bytes between this call and the
				// next readFrameViewH2 invocation that replays them.
				// Without this copy, a multi-LPM DATA frame can corrupt
				// subsequent messages under ring reuse.
				if len(leftover) > 0 {
					leftover = append([]byte(nil), leftover...)
				}
				commitPayload.Commit(int(h2fh.Length))
				if ferr != nil {
					return FrameHeader{}, nil, ferr
				}
				if msg != nil {
					atomic.AddUint64(&shmAccReadFire, 1)
					msgFlags := MessageFlagMORE
					if len(leftover) > 0 {
						holder.pendingFrame = leftover
						holder.pendingStreamID = h2fh.StreamID
						holder.pendingFrameEndStream = h2fh.Flags&H2FlagEndStream != 0
					} else if h2fh.Flags&H2FlagEndStream != 0 {
						msgFlags = 0
						holder.removeLpmAccumulator(h2fh.StreamID)
					}
					return FrameHeader{
						Type:     FrameTypeMESSAGE,
						StreamID: h2fh.StreamID,
						Length:   uint32(len(msg)),
						Flags:    msgFlags,
					}, mem.NewBuffer(&msg, acc.pool), nil
				}
				if h2fh.Flags&H2FlagEndStream != 0 && acc.inProgress() {
					return FrameHeader{}, nil, errors.New("h2: END_STREAM with incomplete LPM in accumulator")
				}
				continue
			}

			// Padded path: heap copy required to strip padding bytes
			// (the 1-byte pad-length prefix + trailing padding can't
			// be stripped while data lives in ring slices that may
			// straddle the wrap boundary).
			payload := make([]byte, h2fh.Length)
			cn := copy(payload, pFirst)
			if cn < int(h2fh.Length) && len(pSecond) > 0 {
				copy(payload[cn:], pSecond)
			}
			commitPayload.Commit(int(h2fh.Length))

			// RFC 7540 §6.1: PADDED DATA. Strip after commit so the
			// ring read pointer is in sync; data slice is heap-owned.
			stripped, serr := stripDataPadding(payload, h2fh.Flags)
			if serr != nil {
				return FrameHeader{}, nil, serr
			}
			payload = stripped

			data := payload
			for len(data) > 0 {
				msg, leftover, ferr := acc.feed(data, h2MaxLPMBodyBytes)
				if ferr != nil {
					return FrameHeader{}, nil, ferr
				}
				if msg != nil {
					atomic.AddUint64(&shmAccReadFire, 1)
					msgFlags := MessageFlagMORE
					if len(leftover) > 0 {
						holder.pendingFrame = leftover
						holder.pendingStreamID = h2fh.StreamID
						holder.pendingFrameEndStream = h2fh.Flags&H2FlagEndStream != 0
					} else if h2fh.Flags&H2FlagEndStream != 0 {
						msgFlags = 0
						holder.removeLpmAccumulator(h2fh.StreamID)
					}
					return FrameHeader{
						Type:     FrameTypeMESSAGE,
						StreamID: h2fh.StreamID,
						Length:   uint32(len(msg)),
						Flags:    msgFlags,
					}, mem.NewBuffer(&msg, acc.pool), nil
				}
				break
			}
			if h2fh.Flags&H2FlagEndStream != 0 && acc.inProgress() {
				return FrameHeader{}, nil, errors.New("h2: END_STREAM with incomplete LPM in accumulator")
			}
			continue

		case H2FrameHEADERS:
			// Decode HPACK directly from ring memory when contiguous —
			// hpack.Decoder.Write+Close consume the slice synchronously
			// (the emit callback already deep-copies any retained bytes
			// via `append([]byte(nil), hf.Value...)`), so no ring slice
			// is held past Close. Skips one ring→scratch memcpy per
			// HEADERS frame; HEADERS payloads can be sizeable when
			// metadata is rich.
			//
			// Wrap-around case still uses scratch to consolidate.
			//
			// END_HEADERS missing → assemble a heap-backed HPACK block
			// across CONTINUATION frames (RFC 7540 §6.10) before
			// decoding. Slow path; common case is END_HEADERS already
			// set on the first fragment.
			var payload []byte
			if h2fh.Length > 0 {
				if len(pSecond) == 0 {
					payload = pFirst[:h2fh.Length]
				} else {
					payload = holder.scratchBytes(int(h2fh.Length))
					cn := copy(payload, pFirst)
					copy(payload[cn:], pSecond)
				}
			}
			// RFC 7540 §6.2: PADDED / PRIORITY flags add prefix bytes
			// and trailing padding. Strip before HPACK decode.
			if h2fh.Flags&(H2FlagPadded|H2FlagPriority) != 0 {
				stripped, serr := stripHeadersPaddingAndPriority(payload, h2fh.Flags)
				if serr != nil {
					if commitPayload != nil {
						commitPayload.Commit(int(h2fh.Length))
					}
					return FrameHeader{}, nil, serr
				}
				payload = stripped
			}
			if h2fh.Flags&H2FlagEndHeaders == 0 {
				// Materialize first fragment (we may have aliased ring
				// memory) before draining; commit the first fragment
				// payload, then read CONTINUATION frames.
				firstCopy := append([]byte(nil), payload...)
				if commitPayload != nil {
					commitPayload.Commit(int(h2fh.Length))
				}
				// Mark commitPayload nil so the post-decode commit
				// below is skipped.
				commitPayload = nil
				assembled, aerr := readH2HeadersWithContinuations(ctx, rx, h2fh.StreamID, firstCopy)
				if aerr != nil {
					return FrameHeader{}, nil, aerr
				}
				payload = assembled
			} else if len(payload) > h2MaxHeaderListSize {
				// Single-fragment HEADERS: the multi-fragment path
				// enforces this cap inside readH2HeadersWithContinuations,
				// but a peer can also fit up to (2^24-1) bytes in a single
				// HEADERS frame when END_HEADERS is set. Apply the same
				// h2MaxHeaderListSize bound here so the DoS guard
				// documented on that constant holds for both paths.
				if commitPayload != nil {
					commitPayload.Commit(int(h2fh.Length))
				}
				return FrameHeader{}, nil, fmt.Errorf(
					"h2 HEADERS payload exceeds %d bytes (got %d)",
					h2MaxHeaderListSize, len(payload))
			}
			h, t, isTrailers, derr := h2DecodeHeaders(holder, payload)
			if commitPayload != nil {
				commitPayload.Commit(int(h2fh.Length))
			}
			if derr != nil {
				return FrameHeader{}, nil, derr
			}
			if isTrailers {
				if acc, ok := holder.lpmAccumulators[h2fh.StreamID]; ok && acc.inProgress() {
					holder.removeLpmAccumulator(h2fh.StreamID)
					return FrameHeader{}, nil, errors.New("h2: TRAILERS with incomplete LPM in accumulator")
				}
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
			// Initial HEADERS may carry END_STREAM (zero-message
			// client stream). Defer a synthetic HALFCLOSE; see the
			// matching block in readFrameH2.
			if h2fh.Flags&H2FlagEndStream != 0 {
				holder.pendingHalfCloseStreamID = h2fh.StreamID
			}
			return FrameHeader{
				Type:     FrameTypeHEADERS,
				StreamID: h2fh.StreamID,
				Length:   uint32(len(out)),
				Flags:    HeadersFlagINITIAL,
			}, mem.Copy(out, mem.DefaultBufferPool()), nil

		case H2FrameRSTSTREAM:
			// mem.Copy directly from the ring when contiguous — saves
			// the ring→scratch then scratch→pool double-copy that the
			// previous code did via holder.scratchBytes().
			var buf mem.Buffer
			if h2fh.Length > 0 {
				if len(pSecond) == 0 {
					buf = mem.Copy(pFirst[:h2fh.Length], mem.DefaultBufferPool())
				} else {
					payload := holder.scratchBytes(int(h2fh.Length))
					cn := copy(payload, pFirst)
					copy(payload[cn:], pSecond)
					buf = mem.Copy(payload, mem.DefaultBufferPool())
				}
				commitPayload.Commit(int(h2fh.Length))
			}
			holder.removeLpmAccumulator(h2fh.StreamID)
			return FrameHeader{
				Type:     FrameTypeCANCEL,
				StreamID: h2fh.StreamID,
				Length:   h2fh.Length,
			}, buf, nil

		case H2FrameGOAWAY, H2FramePING, H2FrameWINDOWUPDATE:
			ft, fl, ok := translateH2ToCustom(h2fh.Type, h2fh.Flags)
			if !ok {
				if commitPayload != nil {
					commitPayload.Commit(int(h2fh.Length))
				}
				continue
			}
			// RFC 7540 §6.9.1: WINDOW_UPDATE increment MUST be non-zero.
			// validateH2ControlFrame already enforced length == 4.
			if h2fh.Type == H2FrameWINDOWUPDATE {
				var hdr [4]byte
				cn := copy(hdr[:], pFirst)
				if cn < 4 && len(pSecond) > 0 {
					copy(hdr[cn:], pSecond)
				}
				inc := binary.BigEndian.Uint32(hdr[:]) & 0x7FFFFFFF
				if inc == 0 {
					commitPayload.Commit(int(h2fh.Length))
					return FrameHeader{}, nil, errors.New("h2 WINDOW_UPDATE increment must be non-zero")
				}
			}
			var buf mem.Buffer
			if h2fh.Length > 0 {
				if len(pSecond) == 0 {
					buf = mem.Copy(pFirst[:h2fh.Length], mem.DefaultBufferPool())
				} else {
					payload := holder.scratchBytes(int(h2fh.Length))
					cn := copy(payload, pFirst)
					copy(payload[cn:], pSecond)
					buf = mem.Copy(payload, mem.DefaultBufferPool())
				}
				commitPayload.Commit(int(h2fh.Length))
			}
			return FrameHeader{
				Type:     ft,
				StreamID: h2fh.StreamID,
				Length:   h2fh.Length,
				Flags:    fl,
			}, buf, nil

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

	// MESSAGE payloads are split into multiple DATA frames in three cases:
	//
	//  (1) Protocol limit: the payload exceeds shmMaxFrameSize. By
	//      default this equals h2MaxFramePayload (RFC ceiling); under
	//      a fair-comparison bench profile that matches HTTP/2's
	//      spec default of 16384 B, this knob is what makes SHM emit
	//      the same DATA frame cadence as TCP / UDS.
	//
	//  (2) Ring capacity: the payload plus the 9-byte H2 header doesn't
	//      fit in the ring's reserve-write budget. ReserveWrite rejects
	//      requests larger than ring capacity; without chunking,
	//      messages exceeding ring capacity would fail to send. Per
	//      gRFC G3 ring framing, a frame larger than ring capacity is
	//      well-formed and must be transported incrementally. The
	//      reader's lpmAccumulator reassembles the chunks.
	if fh.Type == FrameTypeMESSAGE &&
		(len(h2payload) > shmMaxFrameSize ||
			uint64(h2FrameHeaderSize+len(h2payload)) > tx.Capacity()) {
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
// Chunk size is bounded by all three of: shmMaxFrameSize (the
// configurable per-DATA-frame ceiling, default h2MaxFramePayload),
// h2MaxFramePayload (RFC 7540 §4.2 absolute limit) and the ring
// capacity / 4 — the latter ensures the writer can always place the
// next chunk while the reader is still consuming the previous,
// avoiding stall under back-pressure.
func writeFrameH2DataChunked(ctx context.Context, tx *ShmRing, streamID uint32, body []byte, baseFlags byte) error {
	atomic.AddUint64(&shmChunkedWriteFire, 1)
	maxChunk := shmMaxFrameSize
	if maxChunk > h2MaxFramePayload {
		maxChunk = h2MaxFramePayload
	}
	if uint64(maxChunk) > tx.Capacity()/4 {
		maxChunk = int(tx.Capacity() / 4)
	}
	if maxChunk == 0 {
		return fmt.Errorf("h2 chunk: ring capacity %d too small to chunk", tx.Capacity())
	}
	// Signal-batch threshold: see emitH2DataFromCursor for design.
	// Mirrors .NET's RingFrameStream cap/8 chunkSize -- enables
	// producer / consumer pipelining when the message exceeds ring
	// capacity instead of locking into a "fill -> drain -> fill"
	// sequential pattern.
	signalBatch := int(tx.Capacity() / 8)
	if signalBatch < maxChunk {
		signalBatch = maxChunk
	}

	multi := len(body) > maxChunk
	batchOpen := false
	batchBytes := 0
	closeBatch := func() {
		if batchOpen {
			tx.EndBatch()
			batchOpen = false
			batchBytes = 0
		}
	}

	for off := 0; off < len(body); off += maxChunk {
		end := off + maxChunk
		if end > len(body) {
			end = len(body)
		}
		isLast := end == len(body)

		if multi && !batchOpen && !isLast {
			tx.BeginBatch()
			batchOpen = true
			batchBytes = 0
		}

		// Don't propagate END_STREAM to intermediate chunks — only the
		// last one (if baseFlags carries it). gRPC SHM never sets
		// END_STREAM on MESSAGE; TRAILERS ends the stream.
		flags := byte(0)
		if isLast {
			flags = baseFlags
		}
		if err := writeH2Single(ctx, tx, H2FrameDATA, flags, streamID, body[off:end]); err != nil {
			closeBatch()
			return err
		}
		batchBytes += end - off

		if batchOpen && (batchBytes >= signalBatch || isLast) {
			closeBatch()
		}
	}
	return nil
}

// writeFrameH2DataChunkedVec is the vectored cousin of
// writeFrameH2DataChunked. It emits a multi-DATA-frame MESSAGE directly
// from the (lpmHdr + mem.BufferSlice) inputs without first materialising
// them into one contiguous heap buffer. Each chunk reserves
// shmMaxFrameSize+9 bytes in the ring and copies straight from the
// source segments via ringSegWriter, saving one full producer-side
// memcpy of body bytes per RPC.
//
// Constraints mirror writeFrameH2DataChunked: chunk size capped by
// shmMaxFrameSize, h2MaxFramePayload, and tx.Capacity()/4. END_STREAM
// flows only onto the final chunk.
func writeFrameH2DataChunkedVec(
	ctx context.Context,
	tx *ShmRing,
	streamID uint32,
	lpmHdr []byte,
	data mem.BufferSlice,
	baseFlags byte,
) error {
	// vecCursor walks the (lpmHdr || data segments) virtual byte
	// stream a chunk at a time without merging into one slice. Each
	// chunk consumes from the current segment, advancing across
	// segment boundaries automatically.
	cur := vecCursor{lpmHdr: lpmHdr, data: data}
	return emitH2DataFromCursor(ctx, tx, streamID, &cur, len(lpmHdr)+data.Len(), baseFlags)
}

// emitH2DataFromCursor emits `length` bytes from cur into the ring as
// one or more H2 DATA frames (chunked per shmMaxFrameSize / ring
// capacity). baseFlags is applied to the FINAL emitted DATA frame only
// (typically used to carry END_STREAM on the last chunk of a logical
// MESSAGE). The cursor is advanced by exactly `length` bytes on
// success; on error the cursor's position is undefined and the caller
// must not reuse it for further emits.
//
// Exposed as a primitive so callers that already hold a vecCursor
// straddling multiple flow-control / window-grant chunks can keep
// emitting from the same cursor across iterations without
// materialising the source into one contiguous buffer. The slow-path
// chunked client write (shm_client_transport.go) uses this to skip a
// 16-MB-class producer memcpy under fair-default.
//
// Pipelining design: emits are grouped into signal-batches of
// ring.Capacity()/8 bytes each (matching .NET's RingFrameStream
// chunkSize). Within a signal-batch, per-chunk Commit's wake is
// suppressed via BeginBatch so the reader gets one wake per ~ring/8
// of progress instead of one per H2 DATA frame. Between batches the
// writer pauses to EndBatch (which fires the wake) and immediately
// re-opens a new batch for the next group. This gives ~8 reader-wake
// points per ring traversal -- enough to keep the consumer overlapping
// with the producer when the message exceeds ring capacity, but
// coarse enough to amortise the futex cost over many H2 frames.
//
// The pre-pipelining code wrapped the entire `length` of bytes in a
// single BeginBatch / EndBatch pair. That collapsed throughput by 2-3x
// for messages >= ring capacity because the reader could not start
// draining until the producer finished the whole logical MESSAGE.
// BenchmarkGRPCShmLargeUnary/size=64MB on a 64-MiB ring went from
// ~650 MB/s (16 MB message, fits in ring) to ~270 MB/s (64 MB message,
// exactly fills ring) under that regime.
func emitH2DataFromCursor(
	ctx context.Context,
	tx *ShmRing,
	streamID uint32,
	cur *vecCursor,
	length int,
	baseFlags byte,
) error {
	atomic.AddUint64(&shmChunkedWriteVecFire, 1)

	maxChunk := shmMaxFrameSize
	if maxChunk > h2MaxFramePayload {
		maxChunk = h2MaxFramePayload
	}
	if uint64(maxChunk) > tx.Capacity()/4 {
		maxChunk = int(tx.Capacity() / 4)
	}
	if maxChunk == 0 {
		return fmt.Errorf("h2 chunk: ring capacity %d too small to chunk", tx.Capacity())
	}

	// Signal-batch threshold: how many bytes we let accumulate before
	// EndBatch fires the reader wake. ring/8 mirrors the .NET
	// RingFrameStream design (see grpc-dotnet-shm
	// ShmFrameWriter.WriteInlineDirectMultiFrame). Bound below by
	// maxChunk so a single H2 DATA frame is always emitted under one
	// batch, and above by length so we do not over-promise the
	// caller more pipeline points than there is data for.
	signalBatch := int(tx.Capacity() / 8)
	if signalBatch < maxChunk {
		signalBatch = maxChunk
	}

	multi := length > maxChunk
	batchOpen := false
	batchBytes := 0
	closeBatch := func() {
		if batchOpen {
			tx.EndBatch()
			batchOpen = false
			batchBytes = 0
		}
	}

	for written := 0; written < length; {
		chunk := length - written
		if chunk > maxChunk {
			chunk = maxChunk
		}
		isLast := written+chunk == length

		// Open a fresh signal-batch when we are about to emit more
		// than one frame in this batch's window. Skip the batch when
		// the current chunk is the last one (single Commit fires its
		// own wake anyway).
		if multi && !batchOpen && !isLast {
			tx.BeginBatch()
			batchOpen = true
			batchBytes = 0
		}

		flags := byte(0)
		if isLast {
			flags = baseFlags
		}
		if err := writeH2DataFromCursor(ctx, tx, streamID, flags, chunk, cur); err != nil {
			closeBatch()
			return err
		}
		written += chunk
		batchBytes += chunk

		// Close the batch (firing the reader wake) when we have
		// accumulated enough bytes, or when this was the final chunk.
		// `isLast` covers the case where the final chunk was emitted
		// under a still-open batch.
		if batchOpen && (batchBytes >= signalBatch || isLast) {
			closeBatch()
		}
	}
	return nil
}

// writeH2DataFromCursor reserves one H2 DATA frame's worth of ring
// bytes and writes the 9-byte header plus `payloadLen` bytes pulled
// from cur (across segment boundaries as needed). Used by
// writeFrameH2DataChunkedVec.
func writeH2DataFromCursor(
	ctx context.Context,
	tx *ShmRing,
	streamID uint32,
	h2flags byte,
	payloadLen int,
	cur *vecCursor,
) error {
	total := h2FrameHeaderSize + payloadLen
	res, err := tx.ReserveWrite(ctx, total)
	if err != nil {
		return err
	}
	var hdr [h2FrameHeaderSize]byte
	encodeH2FrameHeaderTo(&hdr, H2FrameHeader{
		Length:   uint32(payloadLen),
		Type:     H2FrameDATA,
		Flags:    h2flags,
		StreamID: streamID,
	})
	rw := ringSegWriter{first: res.First, second: res.Second}
	rw.write(hdr[:])
	if err := cur.writeTo(&rw, payloadLen); err != nil {
		_ = res.Commit(0)
		return err
	}
	if rw.err != nil {
		_ = res.Commit(0)
		return rw.err
	}
	return res.Commit(total)
}

// vecCursor is a forward-only iterator over (lpmHdr || data.segments).
// It feeds writeFrameH2DataChunkedVec's per-chunk emitter without
// materialising the virtual stream into one slice. Total bytes consumed
// must not exceed len(lpmHdr) + data.Len() — callers compute exact
// chunk sizes up-front.
type vecCursor struct {
	lpmHdr []byte // remaining LPM-header bytes; shrinks as consumed
	data   mem.BufferSlice
	segOff int // bytes consumed in data[0]
}

// writeTo copies the next n bytes from the cursor into rw, advancing
// the cursor across segment boundaries as needed.
func (c *vecCursor) writeTo(rw *ringSegWriter, n int) error {
	for n > 0 {
		if len(c.lpmHdr) > 0 {
			take := n
			if take > len(c.lpmHdr) {
				take = len(c.lpmHdr)
			}
			rw.write(c.lpmHdr[:take])
			c.lpmHdr = c.lpmHdr[take:]
			n -= take
			continue
		}
		if len(c.data) == 0 {
			return fmt.Errorf("vecCursor: out of bytes, %d remaining", n)
		}
		seg := c.data[0].ReadOnlyData()
		if c.segOff >= len(seg) {
			c.data = c.data[1:]
			c.segOff = 0
			continue
		}
		avail := len(seg) - c.segOff
		take := n
		if take > avail {
			take = avail
		}
		rw.write(seg[c.segOff : c.segOff+take])
		c.segOff += take
		n -= take
		if c.segOff == len(seg) {
			c.data = c.data[1:]
			c.segOff = 0
		}
	}
	return nil
}

// writeFrameH2Message writes a single H2 DATA frame for a MESSAGE
// whose body is composed of a gRPC LPM 5-byte header prefix plus the
// segments of a mem.BufferSlice. The header, prefix, and each segment
// are copied directly into the ring reservation, eliminating the
// per-send heap allocation that writeFrameBuffers' contiguous
// materialisation would otherwise perform on every gRPC SendMsg.
//
// The caller is responsible for ensuring the body fits in a single H2
// DATA frame (i.e. len(lpmHdr) + data.Len() ≤ h2MaxFramePayload) and
// in the ring (h2FrameHeaderSize + body ≤ ring capacity). The MESSAGE
// frame's flags are translated to H2 END_STREAM via the same rule as
// translateCustomToH2.
func writeFrameH2Message(
	ctx context.Context,
	tx *ShmRing,
	streamID uint32,
	msgFlags uint8,
	lpmHdr []byte,
	data mem.BufferSlice,
) error {
	atomic.AddUint64(&shmVectoredWriteFire, 1)
	bodyLen := len(lpmHdr) + data.Len()
	total := h2FrameHeaderSize + bodyLen

	// Defensive: caller (writeFrameBuffers) checks these, but
	// re-assert for callers that might be added later.
	if bodyLen > h2MaxFramePayload {
		return fmt.Errorf("writeFrameH2Message: body %d exceeds max H2 DATA frame %d", bodyLen, h2MaxFramePayload)
	}
	if uint64(total) > tx.Capacity() {
		return fmt.Errorf("writeFrameH2Message: frame %d exceeds ring capacity %d", total, tx.Capacity())
	}

	res, err := tx.ReserveWrite(ctx, total)
	if err != nil {
		return err
	}

	var h2flags byte
	if msgFlags&MessageFlagEndStream != 0 {
		h2flags = H2FlagEndStream
	}
	var hdr [h2FrameHeaderSize]byte
	encodeH2FrameHeaderTo(&hdr, H2FrameHeader{
		Length:   uint32(bodyLen),
		Type:     H2FrameDATA,
		Flags:    h2flags,
		StreamID: streamID,
	})

	// Sequential writer across the two-slice reservation. The wrap
	// boundary between res.First and res.Second may fall anywhere
	// within the H2 header, LPM header, or any data segment.
	rw := ringSegWriter{first: res.First, second: res.Second}
	rw.write(hdr[:])
	if len(lpmHdr) > 0 {
		rw.write(lpmHdr)
	}
	for _, b := range data {
		seg := b.ReadOnlyData()
		if len(seg) > 0 {
			rw.write(seg)
		}
	}
	if rw.err != nil {
		// Invariant violation: release the reservation without
		// publishing bytes (Commit(0) leaves writeIdx unchanged) so
		// the writer state stays consistent and the caller's
		// frameWriter.inlineMu unlock can run.
		_ = res.Commit(0)
		return rw.err
	}
	return res.Commit(total)
}

// ringSegWriter emits sequential bytes into a two-slice ring
// reservation (res.First then res.Second), straddling the ring
// wrap-around boundary transparently. Used by writeFrameH2Message for
// the vectored MESSAGE-write path that avoids materialising header +
// payload into an intermediate heap buffer.
type ringSegWriter struct {
	first, second []byte
	off           int // total bytes written into the reservation so far
	err           error
}

// write copies src into the reservation, advancing the cursor. On
// invariant violation (sum of writes exceeds the reservation), records
// an error and otherwise no-ops; the caller should inspect w.err
// before Commit. The error path returns rather than panics so the
// caller's deferred mutex unlocks (e.g., frameWriter.inlineMu) still
// run; a panic mid-write would leave inlineMu held forever and freeze
// the transport's writer goroutine. The caller is responsible for
// ensuring the reservation has enough remaining capacity
// (len(first) + len(second) - off ≥ len(src)); writeFrameH2Message
// guarantees this by reserving the exact total up-front. The bounds
// check costs one compare per write call on the cold path (≤ 3 calls
// per MESSAGE: H2 hdr, LPM hdr, segments) so the overhead is
// negligible.
func (w *ringSegWriter) write(src []byte) {
	if w.err != nil {
		return
	}
	if w.off < len(w.first) {
		n := copy(w.first[w.off:], src)
		w.off += n
		src = src[n:]
		if len(src) == 0 {
			return
		}
	}
	pos := w.off - len(w.first)
	if pos+len(src) > len(w.second) {
		// Invariant violation: caller reserved fewer bytes than the
		// sum of segment lengths it then tried to write. Always a bug
		// in writeFrameH2Message (or any future caller); silently
		// truncating would corrupt the ring's next frame. Record the
		// error so writeFrameH2Message returns it to its caller; do
		// NOT panic — that would leak the frameWriter.inlineMu lock
		// the caller holds.
		w.err = fmt.Errorf(
			"ringSegWriter overflow: off=%d, src=%d, len(first)=%d, len(second)=%d",
			w.off, len(src), len(w.first), len(w.second))
		return
	}
	copy(w.second[pos:], src)
	w.off += len(src)
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
// Total = 9 + 5 + pSize. ZC eligibility uses the same heuristic as the
// frame must fit contiguously in the ring without wrap.
func writeProtoToRingH2(ctx context.Context, tx *ShmRing, streamID uint32, msg proto.Message, pSize int, flags uint8) (bool, error) {
	total := h2FrameHeaderSize + 5 + pSize

	// Skip ZC for messages that won't fit in a single frame.
	// cap/3 budget keeps headroom for the chunking-path writer.
	if uint64(total) > tx.Capacity()/3 {
		atomic.AddUint64(&shmZCWriteSkipBudget, 1)
		return false, nil
	}
	if uint64(total) > h2MaxFramePayload+h2FrameHeaderSize {
		// Single H2 DATA frame can't carry more than 16MB-1 of body.
		atomic.AddUint64(&shmZCWriteSkipBudget, 1)
		return false, nil
	}
	// Honour the configurable shmMaxFrameSize too — under a fair-
	// comparison bench profile that sets max frame to 16384, ZC
	// would otherwise emit one giant DATA frame while the codec
	// chunking path emits 16 KiB frames; return false so the
	// caller falls back to writeFrameBuffers which respects the
	// knob via writeFrameH2DataChunked.
	if total > h2FrameHeaderSize+shmMaxFrameSize {
		atomic.AddUint64(&shmZCWriteSkipMaxFrame, 1)
		return false, nil
	}
	// Non-blocking contiguous-space check.
	if tx.ContiguousWriteSpace() < uint64(total) {
		atomic.AddUint64(&shmZCWriteSkipSpace, 1)
		return false, nil
	}

	res, err := tx.ReserveWrite(ctx, total)
	if err != nil {
		return false, err
	}

	// H2 DATA frame header (9 bytes).
	var h2hdr [h2FrameHeaderSize]byte
	// END_STREAM mirrors the caller's logical "last message in my
	// send direction" signal. Only the client side sets
	// MessageFlagEndStream (on the last request message of a
	// client-streaming or unary RPC); the server ends its send
	// direction with a TRAILERS frame, NOT END_STREAM on DATA.
	// Setting END_STREAM on a server response DATA would tell the
	// client peer "no more frames from this side" before the
	// TRAILERS arrived — protocol violation and breaks
	// server-streaming.
	var h2flags byte
	if flags&MessageFlagEndStream != 0 {
		h2flags = H2FlagEndStream
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

	atomic.AddUint64(&shmZCWriteFire, 1)
	return true, res.Commit(total)
}
