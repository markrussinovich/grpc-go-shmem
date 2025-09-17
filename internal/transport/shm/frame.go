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

package shm

import (
    "context"
    "encoding/binary"
    "errors"
    "fmt"
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
    FrameTypePAD     FrameType = 0x00
    FrameTypeHEADERS FrameType = 0x01
    FrameTypeMESSAGE FrameType = 0x02
    FrameTypeTRAILERS FrameType = 0x03
    FrameTypeCANCEL  FrameType = 0x04
    FrameTypeGOAWAY  FrameType = 0x05
    FrameTypePING    FrameType = 0x06
    FrameTypePONG    FrameType = 0x07
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
    Version         uint8 // must be 1
    GRPCStatusCode  uint32
    GRPCStatusMsg   string
    Metadata        []KV
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
// necessary and never spins. Headers are placed using ReserveFrameHeader which
// guarantees 16-byte alignment and PAD insertion at end-of-ring as needed.
func writeFrame(tx *ShmRing, fh FrameHeader, payload []byte) error {
    // Fill header fields consistently and set reserved to zero
    fh.Length = uint32(len(payload))
    fh.Reserved = 0
    fh.Reserved2 = 0

    ctx := context.Background()

    // Reserve and write the 16-byte header
    res, err := tx.ReserveFrameHeader(ctx)
    if err != nil {
        return err
    }
    var hdr [frameHeaderSize]byte
    encodeFrameHeaderTo(&hdr, fh)
    n := copy(res.First, hdr[:])
    if n != frameHeaderSize {
        return errors.New("failed to copy frame header")
    }
    if err := res.Commit(frameHeaderSize); err != nil {
        return err
    }

    // Write payload
    if len(payload) > 0 {
        if err := tx.WriteAll(payload, ctx); err != nil {
            return err
        }
    }
    return nil
}

// readFrame reads one non-PAD frame (skipping any PAD frames). It blocks if
// necessary and never spins.
func readFrame(rx *ShmRing) (FrameHeader, []byte, error) {
    ctx := context.Background()
    for {
        // Geometry-aware fast path: if fewer than 16 bytes remain before wrap,
        // skip them (PAD payload) first, then read the header at offset 0.
        hdr := rx.header()
        rIdx := hdr.ReadIndex()
        remToEnd := rx.capacity - (rIdx & rx.capMask)
        if remToEnd < frameHeaderSize && remToEnd > 0 {
            // Consume the tail bytes as PAD payload; the corresponding PAD header
            // was written at offset 0 by the writer. We don't need the header to
            // know the length here; geometry defines it.
            if _, _, commit, err := rx.ReadSlices(int(remToEnd), ctx); err != nil {
                return FrameHeader{}, nil, err
            } else {
                commit(int(remToEnd))
            }
            // Consume the PAD header at offset 0 silently, then continue to the
            // next iteration to parse the actual frame header.
            if _, err := rx.ReadExact(frameHeaderSize, nil, ctx); err != nil {
                return FrameHeader{}, nil, err
            }
            continue
        }

        // Read and parse header at current r (aligned or at start after PAD).
        hb, err := rx.ReadExact(frameHeaderSize, nil, ctx)
        if err != nil {
            return FrameHeader{}, nil, err
        }
        fh, err := decodeFrameHeader(hb)
        if err != nil {
            return FrameHeader{}, nil, err
        }
        if fh.Type == FrameTypePAD {
            // Consume PAD payload and continue
            if fh.Length > 0 {
                if _, err := rx.ReadExact(int(fh.Length), nil, ctx); err != nil {
                    return FrameHeader{}, nil, err
                }
            }
            continue
        }
        // Read payload for this frame
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

// writeMessageChunked writes a MESSAGE payload split across multiple frames if needed.
// For all but the last chunk, the MORE flag is set. Chunking allows backpressure
// and smaller ring capacities to be exercised without requiring a single large frame.
func writeMessageChunked(tx *ShmRing, streamID uint32, payload []byte, chunkSize int) error {
    if chunkSize <= 0 {
        chunkSize = 32 * 1024
    }
    remaining := payload
    for len(remaining) > 0 {
        n := chunkSize
        if n > len(remaining) { n = len(remaining) }
        chunk := remaining[:n]
        remaining = remaining[n:]
        flags := uint8(0)
        if len(remaining) > 0 { flags = MessageFlagMORE }
        if err := writeFrame(tx, FrameHeader{StreamID: streamID, Type: FrameTypeMESSAGE, Flags: flags}, chunk); err != nil {
            return err
        }
    }
    return nil
}
