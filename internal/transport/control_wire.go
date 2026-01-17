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
	ringA uint64
	ringB uint64
}

type connectResponse struct {
	segmentName string
}

type connectReject struct {
	message string
}

func encodeConnectRequest(req connectRequest) []byte {
	// v1 CONNECT is intentionally minimal. We keep the ring offset fields for
	// forward-compat, but the current transport always uses the Segment header's
	// ring offsets.
	//
	// version(1) + ringA(8) + ringB(8)
	b := make([]byte, 1+8+8)
	b[0] = controlWireV1
	binary.LittleEndian.PutUint64(b[1:9], req.ringA)
	binary.LittleEndian.PutUint64(b[9:17], req.ringB)
	return b
}

func decodeConnectRequest(b []byte) (connectRequest, error) {
	if len(b) < 1 {
		return connectRequest{}, errors.New("connect request too short")
	}
	if b[0] != controlWireV1 {
		return connectRequest{}, fmt.Errorf("unsupported connect request version %d", b[0])
	}
	// Allow minimal v1 payloads.
	if len(b) == 1 {
		return connectRequest{}, nil
	}
	if len(b) != 1+8+8 {
		return connectRequest{}, errors.New("connect request invalid length")
	}
	return connectRequest{ringA: binary.LittleEndian.Uint64(b[1:9]), ringB: binary.LittleEndian.Uint64(b[9:17])}, nil
}

func encodeConnectResponse(resp connectResponse) []byte {
	name := []byte(resp.segmentName)
	// version(1) + nameLen(4) + name
	b := make([]byte, 1+4+len(name))
	b[0] = controlWireV1
	binary.LittleEndian.PutUint32(b[1:5], uint32(len(name)))
	copy(b[5:], name)
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
	return connectResponse{segmentName: string(b[5 : 5+nameLen])}, nil
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
