//go:build linux && (amd64 || arm64)

package shm

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestHandshake(t *testing.T) {
	name := fmt.Sprintf("test-handshake-%d", os.Getpid())
	ringCapA := uint64(4096)
	ringCapB := uint64(4096)

	// Ensure clean state
	RemoveSegment(name)
	defer RemoveSegment(name)

	// Server creates segment
	serverSeg, err := CreateSegment(name, ringCapA, ringCapB)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer serverSeg.Close()

	// Server marks itself ready
	serverSeg.H.SetServerReady(true)

	// Client opens segment
	clientSeg, err := OpenSegment(name)
	if err != nil {
		t.Fatalf("Failed to open segment: %v", err)
	}
	defer clientSeg.Close()

	// Client marks itself ready
	clientSeg.H.SetClientReady(true)

	// Test WaitForServer - should return immediately since server is ready
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err = clientSeg.WaitForServer(ctx)
	if err != nil {
		t.Errorf("WaitForServer failed: %v", err)
	}

	// Test WaitForClient - should return immediately since client is ready
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()

	err = serverSeg.WaitForClient(ctx2)
	if err != nil {
		t.Errorf("WaitForClient failed: %v", err)
	}
}

func TestHandshakeTimeout(t *testing.T) {
	name := fmt.Sprintf("test-handshake-timeout-%d", os.Getpid())
	ringCapA := uint64(4096)
	ringCapB := uint64(4096)

	// Ensure clean state
	RemoveSegment(name)
	defer RemoveSegment(name)

	// Server creates segment but doesn't mark ready
	serverSeg, err := CreateSegment(name, ringCapA, ringCapB)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer serverSeg.Close()

	// Client opens segment
	clientSeg, err := OpenSegment(name)
	if err != nil {
		t.Fatalf("Failed to open segment: %v", err)
	}
	defer clientSeg.Close()

	// Test WaitForServer with timeout - should timeout since server not ready
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = clientSeg.WaitForServer(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("Expected WaitForServer to timeout")
	}
	if err == ErrFutexNotSupported {
		t.Skip("Futex not supported on this platform")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("Expected deadline exceeded, got: %v", err)
	}

	// Verify it actually timed out around the expected time
	if elapsed < 40*time.Millisecond || elapsed > 100*time.Millisecond {
		t.Errorf("Timeout took %v, expected around 50ms", elapsed)
	}
}

func TestShmConn(t *testing.T) {
	name := fmt.Sprintf("test-conn-%d", os.Getpid())
	ringCapA := uint64(4096)
	ringCapB := uint64(4096)

	// Ensure clean state
	RemoveSegment(name)
	defer RemoveSegment(name)

	// Server creates segment
	serverSeg, err := CreateSegment(name, ringCapA, ringCapB)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer serverSeg.Close()

	// Client opens segment
	clientSeg, err := OpenSegment(name)
	if err != nil {
		t.Fatalf("Failed to open segment: %v", err)
	}
	defer clientSeg.Close()

	// Create connections
	serverConn := NewServerConn(serverSeg)
	clientConn := NewClientConn(clientSeg)

	// Test data roundtrip: client writes, server reads
	testData := []byte("Hello, shared memory!")

	// Client writes
	n, err := clientConn.Write(testData)
	if err != nil {
		t.Fatalf("Client write failed: %v", err)
	}
	if n != len(testData) {
		t.Errorf("Client wrote %d bytes, expected %d", n, len(testData))
	}

	// Server reads
	readBuf := make([]byte, len(testData))
	n, err = serverConn.Read(readBuf)
	if err != nil {
		t.Fatalf("Server read failed: %v", err)
	}
	if n != len(testData) {
		t.Errorf("Server read %d bytes, expected %d", n, len(testData))
	}

	if string(readBuf) != string(testData) {
		t.Errorf("Data mismatch: got %q, expected %q", string(readBuf), string(testData))
	}

	// Test reverse direction: server writes, client reads
	responseData := []byte("Response from server")

	// Server writes
	n, err = serverConn.Write(responseData)
	if err != nil {
		t.Fatalf("Server write failed: %v", err)
	}
	if n != len(responseData) {
		t.Errorf("Server wrote %d bytes, expected %d", n, len(responseData))
	}

	// Client reads
	responseBuf := make([]byte, len(responseData))
	n, err = clientConn.Read(responseBuf)
	if err != nil {
		t.Fatalf("Client read failed: %v", err)
	}
	if n != len(responseData) {
		t.Errorf("Client read %d bytes, expected %d", n, len(responseData))
	}

	if string(responseBuf) != string(responseData) {
		t.Errorf("Response mismatch: got %q, expected %q", string(responseBuf), string(responseData))
	}

	// Test connection close
	err = clientConn.Close()
	if err != nil {
		t.Errorf("Client close failed: %v", err)
	}

	err = serverConn.Close()
	if err != nil {
		t.Errorf("Server close failed: %v", err)
	}

	// Test operations after close
	_, err = clientConn.Write([]byte("after close"))
	if err != ErrConnectionClosed {
		t.Errorf("Expected ErrConnectionClosed, got: %v", err)
	}

	_, err = serverConn.Read(make([]byte, 10))
	if err != ErrConnectionClosed {
		t.Errorf("Expected ErrConnectionClosed, got: %v", err)
	}
}

func TestShmConnLargeData(t *testing.T) {
	name := fmt.Sprintf("test-conn-large-%d", os.Getpid())
	ringCapA := uint64(16384) // 16KB
	ringCapB := uint64(16384) // 16KB

	// Ensure clean state
	RemoveSegment(name)
	defer RemoveSegment(name)

	// Server creates segment
	serverSeg, err := CreateSegment(name, ringCapA, ringCapB)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer serverSeg.Close()

	// Client opens segment
	clientSeg, err := OpenSegment(name)
	if err != nil {
		t.Fatalf("Failed to open segment: %v", err)
	}
	defer clientSeg.Close()

	// Create connections
	serverConn := NewServerConn(serverSeg)
	clientConn := NewClientConn(clientSeg)
	defer serverConn.Close()
	defer clientConn.Close()

	// Test with data larger than a single ring buffer entry
	largeData := make([]byte, 8192) // 8KB
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}

	// Client writes large data
	n, err := clientConn.Write(largeData)
	if err != nil {
		t.Fatalf("Client write failed: %v", err)
	}
	if n != len(largeData) {
		t.Errorf("Client wrote %d bytes, expected %d", n, len(largeData))
	}

	// Server reads large data
	readBuf := make([]byte, len(largeData))
	n, err = serverConn.Read(readBuf)
	if err != nil {
		t.Fatalf("Server read failed: %v", err)
	}
	if n != len(largeData) {
		t.Errorf("Server read %d bytes, expected %d", n, len(largeData))
	}

	// Verify data integrity
	for i := 0; i < len(largeData); i++ {
		if readBuf[i] != largeData[i] {
			t.Errorf("Data mismatch at byte %d: got %d, expected %d", i, readBuf[i], largeData[i])
			break
		}
	}
}
