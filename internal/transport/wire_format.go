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

// WireFormat identifies the on-ring frame encoding negotiated during the
// control-plane CONNECT/ACCEPT handshake. Both encodings share the same
// in-memory FrameHeader / payload model; the codec implementations differ
// only in how those frames are laid out on the ring.
//
// The wire format is fixed for the lifetime of a data segment.
type WireFormat byte

const (
	// WireFormatCustom16 is the legacy 16-byte custom frame header used
	// by grpc-go-shmem prior to gRFC G3 alignment. The default for
	// backward compatibility with peers that do not advertise H2.
	WireFormatCustom16 WireFormat = 0

	// WireFormatHTTP2 is the HTTP/2 frame format (RFC 7540) with a
	// 9-byte header and HPACK header compression. Used to align the
	// SHM transport with the gRPC over HTTP/2 protocol so a single
	// gRFC describes the wire (with SHM only substituting the
	// connection layer).
	WireFormatHTTP2 WireFormat = 1
)

// String returns a human-readable name for the wire format.
func (w WireFormat) String() string {
	switch w {
	case WireFormatCustom16:
		return "custom16"
	case WireFormatHTTP2:
		return "http2"
	default:
		return "unknown"
	}
}

// IsValid reports whether w is a known wire format.
func (w WireFormat) IsValid() bool {
	return w == WireFormatCustom16 || w == WireFormatHTTP2
}
