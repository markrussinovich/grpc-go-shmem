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
	"sync"

	"google.golang.org/grpc/mem"
)

// Buffer pools for header/trailer encoding to reduce allocations on hot path.
// Small pool (512B) handles typical headers; large pool (4KB) for metadata-heavy cases.
var (
	smallHeaderPool = sync.Pool{
		New: func() any { return make([]byte, 512) },
	}
	largeHeaderPool = sync.Pool{
		New: func() any { return make([]byte, 4096) },
	}
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

type FrameType uint8

const (
	FrameTypePAD           FrameType = 0x00
	FrameTypeHEADERS       FrameType = 0x01
	FrameTypeMESSAGE       FrameType = 0x02
	FrameTypeTRAILERS      FrameType = 0x03
	FrameTypeCANCEL        FrameType = 0x04
	FrameTypeGOAWAY        FrameType = 0x05
	FrameTypePING          FrameType = 0x06
	FrameTypePONG          FrameType = 0x07
	FrameTypeHALFCLOSE     FrameType = 0x08
	FrameTypeWINDOW_UPDATE FrameType = 0x09
)

// Flags
const (
	// HEADERS flags
	HeadersFlagINITIAL = uint8(0x01)

	// MESSAGE flags
	MessageFlagMORE = uint8(0x01)

	// TRAILERS flags
	TrailersFlagEND_STREAM = uint8(0x01)

	// GOAWAY flags
	GoAwayFlagDRAINING  = uint8(0x01)
	GoAwayFlagIMMEDIATE = uint8(0x02)
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

// ringCommitPool defers advancing the ring read index until the payload buffer
// is fully released by the consumer. It holds a COPY of the ReadCommit state
// to avoid races when the ring's embedded ReadCommit is reused by later reads.
type ringCommitPool struct {
	once   sync.Once
	commit ReadCommit // Value copy, not pointer - captures state at creation time
}

func (p *ringCommitPool) Get(n int) *[]byte { return nil }

func (p *ringCommitPool) Put(b *[]byte) {
	p.once.Do(func() {
		shmDebugf("[DEBUG] ringCommitPool.Put: committing %d bytes", len(*b))
		p.commit.Commit(len(*b))
	})
}

// Simple binary v1 payloads.

type KV struct {
	Key    string
	Values [][]byte
}

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

// encodeHeadersPooled encodes headers into a pooled buffer and returns
// the buffer along with the actual data slice and a release function.
// The caller MUST call the release function after the data has been
// written to the ring buffer.
func encodeHeadersPooled(h HeadersV1) (data []byte, release func()) {
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

	// Get buffer from appropriate pool
	var buf []byte
	var pool *sync.Pool
	if size <= 512 {
		pool = &smallHeaderPool
		buf = pool.Get().([]byte)
	} else if size <= 4096 {
		pool = &largeHeaderPool
		buf = pool.Get().([]byte)
	} else {
		// Too large for pools, allocate directly
		buf = make([]byte, size)
		pool = nil
	}

	// Ensure buffer is large enough
	if len(buf) < size {
		buf = make([]byte, size)
		pool = nil // Don't return oversized allocation to pool
	}

	out := buf[:size]
	i := 0
	out[i] = 1
	i++
	out[i] = h.HdrType
	i++
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

	release = func() {
		if pool != nil {
			pool.Put(buf)
		}
	}
	return out, release
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

// encodeTrailersPooled encodes trailers using a pooled buffer.
// The caller MUST call the release function after the data has been
// written to the ring buffer.
func encodeTrailersPooled(t TrailersV1) (data []byte, release func()) {
	size := 1 + 4 + 4 + len(t.GRPCStatusMsg) + 2
	for _, kv := range t.Metadata {
		size += 2 + len(kv.Key)
		size += 2
		for _, v := range kv.Values {
			size += 4 + len(v)
		}
	}

	// Get buffer from appropriate pool
	var buf []byte
	var pool *sync.Pool
	if size <= 512 {
		pool = &smallHeaderPool
		buf = pool.Get().([]byte)
	} else if size <= 4096 {
		pool = &largeHeaderPool
		buf = pool.Get().([]byte)
	} else {
		buf = make([]byte, size)
		pool = nil
	}

	if len(buf) < size {
		buf = make([]byte, size)
		pool = nil
	}

	out := buf[:size]
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

	release = func() {
		if pool != nil {
			pool.Put(buf)
		}
	}
	return out, release
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
func writeFrame(tx *ShmRing, fh FrameHeader, payload []byte, ctx context.Context) error {
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
	res, err := tx.ReserveWrite(total, ctx)
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

// writeFramesBatched writes multiple frames in a single ring buffer commit.
// This reduces the number of futex wakes and mutex acquisitions for small messages.
// Each frame is a (FrameHeader, payload) pair. All frames share the same streamID.
func writeFramesBatched(tx *ShmRing, frames []struct {
	fh      FrameHeader
	payload []byte
}, ctx context.Context) error {
	if len(frames) == 0 {
		return nil
	}

	// Calculate total size needed
	total := 0
	for i := range frames {
		total += frameHeaderSize + len(frames[i].payload)
	}

	// Reserve space for all frames at once
	res, err := tx.ReserveWrite(total, ctx)
	if err != nil {
		return err
	}

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

	// Write each frame's header and payload
	var hdr [frameHeaderSize]byte
	for i := range frames {
		frames[i].fh.Length = uint32(len(frames[i].payload))
		frames[i].fh.Reserved = 0
		frames[i].fh.Reserved2 = 0
		encodeFrameHeaderTo(&hdr, frames[i].fh)

		if err := writeSeq(hdr[:]); err != nil {
			return err
		}
		if len(frames[i].payload) > 0 {
			if err := writeSeq(frames[i].payload); err != nil {
				return err
			}
		}
	}

	if written != total {
		return errors.New("failed to copy frame bytes: short write")
	}

	return res.Commit(total)
}

// writeFrameBuffers writes a frame whose payload is composed of an optional
// header prefix plus a BufferSlice. It avoids building an intermediate
// contiguous payload, reducing allocations and copies on the hot path.
func writeFrameBuffers(tx *ShmRing, fh FrameHeader, hdr []byte, payload mem.BufferSlice, ctx context.Context) error {
	dataLen := payload.Len()
	payloadLen := len(hdr) + dataLen
	fh.Length = uint32(payloadLen)
	fh.Reserved = 0
	fh.Reserved2 = 0

	total := frameHeaderSize + payloadLen
	res, err := tx.ReserveWrite(total, ctx)
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
func writeFrameBuffersChunked(tx *ShmRing, fh FrameHeader, hdr []byte, data mem.BufferSlice, maxFramePayload int, ctx context.Context) error {
	payloadLen := len(hdr) + data.Len()

	// Calculate effective max payload if not specified.
	if maxFramePayload <= 0 {
		// Use half of ring capacity minus header size as a safe default.
		// This leaves room for other frames and prevents blocking.
		cap := int(tx.Capacity())
		maxFramePayload = cap/2 - frameHeaderSize
		if maxFramePayload < 1024 {
			maxFramePayload = 1024 // minimum 1KB chunks
		}
	}

	// Fast path: payload fits in a single frame.
	if payloadLen <= maxFramePayload {
		return writeFrameBuffers(tx, fh, hdr, data, ctx)
	}

	// Slow path: need to chunk the payload.
	// Materialize hdr + data into a contiguous buffer for chunking.
	combined := make([]byte, payloadLen)
	copy(combined, hdr)
	offset := len(hdr)
	for _, buf := range data {
		n := copy(combined[offset:], buf.ReadOnlyData())
		offset += n
	}

	// Write chunks with MORE flag on all but the last.
	remaining := combined
	for len(remaining) > 0 {
		chunkSize := maxFramePayload
		if chunkSize > len(remaining) {
			chunkSize = len(remaining)
		}
		chunk := remaining[:chunkSize]
		remaining = remaining[chunkSize:]

		chunkFH := fh
		if len(remaining) > 0 {
			chunkFH.Flags |= MessageFlagMORE
		}

		if err := writeFrame(tx, chunkFH, chunk, ctx); err != nil {
			return err
		}
	}

	return nil
}

// readFrame reads one non-PAD frame (skipping any PAD frames). It blocks if
// necessary and never spins.
func readFrame(rx *ShmRing, ctx context.Context) (FrameHeader, []byte, error) {
	for {
		// Read exactly the header size, but allow it to straddle the wrap
		first, second, commit, err := rx.ReadSlices(frameHeaderSize, ctx)
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
				if _, err := rx.ReadExact(int(fh.Length), nil, ctx); err != nil {
					return FrameHeader{}, nil, err
				}
			}
			continue
		}

		// Read payload
		var payload []byte
		if fh.Length > 0 {
			p, err := rx.ReadExact(int(fh.Length), nil, ctx)
			if err != nil {
				return FrameHeader{}, nil, err
			}
			payload = p
		}
		return fh, payload, nil
	}
}

// readFrameView reads a frame and returns a zero-copy payload view when
// possible. The returned mem.Buffer must be freed by the caller to release the
// underlying ring reservation.
func readFrameView(rx *ShmRing, ctx context.Context) (FrameHeader, mem.Buffer, error) {
	for {
		first, second, commitHeader, err := rx.ReadSlices(frameHeaderSize, ctx)
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
		commitHeader.Commit(frameHeaderSize)
		if n != frameHeaderSize {
			return FrameHeader{}, nil, errors.New("short header read")
		}

		fh, err := decodeFrameHeader(hb[:])
		if err != nil {
			return FrameHeader{}, nil, err
		}

		if fh.Type == FrameTypePAD {
			if fh.Length > 0 {
				if _, err := rx.ReadExact(int(fh.Length), nil, ctx); err != nil {
					return FrameHeader{}, nil, err
				}
			}
			continue
		}

		if fh.Length == 0 {
			return fh, nil, nil
		}

		payloadLen := int(fh.Length)
		pFirst, pSecond, commitPayload, err := rx.ReadSlices(payloadLen, ctx)
		if err != nil {
			return FrameHeader{}, nil, err
		}

		// Fast-path: contiguous payload; wrap-around is rare with large rings.
		if len(pSecond) == 0 {
			contig := pFirst[:payloadLen]
			// mem.NewBuffer ignores the pool for small buffers (<=1024 bytes),
			// so we must commit immediately for small payloads to avoid blocking
			// the ring buffer reader.
			if mem.IsBelowBufferPoolingThreshold(payloadLen) {
				commitPayload.Commit(payloadLen)
				// Return a copy so caller doesn't hold ring memory
				result := make([]byte, payloadLen)
				copy(result, contig)
				return fh, mem.SliceBuffer(result), nil
			}
			pool := &ringCommitPool{commit: *commitPayload} // Value copy to capture state
			buf := mem.NewBuffer(&contig, pool)
			return fh, buf, nil
		}

		// Wrap-around fallback: copy once into a contiguous buffer, then commit immediately.
		contig := make([]byte, payloadLen)
		copied := copy(contig, pFirst)
		copy(contig[copied:], pSecond)
		commitPayload.Commit(payloadLen)
		return fh, mem.SliceBuffer(contig), nil
	}
}

// writeMessageChunked writes a MESSAGE payload split across multiple frames if needed.
// For all but the last chunk, the MORE flag is set. Chunking allows backpressure
// and smaller ring capacities to be exercised without requiring a single large frame.
func writeMessageChunked(tx *ShmRing, streamID uint32, payload []byte, chunkSize int, ctx context.Context) error {
	if chunkSize <= 0 {
		chunkSize = 32 * 1024
	}
	remaining := payload
	for len(remaining) > 0 {
		n := chunkSize
		if n > len(remaining) {
			n = len(remaining)
		}
		chunk := remaining[:n]
		remaining = remaining[n:]
		flags := uint8(0)
		if len(remaining) > 0 {
			flags = MessageFlagMORE
		}
		if err := writeFrame(tx, FrameHeader{StreamID: streamID, Type: FrameTypeMESSAGE, Flags: flags}, chunk, ctx); err != nil {
			return err
		}
	}
	return nil
}
