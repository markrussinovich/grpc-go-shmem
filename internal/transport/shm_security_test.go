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
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/credentials"
)

// TestShmSecurityHandshakeSuccess tests successful security handshake
func TestShmSecurityHandshakeSuccess(t *testing.T) {
	segName := fmt.Sprintf("test_security_handshake_%d", time.Now().UnixNano())

	// Create segment
	segment, err := CreateSegment(segName, MinRingCapacity, MinRingCapacity)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer func() {
		segment.Close()
		_ = RemoveSegment(segName)
	}()

	// Create rings
	// Ring A: client->server, Ring B: server->client
	clientTxRing := NewShmRingFromSegment(segment.A, segment.Mem)
	clientRxRing := NewShmRingFromSegment(segment.B, segment.Mem)
	serverRxRing := NewShmRingFromSegment(segment.A, segment.Mem)
	serverTxRing := NewShmRingFromSegment(segment.B, segment.Mem)

	// Create handshakers
	clientHandshaker := &ShmSecurityHandshaker{
		Identity: "client-test-identity",
	}
	serverHandshaker := &ShmSecurityHandshaker{
		Identity: "server-test-identity",
	}

	var wg sync.WaitGroup
	var clientAuthInfo, serverAuthInfo *ShmAuthInfo
	var clientErr, serverErr error

	// Run server handshake in background
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		serverAuthInfo, serverErr = serverHandshaker.ServerHandshake(ctx, serverRxRing, serverTxRing)
	}()

	// Run client handshake
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		clientAuthInfo, clientErr = clientHandshaker.ClientHandshake(ctx, clientRxRing, clientTxRing)
	}()

	wg.Wait()

	// Check results
	if clientErr != nil {
		t.Fatalf("Client handshake failed: %v", clientErr)
	}
	if serverErr != nil {
		t.Fatalf("Server handshake failed: %v", serverErr)
	}

	// Verify auth info
	if clientAuthInfo == nil {
		t.Fatal("Client auth info is nil")
	}
	if serverAuthInfo == nil {
		t.Fatal("Server auth info is nil")
	}

	// Check identities
	if clientAuthInfo.LocalIdentity != "client-test-identity" {
		t.Errorf("Client local identity: got %q, want %q", clientAuthInfo.LocalIdentity, "client-test-identity")
	}
	if clientAuthInfo.RemoteIdentity != "server-test-identity" {
		t.Errorf("Client remote identity: got %q, want %q", clientAuthInfo.RemoteIdentity, "server-test-identity")
	}
	if serverAuthInfo.LocalIdentity != "server-test-identity" {
		t.Errorf("Server local identity: got %q, want %q", serverAuthInfo.LocalIdentity, "server-test-identity")
	}
	if serverAuthInfo.RemoteIdentity != "client-test-identity" {
		t.Errorf("Server remote identity: got %q, want %q", serverAuthInfo.RemoteIdentity, "client-test-identity")
	}

	// Check security level
	if clientAuthInfo.SecurityLevel != credentials.PrivacyAndIntegrity {
		t.Errorf("Client security level: got %v, want %v", clientAuthInfo.SecurityLevel, credentials.PrivacyAndIntegrity)
	}
	if serverAuthInfo.SecurityLevel != credentials.PrivacyAndIntegrity {
		t.Errorf("Server security level: got %v, want %v", serverAuthInfo.SecurityLevel, credentials.PrivacyAndIntegrity)
	}

	// Check auth type
	if clientAuthInfo.AuthType() != "shm" {
		t.Errorf("Client auth type: got %q, want %q", clientAuthInfo.AuthType(), "shm")
	}

	t.Logf("✅ Security handshake successful: client=%s, server=%s", clientAuthInfo.RemoteIdentity, serverAuthInfo.RemoteIdentity)
}

// TestShmSecurityHandshakeIdentityRejected tests that mismatched identity tokens are rejected
func TestShmSecurityHandshakeIdentityRejected(t *testing.T) {
	segName := fmt.Sprintf("test_security_reject_%d", time.Now().UnixNano())

	// Create segment
	segment, err := CreateSegment(segName, MinRingCapacity, MinRingCapacity)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer func() {
		segment.Close()
		_ = RemoveSegment(segName)
	}()

	// Create rings
	clientTxRing := NewShmRingFromSegment(segment.A, segment.Mem)
	clientRxRing := NewShmRingFromSegment(segment.B, segment.Mem)
	serverRxRing := NewShmRingFromSegment(segment.A, segment.Mem)
	serverTxRing := NewShmRingFromSegment(segment.B, segment.Mem)

	// Create handshakers with identity verification
	clientHandshaker := &ShmSecurityHandshaker{
		Identity: "untrusted-client",
	}
	serverHandshaker := &ShmSecurityHandshaker{
		Identity: "server-identity",
		VerifyIdentity: func(remoteIdentity string) error {
			if !strings.HasPrefix(remoteIdentity, "trusted-") {
				return errors.New("identity not trusted")
			}
			return nil
		},
	}

	var wg sync.WaitGroup
	var serverErr error

	// Run server handshake in background
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, serverErr = serverHandshaker.ServerHandshake(ctx, serverRxRing, serverTxRing)
	}()

	// Run client handshake
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = clientHandshaker.ClientHandshake(ctx, clientRxRing, clientTxRing)
	}()

	wg.Wait()

	// Server should reject the connection
	if serverErr == nil {
		t.Fatal("Expected server handshake to fail due to identity verification")
	}
	if !strings.Contains(serverErr.Error(), "identity") {
		t.Errorf("Expected identity-related error, got: %v", serverErr)
	}

	t.Logf("✅ Identity rejection works: %v", serverErr)
}

// TestShmSecurityHandshakeClientRejectsServer tests that client can reject server identity
func TestShmSecurityHandshakeClientRejectsServer(t *testing.T) {
	segName := fmt.Sprintf("test_security_client_reject_%d", time.Now().UnixNano())

	// Create segment
	segment, err := CreateSegment(segName, MinRingCapacity, MinRingCapacity)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer func() {
		segment.Close()
		_ = RemoveSegment(segName)
	}()

	// Create rings
	clientTxRing := NewShmRingFromSegment(segment.A, segment.Mem)
	clientRxRing := NewShmRingFromSegment(segment.B, segment.Mem)
	serverRxRing := NewShmRingFromSegment(segment.A, segment.Mem)
	serverTxRing := NewShmRingFromSegment(segment.B, segment.Mem)

	// Create handshakers - client rejects server
	clientHandshaker := &ShmSecurityHandshaker{
		Identity: "client-identity",
		VerifyIdentity: func(remoteIdentity string) error {
			if remoteIdentity == "malicious-server" {
				return errors.New("server identity rejected")
			}
			return nil
		},
	}
	serverHandshaker := &ShmSecurityHandshaker{
		Identity: "malicious-server",
	}

	var wg sync.WaitGroup
	var clientErr error

	// Run server handshake in background
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = serverHandshaker.ServerHandshake(ctx, serverRxRing, serverTxRing)
	}()

	// Run client handshake
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, clientErr = clientHandshaker.ClientHandshake(ctx, clientRxRing, clientTxRing)
	}()

	wg.Wait()

	// Client should reject the connection
	if clientErr == nil {
		t.Fatal("Expected client handshake to fail due to server identity rejection")
	}
	if !strings.Contains(clientErr.Error(), "identity") {
		t.Errorf("Expected identity-related error, got: %v", clientErr)
	}

	t.Logf("✅ Client rejection works: %v", clientErr)
}

// TestShmAuthInfoValidateAuthority tests that ShmAuthInfo allows any authority
func TestShmAuthInfoValidateAuthority(t *testing.T) {
	authInfo := ShmAuthInfo{
		CommonAuthInfo: credentials.CommonAuthInfo{
			SecurityLevel: credentials.PrivacyAndIntegrity,
		},
		LocalIdentity:  "local",
		RemoteIdentity: "remote",
	}

	// Should accept any authority
	testCases := []string{
		"localhost",
		"example.com",
		"127.0.0.1:8080",
		"",
	}

	for _, authority := range testCases {
		if err := authInfo.ValidateAuthority(authority); err != nil {
			t.Errorf("ValidateAuthority(%q) returned error: %v", authority, err)
		}
	}
}

// TestHandshakeFrameEncoding tests encoding/decoding of handshake frames
func TestHandshakeFrameEncoding(t *testing.T) {
	t.Run("handshake_init", func(t *testing.T) {
		nonce, _ := generateNonce()
		init := handshakeInit{
			version:  handshakeVersion,
			identity: []byte("test-identity"),
			nonce:    nonce,
		}

		encoded := encodeHandshakeInit(init)
		decoded, err := decodeHandshakeInit(encoded)
		if err != nil {
			t.Fatalf("decodeHandshakeInit failed: %v", err)
		}

		if decoded.version != init.version {
			t.Errorf("version: got %d, want %d", decoded.version, init.version)
		}
		if string(decoded.identity) != string(init.identity) {
			t.Errorf("identity: got %q, want %q", decoded.identity, init.identity)
		}
		if decoded.nonce != init.nonce {
			t.Errorf("nonce mismatch")
		}
	})

	t.Run("handshake_resp", func(t *testing.T) {
		nonce, _ := generateNonce()
		resp := handshakeResp{
			version:  handshakeVersion,
			identity: []byte("server-identity"),
			nonce:    nonce,
		}

		encoded := encodeHandshakeResp(resp)
		decoded, err := decodeHandshakeResp(encoded)
		if err != nil {
			t.Fatalf("decodeHandshakeResp failed: %v", err)
		}

		if decoded.version != resp.version {
			t.Errorf("version: got %d, want %d", decoded.version, resp.version)
		}
		if string(decoded.identity) != string(resp.identity) {
			t.Errorf("identity: got %q, want %q", decoded.identity, resp.identity)
		}
	})

	t.Run("handshake_ack", func(t *testing.T) {
		ack := handshakeAck{version: handshakeVersion, status: 0}
		encoded := encodeHandshakeAck(ack)
		decoded, err := decodeHandshakeAck(encoded)
		if err != nil {
			t.Fatalf("decodeHandshakeAck failed: %v", err)
		}

		if decoded.version != ack.version {
			t.Errorf("version: got %d, want %d", decoded.version, ack.version)
		}
		if decoded.status != ack.status {
			t.Errorf("status: got %d, want %d", decoded.status, ack.status)
		}
	})

	t.Run("handshake_fail", func(t *testing.T) {
		fail := handshakeFail{
			version: handshakeVersion,
			code:    HandshakeErrIdentityInvalid,
			message: []byte("identity not trusted"),
		}
		encoded := encodeHandshakeFail(fail)
		decoded, err := decodeHandshakeFail(encoded)
		if err != nil {
			t.Fatalf("decodeHandshakeFail failed: %v", err)
		}

		if decoded.version != fail.version {
			t.Errorf("version: got %d, want %d", decoded.version, fail.version)
		}
		if decoded.code != fail.code {
			t.Errorf("code: got %d, want %d", decoded.code, fail.code)
		}
		if string(decoded.message) != string(fail.message) {
			t.Errorf("message: got %q, want %q", decoded.message, fail.message)
		}
	})
}

// TestDefaultShmHandshaker tests the default handshaker creation
func TestDefaultShmHandshaker(t *testing.T) {
	handshaker := DefaultShmHandshaker()
	if handshaker == nil {
		t.Fatal("DefaultShmHandshaker returned nil")
	}
	if !strings.HasPrefix(handshaker.Identity, "pid:") {
		t.Errorf("Default identity should start with 'pid:', got %q", handshaker.Identity)
	}
}

// TestGenerateNonce tests that nonce generation produces unique values
func TestGenerateNonce(t *testing.T) {
	nonces := make(map[[NonceSize]byte]bool)
	for i := 0; i < 100; i++ {
		nonce, err := generateNonce()
		if err != nil {
			t.Fatalf("generateNonce failed: %v", err)
		}
		if nonces[nonce] {
			t.Fatalf("Duplicate nonce generated")
		}
		nonces[nonce] = true
	}
}
