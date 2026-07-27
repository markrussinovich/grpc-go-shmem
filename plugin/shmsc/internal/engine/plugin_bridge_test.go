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
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	client "google.golang.org/grpc/experimental/transport/client"
	server "google.golang.org/grpc/experimental/transport/server"
	"google.golang.org/grpc/status"
)

// fakeTC is a minimal credentials.TransportCredentials reporting a configurable
// security protocol, used to prove the fail-closed rejection paths without a
// real TLS setup.
type fakeTC struct{ proto string }

func (f fakeTC) ClientHandshake(context.Context, string, net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, nil
}
func (f fakeTC) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, nil
}
func (f fakeTC) Info() credentials.ProtocolInfo         { return credentials.ProtocolInfo{SecurityProtocol: f.proto} }
func (f fakeTC) Clone() credentials.TransportCredentials { return f }
func (f fakeTC) OverrideServerName(string) error        { return nil }

// fakePerRPC is a minimal per-RPC credential that requires transport security.
type fakePerRPC struct {
	requireSec bool
	err        error
}

func (f fakePerRPC) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return map[string]string{"authorization": "bearer x"}, nil
}
func (f fakePerRPC) RequireTransportSecurity() bool { return f.requireSec }

// TestApplyPerRPCCreds proves per-RPC credentials are applied (not silently
// dropped) for non-secure creds and rejected fail-closed when they require
// transport security on an insecure connection.
func TestApplyPerRPCCreds(t *testing.T) {
	callHdr := &client.CallHdr{Method: "/svc/Method", Authority: "authority"}

	// Non-secure channel-level credential over an insecure connection: applied.
	tr := &shmClientTransport{perRPCCreds: []credentials.PerRPCCredentials{fakePerRPC{requireSec: false}}}
	data, err := tr.applyPerRPCCreds(context.Background(), callHdr)
	if err != nil {
		t.Fatalf("applyPerRPCCreds (non-secure): %v", err)
	}
	if data["authorization"] != "bearer x" {
		t.Errorf("non-secure per-RPC creds not applied: got %v", data)
	}

	// RequireTransportSecurity credential over an insecure connection: rejected.
	tr2 := &shmClientTransport{perRPCCreds: []credentials.PerRPCCredentials{fakePerRPC{requireSec: true}}}
	if _, err := tr2.applyPerRPCCreds(context.Background(), callHdr); err == nil || status.Code(err) != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated for secure creds on insecure conn, got %v", err)
	}

	// Per-call credential is applied.
	callHdr2 := &client.CallHdr{Method: "/svc/Method", Authority: "authority", CallCredentials: fakePerRPC{requireSec: false}}
	data3, err := (&shmClientTransport{}).applyPerRPCCreds(context.Background(), callHdr2)
	if err != nil || data3["authorization"] != "bearer x" {
		t.Errorf("per-call cred not applied: data=%v err=%v", data3, err)
	}

	// No credentials: nil, nil.
	if data, err := (&shmClientTransport{}).applyPerRPCCreds(context.Background(), &client.CallHdr{Method: "/s/m"}); err != nil || data != nil {
		t.Errorf("no creds should return (nil, nil); got (%v, %v)", data, err)
	}

	// gRFC A54: a restricted control-plane code from a credential is normalized to Internal.
	trR := &shmClientTransport{perRPCCreds: []credentials.PerRPCCredentials{fakePerRPC{err: status.Error(codes.FailedPrecondition, "no")}}}
	if _, err := trR.applyPerRPCCreds(context.Background(), callHdr); status.Code(err) != codes.Internal {
		t.Errorf("restricted control-plane code should normalize to Internal, got %v", status.Code(err))
	}
	// An allowed status code (e.g. Unavailable) passes through unchanged.
	trA := &shmClientTransport{perRPCCreds: []credentials.PerRPCCredentials{fakePerRPC{err: status.Error(codes.Unavailable, "retry")}}}
	if _, err := trA.applyPerRPCCreds(context.Background(), callHdr); status.Code(err) != codes.Unavailable {
		t.Errorf("allowed status code should pass through, got %v", status.Code(err))
	}
	// Channel-level plain error -> Unauthenticated; per-call plain error -> Internal.
	trU := &shmClientTransport{perRPCCreds: []credentials.PerRPCCredentials{fakePerRPC{err: errors.New("boom")}}}
	if _, err := trU.applyPerRPCCreds(context.Background(), callHdr); status.Code(err) != codes.Unauthenticated {
		t.Errorf("channel-level plain error should be Unauthenticated, got %v", status.Code(err))
	}
	callHdrErr := &client.CallHdr{Method: "/svc/Method", Authority: "authority", CallCredentials: fakePerRPC{err: errors.New("boom")}}
	if _, err := (&shmClientTransport{}).applyPerRPCCreds(context.Background(), callHdrErr); status.Code(err) != codes.Internal {
		t.Errorf("per-call plain error should be Internal, got %v", status.Code(err))
	}
}

func TestDialClientRejectsRealTransportCredentials(t *testing.T) {
	_, err := DialClient(context.Background(), "unused", client.BuildOptions{
		TransportCredentials: fakeTC{proto: "tls"},
	})
	if err == nil || !strings.Contains(err.Error(), "transport security") {
		t.Fatalf("expected fail-closed rejection of TLS credentials, got err=%v", err)
	}
}

func TestDialClientAcceptsInsecureCredentials(t *testing.T) {
	// An insecure credential must NOT be rejected by the security gate. It will
	// fail later in DialShm (no server segment named "unused"), which is fine —
	// we only assert the error is NOT the security rejection.
	_, err := DialClient(context.Background(), "unused_no_server", client.BuildOptions{
		TransportCredentials: fakeTC{proto: "insecure"},
	})
	if err != nil && strings.Contains(err.Error(), "transport security") {
		t.Fatalf("insecure credentials must pass the security gate, got %v", err)
	}
}

func TestBuildServerRejectsRealCredentials(t *testing.T) {
	_, err := BuildServer(nil, server.BuildOptions{Credentials: fakeTC{proto: "tls"}})
	if err == nil || !strings.Contains(err.Error(), "transport security") {
		t.Fatalf("expected fail-closed rejection of real server credentials, got err=%v", err)
	}
}

// TestServerTransportCloseCleansConnection proves the connection-leak fix:
// closing ONLY the server transport (as grpc-go does after serving, without
// calling the raw conn's Close) must release the accepted connection's
// listener-owned resources — removing it from activeSegments and unlinking the
// segment — via the onClose hook the server Builder wires.
func TestServerTransportCloseCleansConnection(t *testing.T) {
	name := fmt.Sprintf("shmsc_leak_%d", time.Now().UnixNano())
	lis, err := NewShmListener(&ShmAddr{Name: name}, DefaultSegmentSize, DefaultRingASize, DefaultRingBSize)
	if err != nil {
		t.Fatalf("NewShmListener: %v", err)
	}
	defer lis.Close()

	// Dial concurrently; DialShm blocks until the handshake completes, and
	// Accept below drives that handshake.
	dialDone := make(chan *shmClientTransport, 1)
	dialErr := make(chan error, 1)
	go func() {
		dopts := DefaultDialOptions()
		dopts.ConnectTimeout = 10 * time.Second
		ct, derr := DialShm(context.Background(), name, dopts)
		if derr != nil {
			dialErr <- derr
			return
		}
		dialDone <- ct
	}()

	rawConn, err := lis.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	sc, ok := rawConn.(*shmConn)
	if !ok {
		t.Fatalf("Accept returned %T, want *shmConn", rawConn)
	}
	segName := sc.segmentName

	st, err := BuildServer(sc, server.BuildOptions{})
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}

	// The accepted connection is registered before close.
	lis.mu.Lock()
	_, present := lis.activeSegments[segName]
	lis.mu.Unlock()
	if !present {
		t.Fatalf("segment %q not registered in activeSegments after Accept", segName)
	}

	// Close ONLY the transport (mimicking grpc-go's post-serve teardown).
	st.Close(errors.New("test close"))

	lis.mu.Lock()
	_, stillPresent := lis.activeSegments[segName]
	lis.mu.Unlock()
	if stillPresent {
		t.Errorf("LEAK: segment %q still in activeSegments after transport Close alone", segName)
	}
	if !sc.closed.Load() {
		t.Errorf("shmConn not marked closed after transport Close")
	}
	// Idempotency: a subsequent raw conn Close must not panic or double-free.
	if cerr := sc.Close(); cerr != nil {
		t.Errorf("second Close returned %v", cerr)
	}

	// Drain the dialer.
	select {
	case ct := <-dialDone:
		ct.Close(errors.New("test done"))
	case derr := <-dialErr:
		t.Logf("dial (best-effort, not asserted): %v", derr)
	case <-time.After(2 * time.Second):
		t.Log("dialer did not complete within 2s (best-effort)")
	}
}
