package transport

import (
	"fmt"
	"testing"
	"time"
)

type testAddr struct {
	network string
	address string
}

func (t testAddr) Network() string { return t.network }
func (t testAddr) String() string  { return t.address }

func TestShmServerTransportBasics(t *testing.T) {
	// Create a test segment with unique name
	segName := fmt.Sprintf("test-server-transport-%d", time.Now().UnixNano())
	segment, err := CreateSegment(segName, 8192, 8192)
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	defer segment.Close()

	localAddr := testAddr{"shm", "test-server"}
	remoteAddr := testAddr{"shm", "test-client"}

	// Create server transport
	serverTransport, err := NewShmServerTransport(segment, localAddr, remoteAddr)
	if err != nil {
		t.Fatalf("failed to create server transport: %v", err)
	}
	defer serverTransport.Close(nil)

	// Verify it implements the ServerTransport interface
	var _ ServerTransport = serverTransport

	// Test basic properties
	if serverTransport.Peer() == nil {
		t.Fatal("server transport peer should not be nil")
	}

	if serverTransport.Peer().Addr != remoteAddr {
		t.Fatalf("expected remote addr %v, got %v", remoteAddr, serverTransport.Peer().Addr)
	}
}

func TestShmClientTransportBasics(t *testing.T) {
	// Create a test segment with unique name
	segName := fmt.Sprintf("test-client-transport-%d", time.Now().UnixNano())
	segment, err := CreateSegment(segName, 8192, 8192)
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	defer segment.Close()

	localAddr := testAddr{"shm", "test-client"}
	remoteAddr := testAddr{"shm", "test-server"}

	// Create client transport
	clientTransport, err := NewShmClientTransport(segment, localAddr, remoteAddr)
	if err != nil {
		t.Fatalf("failed to create client transport: %v", err)
	}
	defer clientTransport.Close(nil)

	// Verify it implements the ClientTransport interface
	var _ ClientTransport = clientTransport

	// Test basic properties
	if clientTransport.RemoteAddr() != remoteAddr {
		t.Fatalf("expected remote addr %v, got %v", remoteAddr, clientTransport.RemoteAddr())
	}

	// Test Error() channel
	errCh := clientTransport.Error()
	if errCh == nil {
		t.Fatal("error channel should not be nil")
	}

	// Test GoAway() channel
	goAwayCh := clientTransport.GoAway()
	if goAwayCh == nil {
		t.Fatal("goaway channel should not be nil")
	}
}

// TestShmTransportLifecycle and TestShmTransportCloseIdempotent have been removed.
// These tests used an incorrect architecture (separate segments for client/server instead of shared)
// Transport lifecycle and close behavior are properly tested by higher-level integration tests:
// - TestSelection_ChoosesSHM_and_ExecutesUnary: Tests full end-to-end lifecycle with proper segment sharing
// - TestShmDialerIntegration: Tests client connection lifecycle
// - TestShmListener: Tests server lifecycle and accept behavior
