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
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	shmControlSuffix = "_ctl"
	// controlWireVersion is the version byte emitted at the start of
	// every control-plane frame. While the gRFC and both reference
	// implementations remain pre-release, the wire layout is allowed to
	// evolve freely and this byte is held at 1: mismatched-version
	// peers are hard-rejected at the handshake boundary, which is the
	// only behaviour we need during development. Version bumps are
	// reserved for post-release wire-format evolution.
	//
	// Current v1 layout (subject to change before the gRFC is ratified):
	//   - CONNECT carries a Flags byte and an 8-byte per-request
	//     correlation nonce.
	//   - ACCEPT carries a reserved Flags byte and echoes the nonce.
	//   - REJECT echoes the nonce (or zero when CONNECT could not be
	//     decoded).
	// The nonce closes the CONNECT/ACCEPT misbinding race in which a
	// stale response left on the shared Ring B by a previously
	// timed-out dialer could otherwise be mis-consumed by the next
	// dialer (binding it with the wrong peer's singleStreamMode flag).
	controlWireVersion = uint8(1)

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
	nonce            uint64
}

type connectResponse struct {
	segmentName string
	nonce       uint64
}

type connectReject struct {
	message string
	nonce   uint64
}

// newConnectNonce returns a 64-bit random value used to correlate a
// CONNECT with its ACCEPT/REJECT. crypto/rand is used (not math/rand)
// so the nonce is unpredictable, closing any future "guess the nonce"
// vector even though the current threat model only needs uniqueness.
//
// crypto/rand.Read is documented as never failing on supported
// platforms, but the runtime contract is best-effort and an unexpected
// kernel-entropy failure is preferable surfaced as a dial-time error
// than silently producing zero entropy (which would defeat stale-
// response correlation under bug-replay conditions). Callers fail the
// dial on error.
func newConnectNonce() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("shm: crypto/rand.Read failed generating connect nonce: %w", err)
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}

func encodeConnectRequest(req connectRequest) []byte {
	// 28 bytes total:
	//   version(1) + ringA(8) + ringB(8) + flags(1)
	//   + wireFormatCount(1) + wireFormat(1) + nonce(8)
	//
	// The wire-format bytes (count=1, format=H2) are mandatory; the
	// trailing 8-byte nonce correlates the server's ACCEPT/REJECT
	// back to this exact CONNECT.
	b := make([]byte, 1+8+8+1+1+1+8)
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
	binary.LittleEndian.PutUint64(b[20:28], req.nonce)
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

	// Nonce: mandatory 8 bytes after the wire-format advertisement.
	nonceOff := 19 + count
	if len(b) < nonceOff+8 {
		return connectRequest{}, errors.New("connect request missing correlation nonce")
	}
	req.nonce = binary.LittleEndian.Uint64(b[nonceOff : nonceOff+8])
	return req, nil
}

func encodeConnectResponse(resp connectResponse) []byte {
	name := []byte(resp.segmentName)
	// version(1) + nameLen(4) + name(N) + selectedWire(1)=H2
	//   + flags(1) + nonce(8).
	//
	// Flags is reserved (always zero). The trailing 8-byte nonce
	// echoes the CONNECT nonce so the dialer can confirm this ACCEPT
	// answers its own in-flight request.
	b := make([]byte, 1+4+len(name)+1+1+8)
	b[0] = controlWireVersion
	binary.LittleEndian.PutUint32(b[1:5], uint32(len(name)))
	copy(b[5:5+len(name)], name)
	b[5+len(name)] = wireFormatH2
	b[5+len(name)+1] = 0 // reserved flags byte
	binary.LittleEndian.PutUint64(b[5+len(name)+2:5+len(name)+10], resp.nonce)
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
	if nameLen <= 0 {
		// Empty segment name is meaningless on the wire — the dialer
		// has no segment to open. Reject explicitly so a buggy /
		// malicious peer cannot trip a generic "OpenSegment empty
		// name" error path later. Also rejects nameLen<0 which
		// uint32 conversion to int would normally hide as a huge
		// positive value (but len(b[5:]) >= nameLen would still
		// catch oversize).
		return connectResponse{}, errors.New("connect response name length must be > 0")
	}
	if nameLen > maxSegmentNameLen {
		// Defence-in-depth: the segment-name grammar caps at
		// maxSegmentNameLen. Reject early so we do not allocate a
		// >200 B string from peer-controlled input.
		return connectResponse{}, fmt.Errorf("connect response name length %d exceeds max %d", nameLen, maxSegmentNameLen)
	}
	if len(b[5:]) < nameLen {
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
	// Flags byte is mandatory.
	if len(b) <= 5+nameLen+1 {
		return connectResponse{}, errors.New(
			"connect response missing flags byte; server MUST include the reserved flags byte")
	}
	// Nonce: mandatory 8 bytes after the flags byte.
	nonceOff := 5 + nameLen + 2
	if len(b) < nonceOff+8 {
		return connectResponse{}, errors.New("connect response missing correlation nonce")
	}
	// Exact-length check: anything after the nonce is unexpected
	// trailing junk. Reject so a malformed peer cannot smuggle
	// payload past the strict-length contract.
	if len(b) != nonceOff+8 {
		return connectResponse{}, fmt.Errorf("connect response has %d trailing byte(s) after nonce", len(b)-(nonceOff+8))
	}
	// Validate the segment name against the on-wire grammar BEFORE
	// returning it. The dialer trusts the result directly as input
	// to OpenSegment / per-data-segment FD-pass socket name
	// derivation, so an invalid name would surface as a less-helpful
	// error from those lower-level paths and (worst case) admit
	// reserved suffixes such as ".lock" / ".fds.sock" that the
	// transport reserves for its own siblings.
	segName := string(b[5 : 5+nameLen])
	if err := validateSegmentName(segName); err != nil {
		return connectResponse{}, fmt.Errorf("connect response: %w", err)
	}
	return connectResponse{
		segmentName: segName,
		nonce:       binary.LittleEndian.Uint64(b[nonceOff : nonceOff+8]),
	}, nil
}

func encodeConnectReject(r connectReject) []byte {
	msg := []byte(r.message)
	// version(1) + msgLen(4) + msg(N) + nonce(8). The trailing nonce
	// echoes the CONNECT nonce so the dialer can correlate the REJECT
	// to its own request. When the server could not decode the CONNECT
	// (and thus has no nonce) it echoes zero.
	b := make([]byte, 1+4+len(msg)+8)
	b[0] = controlWireVersion
	binary.LittleEndian.PutUint32(b[1:5], uint32(len(msg)))
	copy(b[5:5+len(msg)], msg)
	binary.LittleEndian.PutUint64(b[5+len(msg):5+len(msg)+8], r.nonce)
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
	nonceOff := 5 + msgLen
	if len(b) < nonceOff+8 {
		return connectReject{}, errors.New("connect reject missing correlation nonce")
	}
	return connectReject{
		message: string(b[5 : 5+msgLen]),
		nonce:   binary.LittleEndian.Uint64(b[nonceOff : nonceOff+8]),
	}, nil
}

// peekResponseNonce extracts the echoed CONNECT nonce from an ACCEPT or
// REJECT payload for correlation. The bool is false when the frame type
// carries no nonce or fails to decode; the dialer then stops looping
// and lets its response switch surface the appropriate error rather
// than spinning on an undecodable frame.
func peekResponseNonce(ft FrameType, payload []byte) (uint64, bool) {
	switch ft {
	case FrameTypeACCEPT:
		resp, err := decodeConnectResponse(payload)
		if err != nil {
			return 0, false
		}
		return resp.nonce, true
	case FrameTypeREJECT:
		r, err := decodeConnectReject(payload)
		if err != nil {
			return 0, false
		}
		return r.nonce, true
	default:
		return 0, false
	}
}
