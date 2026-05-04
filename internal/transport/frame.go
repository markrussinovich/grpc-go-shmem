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
	"sync/atomic"

	"google.golang.org/grpc/mem"
)

// Helpers operate on the blocking shared-memory ring (ShmRing).

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

// writeFrame writes one frame (header + payload) to the ring. It blocks if
// necessary and never spins. Headers may straddle wraps; ReserveFrameHeader
// can return split slices which are both written.
func writeFrame(ctx context.Context, tx *ShmRing, fh FrameHeader, payload []byte) error {
	// Dispatch to the H2 codec when the ring is configured for HTTP/2.
	if tx.wire == WireFormatHTTP2 {
		holder := tx.h2Encoder()
		return writeFrameH2(ctx, tx, fh, payload, holder.enc, holder.scratch)
	}

	// Fill header fields consistently and set reserved to zero
	fh.Length = uint32(len(payload))
	fh.Reserved = 0
	fh.Reserved2 = 0

	// Atomically write header+payload in a single reservation/commit.
	//
	// This is critical for correctness under backpressure: if we commit the header
	// first and later block writing the payload, a reader can observe the header
	// and then block waiting for the full payload length, preventing it from
	// consuming any bytes and freeing space for the writer.
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
		// First segment
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

// writeFrameBuffers writes a frame whose payload is composed of an optional
// header prefix plus a BufferSlice. It avoids building an intermediate
// contiguous payload, reducing allocations and copies on the hot path.
func writeFrameBuffers(ctx context.Context, tx *ShmRing, fh FrameHeader, hdr []byte, payload mem.BufferSlice) error {
	if tx.wire == WireFormatHTTP2 {
		// Materialize hdr + payload into a contiguous buffer for the H2 codec.
		// The H2 codec further translates HEADERS/TRAILERS into HPACK; for
		// MESSAGE frames the bytes are written directly as DATA.
		dataLen := payload.Len()
		buf := make([]byte, len(hdr)+dataLen)
		copy(buf, hdr)
		off := len(hdr)
		for _, b := range payload {
			off += copy(buf[off:], b.ReadOnlyData())
		}
		holder := tx.h2Encoder()
		return writeFrameH2(ctx, tx, fh, buf, holder.enc, holder.scratch)
	}
	dataLen := payload.Len()
	payloadLen := len(hdr) + dataLen
	fh.Length = uint32(payloadLen)
	fh.Reserved = 0
	fh.Reserved2 = 0

	total := frameHeaderSize + payloadLen
	res, err := tx.ReserveWrite(ctx, total)
	if err != nil {
		return err
	}

	var fhBytes [frameHeaderSize]byte
	encodeFrameHeaderTo(&fhBytes, fh)

	written := 0
	writeSeq := func(src []byte) error {
		for len(src) > 0 {
			if written < len(res.First) {
				n := copy(res.First[written:], src)
				written += n
				src = src[n:]
				if len(src) == 0 {
					return nil
				}
			}

			secondOff := written - len(res.First)
			if secondOff >= len(res.Second) {
				return errors.New("failed to copy frame bytes: reservation overflow")
			}

			n := copy(res.Second[secondOff:], src)
			written += n
			src = src[n:]
		}
		return nil
	}

	if err := writeSeq(fhBytes[:]); err != nil {
		return err
	}
	if len(hdr) > 0 {
		if err := writeSeq(hdr); err != nil {
			return err
		}
	}
	for _, buf := range payload {
		data := buf.ReadOnlyData()
		if len(data) == 0 {
			continue
		}
		if err := writeSeq(data); err != nil {
			return err
		}
	}

	if written != total {
		return errors.New("failed to copy frame bytes: short write")
	}

	return res.Commit(total)
}

// writeFrameBuffersChunked writes a MESSAGE frame whose payload (hdr + data) may
// exceed the ring capacity. If the payload fits in a single frame (fast path), it
// is written directly. Otherwise, it is split into multiple frames with the MORE
// flag set on all but the final chunk.
//
// The maxFramePayload parameter specifies the maximum payload size per frame. If
// zero, it defaults to ringCapacity - frameHeaderSize - safetyMargin. A sensible
// default is 32KB or (capacity/2) whichever is smaller.
func writeFrameBuffersChunked(ctx context.Context, tx *ShmRing, fh FrameHeader, hdr []byte, data mem.BufferSlice, maxFramePayload int) error {
	if tx.wire == WireFormatHTTP2 {
		// H2 path doesn't currently chunk: H2's max frame size is 16MB which
		// is sufficient for almost all gRPC messages. For larger messages,
		// future work is to emit multiple H2 DATA frames with END_STREAM=0.
		return writeFrameBuffers(ctx, tx, fh, hdr, data)
	}
	payloadLen := len(hdr) + data.Len()

	// Calculate effective max payload if not specified.
	if maxFramePayload <= 0 {
		cap := int(tx.Capacity())
		maxFramePayload = cap/2 - frameHeaderSize
		if maxFramePayload < 1024 {
			maxFramePayload = 1024
		}
	}

	// Fast path: payload fits in a single frame.
	if payloadLen <= maxFramePayload {
		return writeFrameBuffers(ctx, tx, fh, hdr, data)
	}

	// Slow path: stream chunk payloads directly from hdr + data buffers
	// without materializing a contiguous intermediate buffer. This avoids
	// a 256MB allocation for large messages.
	// Per-chunk signals are needed so the reader consumes chunks and
	// frees ring space for subsequent chunks (pipelining).
	cursor := newPayloadCursor(hdr, data)
	for cursor.remaining() > 0 {
		remainingBefore := cursor.remaining()
		chunkSize := maxFramePayload
		if chunkSize > remainingBefore {
			chunkSize = remainingBefore
		}

		chunkFH := fh
		if remainingBefore > chunkSize {
			chunkFH.Flags |= MessageFlagMORE
		}

		if err := writeFrameChunkFromCursor(ctx, tx, chunkFH, cursor, chunkSize); err != nil {
			return err
		}
	}

	return nil
}

// payloadCursor streams data from a gRPC header + BufferSlice without
// materializing a contiguous buffer. Used by writeFrameChunkFromCursor.
type payloadCursor struct {
	hdr    []byte
	hdrOff int
	bufs   mem.BufferSlice
	bufIdx int
	bufOff int
}

func newPayloadCursor(hdr []byte, bufs mem.BufferSlice) *payloadCursor {
	return &payloadCursor{hdr: hdr, bufs: bufs}
}

func (c *payloadCursor) remaining() int {
	rem := len(c.hdr) - c.hdrOff
	for i := c.bufIdx; i < len(c.bufs); i++ {
		data := c.bufs[i].ReadOnlyData()
		if i == c.bufIdx {
			rem += len(data) - c.bufOff
		} else {
			rem += len(data)
		}
	}
	return rem
}

// writeN writes exactly n bytes from the cursor into dst via writeSeq.
func (c *payloadCursor) writeN(writeSeq func([]byte) error, n int) (int, error) {
	written := 0
	for written < n {
		// Drain header first.
		if c.hdrOff < len(c.hdr) {
			want := n - written
			avail := len(c.hdr) - c.hdrOff
			if avail > want {
				avail = want
			}
			if err := writeSeq(c.hdr[c.hdrOff : c.hdrOff+avail]); err != nil {
				return written, err
			}
			c.hdrOff += avail
			written += avail
			continue
		}
		// Then drain data buffers.
		if c.bufIdx >= len(c.bufs) {
			break
		}
		data := c.bufs[c.bufIdx].ReadOnlyData()
		if c.bufOff >= len(data) {
			c.bufIdx++
			c.bufOff = 0
			continue
		}
		want := n - written
		avail := len(data) - c.bufOff
		if avail > want {
			avail = want
		}
		if err := writeSeq(data[c.bufOff : c.bufOff+avail]); err != nil {
			return written, err
		}
		c.bufOff += avail
		written += avail
		if c.bufOff >= len(data) {
			c.bufIdx++
			c.bufOff = 0
		}
	}
	return written, nil
}

// writeFrameChunkFromCursor writes a single frame whose payload is pulled
// from the cursor, directly into a ring reservation. No intermediate buffer.
func writeFrameChunkFromCursor(ctx context.Context, tx *ShmRing, fh FrameHeader, cursor *payloadCursor, payloadLen int) error {
	fh.Length = uint32(payloadLen)
	fh.Reserved = 0
	fh.Reserved2 = 0

	total := frameHeaderSize + payloadLen
	res, err := tx.ReserveWrite(ctx, total)
	if err != nil {
		return err
	}

	var fhBytes [frameHeaderSize]byte
	encodeFrameHeaderTo(&fhBytes, fh)

	written := 0
	writeSeq := func(src []byte) error {
		for len(src) > 0 {
			if written < len(res.First) {
				n := copy(res.First[written:], src)
				written += n
				src = src[n:]
			} else {
				off := written - len(res.First)
				if off >= len(res.Second) {
					return errors.New("reservation overflow")
				}
				n := copy(res.Second[off:], src)
				written += n
				src = src[n:]
			}
		}
		return nil
	}

	if err := writeSeq(fhBytes[:]); err != nil {
		return err
	}
	if _, err := cursor.writeN(writeSeq, payloadLen); err != nil {
		return err
	}

	return res.Commit(total)
}

// readFrame reads one non-PAD frame (skipping any PAD frames). It blocks if
// necessary and never spins.
func readFrame(ctx context.Context, rx *ShmRing) (FrameHeader, []byte, error) {
	if rx.wire == WireFormatHTTP2 {
		return readFrameH2(ctx, rx, rx.h2Decoder())
	}
	for {
		// Read exactly the header size, but allow it to straddle the wrap
		first, second, commit, err := rx.ReadSlices(ctx, frameHeaderSize)
		if err != nil {
			return FrameHeader{}, nil, err
		}
		var hb [frameHeaderSize]byte
		n := 0
		if len(first) > 0 {
			k := copy(hb[:], first)
			n += k
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

		// Skip PAD frames transparently (no geometry tricks needed)
		if fh.Type == FrameTypePAD {
			if fh.Length > 0 {
				if _, err := rx.ReadExact(ctx, int(fh.Length), nil); err != nil {
					return FrameHeader{}, nil, err
				}
			}
			continue
		}

		// Read payload
		var payload []byte
		if fh.Length > 0 {
			p, err := rx.ReadExact(ctx, int(fh.Length), nil)
			if err != nil {
				return FrameHeader{}, nil, err
			}
			payload = p
		}
		return fh, payload, nil
	}
}

// readFrameView reads a frame and returns a payload buffer. For contiguous
// MESSAGE payloads, the returned buffer references ring memory directly
// (speculative pre-committed zero-copy). For wrap-around payloads and
// non-MESSAGE frames, data is copied to a pooled buffer.
//
// Safety: ReadIdx is committed immediately, so the writer can advance past
// the returned data. The data remains valid as long as the writer does not
// wrap the entire ring capacity (64MB). Since gRPC-internal buffer
// consumption (read → copy to codec buffer → Unmarshal) completes in
// microseconds while filling 64MB takes milliseconds, this is safe for
// all practical workloads.
func readFrameView(ctx context.Context, rx *ShmRing) (FrameHeader, mem.Buffer, error) {
	if rx.wire == WireFormatHTTP2 {
		return readFrameViewH2(ctx, rx, rx.h2Decoder())
	}
	for {
		first, second, commitHeader, err := rx.ReadSlices(ctx, frameHeaderSize)
		if err != nil {
			return FrameHeader{}, nil, err
		}
		var hb [frameHeaderSize]byte
		n := 0
		if len(first) > 0 {
			k := copy(hb[:], first)
			n += k
		}
		if n < frameHeaderSize && len(second) > 0 {
			n += copy(hb[n:], second)
		}
		if n != frameHeaderSize {
			commitHeader.Commit(frameHeaderSize)
			return FrameHeader{}, nil, errors.New("short header read")
		}

		fh, err := decodeFrameHeader(hb[:])
		if err != nil {
			commitHeader.Commit(frameHeaderSize)
			return FrameHeader{}, nil, err
		}

		if fh.Type == FrameTypePAD {
			commitHeader.Commit(frameHeaderSize)
			if fh.Length > 0 {
				if _, err := rx.ReadExact(ctx, int(fh.Length), nil); err != nil {
					return FrameHeader{}, nil, err
				}
			}
			continue
		}

		if fh.Length == 0 {
			commitHeader.Commit(frameHeaderSize)
			return fh, nil, nil
		}

		// Commit header immediately to free ring space for the writer.
		// Merged commit (deferring header commit to payload) was tested but
		// adds ~6% latency on Linux for 4-64KB payloads due to the extra
		// headerReadIdx save/restore overhead. Since Linux futex costs <1µs,
		// saving one Commit cycle doesn't compensate.
		commitHeader.Commit(frameHeaderSize)
		payloadLen := int(fh.Length)
		pFirst, pSecond, commitPayload, err := rx.ReadSlices(ctx, payloadLen)
		if err != nil {
			return FrameHeader{}, nil, err
		}

		// Zero-copy for contiguous single-frame MESSAGE payloads only.
		//
		// Multi-frame chunks (MORE flag set) always use the copy path:
		// the caller must reassemble chunks into a contiguous buffer
		// anyway, so speculative ZC provides no copy savings, and the
		// deferred-publish protocol holds header.ReadIdx across the
		// chain which would stall the writer for chains larger than
		// cap/2. Matches grpc-dotnet-shm's chain-zc-only-for-cap/2 rule
		// (which we don't yet implement on the Go side).
		isMore := fh.Flags&MessageFlagMORE != 0

		// When MORE flag is set, boost the reader's spin cutoff so the
		// next ReadSlices call stays in user-space polling rather than
		// falling through to WaitOnAddress. While the reader spins,
		// DataWaiters == 0, causing the writer to also skip
		// WakeByAddress. This eliminates cgocall on BOTH sides for
		// intermediate chunks (C#-style fire-and-forget).
		if isMore {
			atomic.StoreUint32(&rx.dataSpinCutoff, spinMoreBoost)
		}

		// Speculative zero-copy decision tree for MESSAGE frames:
		//
		// Single-frame (!isMore) message:
		//   * ZC if eligible (no chain in flight, large enough payload,
		//     contiguous, ring not under back-pressure). Uses the fused
		//     single-frame anchor for one Begin+Commit step.
		//
		// Multi-frame chain:
		//   * First chunk (isMore && !chainOpen && !chainCopyMode):
		//     peek the LPM length; if totalMsg ≤ ChainZcBudget AND
		//     IsSpeculativeZCEligible passes, open a chain anchor.
		//     Otherwise enter chainCopyMode for the rest of the message.
		//   * Continuation chunk in ZC mode (isMore && chainOpen):
		//     emit body as ring-backed buffer, deferred Commit, no new
		//     anchor.
		//   * Final chunk in ZC mode (!isMore && chainOpen):
		//     emit ring-backed buffer, close the chain marker; the
		//     consumer's last Buffer.Free triggers EndZcReservation.
		//   * Any chunk in chainCopyMode: copy.
		if fh.Type == FrameTypeMESSAGE && len(pSecond) == 0 {
			// === Single-frame (no MORE) ===
			if !isMore && !rx.IsZcChainActive() && !rx.ChainCopyMode() &&
				rx.IsSpeculativeZCEligible(payloadLen, true) {
				baseIdx := commitPayload.commitReadIdx
				rx.BeginSingleFrameZcCommit(baseIdx, payloadLen)
				rx.AddChainZcInFlight()
				ringSlice := pFirst[:payloadLen:payloadLen]
				pool := &zcChainReleasePool{ring: rx}
				buf := mem.NewBuffer(&ringSlice, pool)
				return fh, buf, nil
			}

			// === Multi-frame chain start ===
			if isMore && !rx.IsZcChainActive() && !rx.ChainCopyMode() {
				// Peek LPM length to decide ZC vs copy for the chain.
				// LPM = 5 bytes: 1-byte compressed flag + 4-byte big-
				// endian length. Total message bytes = 5 + lpmBodyLen.
				if payloadLen >= 5 && rx.IsSpeculativeZCEligible(payloadLen, true) {
					lpmBodyLen := int64(binary.BigEndian.Uint32(pFirst[1:5]))
					totalMsg := int64(5) + lpmBodyLen
					if totalMsg > 0 && totalMsg <= rx.ChainZcBudget() {
						// Open chain anchor.
						baseIdx := commitPayload.commitReadIdx
						rx.BeginZcReservation(baseIdx)
						rx.OpenZcChain()
						rx.AddChainZcInFlight()
						commitPayload.Commit(payloadLen)
						ringSlice := pFirst[:payloadLen:payloadLen]
						pool := &zcChainReleasePool{ring: rx}
						buf := mem.NewBuffer(&ringSlice, pool)
						return fh, buf, nil
					}
				}
				// Reject: enter copy mode for the rest of the message.
				rx.SetChainCopyMode(true)
			}

			// === Multi-frame chain continuation in ZC mode ===
			//
			// Gate on IsChainOpen (codec-side chain marker), NOT
			// IsZcChainActive (which includes single-frame holds where
			// no chain is in progress). A single-frame ZC buffer being
			// held while a subsequent unrelated message arrives must
			// NOT enter chain continuation; that subsequent message
			// goes through the regular copy path and its commit is
			// deferred via the zcActive=1 branch in ReadCommit.Commit.
			if rx.IsChainOpen() && !rx.ChainCopyMode() {
				// Anchor already open. Just hand back ring slice; the
				// chain anchor's deferred-publish handles the read
				// index. AddChainZcInFlight + deferred Commit on this
				// chunk's bytes accumulate into zcDeferredTarget.
				rx.AddChainZcInFlight()
				commitPayload.Commit(payloadLen)
				if !isMore {
					// Final chunk — close chain marker. The consumer's
					// last Buffer.Free triggers EndZcReservation.
					rx.CloseZcChain()
					rx.SetChainCopyMode(false)
				}
				ringSlice := pFirst[:payloadLen:payloadLen]
				pool := &zcChainReleasePool{ring: rx}
				buf := mem.NewBuffer(&ringSlice, pool)
				return fh, buf, nil
			}
		}

		// Copy paths: copy data BEFORE Commit. Commit frees ring space
		// for the writer; without copying first, the writer could
		// overwrite the data between Commit and Copy.
		//
		// Chain bookkeeping: zcInFlight ONLY tracks ring-backed ZC
		// buffers, not copy buffers. But chainOpen MUST be cleared on
		// the final !MORE chunk regardless of which path it took, so
		// the consumer's eventual Free of the chain's ZC buffers can
		// fire EndZcReservation. If the final chunk happens to be in
		// the copy path (wrap, mid-chain rejection, etc.), we close
		// the chain marker here.
		var buf mem.Buffer
		if len(pSecond) == 0 {
			buf = mem.Copy(pFirst[:payloadLen], mem.DefaultBufferPool())
			commitPayload.Commit(payloadLen)
		} else {
			// Wrap-around: copy both parts to pooled buffer.
			pool := mem.DefaultBufferPool()
			poolBuf := pool.Get(payloadLen)
			copied := copy(*poolBuf, pFirst)
			copy((*poolBuf)[copied:], pSecond)
			buf = mem.NewBuffer(poolBuf, pool)
			commitPayload.Commit(payloadLen)
		}
		// Final chunk of a multi-frame message — close any chain state
		// so the consumer's last ZC buffer Free triggers EndZc.
		if fh.Type == FrameTypeMESSAGE && !isMore {
			if rx.IsChainOpen() {
				rx.CloseZcChain()
			}
			if rx.ChainCopyMode() {
				rx.SetChainCopyMode(false)
			}
		}
		return fh, buf, nil
	}
}
