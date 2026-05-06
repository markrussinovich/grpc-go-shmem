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
	"encoding/binary"
	"fmt"
)

// HTTP/2 frame header (RFC 7540 §4.1):
//
//	+-----------------------------------------------+
//	|                 Length (24)                   |
//	+---------------+---------------+---------------+
//	|   Type (8)    |   Flags (8)   |
//	+-+-------------+---------------+-------------------------------+
//	|R|                 Stream Identifier (31)                      |
//	+=+=============================================================+
const h2FrameHeaderSize = 9

// h2MaxFramePayload is the largest payload allowed in a single H2 frame
// (RFC 7540 §4.2). 24-bit length field maximum.
const h2MaxFramePayload = (1 << 24) - 1

// H2FrameType identifies an HTTP/2 frame type (RFC 7540 §11.2).
type H2FrameType byte

// HTTP/2 frame type constants.
const (
	H2FrameDATA         H2FrameType = 0x0
	H2FrameHEADERS      H2FrameType = 0x1
	H2FramePRIORITY     H2FrameType = 0x2
	H2FrameRSTSTREAM    H2FrameType = 0x3
	H2FrameSETTINGS     H2FrameType = 0x4
	H2FramePUSHPROMISE  H2FrameType = 0x5
	H2FramePING         H2FrameType = 0x6
	H2FrameGOAWAY       H2FrameType = 0x7
	H2FrameWINDOWUPDATE H2FrameType = 0x8
	H2FrameCONTINUATION H2FrameType = 0x9
)

// HTTP/2 frame flags (RFC 7540 §6).
const (
	// DATA flags
	H2FlagEndStream byte = 0x01
	H2FlagPadded    byte = 0x08

	// HEADERS flags (EndStream and Padded shared with DATA)
	H2FlagEndHeaders byte = 0x04
	H2FlagPriority   byte = 0x20

	// SETTINGS flags
	H2FlagAck byte = 0x01

	// PING flags (Ack shared with SETTINGS)
)

// H2ErrorCode is an HTTP/2 error code as defined in RFC 7540 §7.
type H2ErrorCode uint32

// HTTP/2 error code constants.
const (
	H2ErrNoError            H2ErrorCode = 0x0
	H2ErrProtocolError      H2ErrorCode = 0x1
	H2ErrInternalError      H2ErrorCode = 0x2
	H2ErrFlowControlError   H2ErrorCode = 0x3
	H2ErrSettingsTimeout    H2ErrorCode = 0x4
	H2ErrStreamClosed       H2ErrorCode = 0x5
	H2ErrFrameSizeError     H2ErrorCode = 0x6
	H2ErrRefusedStream      H2ErrorCode = 0x7
	H2ErrCancel             H2ErrorCode = 0x8
	H2ErrCompressionError   H2ErrorCode = 0x9
	H2ErrConnectError       H2ErrorCode = 0xA
	H2ErrEnhanceYourCalm    H2ErrorCode = 0xB
	H2ErrInadequateSecurity H2ErrorCode = 0xC
	H2ErrHTTP11Required     H2ErrorCode = 0xD
)

// H2FrameHeader represents the in-memory form of an HTTP/2 frame header.
type H2FrameHeader struct {
	Length   uint32 // 24-bit payload length (excludes 9-byte header)
	Type     H2FrameType
	Flags    byte
	StreamID uint32 // 31-bit stream identifier (high reserved bit always 0)
}

// encodeH2FrameHeaderTo writes the 9-byte HTTP/2 frame header in network
// (big-endian) byte order to dst. Length must fit in 24 bits and StreamID
// in 31 bits; the caller is responsible for these invariants.
func encodeH2FrameHeaderTo(dst *[h2FrameHeaderSize]byte, fh H2FrameHeader) {
	// 24-bit length (big-endian)
	dst[0] = byte(fh.Length >> 16)
	dst[1] = byte(fh.Length >> 8)
	dst[2] = byte(fh.Length)
	dst[3] = byte(fh.Type)
	dst[4] = fh.Flags
	// 32-bit stream id (big-endian) with reserved high bit cleared
	binary.BigEndian.PutUint32(dst[5:9], fh.StreamID&0x7FFFFFFF)
}

// decodeH2FrameHeader parses a 9-byte HTTP/2 frame header from b.
func decodeH2FrameHeader(b []byte) (H2FrameHeader, error) {
	if len(b) < h2FrameHeaderSize {
		return H2FrameHeader{}, fmt.Errorf("h2 frame header: short read (%d bytes, need %d)", len(b), h2FrameHeaderSize)
	}
	length := uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
	if length > h2MaxFramePayload {
		return H2FrameHeader{}, fmt.Errorf("h2 frame header: length %d exceeds max %d", length, h2MaxFramePayload)
	}
	streamID := binary.BigEndian.Uint32(b[5:9]) & 0x7FFFFFFF
	return H2FrameHeader{
		Length:   length,
		Type:     H2FrameType(b[3]),
		Flags:    b[4],
		StreamID: streamID,
	}, nil
}

// String returns a human-readable name for the frame type.
func (t H2FrameType) String() string {
	switch t {
	case H2FrameDATA:
		return "DATA"
	case H2FrameHEADERS:
		return "HEADERS"
	case H2FramePRIORITY:
		return "PRIORITY"
	case H2FrameRSTSTREAM:
		return "RST_STREAM"
	case H2FrameSETTINGS:
		return "SETTINGS"
	case H2FramePUSHPROMISE:
		return "PUSH_PROMISE"
	case H2FramePING:
		return "PING"
	case H2FrameGOAWAY:
		return "GOAWAY"
	case H2FrameWINDOWUPDATE:
		return "WINDOW_UPDATE"
	case H2FrameCONTINUATION:
		return "CONTINUATION"
	default:
		return fmt.Sprintf("unknown(0x%x)", byte(t))
	}
}
