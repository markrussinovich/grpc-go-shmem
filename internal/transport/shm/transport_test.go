package shm

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/internal/transport"
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
	var _ transport.ServerTransport = serverTransport

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
	var _ transport.ClientTransport = clientTransport

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

func TestShmTransportLifecycle(t *testing.T) {
	// Create separate segments for each transport to avoid ring conflicts
	serverSegName := fmt.Sprintf("test-transport-lifecycle-server-%d", time.Now().UnixNano())
	serverSegment, err := CreateSegment(serverSegName, 8192, 8192)
	if err != nil {
		t.Fatalf("failed to create server segment: %v", err)
	}
	defer serverSegment.Close()

	clientSegName := fmt.Sprintf("test-transport-lifecycle-client-%d", time.Now().UnixNano())
	clientSegment, err := CreateSegment(clientSegName, 8192, 8192)
	if err != nil {
		t.Fatalf("failed to create client segment: %v", err)
	}
	defer clientSegment.Close()

	localAddr := testAddr{"shm", "test-local"}
	remoteAddr := testAddr{"shm", "test-remote"}

	// Create both transports with separate segments
	serverTransport, err := NewShmServerTransport(serverSegment, localAddr, remoteAddr)
	if err != nil {
		t.Fatalf("failed to create server transport: %v", err)
	}

	clientTransport, err := NewShmClientTransport(clientSegment, remoteAddr, localAddr)
	if err != nil {
		t.Fatalf("failed to create client transport: %v", err)
	}

	// Test HandleStreams doesn't block immediately
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		serverTransport.HandleStreams(ctx, func(s *transport.ServerStream) {
			// Test handler - should not be called in this test
			t.Error("unexpected stream handler call")
		})
	}()

	// Wait for either timeout or completion
	select {
	case <-done:
		// HandleStreams should exit when context is cancelled
	case <-time.After(200 * time.Millisecond):
		t.Fatal("HandleStreams did not exit after context timeout")
	}

	// Close transports
	clientTransport.Close(nil)
	serverTransport.Close(nil)

	// Verify Error channels are closed
	select {
	case <-clientTransport.Error():
		// Should be closed
	case <-time.After(100 * time.Millisecond):
		t.Fatal("client transport error channel should be closed after Close()")
	}
}

func TestShmTransportCloseIdempotent(t *testing.T) {
	// Create a test segment with unique name
	segName := fmt.Sprintf("test-transport-close-idempotent-%d", time.Now().UnixNano())
	segment, err := CreateSegment(segName, 8192, 8192)
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	defer segment.Close()

	localAddr := testAddr{"shm", "test-local"}
	remoteAddr := testAddr{"shm", "test-remote"}

	// Create transport
	clientTransport, err := NewShmClientTransport(segment, localAddr, remoteAddr)
	if err != nil {
		t.Fatalf("failed to create client transport: %v", err)
	}

	// Close multiple times - should not panic
	clientTransport.Close(nil)
	clientTransport.Close(nil)
	clientTransport.Close(nil)

	// GracefulClose after Close should not panic
	clientTransport.GracefulClose()
}
