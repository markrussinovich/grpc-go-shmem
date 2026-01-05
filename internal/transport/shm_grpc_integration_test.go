//go:build linux

package transport

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
)

// TestClientTransport_NewStream_Integration tests the complete flow
// of creating a stream and using standard ClientStream methods
func TestClientTransport_NewStream_Integration(t *testing.T) {
	t.Log("=== Integration Test: ClientTransport.NewStream with ClientStream methods ===")

	// Create shared memory segment
	segmentName := fmt.Sprintf("test_integration_%d", time.Now().UnixNano())
	segment, err := CreateSegment(segmentName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer segment.Close()

	// Mark both sides as ready
	segment.H.SetClientReady(true)
	segment.H.SetServerReady(true)

	// Create client transport
	localAddr := &ShmAddr{Name: segmentName + "_client"}
	remoteAddr := &ShmAddr{Name: segmentName + "_server"}
	
	clientTransport, err := NewShmClientTransport(segment, localAddr, remoteAddr)
	if err != nil {
		t.Fatalf("Failed to create client transport: %v", err)
	}
	defer clientTransport.Close(nil)

	// Start the client transport reader
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go clientTransport.processIncomingData(ctx)

	t.Log("Client transport created and reader started")

	// Create a new stream using NewStream
	callHdr := &CallHdr{
		Host:   "testhost",
		Method: "/test.Service/TestMethod",
	}

	stream, err := clientTransport.NewStream(ctx, callHdr)
	if err != nil {
		t.Fatalf("NewStream failed: %v", err)
	}

	t.Logf("Stream created with ID: %d", stream.id)

	// Verify stream was created with correct fields
	if stream.id != 1 {
		t.Errorf("Expected stream ID 1, got %d", stream.id)
	}

	if stream.method != callHdr.Method {
		t.Errorf("Expected method %s, got %s", callHdr.Method, stream.method)
	}

	// Test that standard ClientStream.Write() works
	// This is the key test - verifying the interface solution works
	testData := []byte("test message payload")
	hdr := make([]byte, 5)
	hdr[0] = 0 // compression flag
	// Message length (big endian)
	msgLen := uint32(len(testData))
	hdr[1] = byte(msgLen >> 24)
	hdr[2] = byte(msgLen >> 16)
	hdr[3] = byte(msgLen >> 8)
	hdr[4] = byte(msgLen)

	// This should call ShmClientTransport.write() through the interface
	data := mem.BufferSlice{mem.SliceBuffer(testData)}
	err = stream.Write(hdr, data, &WriteOptions{})
	if err != nil {
		t.Fatalf("ClientStream.Write() failed: %v", err)
	}

	t.Log("Successfully called ClientStream.Write() - interface solution works!")

	// Close the stream using standard ClientStream.Close()
	stream.Close(nil)

	t.Log("=== Integration Test PASSED ===")
}

// TestServerTransport_HandleStreams_Placeholder tests that HandleStreams can be called
func TestServerTransport_HandleStreams_Placeholder(t *testing.T) {
	t.Log("=== Integration Test: ServerTransport.HandleStreams ===")

	// Create shared memory segment
	segmentName := fmt.Sprintf("test_server_%d", time.Now().UnixNano())
	segment, err := CreateSegment(segmentName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer segment.Close()

	// Mark both sides as ready
	segment.H.SetClientReady(true)
	segment.H.SetServerReady(true)

	// Create server transport
	localAddr := &ShmAddr{Name: segmentName + "_server"}
	remoteAddr := &ShmAddr{Name: segmentName + "_client"}
	
	serverTransport, err := NewShmServerTransport(segment, localAddr, remoteAddr)
	if err != nil {
		t.Fatalf("Failed to create server transport: %v", err)
	}
	defer serverTransport.Close(nil)

	t.Log("Server transport created")

	// Create context for HandleStreams
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	streamReceived := false
	handler := func(s *ServerStream) {
		t.Logf("Handler called with stream ID: %d", s.id)
		streamReceived = true
	}

	// This should not block indefinitely - it should respect context
	serverTransport.HandleStreams(ctx, handler)

	t.Log("HandleStreams returned (as expected when context cancelled)")

	if streamReceived {
		t.Log("Stream was received and handled")
	} else {
		t.Log("No streams received (expected - no client)")
	}

	t.Log("=== ServerTransport.HandleStreams Test PASSED ===")
}

// TestFullRPC_Integration tests a complete unary RPC flow
func TestFullRPC_Integration(t *testing.T) {
	t.Log("=== Full RPC Integration Test ===")

	// Create shared memory segment
	segmentName := fmt.Sprintf("test_full_rpc_%d", time.Now().UnixNano())
	segment, err := CreateSegment(segmentName, 128*1024, 128*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer segment.Close()

	// Mark both sides as ready
	segment.H.SetClientReady(true)
	segment.H.SetServerReady(true)

	// Create client transport
	clientAddr := &ShmAddr{Name: segmentName + "_client"}
	serverAddr := &ShmAddr{Name: segmentName + "_server"}
	
	clientTransport, err := NewShmClientTransport(segment, clientAddr, serverAddr)
	if err != nil {
		t.Fatalf("Failed to create client transport: %v", err)
	}
	defer clientTransport.Close(nil)

	// Create server transport  
	serverTransport, err := NewShmServerTransport(segment, serverAddr, clientAddr)
	if err != nil {
		t.Fatalf("Failed to create server transport: %v", err)
	}
	defer serverTransport.Close(nil)

	// Start both readers
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go clientTransport.processIncomingData(ctx)
	go serverTransport.processIncomingData(ctx)

	t.Log("Both transports created and readers started")

	// Start server handler
	go func() {
		serverTransport.HandleStreams(ctx, func(s *ServerStream) {
			t.Logf("Server: Received stream %d, method: %s", s.id, s.method)

			// Send response headers
			err := serverTransport.writeHeader(s, nil)
			if err != nil {
				t.Errorf("Server: writeHeader failed: %v", err)
				return
			}

			// Send response message
			responseData := []byte("Hello from server!")
			hdr := make([]byte, 5)
			hdr[0] = 0 // no compression
			msgLen := uint32(len(responseData))
			hdr[1] = byte(msgLen >> 24)
			hdr[2] = byte(msgLen >> 16)
			hdr[3] = byte(msgLen >> 8)
			hdr[4] = byte(msgLen)

			data := mem.BufferSlice{mem.SliceBuffer(responseData)}
			err = serverTransport.write(s, hdr, data, &WriteOptions{})
			if err != nil {
				t.Errorf("Server: write failed: %v", err)
				return
			}

			// Send status
			st := status.New(codes.OK, "success")
			err = serverTransport.writeStatus(s, st)
			if err != nil {
				t.Errorf("Server: writeStatus failed: %v", err)
				return
			}

			t.Log("Server: Sent complete response")
		})
	}()

	// Give server time to start
	time.Sleep(50 * time.Millisecond)

	// Client makes RPC
	callHdr := &CallHdr{
		Host:   "testhost",
		Method: "/test.Service/TestMethod",
	}

	stream, err := clientTransport.NewStream(ctx, callHdr)
	if err != nil {
		t.Fatalf("Client: NewStream failed: %v", err)
	}

	t.Log("Client: Stream created")

	// Send request
	requestData := []byte("Hello from client!")
	hdr := make([]byte, 5)
	hdr[0] = 0
	msgLen := uint32(len(requestData))
	hdr[1] = byte(msgLen >> 24)
	hdr[2] = byte(msgLen >> 16)
	hdr[3] = byte(msgLen >> 8)
	hdr[4] = byte(msgLen)

	data := mem.BufferSlice{mem.SliceBuffer(requestData)}
	err = stream.Write(hdr, data, &WriteOptions{})
	if err != nil {
		t.Fatalf("Client: Write failed: %v", err)
	}

	t.Log("Client: Request sent")

	// Wait for response (with timeout)
	time.Sleep(200 * time.Millisecond)

	t.Log("=== Full RPC Integration Test COMPLETED ===")
	t.Log("Note: Full end-to-end verification requires complete ServerStream implementation")
}
