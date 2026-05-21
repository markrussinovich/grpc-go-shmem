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
	"strings"
	"testing"
)

// TestConnectRequest_RoundTrip exercises the happy path: encode a
// request and decode it, expecting the same fields back.
func TestConnectRequest_RoundTrip(t *testing.T) {
	in := connectRequest{
		ringA:            1 << 20,
		ringB:            2 << 20,
		singleStreamMode: true,
	}
	enc := encodeConnectRequest(in)
	if len(enc) != 20 {
		t.Fatalf("encoded length: got %d want 20", len(enc))
	}
	out, err := decodeConnectRequest(enc)
	if err != nil {
		t.Fatalf("decodeConnectRequest: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

// TestConnectRequest_RejectsLegacyNoExtension verifies that a peer that
// sends an 18-byte CONNECT (no wire-format advertisement, the old
// pre-H2 layout) is rejected at the handshake boundary. Without the
// strict check the server would accept the connection and the data
// plane would start with a wire-format mismatch.
func TestConnectRequest_RejectsLegacyNoExtension(t *testing.T) {
	b := make([]byte, 18)
	b[0] = controlWireVersion
	binary.LittleEndian.PutUint64(b[1:9], 1<<20)
	binary.LittleEndian.PutUint64(b[9:17], 1<<20)
	_, err := decodeConnectRequest(b)
	if err == nil {
		t.Fatal("expected error for missing wire-format advertisement, got nil")
	}
	if !strings.Contains(err.Error(), "wire-format") {
		t.Fatalf("error message should mention wire-format; got %q", err.Error())
	}
}

// TestConnectRequest_RejectsNonH2Advertisement verifies that a peer
// that advertises only a non-H2 wire format (e.g., 0x00 = legacy
// Custom16) is rejected.
func TestConnectRequest_RejectsNonH2Advertisement(t *testing.T) {
	b := make([]byte, 20)
	b[0] = controlWireVersion
	binary.LittleEndian.PutUint64(b[1:9], 1<<20)
	binary.LittleEndian.PutUint64(b[9:17], 1<<20)
	b[18] = 1    // count
	b[19] = 0x00 // Custom16 — no longer supported
	_, err := decodeConnectRequest(b)
	if err == nil {
		t.Fatal("expected error for non-H2 wire format, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP/2") {
		t.Fatalf("error message should mention HTTP/2; got %q", err.Error())
	}
}

// TestConnectRequest_RejectsTruncatedAdvertisement verifies that a
// payload whose declared wire-format count exceeds the bytes available
// is rejected as malformed (defense against a peer that under-runs the
// advertised count).
func TestConnectRequest_RejectsTruncatedAdvertisement(t *testing.T) {
	b := make([]byte, 19)
	b[0] = controlWireVersion
	binary.LittleEndian.PutUint64(b[1:9], 1<<20)
	binary.LittleEndian.PutUint64(b[9:17], 1<<20)
	b[18] = 3 // declares 3 formats but no bytes follow
	_, err := decodeConnectRequest(b)
	if err == nil {
		t.Fatal("expected error for truncated advertisement, got nil")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error message should mention truncated; got %q", err.Error())
	}
}

// TestConnectRequest_RejectsUnknownVersion verifies that a peer
// emitting an unknown version byte is rejected.
func TestConnectRequest_RejectsUnknownVersion(t *testing.T) {
	b := make([]byte, 20)
	b[0] = 0xFF // unknown version
	_, err := decodeConnectRequest(b)
	if err == nil {
		t.Fatal("expected error for unknown version, got nil")
	}
}

// TestConnectResponse_RoundTrip exercises the happy path.
func TestConnectResponse_RoundTrip(t *testing.T) {
	in := connectResponse{segmentName: "shm-foo-bar-baz"}
	enc := encodeConnectResponse(in)
	out, err := decodeConnectResponse(enc)
	if err != nil {
		t.Fatalf("decodeConnectResponse: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

// TestConnectResponse_RejectsLegacyNoExtension verifies that a server
// that responds without the trailing selected-wire byte is rejected
// (would otherwise imply legacy Custom16 selection).
func TestConnectResponse_RejectsLegacyNoExtension(t *testing.T) {
	name := []byte("shm-foo")
	b := make([]byte, 1+4+len(name))
	b[0] = controlWireVersion
	binary.LittleEndian.PutUint32(b[1:5], uint32(len(name)))
	copy(b[5:], name)
	_, err := decodeConnectResponse(b)
	if err == nil {
		t.Fatal("expected error for missing selected-wire byte, got nil")
	}
	if !strings.Contains(err.Error(), "wire-format") {
		t.Fatalf("error message should mention wire-format; got %q", err.Error())
	}
}

// TestConnectResponse_RejectsNonH2Selection verifies that a server
// that selects a non-H2 wire format is rejected.
func TestConnectResponse_RejectsNonH2Selection(t *testing.T) {
	name := []byte("shm-foo")
	b := make([]byte, 1+4+len(name)+1)
	b[0] = controlWireVersion
	binary.LittleEndian.PutUint32(b[1:5], uint32(len(name)))
	copy(b[5:5+len(name)], name)
	b[5+len(name)] = 0x00 // Custom16 selection
	_, err := decodeConnectResponse(b)
	if err == nil {
		t.Fatal("expected error for non-H2 selection, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP/2") {
		t.Fatalf("error message should mention HTTP/2; got %q", err.Error())
	}
}

// TestConnectReject_RoundTrip exercises REJECT happy path.
func TestConnectReject_RoundTrip(t *testing.T) {
	in := connectReject{message: "no streams available"}
	enc := encodeConnectReject(in)
	out, err := decodeConnectReject(enc)
	if err != nil {
		t.Fatalf("decodeConnectReject: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}
