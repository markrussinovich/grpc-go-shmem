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
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"
)

// protoMessage is an interface satisfied by all protobuf v2 messages.
type protoMessage = proto.Message

// protoSize returns the serialized size of a proto message.
func protoSize(msg proto.Message) int {
	return proto.Size(msg)
}

// protoMarshalAppend serializes msg and appends the result to dst.
func protoMarshalAppend(dst []byte, msg proto.Message) ([]byte, error) {
	return proto.MarshalOptions{UseCachedSize: true}.MarshalAppend(dst, msg)
}

// writeProtoToRing serializes a proto.Message directly into the ring buffer
// (zero-copy write). Layout: [16B frame header][5B gRPC header][proto payload].
// Returns false if the message cannot fit contiguously at this moment.
// The caller should fall back to writeProtoCopyToRing which handles chunking.
//
// pSize is the pre-computed proto.Size result. Passing it avoids a redundant
// proto.Size call (the caller typically already computed it for flow control).
// Pass -1 to compute it internally.
func writeProtoToRing(ctx context.Context, tx *ShmRing, streamID uint32, msg proto.Message, pSize int, flags uint8) (bool, error) {
	if pSize < 0 {
		pSize = proto.Size(msg)
	}

	// Dispatch to the H2 ZC path when the ring is configured for HTTP/2.
	if tx.WireFormat() == WireFormatHTTP2 {
		return writeProtoToRingH2(ctx, tx, streamID, msg, pSize, flags)
	}

	total := frameHeaderSize + 5 + pSize

	// Skip ZC for messages that will never fit in a single frame.
	// cap/3 is the max frame payload used by the chunking path.
	if uint64(total) > tx.Capacity()/3 {
		return false, nil
	}

	// Non-blocking check: is there enough contiguous space right now?
	if tx.ContiguousWriteSpace() < uint64(total) {
		return false, nil // fall back to copy path
	}

	res, err := tx.ReserveWrite(ctx, total)
	if err != nil {
		return false, err
	}

	// Frame header
	var hdr [frameHeaderSize]byte
	encodeFrameHeaderTo(&hdr, FrameHeader{
		Type:     FrameTypeMESSAGE,
		StreamID: streamID,
		Length:   uint32(5 + pSize),
		Flags:    flags,
	})
	copy(res.First[0:frameHeaderSize], hdr[:])

	// gRPC 5-byte header
	res.First[frameHeaderSize] = 0
	binary.BigEndian.PutUint32(res.First[frameHeaderSize+1:frameHeaderSize+5], uint32(pSize))

	// Marshal directly into ring
	dst := res.First[frameHeaderSize+5 : frameHeaderSize+5]
	out, err := protoMarshalAppend(dst, msg)
	if err != nil {
		return false, err
	}
	if len(out) != pSize {
		return false, fmt.Errorf("writeProtoToRing: size mismatch: %d vs %d", pSize, len(out))
	}

	return true, res.Commit(total)
}

// marshalBufPool reuses large marshal buffers for writeProtoCopyToRing.
// Reduces GC pressure for repeated large message writes (e.g., 256MB).
var marshalBufPool = sync.Pool{}

// writeProtoCopyToRing marshals a proto.Message to a heap buffer, then writes
// it to the ring via writeFrame. For large payloads that exceed half the ring
// capacity, uses chunked writes with MORE flag so the reader can consume
// pieces while the writer continues.
func writeProtoCopyToRing(ctx context.Context, tx *ShmRing, streamID uint32, msg proto.Message) error {
	pSize := proto.Size(msg)
	needed := 5 + pSize

	// Try to get a pooled buffer for large messages.
	var buf []byte
	if needed > 64*1024 {
		if pooled, ok := marshalBufPool.Get().(*[]byte); ok && cap(*pooled) >= needed {
			buf = (*pooled)[:5]
		}
	}
	if buf == nil {
		buf = make([]byte, 5, needed)
	}

	// gRPC 5-byte header
	buf[0] = 0 // no compression
	binary.BigEndian.PutUint32(buf[1:5], uint32(pSize))

	var err error
	buf, err = proto.MarshalOptions{UseCachedSize: true}.MarshalAppend(buf, msg)
	if err != nil {
		if needed > 64*1024 {
			marshalBufPool.Put(&buf)
		}
		return err
	}

	fh := FrameHeader{
		Type:     FrameTypeMESSAGE,
		StreamID: streamID,
	}

	// For large payloads, chunk into pieces that fit in the ring.
	// maxChunk = ring capacity / 8. Smaller chunks enable reader/writer
	// pipelining: the reader can start processing chunk N while the writer
	// is still writing chunk N+1. With cap/2, the ring only holds 2 chunks,
	// forcing serial reader/writer execution. With cap/8, the ring holds 8
	// chunks, allowing up to 4 chunks of concurrent overlap. This reduces
	// futex wait time dramatically for large payloads (especially on Windows
	// where each WaitOnAddress/cgocall costs ~40µs of CPU).
	// Multi-frame chunks (MORE flag) do NOT use speculative zero-copy reads,
	// so the cap/3 constraint does not apply here.
	// Benchmarked cap/4 (16MB) vs cap/8 (8MB): no consistent throughput
	// advantage, and cap/4 causes intermittent data corruption when
	// payload ≈ ring capacity due to reduced pipeline safety margin.
	maxChunk := int(tx.Capacity()) / 8
	if maxChunk < 1024 {
		maxChunk = 1024
	}

	if len(buf) <= maxChunk {
		err = writeFrame(ctx, tx, fh, buf)
		if needed > 64*1024 {
			marshalBufPool.Put(&buf)
		}
		return err
	}

	// Chunked write with MORE flag.
	// Per-chunk signals are needed so the reader consumes chunks and
	// frees ring space for subsequent chunks (pipelining).
	remaining := buf
	for len(remaining) > 0 {
		chunk := remaining
		if len(chunk) > maxChunk {
			chunk = remaining[:maxChunk]
		}
		remaining = remaining[len(chunk):]

		chunkFH := fh
		if len(remaining) > 0 {
			chunkFH.Flags |= MessageFlagMORE
		}
		if err := writeFrame(ctx, tx, chunkFH, chunk); err != nil {
			if needed > 64*1024 {
				marshalBufPool.Put(&buf)
			}
			return err
		}
	}
	if needed > 64*1024 {
		marshalBufPool.Put(&buf)
	}
	return nil
}
