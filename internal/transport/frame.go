//go:build linux || windows

/*
 *
 * Copyright 2025 gRPC authors.
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
	"errors"
	"fmt"

	"google.golang.org/grpc/mem"
)

// Helpers operate on the blocking shared-memory ring (ShmRing).

// errMalformedCtlFrame is returned by readCtlFrame when a control-plane
// frame's length field exceeds the allowed maximum (maxCtlPayload).
// Callers (in particular the listener's Accept loop) should treat this
// as a non-fatal, per-frame error: log it and continue, rather than
// tearing down the listener, because the peer may be buggy or hostile
// but other clients on the same control segment should still be served.
// The underlying ring may be left misaligned by the time this error is
// returned (readCtlFrame attempts a best-effort bounded drain), so a
// burst of these errors is also possible.
var errMalformedCtlFrame = errors.New("malformed control-plane frame")

// Frame header layout (16 bytes, little-endian, aligned 16):
// uint32 length    // payload length in bytes (excludes 16-byte header)
// uint32 streamID  // opaque stream identifier (client odd; server even)
// uint8  type      // enum FrameType
// uint8  flags     // per-type flags
// uint16 reserved  // set to zero; future use
// uint32 reserved2 // set to zero; future use
const frameHeaderSize = 16

// FrameType represents the type of a shared memory transport frame.
type FrameType uint8

// Frame type constants for the shared memory transport protocol.
const (
	FrameTypePAD          FrameType = 0x00
	FrameTypeHEADERS      FrameType = 0x01
	FrameTypeMESSAGE      FrameType = 0x02
	FrameTypeTRAILERS     FrameType = 0x03
	FrameTypeCANCEL       FrameType = 0x04
	FrameTypeGOAWAY       FrameType = 0x05
	FrameTypePING         FrameType = 0x06
	FrameTypePONG         FrameType = 0x07
	FrameTypeHALFCLOSE    FrameType = 0x08
	FrameTypeWindowUpdate FrameType = 0x09
)

// Flags
const (
	// HEADERS flags
	HeadersFlagINITIAL = uint8(0x01)

	// MESSAGE flags
	MessageFlagMORE = uint8(0x01)
	// MessageFlagEndStream signals to the writer that this MESSAGE
	// terminates the stream from THIS peer's send direction. Set by
	// the client transport on the last logical request message of a
	// client-streaming or unary RPC; never set by the server (server
	// ends its send direction with a TRAILERS frame). The H2 codec's
	// writeProtoToRingH2 maps this to H2's END_STREAM flag on the
	// emitted DATA frame; the H2 reader maps the same DATA's
	// END_STREAM back to MessageFlagMORE = 0 (clearing MORE) on the
	// surfaced MESSAGE so ShmServerTransport.handleMessage's MORE=0
	// EOF logic fires correctly.
	MessageFlagEndStream = uint8(0x02)

	// TRAILERS flags
	TrailersFlagEndStream = uint8(0x01)

	// GOAWAY flags
	GoAwayFlagDRAINING  = uint8(0x01)
	GoAwayFlagIMMEDIATE = uint8(0x02)

	// PING flags (RFC A73 Phase 5: Flow Control)
	PingFlagBDP = uint8(0x01) // Indicates this is a BDP estimation ping
	PingFlagACK = uint8(0x02) // Indicates this is a ping acknowledgment
)

// FrameHeader represents the on-wire 16B header.
type FrameHeader struct {
	Length    uint32
	StreamID  uint32
	Type      FrameType
	Flags     uint8
	Reserved  uint16
	Reserved2 uint32
}

func encodeFrameHeaderTo(dst *[frameHeaderSize]byte, fh FrameHeader) {
	b := dst[:]
	binary.LittleEndian.PutUint32(b[0:4], fh.Length)
	binary.LittleEndian.PutUint32(b[4:8], fh.StreamID)
	b[8] = byte(fh.Type)
	b[9] = fh.Flags
	binary.LittleEndian.PutUint16(b[10:12], fh.Reserved)
	binary.LittleEndian.PutUint32(b[12:16], fh.Reserved2)
}

func decodeFrameHeader(b []byte) (FrameHeader, error) {
	if len(b) < frameHeaderSize {
		return FrameHeader{}, errors.New("frame header too short")
	}
	var fh FrameHeader
	fh.Length = binary.LittleEndian.Uint32(b[0:4])
	fh.StreamID = binary.LittleEndian.Uint32(b[4:8])
	fh.Type = FrameType(b[8])
	fh.Flags = b[9]
	fh.Reserved = binary.LittleEndian.Uint16(b[10:12])
	fh.Reserved2 = binary.LittleEndian.Uint32(b[12:16])
	return fh, nil
}

// readFrameView uses merged header+payload commits and speculative zero-copy
// for single-frame MESSAGE payloads. Multi-frame (MORE) chunks use pooled
// copy buffers to avoid memclr overhead on large allocations.

// Simple binary v1 payloads.

// KV represents a key-value pair for metadata in shared memory transport headers.
type KV struct {
	Key    string
	Values [][]byte
}

// HeadersV1 represents the version 1 header frame payload for shared memory transport.
type HeadersV1 struct {
	Version          uint8  // must be 1
	HdrType          uint8  // 0=client-initial, 1=server-initial
	Method           string // present iff HdrType==0
	Authority        string
	DeadlineUnixNano uint64 // 0 if none
	Metadata         []KV
}

func encodeHeaders(h HeadersV1) []byte {
	// Size calculation
	size := 1 + 1 + 4 // version + hdrType + methodLen
	size += len(h.Method)
	size += 4 + len(h.Authority)
	size += 8 // deadline
	size += 2 // mdCount
	for _, kv := range h.Metadata {
		size += 2 + len(kv.Key)
		size += 2 // valCount
		for _, v := range kv.Values {
			size += 4 + len(v)
		}
	}
	out := make([]byte, size)
	i := 0
	out[i] = 1
	i++
	out[i] = h.HdrType
	i++
	// method length and bytes (only when HdrType==0). For HdrType!=0, methodLen=0
	if h.HdrType == 0 {
		binary.LittleEndian.PutUint32(out[i:i+4], uint32(len(h.Method)))
		i += 4
		copy(out[i:i+len(h.Method)], []byte(h.Method))
		i += len(h.Method)
	} else {
		binary.LittleEndian.PutUint32(out[i:i+4], 0)
		i += 4
	}
	binary.LittleEndian.PutUint32(out[i:i+4], uint32(len(h.Authority)))
	i += 4
	copy(out[i:i+len(h.Authority)], []byte(h.Authority))
	i += len(h.Authority)
	binary.LittleEndian.PutUint64(out[i:i+8], h.DeadlineUnixNano)
	i += 8
	binary.LittleEndian.PutUint16(out[i:i+2], uint16(len(h.Metadata)))
	i += 2
	for _, kv := range h.Metadata {
		binary.LittleEndian.PutUint16(out[i:i+2], uint16(len(kv.Key)))
		i += 2
		copy(out[i:i+len(kv.Key)], []byte(kv.Key))
		i += len(kv.Key)
		binary.LittleEndian.PutUint16(out[i:i+2], uint16(len(kv.Values)))
		i += 2
		for _, v := range kv.Values {
			binary.LittleEndian.PutUint32(out[i:i+4], uint32(len(v)))
			i += 4
			copy(out[i:i+len(v)], v)
			i += len(v)
		}
	}
	return out
}

func decodeHeaders(b []byte) (HeadersV1, error) {
	var h HeadersV1
	i := 0
	if len(b) < 2 {
		return h, errors.New("headers too short")
	}
	ver := b[i]
	i++
	if ver != 1 {
		return h, fmt.Errorf("unsupported headers version %d", ver)
	}
	h.Version = ver
	h.HdrType = b[i]
	i++
	if len(b[i:]) < 4 {
		return h, errors.New("headers methodLen missing")
	}
	methodLen := int(binary.LittleEndian.Uint32(b[i : i+4]))
	i += 4
	if len(b[i:]) < methodLen {
		return h, errors.New("headers method bytes missing")
	}
	if h.HdrType == 0 {
		h.Method = string(b[i : i+methodLen])
	}
	i += methodLen
	if len(b[i:]) < 4 {
		return h, errors.New("headers authorityLen missing")
	}
	authLen := int(binary.LittleEndian.Uint32(b[i : i+4]))
	i += 4
	if len(b[i:]) < authLen+8+2 {
		return h, errors.New("headers authority/deadline/mdCount missing")
	}
	h.Authority = string(b[i : i+authLen])
	i += authLen
	h.DeadlineUnixNano = binary.LittleEndian.Uint64(b[i : i+8])
	i += 8
	mdCount := int(binary.LittleEndian.Uint16(b[i : i+2]))
	i += 2
	if mdCount < 0 {
		return h, errors.New("headers negative mdCount")
	}
	h.Metadata = make([]KV, 0, mdCount)
	for j := 0; j < mdCount; j++ {
		if len(b[i:]) < 2 {
			return h, errors.New("headers kv keyLen missing")
		}
		keyLen := int(binary.LittleEndian.Uint16(b[i : i+2]))
		i += 2
		if len(b[i:]) < keyLen+2 {
			return h, errors.New("headers kv key/valCount missing")
		}
		key := string(b[i : i+keyLen])
		i += keyLen
		valCount := int(binary.LittleEndian.Uint16(b[i : i+2]))
		i += 2
		if valCount < 0 {
			return h, errors.New("headers kv negative valCount")
		}
		vals := make([][]byte, 0, valCount)
		for k := 0; k < valCount; k++ {
			if len(b[i:]) < 4 {
				return h, errors.New("headers kv val len missing")
			}
			l := int(binary.LittleEndian.Uint32(b[i : i+4]))
			i += 4
			if len(b[i:]) < l {
				return h, errors.New("headers kv val bytes missing")
			}
			vals = append(vals, append([]byte(nil), b[i:i+l]...))
			i += l
		}
		h.Metadata = append(h.Metadata, KV{Key: key, Values: vals})
	}
	return h, nil
}

// TrailersV1 represents the version 1 trailer frame payload for shared memory transport.
type TrailersV1 struct {
	Version        uint8 // must be 1
	GRPCStatusCode uint32
	GRPCStatusMsg  string
	Metadata       []KV
}

func encodeTrailers(t TrailersV1) []byte {
	size := 1 + 4 + 4 + len(t.GRPCStatusMsg) + 2
	for _, kv := range t.Metadata {
		size += 2 + len(kv.Key)
		size += 2
		for _, v := range kv.Values {
			size += 4 + len(v)
		}
	}
	out := make([]byte, size)
	i := 0
	out[i] = 1
	i++
	binary.LittleEndian.PutUint32(out[i:i+4], t.GRPCStatusCode)
	i += 4
	binary.LittleEndian.PutUint32(out[i:i+4], uint32(len(t.GRPCStatusMsg)))
	i += 4
	copy(out[i:i+len(t.GRPCStatusMsg)], []byte(t.GRPCStatusMsg))
	i += len(t.GRPCStatusMsg)
	binary.LittleEndian.PutUint16(out[i:i+2], uint16(len(t.Metadata)))
	i += 2
	for _, kv := range t.Metadata {
		binary.LittleEndian.PutUint16(out[i:i+2], uint16(len(kv.Key)))
		i += 2
		copy(out[i:i+len(kv.Key)], []byte(kv.Key))
		i += len(kv.Key)
		binary.LittleEndian.PutUint16(out[i:i+2], uint16(len(kv.Values)))
		i += 2
		for _, v := range kv.Values {
			binary.LittleEndian.PutUint32(out[i:i+4], uint32(len(v)))
			i += 4
			copy(out[i:i+len(v)], v)
			i += len(v)
		}
	}
	return out
}

func decodeTrailers(b []byte) (TrailersV1, error) {
	var t TrailersV1
	i := 0
	if len(b) < 1+4+4 {
		return t, errors.New("trailers too short")
	}
	ver := b[i]
	i++
	if ver != 1 {
		return t, fmt.Errorf("unsupported trailers version %d", ver)
	}
	t.Version = ver
	t.GRPCStatusCode = binary.LittleEndian.Uint32(b[i : i+4])
	i += 4
	msgLen := int(binary.LittleEndian.Uint32(b[i : i+4]))
	i += 4
	if len(b[i:]) < msgLen+2 {
		return t, errors.New("trailers msg/mdCount missing")
	}
	t.GRPCStatusMsg = string(b[i : i+msgLen])
	i += msgLen
	mdCount := int(binary.LittleEndian.Uint16(b[i : i+2]))
	i += 2
	t.Metadata = make([]KV, 0, mdCount)
	for j := 0; j < mdCount; j++ {
		if len(b[i:]) < 2 {
			return t, errors.New("trailers kv keyLen missing")
		}
		keyLen := int(binary.LittleEndian.Uint16(b[i : i+2]))
		i += 2
		if len(b[i:]) < keyLen+2 {
			return t, errors.New("trailers kv key/valCount missing")
		}
		key := string(b[i : i+keyLen])
		i += keyLen
		valCount := int(binary.LittleEndian.Uint16(b[i : i+2]))
		i += 2
		vals := make([][]byte, 0, valCount)
		for k := 0; k < valCount; k++ {
			if len(b[i:]) < 4 {
				return t, errors.New("trailers kv val len missing")
			}
			l := int(binary.LittleEndian.Uint32(b[i : i+4]))
			i += 4
			if len(b[i:]) < l {
				return t, errors.New("trailers kv val bytes missing")
			}
			vals = append(vals, append([]byte(nil), b[i:i+l]...))
			i += l
		}
		t.Metadata = append(t.Metadata, KV{Key: key, Values: vals})
	}
	return t, nil
}

// writeFrame writes one logical SHM frame (header + payload) using the
// HTTP/2 wire codec. The caller's FrameHeader+payload model is translated
// into the corresponding H2 frame(s) via the per-ring HPACK encoder.
//
// Used by the data plane (gRPC HEADERS/MESSAGE/TRAILERS/CANCEL/etc.) and
// by tests that drive the transport. Control-plane frames
// (CONNECT/ACCEPT/REJECT) and security-handshake frames have no H2
// mapping and must use writeCtlFrame instead.
func writeFrame(ctx context.Context, tx *ShmRing, fh FrameHeader, payload []byte) error {
	holder := tx.h2Encoder()
	return writeFrameH2(ctx, tx, fh, payload, holder.enc, holder.scratch)
}

// writeFrameBuffers writes a frame whose payload is composed of an
// optional gRPC-LPM header prefix plus a BufferSlice.
//
// For MESSAGE frames that fit in a single H2 DATA frame and in the
// ring without straddle-induced complications, the segments are
// streamed directly into the ring reservation via writeFrameH2Message —
// no intermediate contiguous heap buffer. This eliminates a per-send
// allocation of (len(hdr) + payload.Len()) bytes plus an extra memcpy
// on the streaming hot path; it is the production analogue of the
// writeProtoToRingH2 unary ZC path.
//
// Non-MESSAGE frame types (HEADERS, TRAILERS, CANCEL, ...) and MESSAGE
// frames that require chunking (body > h2MaxFramePayload or > ring
// capacity) still materialise into a contiguous buffer here, because
// the H2 codec path for those types operates on the materialised form
// (HPACK re-encode, multi-DATA chunking).
func writeFrameBuffers(ctx context.Context, tx *ShmRing, fh FrameHeader, hdr []byte, payload mem.BufferSlice) error {
	dataLen := payload.Len()
	if len(hdr) == 0 && dataLen == 0 {
		return writeFrame(ctx, tx, fh, nil)
	}
	// Vectored fast path for MESSAGE frames: writes hdr + segments
	// directly into the ring reservation when the body fits in a
	// single H2 DATA frame. Use shmMaxFrameSize (the configurable
	// per-DATA-frame ceiling) instead of h2MaxFramePayload (the RFC
	// absolute limit) so a fair-comparison bench profile that sets
	// max frame to 16384 actually chunks here too.
	//
	// Order matters: this must come BEFORE the generic single-buffer
	// shortcut below, otherwise a MESSAGE with no LPM hdr and one
	// large BufferSlice element (rare but possible from custom
	// callers / tests) would fall back to the legacy materialise +
	// chunked path and bypass the producer-ZC optimisation.
	if fh.Type == FrameTypeMESSAGE {
		bodyLen := len(hdr) + dataLen
		if bodyLen <= shmMaxFrameSize && uint64(h2FrameHeaderSize+bodyLen) <= tx.Capacity() {
			return writeFrameH2Message(ctx, tx, fh.StreamID, fh.Flags, hdr, payload)
		}
		// Multi-frame MESSAGE (e.g. LargeUnary 16 MB under
		// fair-default's 16 KiB max-frame): emit DATA frames straight
		// from the BufferSlice without materialising into a single
		// contiguous buf. Saves a 16-MB-class memcpy on the producer
		// hot path. The legacy materialise-then-chunked path
		// (writeFrameH2DataChunked) is still used by writeFrame
		// callers that already pass a single []byte (HEADERS,
		// TRAILERS, plus tests).
		if uint64(h2FrameHeaderSize+shmMaxFrameSize) <= tx.Capacity() {
			_, h2f := translateCustomToH2(fh)
			return writeFrameH2DataChunkedVec(ctx, tx, fh.StreamID, hdr, payload, h2f)
		}
	}
	if len(hdr) == 0 && len(payload) == 1 {
		// Fast path: single buffer, no header prefix. Applies only to
		// non-MESSAGE frame types here (MESSAGE is handled above).
		return writeFrame(ctx, tx, fh, payload[0].ReadOnlyData())
	}
	buf := make([]byte, len(hdr)+dataLen)
	copy(buf, hdr)
	off := len(hdr)
	for _, b := range payload {
		off += copy(buf[off:], b.ReadOnlyData())
	}
	return writeFrame(ctx, tx, fh, buf)
}

// readFrame reads one logical SHM frame from a ring using the HTTP/2
// wire codec. Multi-frame H2 payloads (CONTINUATION / fragmented
// HEADERS / chunked DATA) are coalesced into a single
// FrameHeader+payload return.
func readFrame(ctx context.Context, rx *ShmRing) (FrameHeader, []byte, error) {
	return readFrameH2(ctx, rx, rx.h2Decoder())
}

// readFrameView reads a frame and returns a payload buffer. For
// contiguous single-frame MESSAGE payloads, the returned buffer
// references ring memory directly (speculative pre-committed
// zero-copy). For wrap-around payloads and non-MESSAGE frames, data is
// copied to a pooled buffer.
func readFrameView(ctx context.Context, rx *ShmRing) (FrameHeader, mem.Buffer, error) {
	return readFrameViewH2(ctx, rx, rx.h2Decoder())
}

// writeCtlFrame writes one control-plane frame (header + payload) using
// a fixed 16-byte ring frame format. Used only for the CONNECT/ACCEPT/
// REJECT control-segment handshake and for the security handshake on the
// data segment; once the handshake completes the data segment switches
// to HTTP/2 framing (see writeFrame).
//
// Coexistence on the data segment: the security handshake (when
// configured) runs on the same data ring pair as subsequent gRPC
// traffic, but the two formats never interleave because:
//
//  1. The handshake is fully synchronous — ClientHandshake /
//     ServerHandshake do not return until the final ack/fail frame
//     has been observed (see shm_security.go).
//  2. NewShmClientTransport / NewShmServerTransport — which start the
//     H2 reader and writer goroutines — are constructed AFTER the
//     handshake returns successfully (see shm_dialer.go and
//     shm_listener.go).
//  3. The two formats use disjoint FrameType ranges: handshake uses
//     FrameTypeHandshake{Init,Resp,Ack,Fail} (0x20-0x23); after
//     handshake the H2 codec consumes raw 9-byte H2 frame headers,
//     never the legacy 16-byte FrameType byte.
//
// Therefore at any moment exactly one format is active on a given ring
// and a peer that respects the protocol cannot induce confusion. This
// function blocks if necessary and never spins. Headers may straddle
// wraps.
func writeCtlFrame(ctx context.Context, tx *ShmRing, fh FrameHeader, payload []byte) error {
	// Fill header fields consistently and set reserved to zero.
	fh.Length = uint32(len(payload))
	fh.Reserved = 0
	fh.Reserved2 = 0

	// Atomically write header+payload in a single reservation/commit.
	// Critical for correctness under backpressure: if we commit the
	// header first and later block writing the payload, a reader can
	// observe the header and then block waiting for the full payload
	// length, preventing it from consuming any bytes and freeing space
	// for the writer.
	total := frameHeaderSize + len(payload)
	res, err := tx.ReserveWrite(ctx, total)
	if err != nil {
		return err
	}
	var hdr [frameHeaderSize]byte
	encodeFrameHeaderTo(&hdr, fh)

	writeAt := func(off int, src []byte) error {
		if len(src) == 0 {
			return nil
		}
		if off < len(res.First) {
			n := copy(res.First[off:], src)
			src = src[n:]
			off = 0
		} else {
			off -= len(res.First)
		}
		if len(src) == 0 {
			return nil
		}
		if off > len(res.Second) {
			return errors.New("failed to copy frame bytes")
		}
		copy(res.Second[off:], src)
		return nil
	}

	if err := writeAt(0, hdr[:]); err != nil {
		return err
	}
	if err := writeAt(frameHeaderSize, payload); err != nil {
		return err
	}
	return res.Commit(total)
}

// readCtlFrame reads one non-PAD control-plane frame (skipping any PAD
// frames) using the 16-byte ring frame format. Used only for the
// CONNECT/ACCEPT/REJECT control-segment handshake and for the security
// handshake on the data segment; once the handshake completes the data
// segment switches to HTTP/2 framing (see readFrame). For the
// non-overlap argument with H2 traffic on the same ring, see
// writeCtlFrame's docstring. It blocks if necessary and never spins.
func readCtlFrame(ctx context.Context, rx *ShmRing) (FrameHeader, []byte, error) {
	// maxCtlPayload bounds the payload length we are willing to allocate
	// for a control-plane frame. The largest legitimate control payload
	// is a connectResponse (segment name, ~128 bytes) or a security
	// HandshakeInit (identity + nonce + version, capped well below
	// 1 KiB). 4 KiB is generous headroom; anything larger is treated
	// as a malformed peer (or a malicious peer trying to force an
	// arbitrary-size allocation via the 32-bit Length field).
	const maxCtlPayload = 4096
	for {
		first, second, commit, err := rx.ReadSlices(ctx, frameHeaderSize)
		if err != nil {
			return FrameHeader{}, nil, err
		}
		var hb [frameHeaderSize]byte
		n := 0
		if len(first) > 0 {
			n += copy(hb[:], first)
		}
		if n < frameHeaderSize && len(second) > 0 {
			n += copy(hb[n:], second)
		}
		commit.Commit(frameHeaderSize)
		if n != frameHeaderSize {
			return FrameHeader{}, nil, errors.New("short header read")
		}

		fh, err := decodeFrameHeader(hb[:])
		if err != nil {
			return FrameHeader{}, nil, err
		}

		if fh.Type == FrameTypePAD {
			if fh.Length > 0 {
				if fh.Length > maxCtlPayload {
					// Malformed PAD: drain at most what is currently
					// buffered to avoid blocking on bytes that may never
					// arrive (a hostile peer can advertise a huge
					// Length without ever writing the payload). The
					// caller should log and continue accepting; the
					// ring may be left misaligned but subsequent
					// CONNECT frames from well-behaved clients will
					// eventually re-align.
					if avail := rx.Available(); avail > 0 {
						drainN := uint64(fh.Length)
						if drainN > avail {
							drainN = avail
						}
						if _, derr := rx.ReadExact(ctx, int(drainN), nil); derr != nil {
							return FrameHeader{}, nil, derr
						}
					}
					return FrameHeader{}, nil, fmt.Errorf("%w: PAD length=%d (max %d)", errMalformedCtlFrame, fh.Length, maxCtlPayload)
				}
				if _, err := rx.ReadExact(ctx, int(fh.Length), nil); err != nil {
					return FrameHeader{}, nil, err
				}
			}
			continue
		}

		var payload []byte
		if fh.Length > 0 {
			if fh.Length > maxCtlPayload {
				// Same recovery strategy as for malformed PAD frames
				// above: bounded best-effort drain so the listener
				// loop can continue accepting future clients.
				if avail := rx.Available(); avail > 0 {
					drainN := uint64(fh.Length)
					if drainN > avail {
						drainN = avail
					}
					if _, derr := rx.ReadExact(ctx, int(drainN), nil); derr != nil {
						return FrameHeader{}, nil, derr
					}
				}
				return FrameHeader{}, nil, fmt.Errorf("%w: type=%d length=%d (max %d)", errMalformedCtlFrame, fh.Type, fh.Length, maxCtlPayload)
			}
			p, err := rx.ReadExact(ctx, int(fh.Length), nil)
			if err != nil {
				return FrameHeader{}, nil, err
			}
			payload = p
		}
		return fh, payload, nil
	}
}
