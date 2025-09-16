//go:build linux && (amd64 || arm64)

package shm

import (
	"context"
	"testing"
	"time"
)

// TestStep5Implementation tests the complete Step 5 implementation:
// handshake and ShmConn functionality
func TestStep5Implementation(t *testing.T) {
	name := "test-step5-implementation"
	ringCapA := uint64(8192)
	ringCapB := uint64(8192)

	// 1. Server creates segment
	serverSeg, err := CreateSegment(name, ringCapA, ringCapB)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer serverSeg.Close()

	// 2. Client opens segment 
	clientSeg, err := OpenSegment(name)
	if err != nil {
		t.Fatalf("Failed to open segment: %v", err)
	}
	defer clientSeg.Close()

	// 3. Test handshake protocol
	// Server sets ready
	serverSeg.H.SetServerReady(true)
	
	// Client sets ready
	clientSeg.H.SetClientReady(true)

	// Client waits for server (should be immediate)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if err := clientSeg.WaitForServer(ctx); err != nil {
		t.Fatalf("Client WaitForServer failed: %v", err)
	}

	// Server waits for client (should be immediate)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()

	if err := serverSeg.WaitForClient(ctx2); err != nil {
		t.Fatalf("Server WaitForClient failed: %v", err)
	}

	// 4. Test ShmConn duplex communication
	serverConn := NewServerConn(serverSeg)
	clientConn := NewClientConn(clientSeg)

	// Test client->server communication
	message1 := []byte("Hello from client")
	n, err := clientConn.Write(message1)
	if err != nil {
		t.Fatalf("Client write failed: %v", err)
	}
	if n != len(message1) {
		t.Errorf("Client wrote %d bytes, expected %d", n, len(message1))
	}

	receiveBuf1 := make([]byte, len(message1))
	n, err = serverConn.Read(receiveBuf1)
	if err != nil {
		t.Fatalf("Server read failed: %v", err)
	}
	if n != len(message1) {
		t.Errorf("Server read %d bytes, expected %d", n, len(message1))
	}
	if string(receiveBuf1) != string(message1) {
		t.Errorf("Message mismatch: got %q, expected %q", string(receiveBuf1), string(message1))
	}

	// Test server->client communication
	message2 := []byte("Hello from server")
	n, err = serverConn.Write(message2)
	if err != nil {
		t.Fatalf("Server write failed: %v", err)
	}
	if n != len(message2) {
		t.Errorf("Server wrote %d bytes, expected %d", n, len(message2))
	}

	receiveBuf2 := make([]byte, len(message2))
	n, err = clientConn.Read(receiveBuf2)
	if err != nil {
		t.Fatalf("Client read failed: %v", err)
	}
	if n != len(message2) {
		t.Errorf("Client read %d bytes, expected %d", n, len(message2))
	}
	if string(receiveBuf2) != string(message2) {
		t.Errorf("Response mismatch: got %q, expected %q", string(receiveBuf2), string(message2))
	}

	// 5. Test connection closure
	if err := clientConn.Close(); err != nil {
		t.Errorf("Client close failed: %v", err)
	}

	if err := serverConn.Close(); err != nil {
		t.Errorf("Server close failed: %v", err)
	}

	// Verify operations fail after close
	_, err = clientConn.Write([]byte("after close"))
	if err != ErrConnectionClosed {
		t.Errorf("Expected ErrConnectionClosed after client close, got: %v", err)
	}

	_, err = serverConn.Read(make([]byte, 10))
	if err != ErrConnectionClosed {
		t.Errorf("Expected ErrConnectionClosed after server close, got: %v", err)
	}

	t.Log("✅ Step 5 implementation: handshake and ShmConn working correctly")
}

// TestStep5HandshakeScenarios tests various handshake scenarios
func TestStep5HandshakeScenarios(t *testing.T) {
	testCases := []struct {
		name        string
		serverFirst bool
		expectOk    bool
	}{
		{"server_ready_first", true, true},
		{"client_ready_first", false, true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			segName := "test-handshake-" + tc.name
			
			// Create segments
			serverSeg, err := CreateSegment(segName, 4096, 4096)
			if err != nil {
				t.Fatalf("Failed to create segment: %v", err)
			}
			defer serverSeg.Close()

			clientSeg, err := OpenSegment(segName)
			if err != nil {
				t.Fatalf("Failed to open segment: %v", err)
			}
			defer clientSeg.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()

			if tc.serverFirst {
				// Server marks ready first
				serverSeg.H.SetServerReady(true)
				time.Sleep(5 * time.Millisecond) // Small delay
				clientSeg.H.SetClientReady(true)

				// Both should succeed immediately
				if err := clientSeg.WaitForServer(ctx); err != nil {
					t.Errorf("Client wait failed: %v", err)
				}
				if err := serverSeg.WaitForClient(ctx); err != nil {
					t.Errorf("Server wait failed: %v", err)
				}
			} else {
				// Client marks ready first
				clientSeg.H.SetClientReady(true)
				time.Sleep(5 * time.Millisecond) // Small delay
				serverSeg.H.SetServerReady(true)

				// Both should succeed immediately
				if err := serverSeg.WaitForClient(ctx); err != nil {
					t.Errorf("Server wait failed: %v", err)
				}
				if err := clientSeg.WaitForServer(ctx); err != nil {
					t.Errorf("Client wait failed: %v", err)
				}
			}
		})
	}
}