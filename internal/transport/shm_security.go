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
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc/credentials"
)

// Security handshake frame types (range 0x20-0x2F reserved for security)
const (
	FrameTypeHandshakeInit FrameType = 0x20 // Client -> Server: initiate handshake
	FrameTypeHandshakeResp FrameType = 0x21 // Server -> Client: handshake response
	FrameTypeHandshakeAck  FrameType = 0x22 // Client -> Server: handshake acknowledgement
	FrameTypeHandshakeFail FrameType = 0x23 // Either direction: handshake failure
)

// Handshake protocol version
const (
	handshakeVersion = uint8(1)

	// NonceSize is the size of the nonce in bytes
	NonceSize = 16

	// MaxIdentitySize is the maximum size of an identity token
	MaxIdentitySize = 256

	// HandshakeTimeout is the default timeout for security handshake
	HandshakeTimeout = 5 * time.Second
)

// HandshakeError represents a security handshake error
type HandshakeError struct {
	Code    HandshakeErrorCode
	Message string
}

// HandshakeErrorCode defines handshake failure reasons
type HandshakeErrorCode uint8

// Handshake error codes for shared memory security handshake.
const (
	// HandshakeErrNone indicates no error occurred.
	HandshakeErrNone            HandshakeErrorCode = 0
	HandshakeErrVersionMismatch HandshakeErrorCode = 1
	HandshakeErrIdentityInvalid HandshakeErrorCode = 2
	HandshakeErrNonceMismatch   HandshakeErrorCode = 3
	HandshakeErrTimeout         HandshakeErrorCode = 4
	HandshakeErrInternal        HandshakeErrorCode = 5
)

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("handshake error %d: %s", e.Code, e.Message)
}

// ShmAuthInfo contains authentication information for shared memory connections.
// It implements credentials.AuthInfo.
type ShmAuthInfo struct {
	credentials.CommonAuthInfo
	// LocalIdentity is the identity of the local process
	LocalIdentity string
	// RemoteIdentity is the identity of the remote process
	RemoteIdentity string
	// Nonce is the challenge nonce used in the handshake
	Nonce [NonceSize]byte
}

// AuthType returns the type of ShmAuthInfo
func (s ShmAuthInfo) AuthType() string {
	return "shm"
}

// ValidateAuthority allows any authority override for shm connections
func (s ShmAuthInfo) ValidateAuthority(_ string) error {
	return nil
}

// handshakeInit is the initial handshake message from client to server
type handshakeInit struct {
	version  uint8
	identity []byte          // Client identity token
	nonce    [NonceSize]byte // Random challenge nonce
}

// handshakeResp is the server's response to the handshake init
type handshakeResp struct {
	version  uint8
	identity []byte          // Server identity token
	nonce    [NonceSize]byte // Echo client nonce + server nonce
}

// handshakeAck is the client's acknowledgement of successful handshake
type handshakeAck struct {
	version uint8
	status  uint8 // 0 = success
}

// handshakeFail is sent when handshake fails
type handshakeFail struct {
	version uint8
	code    HandshakeErrorCode
	message []byte
}

// generateNonce generates a random nonce for handshake
func generateNonce() ([NonceSize]byte, error) {
	var nonce [NonceSize]byte
	_, err := rand.Read(nonce[:])
	return nonce, err
}

// encodeHandshakeInit encodes a handshake init message
func encodeHandshakeInit(init handshakeInit) []byte {
	// version(1) + identityLen(2) + identity + nonce(16)
	identityLen := len(init.identity)
	if identityLen > MaxIdentitySize {
		identityLen = MaxIdentitySize
	}
	b := make([]byte, 1+2+identityLen+NonceSize)
	b[0] = init.version
	binary.LittleEndian.PutUint16(b[1:3], uint16(identityLen))
	copy(b[3:3+identityLen], init.identity[:identityLen])
	copy(b[3+identityLen:], init.nonce[:])
	return b
}

// decodeHandshakeInit decodes a handshake init message
func decodeHandshakeInit(b []byte) (handshakeInit, error) {
	if len(b) < 1+2+NonceSize {
		return handshakeInit{}, errors.New("handshake init too short")
	}
	version := b[0]
	if version != handshakeVersion {
		return handshakeInit{}, fmt.Errorf("unsupported handshake version %d", version)
	}
	identityLen := int(binary.LittleEndian.Uint16(b[1:3]))
	if identityLen > MaxIdentitySize || len(b) < 3+identityLen+NonceSize {
		return handshakeInit{}, errors.New("invalid identity length")
	}
	init := handshakeInit{
		version:  version,
		identity: make([]byte, identityLen),
	}
	copy(init.identity, b[3:3+identityLen])
	copy(init.nonce[:], b[3+identityLen:3+identityLen+NonceSize])
	return init, nil
}

// encodeHandshakeResp encodes a handshake response message
func encodeHandshakeResp(resp handshakeResp) []byte {
	// version(1) + identityLen(2) + identity + nonce(16)
	identityLen := len(resp.identity)
	if identityLen > MaxIdentitySize {
		identityLen = MaxIdentitySize
	}
	b := make([]byte, 1+2+identityLen+NonceSize)
	b[0] = resp.version
	binary.LittleEndian.PutUint16(b[1:3], uint16(identityLen))
	copy(b[3:3+identityLen], resp.identity[:identityLen])
	copy(b[3+identityLen:], resp.nonce[:])
	return b
}

// decodeHandshakeResp decodes a handshake response message
func decodeHandshakeResp(b []byte) (handshakeResp, error) {
	if len(b) < 1+2+NonceSize {
		return handshakeResp{}, errors.New("handshake response too short")
	}
	version := b[0]
	if version != handshakeVersion {
		return handshakeResp{}, fmt.Errorf("unsupported handshake version %d", version)
	}
	identityLen := int(binary.LittleEndian.Uint16(b[1:3]))
	if identityLen > MaxIdentitySize || len(b) < 3+identityLen+NonceSize {
		return handshakeResp{}, errors.New("invalid identity length")
	}
	resp := handshakeResp{
		version:  version,
		identity: make([]byte, identityLen),
	}
	copy(resp.identity, b[3:3+identityLen])
	copy(resp.nonce[:], b[3+identityLen:3+identityLen+NonceSize])
	return resp, nil
}

// encodeHandshakeAck encodes a handshake acknowledgement
func encodeHandshakeAck(ack handshakeAck) []byte {
	return []byte{ack.version, ack.status}
}

// decodeHandshakeAck decodes a handshake acknowledgement
func decodeHandshakeAck(b []byte) (handshakeAck, error) {
	if len(b) < 2 {
		return handshakeAck{}, errors.New("handshake ack too short")
	}
	return handshakeAck{version: b[0], status: b[1]}, nil
}

// encodeHandshakeFail encodes a handshake failure message
func encodeHandshakeFail(fail handshakeFail) []byte {
	// version(1) + code(1) + msgLen(2) + msg
	msgLen := len(fail.message)
	b := make([]byte, 1+1+2+msgLen)
	b[0] = fail.version
	b[1] = uint8(fail.code)
	binary.LittleEndian.PutUint16(b[2:4], uint16(msgLen))
	copy(b[4:], fail.message)
	return b
}

// decodeHandshakeFail decodes a handshake failure message
func decodeHandshakeFail(b []byte) (handshakeFail, error) {
	if len(b) < 4 {
		return handshakeFail{}, errors.New("handshake fail too short")
	}
	msgLen := int(binary.LittleEndian.Uint16(b[2:4]))
	if len(b) < 4+msgLen {
		return handshakeFail{}, errors.New("handshake fail message truncated")
	}
	return handshakeFail{
		version: b[0],
		code:    HandshakeErrorCode(b[1]),
		message: b[4 : 4+msgLen],
	}, nil
}

// ShmSecurityHandshaker handles security handshake for shm transport
type ShmSecurityHandshaker struct {
	// Identity is the local identity token to use in handshake
	Identity string
	// VerifyIdentity is an optional function to validate remote identity
	// Returns nil if identity is valid, error otherwise
	VerifyIdentity func(remoteIdentity string) error
}

// ClientHandshake performs the client-side security handshake
func (h *ShmSecurityHandshaker) ClientHandshake(ctx context.Context, ring *ShmRing, txRing *ShmRing) (*ShmAuthInfo, error) {
	// Generate nonce
	nonce, err := generateNonce()
	if err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	// Send handshake init
	init := handshakeInit{
		version:  handshakeVersion,
		identity: []byte(h.Identity),
		nonce:    nonce,
	}
	if err := writeFrame(ctx, txRing, FrameHeader{Type: FrameTypeHandshakeInit}, encodeHandshakeInit(init)); err != nil {
		return nil, fmt.Errorf("failed to send handshake init: %w", err)
	}

	// Wait for response
	fh, payload, err := readFrame(ctx, ring)
	if err != nil {
		return nil, fmt.Errorf("failed to read handshake response: %w", err)
	}

	switch fh.Type {
	case FrameTypeHandshakeResp:
		resp, err := decodeHandshakeResp(payload)
		if err != nil {
			return nil, err
		}

		// Verify server identity if verifier is configured
		remoteIdentity := string(resp.identity)
		if h.VerifyIdentity != nil {
			if err := h.VerifyIdentity(remoteIdentity); err != nil {
				// Send failure
				fail := handshakeFail{
					version: handshakeVersion,
					code:    HandshakeErrIdentityInvalid,
					message: []byte(err.Error()),
				}
				_ = writeFrame(ctx, txRing, FrameHeader{Type: FrameTypeHandshakeFail}, encodeHandshakeFail(fail))
				return nil, fmt.Errorf("server identity verification failed: %w", err)
			}
		}

		// Send acknowledgement
		ack := handshakeAck{version: handshakeVersion, status: 0}
		if err := writeFrame(ctx, txRing, FrameHeader{Type: FrameTypeHandshakeAck}, encodeHandshakeAck(ack)); err != nil {
			return nil, fmt.Errorf("failed to send handshake ack: %w", err)
		}

		return &ShmAuthInfo{
			CommonAuthInfo: credentials.CommonAuthInfo{
				SecurityLevel: credentials.PrivacyAndIntegrity, // Shm is same-machine, considered private
			},
			LocalIdentity:  h.Identity,
			RemoteIdentity: remoteIdentity,
			Nonce:          nonce,
		}, nil

	case FrameTypeHandshakeFail:
		fail, _ := decodeHandshakeFail(payload)
		return nil, &HandshakeError{Code: fail.code, Message: string(fail.message)}

	default:
		return nil, fmt.Errorf("unexpected frame type during handshake: %d", fh.Type)
	}
}

// ServerHandshake performs the server-side security handshake
func (h *ShmSecurityHandshaker) ServerHandshake(ctx context.Context, ring *ShmRing, txRing *ShmRing) (*ShmAuthInfo, error) {
	// Wait for handshake init
	fh, payload, err := readFrame(ctx, ring)
	if err != nil {
		return nil, fmt.Errorf("failed to read handshake init: %w", err)
	}

	if fh.Type != FrameTypeHandshakeInit {
		return nil, fmt.Errorf("expected handshake init, got frame type %d", fh.Type)
	}

	init, err := decodeHandshakeInit(payload)
	if err != nil {
		fail := handshakeFail{
			version: handshakeVersion,
			code:    HandshakeErrInternal,
			message: []byte(err.Error()),
		}
		_ = writeFrame(ctx, txRing, FrameHeader{Type: FrameTypeHandshakeFail}, encodeHandshakeFail(fail))
		return nil, err
	}

	// Verify client identity if verifier is configured
	remoteIdentity := string(init.identity)
	if h.VerifyIdentity != nil {
		if err := h.VerifyIdentity(remoteIdentity); err != nil {
			fail := handshakeFail{
				version: handshakeVersion,
				code:    HandshakeErrIdentityInvalid,
				message: []byte(err.Error()),
			}
			_ = writeFrame(ctx, txRing, FrameHeader{Type: FrameTypeHandshakeFail}, encodeHandshakeFail(fail))
			return nil, fmt.Errorf("client identity verification failed: %w", err)
		}
	}

	// Generate server nonce and send response
	serverNonce, err := generateNonce()
	if err != nil {
		return nil, fmt.Errorf("failed to generate server nonce: %w", err)
	}

	resp := handshakeResp{
		version:  handshakeVersion,
		identity: []byte(h.Identity),
		nonce:    serverNonce,
	}
	if err := writeFrame(ctx, txRing, FrameHeader{Type: FrameTypeHandshakeResp}, encodeHandshakeResp(resp)); err != nil {
		return nil, fmt.Errorf("failed to send handshake response: %w", err)
	}

	// Wait for acknowledgement
	fh, payload, err = readFrame(ctx, ring)
	if err != nil {
		return nil, fmt.Errorf("failed to read handshake ack: %w", err)
	}

	switch fh.Type {
	case FrameTypeHandshakeAck:
		ack, err := decodeHandshakeAck(payload)
		if err != nil {
			return nil, err
		}
		if ack.status != 0 {
			return nil, fmt.Errorf("handshake failed with status %d", ack.status)
		}

		return &ShmAuthInfo{
			CommonAuthInfo: credentials.CommonAuthInfo{
				SecurityLevel: credentials.PrivacyAndIntegrity,
			},
			LocalIdentity:  h.Identity,
			RemoteIdentity: remoteIdentity,
			Nonce:          init.nonce,
		}, nil

	case FrameTypeHandshakeFail:
		fail, _ := decodeHandshakeFail(payload)
		return nil, &HandshakeError{Code: fail.code, Message: string(fail.message)}

	default:
		return nil, fmt.Errorf("unexpected frame type during handshake: %d", fh.Type)
	}
}

// DefaultShmHandshaker returns a handshaker with default process identity
func DefaultShmHandshaker() *ShmSecurityHandshaker {
	return &ShmSecurityHandshaker{
		Identity: fmt.Sprintf("pid:%d", os.Getpid()),
	}
}
