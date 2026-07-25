//go:build linux || windows

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

package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	shmControlSuffix = "_ctl"
	// controlWireVersion is the version byte emitted at the start of
	// every control-plane frame. v2 introduces a Flags byte on CONNECT
	// (currently carrying SINGLE_STREAM bit 0) and an echoed Flags byte
	// on ACCEPT after the selected-wire byte so the server can confirm
	// or override the client's mode preference.
	//
	// v1 (legacy) peers without the ACCEPT Flags byte are rejected
	// at the handshake boundary because the bumped version forces a
	// matched-version handshake. The grpc-go and grpc-dotnet
	// implementations are still pre-1.0 so a breaking wire change is
	// acceptable.
	controlWireVersion = uint8(2)

	// wireFormatH2 is the on-wire byte for the HTTP/2 data plane.
	// Matches grpc-dotnet-shm's ControlWire.ProtocolWireHttp2. The
	// historical 0x00 byte (Custom16) is rejected.
	wireFormatH2 = uint8(1)

	// CONNECT / ACCEPT Flags bits.
	//
	// Bit 0 (SINGLE_STREAM_MODE) opts the connection into the
	// single-stream fast path on both sides (inline writes, writer-
	// loop bypass via inlineMu.TryLock, cachedStream MRU dispatch).
	// Bit 1 is RESERVED (must be 0): earlier drafts used it to opt
	// into HTTP/2-compatible flow control, but that mode is now the
	// only profile and the bit no longer carries semantics.
	connectFlagSingleStream uint8 = 1 << 0
)

// Control-plane frame types (used only on the control segment).
const (
	FrameTypeCONNECT FrameType = 0x10
	FrameTypeACCEPT  FrameType = 0x11
	FrameTypeREJECT  FrameType = 0x12
)

type connectRequest struct {
	ringA            uint64
	ringB            uint64
	singleStreamMode bool
}

type connectResponse struct {
	segmentName string
}

type connectReject struct {
	message string
}

func encodeConnectRequest(req connectRequest) []byte {
	// 20 bytes total:
	//   version(1) + ringA(8) + ringB(8) + flags(1)
	//   + wireFormatCount(1) + wireFormat(1)
	//
	// The trailing 2 bytes (count=1, format=H2) are mandatory on the
	// wire; the .NET peer rejects connections that omit them.
	b := make([]byte, 1+8+8+1+1+1)
	b[0] = controlWireVersion
	binary.LittleEndian.PutUint64(b[1:9], req.ringA)
	binary.LittleEndian.PutUint64(b[9:17], req.ringB)
	var flags uint8
	if req.singleStreamMode {
		flags |= connectFlagSingleStream
	}
	b[17] = flags
	b[18] = 1            // wireFormatCount
	b[19] = wireFormatH2 // only H2 is advertised
	return b
}

func decodeConnectRequest(b []byte) (connectRequest, error) {
	if len(b) < 1 {
		return connectRequest{}, errors.New("connect request too short")
	}
	if b[0] != controlWireVersion {
		return connectRequest{}, fmt.Errorf("unsupported connect request version %d (this peer speaks v%d)", b[0], controlWireVersion)
	}
	if len(b) < 1+8+8 {
		return connectRequest{}, fmt.Errorf("connect request invalid length %d (need >= 17)", len(b))
	}
	req := connectRequest{
		ringA: binary.LittleEndian.Uint64(b[1:9]),
		ringB: binary.LittleEndian.Uint64(b[9:17]),
	}
	if len(b) > 17 {
		flags := b[17]
		req.singleStreamMode = flags&connectFlagSingleStream != 0
	}

	// Wire-format advertisement is mandatory: the peer must explicitly
	// declare H2 support. A legacy Custom16-only peer (which omits the
	// extension entirely) is rejected at the handshake boundary so the
	// data plane cannot start with a wire-format mismatch.
	if len(b) <= 18 {
		return connectRequest{}, errors.New(
			"connect request missing wire-format advertisement; peer must support HTTP/2")
	}
	count := int(b[18])
	if count == 0 {
		return connectRequest{}, errors.New(
			"connect request advertises zero wire formats; peer must support HTTP/2")
	}
	if len(b) < 19+count {
		return connectRequest{}, fmt.Errorf(
			"connect request truncated: declared %d wire formats but only %d byte(s) of advertisement available",
			count, len(b)-19)
	}
	sawH2 := false
	for i := 0; i < count; i++ {
		if b[19+i] == wireFormatH2 {
			sawH2 = true
			break
		}
	}
	if !sawH2 {
		return connectRequest{}, errors.New(
			"connect request does not advertise HTTP/2; legacy Custom16-only peers are not supported")
	}
	return req, nil
}

func encodeConnectResponse(resp connectResponse) []byte {
	name := []byte(resp.segmentName)
	// version(1) + nameLen(4) + name(N) + selectedWire(1)=H2 + flags(1).
	//
	// The selected-wire byte and Flags byte are both mandatory in v2
	// of the control wire format. Flags is currently always zero; bit 1
	// was reserved earlier for the HTTP/2 flow-control mode flag, which
	// has been removed (HTTP/2-compatible flow control is the only
	// profile and is unconditional).
	b := make([]byte, 1+4+len(name)+1+1)
	b[0] = controlWireVersion
	binary.LittleEndian.PutUint32(b[1:5], uint32(len(name)))
	copy(b[5:5+len(name)], name)
	b[5+len(name)] = wireFormatH2
	b[5+len(name)+1] = 0 // reserved flags byte
	return b
}

func decodeConnectResponse(b []byte) (connectResponse, error) {
	if len(b) < 1+4 {
		return connectResponse{}, errors.New("connect response too short")
	}
	if b[0] != controlWireVersion {
		return connectResponse{}, fmt.Errorf("unsupported connect response version %d (this peer speaks v%d)", b[0], controlWireVersion)
	}
	nameLen := int(binary.LittleEndian.Uint32(b[1:5]))
	if nameLen < 0 || len(b[5:]) < nameLen {
		return connectResponse{}, errors.New("connect response name missing")
	}
	// Selected-wire byte is mandatory; legacy responses without it
	// would imply Custom16 selection which is no longer supported.
	if len(b) <= 5+nameLen {
		return connectResponse{}, errors.New(
			"connect response missing wire-format byte; server must select HTTP/2")
	}
	selected := b[5+nameLen]
	if selected != wireFormatH2 {
		return connectResponse{}, fmt.Errorf(
			"connect response selects wire format 0x%02x, expected HTTP/2 (0x%02x)",
			selected, wireFormatH2)
	}
	// Flags byte is mandatory in v2.
	if len(b) <= 5+nameLen+1 {
		return connectResponse{}, errors.New(
			"connect response missing flags byte; v2 servers MUST include the reserved flags byte")
	}
	// Flags bit 1 was the HTTP/2 flow-control mode flag in earlier drafts;
	// it is now reserved and ignored.
	return connectResponse{
		segmentName: string(b[5 : 5+nameLen]),
	}, nil
}

func encodeConnectReject(r connectReject) []byte {
	msg := []byte(r.message)
	// version(1) + msgLen(4) + msg. No wire-format byte on REJECT —
	// the handshake never reached the negotiation step, and .NET does
	// not emit one either.
	b := make([]byte, 1+4+len(msg))
	b[0] = controlWireVersion
	binary.LittleEndian.PutUint32(b[1:5], uint32(len(msg)))
	copy(b[5:], msg)
	return b
}

func decodeConnectReject(b []byte) (connectReject, error) {
	if len(b) < 1+4 {
		return connectReject{}, errors.New("connect reject too short")
	}
	if b[0] != controlWireVersion {
		return connectReject{}, fmt.Errorf("unsupported connect reject version %d", b[0])
	}
	msgLen := int(binary.LittleEndian.Uint32(b[1:5]))
	if msgLen < 0 || len(b[5:]) < msgLen {
		return connectReject{}, errors.New("connect reject message missing")
	}
	return connectReject{message: string(b[5 : 5+msgLen])}, nil
}
