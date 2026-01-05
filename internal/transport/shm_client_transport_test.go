//go:build linux

package transport

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/mem"
)

// TestClientTransportNewStreamAndWrite tests that NewStream creates a stream
// and that the stream's Write method works via the transport.
func TestClientTransportNewStreamAndWrite(t *testing.T) {
	// Create a segment for testing
	seg, err := CreateSegment("test-client-write", 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer seg.Close()

	// Create client transport
	transport, err := NewShmClientTransport(seg, &shmAddr{s: "client"}, &shmAddr{s: "server"})
	if err != nil {
		t.Fatalf("Failed to create transport: %v", err)
	}
	defer transport.Close(nil)

	// Create a stream
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	callHdr := &CallHdr{
		Host:   "localhost",
		Method: "/test.Service/Method",
	}

	stream, err := transport.NewStream(ctx, callHdr)
	if err != nil {
		t.Fatalf("NewStream failed: %v", err)
	}

	// Verify stream was created with correct ID
	if stream.id != 1 {
		t.Errorf("Expected stream ID 1, got %d", stream.id)
	}

	// Test writing data through ClientStream.Write()
	testData := []byte("Hello, shared memory!")
	bufSlice := mem.BufferSlice{mem.NewBuffer(&testData, nil)}

	// This should now work because we set ct field and implemented write() method
	err = stream.Write(nil, bufSlice, &WriteOptions{Last: false})
	if err != nil {
		t.Fatalf("stream.Write() failed: %v", err)
	}

	t.Log("Successfully created stream and wrote data via ClientStream.Write()")
}

// Simple address type for testing
type shmAddr struct {
	s string
}

func (a *shmAddr) Network() string { return "shm" }
func (a *shmAddr) String() string  { return a.s }
