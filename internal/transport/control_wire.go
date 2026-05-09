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

package transport

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	shmControlSuffix = "_ctl"
	// controlWireV1 was the version emitted by the legacy
	// (Custom16-or-HTTP/2-negotiable) wire. Servers running v1 expect
	// the connectRequest to optionally carry a supportedWireFormats
	// list at byte 18+; clients running v1 expect the connectResponse
	// to optionally carry a selectedWire byte after the segment name.
	// We retain the constant so v2 decoders can produce a clear error
	// when an old peer dials in.
	controlWireV1 = uint8(1)
	// controlWireV2 drops Custom16 entirely. The connectRequest /
	// connectResponse no longer carry wire-format negotiation fields.
	// Bumping the version forces clean handshake-time rejection
	// instead of silent data-plane corruption when an old v1 peer
	// (which would write 16-byte Custom16 frames or expect a
	// selectedWire byte) tries to talk to a v2 peer (which only
	// understands H2 9-byte data frames).
	controlWireV2 = uint8(2)
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
	// v2 baseline (18 bytes): version(1) + ringA(8) + ringB(8) + flags(1).
	// v2 dropped the optional supportedWireFormats list that v1 carried
	// at byte 18+; the data plane is now H2-only and no longer
	// negotiates a wire format.
	b := make([]byte, 1+8+8+1)
	b[0] = controlWireV2
	binary.LittleEndian.PutUint64(b[1:9], req.ringA)
	binary.LittleEndian.PutUint64(b[9:17], req.ringB)
	if req.singleStreamMode {
		b[17] = 1
	}
	return b
}

func decodeConnectRequest(b []byte) (connectRequest, error) {
	if len(b) < 1 {
		return connectRequest{}, errors.New("connect request too short")
	}
	switch b[0] {
	case controlWireV2:
		// proceed below
	case controlWireV1:
		return connectRequest{}, errors.New("connect request: peer is on legacy v1 control protocol (Custom16 wire format); upgrade the peer to v2 (HTTP/2 only)")
	default:
		return connectRequest{}, fmt.Errorf("unsupported connect request version %d", b[0])
	}
	if len(b) < 1+8+8 {
		if len(b) == 1 {
			// Minimal v2 payload: just version, no ring sizes.
			return connectRequest{}, nil
		}
		return connectRequest{}, fmt.Errorf("connect request invalid length %d (need >= 17)", len(b))
	}
	req := connectRequest{
		ringA: binary.LittleEndian.Uint64(b[1:9]),
		ringB: binary.LittleEndian.Uint64(b[9:17]),
	}
	if len(b) > 17 {
		req.singleStreamMode = b[17]&1 != 0
	}
	return req, nil
}

func encodeConnectResponse(resp connectResponse) []byte {
	name := []byte(resp.segmentName)
	// v2 baseline: version(1) + nameLen(4) + name(N). v2 dropped the
	// optional selectedWire byte that v1 appended after the name.
	b := make([]byte, 1+4+len(name))
	b[0] = controlWireV2
	binary.LittleEndian.PutUint32(b[1:5], uint32(len(name)))
	copy(b[5:5+len(name)], name)
	return b
}

func decodeConnectResponse(b []byte) (connectResponse, error) {
	if len(b) < 1+4 {
		return connectResponse{}, errors.New("connect response too short")
	}
	switch b[0] {
	case controlWireV2:
		// proceed below
	case controlWireV1:
		return connectResponse{}, errors.New("connect response: peer is on legacy v1 control protocol (Custom16 wire format); upgrade the peer to v2 (HTTP/2 only)")
	default:
		return connectResponse{}, fmt.Errorf("unsupported connect response version %d", b[0])
	}
	nameLen := int(binary.LittleEndian.Uint32(b[1:5]))
	if nameLen < 0 || len(b[5:]) < nameLen {
		return connectResponse{}, errors.New("connect response name missing")
	}
	return connectResponse{
		segmentName: string(b[5 : 5+nameLen]),
	}, nil
}

func encodeConnectReject(r connectReject) []byte {
	msg := []byte(r.message)
	// version(1) + msgLen(4) + msg
	b := make([]byte, 1+4+len(msg))
	b[0] = controlWireV2
	binary.LittleEndian.PutUint32(b[1:5], uint32(len(msg)))
	copy(b[5:], msg)
	return b
}

func decodeConnectReject(b []byte) (connectReject, error) {
	if len(b) < 1+4 {
		return connectReject{}, errors.New("connect reject too short")
	}
	switch b[0] {
	case controlWireV2:
		// proceed below
	case controlWireV1:
		return connectReject{}, errors.New("connect reject: peer is on legacy v1 control protocol (Custom16 wire format); upgrade the peer to v2 (HTTP/2 only)")
	default:
		return connectReject{}, fmt.Errorf("unsupported connect reject version %d", b[0])
	}
	msgLen := int(binary.LittleEndian.Uint32(b[1:5]))
	if msgLen < 0 || len(b[5:]) < msgLen {
		return connectReject{}, errors.New("connect reject message missing")
	}
	return connectReject{message: string(b[5 : 5+msgLen])}, nil
}
