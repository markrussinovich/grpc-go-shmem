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
	controlWireV1    = uint8(1)
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
	// supportedWireFormats lists the wire formats the client is willing to
	// use, in preference order. Empty means the client only supports the
	// legacy default (Custom16). Negotiated by the server which picks one
	// from this list and echoes it back in connectResponse.selectedWire.
	supportedWireFormats []WireFormat
}

type connectResponse struct {
	segmentName string
	// selectedWire is the wire format the server picked from the client's
	// supportedWireFormats list. Defaults to Custom16 for backward
	// compatibility with peers that don't advertise.
	selectedWire WireFormat
}

type connectReject struct {
	message string
}

func encodeConnectRequest(req connectRequest) []byte {
	// v1 baseline (18 bytes): version(1) + ringA(8) + ringB(8) + flags(1).
	// Optional v1 extension (advertised wire formats, backward compatible):
	//   wireFormatCount(1) + wireFormats(N)
	extLen := 0
	if len(req.supportedWireFormats) > 0 {
		extLen = 1 + len(req.supportedWireFormats)
	}
	b := make([]byte, 1+8+8+1+extLen)
	b[0] = controlWireV1
	binary.LittleEndian.PutUint64(b[1:9], req.ringA)
	binary.LittleEndian.PutUint64(b[9:17], req.ringB)
	if req.singleStreamMode {
		b[17] = 1
	}
	if extLen > 0 {
		b[18] = byte(len(req.supportedWireFormats))
		for i, w := range req.supportedWireFormats {
			b[19+i] = byte(w)
		}
	}
	return b
}

func decodeConnectRequest(b []byte) (connectRequest, error) {
	if len(b) < 1 {
		return connectRequest{}, errors.New("connect request too short")
	}
	if b[0] != controlWireV1 {
		return connectRequest{}, fmt.Errorf("unsupported connect request version %d", b[0])
	}
	if len(b) < 1+8+8 {
		if len(b) == 1 {
			// Minimal v1 payload: just version, no ring sizes.
			return connectRequest{}, nil
		}
		return connectRequest{}, fmt.Errorf("connect request invalid length %d (need >= 17)", len(b))
	}
	// Accept >= 17 bytes for forward compatibility: .NET sends 18 bytes
	// with a flags byte at offset 17. Go also sends 18 bytes now.
	req := connectRequest{
		ringA: binary.LittleEndian.Uint64(b[1:9]),
		ringB: binary.LittleEndian.Uint64(b[9:17]),
	}
	if len(b) > 17 {
		req.singleStreamMode = b[17]&1 != 0
	}
	// Optional wire-format extension at offset 18+. Strict validation:
	// a peer that advertises N formats but doesn't supply N bytes is
	// malformed (don't silently truncate).
	if len(b) > 18 {
		count := int(b[18])
		if count > 0 {
			if len(b) < 19+count {
				return connectRequest{}, fmt.Errorf("connect request truncated: declared %d wire formats but only %d byte(s) of advertisement", count, len(b)-19)
			}
			req.supportedWireFormats = make([]WireFormat, 0, count)
			for i := 0; i < count; i++ {
				w := WireFormat(b[19+i])
				if !w.IsValid() {
					return connectRequest{}, fmt.Errorf("connect request advertises unknown wire format 0x%x at index %d", byte(w), i)
				}
				req.supportedWireFormats = append(req.supportedWireFormats, w)
			}
		}
	}
	return req, nil
}

func encodeConnectResponse(resp connectResponse) []byte {
	name := []byte(resp.segmentName)
	// v1 baseline: version(1) + nameLen(4) + name(N).
	// Optional v1 extension (backward compatible): selectedWireFormat(1).
	// Always emit the extension so peers that understand it can read the
	// negotiated format; legacy peers that stop after nameLen+name
	// silently ignore the trailing byte.
	b := make([]byte, 1+4+len(name)+1)
	b[0] = controlWireV1
	binary.LittleEndian.PutUint32(b[1:5], uint32(len(name)))
	copy(b[5:5+len(name)], name)
	b[5+len(name)] = byte(resp.selectedWire)
	return b
}

func decodeConnectResponse(b []byte) (connectResponse, error) {
	if len(b) < 1+4 {
		return connectResponse{}, errors.New("connect response too short")
	}
	if b[0] != controlWireV1 {
		return connectResponse{}, fmt.Errorf("unsupported connect response version %d", b[0])
	}
	nameLen := int(binary.LittleEndian.Uint32(b[1:5]))
	if nameLen < 0 || len(b[5:]) < nameLen {
		return connectResponse{}, errors.New("connect response name missing")
	}
	resp := connectResponse{
		segmentName:  string(b[5 : 5+nameLen]),
		selectedWire: WireFormatCustom16, // default for legacy peers
	}
	// Optional selectedWireFormat byte after name.
	if len(b) >= 5+nameLen+1 {
		w := WireFormat(b[5+nameLen])
		if !w.IsValid() {
			return connectResponse{}, fmt.Errorf("connect response selected unknown wire format 0x%x", byte(w))
		}
		resp.selectedWire = w
	}
	return resp, nil
}

func encodeConnectReject(r connectReject) []byte {
	msg := []byte(r.message)
	// version(1) + msgLen(4) + msg
	b := make([]byte, 1+4+len(msg))
	b[0] = controlWireV1
	binary.LittleEndian.PutUint32(b[1:5], uint32(len(msg)))
	copy(b[5:], msg)
	return b
}

func decodeConnectReject(b []byte) (connectReject, error) {
	if len(b) < 1+4 {
		return connectReject{}, errors.New("connect reject too short")
	}
	if b[0] != controlWireV1 {
		return connectReject{}, fmt.Errorf("unsupported connect reject version %d", b[0])
	}
	msgLen := int(binary.LittleEndian.Uint32(b[1:5]))
	if msgLen < 0 || len(b[5:]) < msgLen {
		return connectReject{}, errors.New("connect reject message missing")
	}
	return connectReject{message: string(b[5 : 5+msgLen])}, nil
}
