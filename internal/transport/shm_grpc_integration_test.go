//go:build linux || windows

/*
 *
 * Copyright 2025 gRPC authors.
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
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestClientTransport_NewStream_Integration tests the complete flow
// of creating a stream and using standard ClientStream methods
func TestClientTransport_NewStream_Integration(t *testing.T) {
	t.Log("=== Integration Test: ClientTransport.NewStream with ClientStream methods ===")

	// Create shared memory segment
	segmentName := fmt.Sprintf("test_integration_%d", time.Now().UnixNano())
	defer RemoveSegment(segmentName)
	segment, err := CreateSegment(segmentName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	t.Log("Client transport created")

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
	defer RemoveSegment(segmentName)
	segment, err := CreateSegment(segmentName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}

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
	defer RemoveSegment(segmentName)
	serverSeg, err := CreateSegment(segmentName, 128*1024, 128*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	serverSeg.H.SetServerReady(true)

	clientSeg, err := OpenSegment(segmentName)
	if err != nil {
		t.Fatalf("Failed to open segment for client: %v", err)
	}

	// Create client transport
	clientAddr := &ShmAddr{Name: segmentName + "_client"}
	serverAddr := &ShmAddr{Name: segmentName + "_server"}

	clientTransport, err := NewShmClientTransport(clientSeg, clientAddr, serverAddr)
	if err != nil {
		t.Fatalf("Failed to create client transport: %v", err)
	}
	defer clientTransport.Close(nil)

	// Create server transport
	serverTransport, err := NewShmServerTransport(serverSeg, serverAddr, clientAddr)
	if err != nil {
		t.Fatalf("Failed to create server transport: %v", err)
	}
	defer serverTransport.Close(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Log("Both transports created")

	// Start server handler (HandleStreams starts the server reader)
	go serverTransport.HandleStreams(ctx, func(s *ServerStream) {
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

	// Read response - wait for headers, message, and trailers
	respData, err := stream.Read(1024)
	if err != nil {
		t.Logf("Client: Read returned: %v (may include EOF)", err)
	}
	if respData != nil {
		t.Logf("Client: Received %d bytes of response data", respData.Len())
		respData.Free()
	}

	// Wait for stream to complete
	select {
	case <-stream.Done():
		t.Log("Client: Stream completed")
	case <-ctx.Done():
		t.Fatalf("Client: Timeout waiting for stream completion")
	}

	t.Log("=== Full RPC Integration Test COMPLETED ===")
}

func TestShmDeadlinePropagation(t *testing.T) {
	// Create shared memory segment
	segmentName := fmt.Sprintf("test_deadline_%d", time.Now().UnixNano())
	defer RemoveSegment(segmentName)

	serverSeg, err := CreateSegment(segmentName, 128*1024, 128*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	serverSeg.H.SetServerReady(true)

	clientSeg, err := OpenSegment(segmentName)
	if err != nil {
		t.Fatalf("Failed to open segment for client: %v", err)
	}

	clientAddr := &ShmAddr{Name: segmentName + "_client"}
	serverAddr := &ShmAddr{Name: segmentName + "_server"}

	clientTransport, err := NewShmClientTransport(clientSeg, clientAddr, serverAddr)
	if err != nil {
		t.Fatalf("Failed to create client transport: %v", err)
	}
	defer clientTransport.Close(nil)

	serverTransport, err := NewShmServerTransport(serverSeg, serverAddr, clientAddr)
	if err != nil {
		t.Fatalf("Failed to create server transport: %v", err)
	}
	defer serverTransport.Close(nil)

	type deadlineResult struct {
		ok       bool
		unixNano int64
		ctxErr   error
	}
	gotCh := make(chan deadlineResult, 1)

	serverCtx, serverCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer serverCancel()

	go serverTransport.HandleStreams(serverCtx, func(s *ServerStream) {
		d, ok := s.Context().Deadline()
		<-s.Context().Done()
		gotCh <- deadlineResult{ok: ok, unixNano: d.UnixNano(), ctxErr: s.Context().Err()}
	})

	deadlineDuration := 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadlineDuration)
	defer cancel()
	deadline := time.Now().Add(deadlineDuration)

	callHdr := &CallHdr{Host: "testhost", Method: "/test.Service/Deadline"}
	_, err = clientTransport.NewStream(ctx, callHdr)
	if err != nil {
		t.Fatalf("Client: NewStream failed: %v", err)
	}

	// Wait for the server to observe the deadline and for it to fire.
	var wait time.Duration
	if until := time.Until(deadline); until > 0 {
		wait = until + 500*time.Millisecond
	} else {
		wait = 500 * time.Millisecond
	}

	select {
	case res := <-gotCh:
		if !res.ok {
			t.Fatalf("Server stream context missing deadline")
		}
		wantUnix := deadline.UnixNano()
		diff := res.unixNano - wantUnix
		if diff < 0 {
			diff = -diff
		}
		if diff > int64(5*time.Millisecond) {
			t.Fatalf("Deadline mismatch: got %d want %d (diff=%dns)", res.unixNano, wantUnix, diff)
		}
		if res.ctxErr != context.DeadlineExceeded {
			t.Fatalf("Server context err=%v, want %v", res.ctxErr, context.DeadlineExceeded)
		}
	case <-time.After(wait):
		t.Fatalf("Timed out waiting for server to observe/cancel deadline")
	}
}

func TestShmMetadataPropagation(t *testing.T) {
	segmentName := fmt.Sprintf("test_metadata_%d", time.Now().UnixNano())
	defer RemoveSegment(segmentName)

	serverSeg, err := CreateSegment(segmentName, 128*1024, 128*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	serverSeg.H.SetServerReady(true)

	clientSeg, err := OpenSegment(segmentName)
	if err != nil {
		t.Fatalf("Failed to open segment for client: %v", err)
	}

	clientAddr := &ShmAddr{Name: segmentName + "_client"}
	serverAddr := &ShmAddr{Name: segmentName + "_server"}

	clientTransport, err := NewShmClientTransport(clientSeg, clientAddr, serverAddr)
	if err != nil {
		t.Fatalf("Failed to create client transport: %v", err)
	}
	defer clientTransport.Close(nil)

	serverTransport, err := NewShmServerTransport(serverSeg, serverAddr, clientAddr)
	if err != nil {
		t.Fatalf("Failed to create server transport: %v", err)
	}
	defer serverTransport.Close(nil)

	serverSawOutgoing := make(chan struct{}, 1)
	serverDone := make(chan struct{})

	serverCtx, serverCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer serverCancel()

	go serverTransport.HandleStreams(serverCtx, func(s *ServerStream) {
		defer close(serverDone)
		inMD, _ := metadata.FromIncomingContext(s.Context())
		if got := inMD.Get("x-test"); len(got) != 1 || got[0] != "abc" {
			t.Errorf("Server incoming metadata x-test=%v, want [abc]", got)
			return
		}
		serverSawOutgoing <- struct{}{}

		// Respond with header metadata.
		if err := serverTransport.writeHeader(s, metadata.Pairs("x-resp", "def")); err != nil {
			t.Errorf("Server writeHeader failed: %v", err)
			return
		}
		_ = serverTransport.writeStatus(s, status.New(codes.OK, ""))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("x-test", "abc"))

	stream, err := clientTransport.NewStream(ctx, &CallHdr{Host: "testhost", Method: "/test.Service/Metadata"})
	if err != nil {
		t.Fatalf("Client: NewStream failed: %v", err)
	}

	select {
	case <-serverSawOutgoing:
	case <-ctx.Done():
		t.Fatalf("Timed out waiting for server to observe outgoing metadata: %v", ctx.Err())
	}

	md, err := stream.Header()
	if err != nil {
		t.Fatalf("Client Header() error: %v", err)
	}
	if got := md.Get("x-resp"); len(got) != 1 || got[0] != "def" {
		t.Fatalf("Client header x-resp=%v, want [def]", got)
	}

	// Ensure the stream finishes cleanly.
	<-stream.Done()

	// Wait for server handler to finish before closing transport
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatalf("Timed out waiting for server handler to complete: %v", ctx.Err())
	}
}

func TestShmContentTypeAndEncodingNegotiation(t *testing.T) {
	segmentName := fmt.Sprintf("test_ct_enc_%d", time.Now().UnixNano())
	defer RemoveSegment(segmentName)

	serverSeg, err := CreateSegment(segmentName, 128*1024, 128*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	serverSeg.H.SetServerReady(true)

	clientSeg, err := OpenSegment(segmentName)
	if err != nil {
		t.Fatalf("Failed to open segment for client: %v", err)
	}

	clientAddr := &ShmAddr{Name: segmentName + "_client"}
	serverAddr := &ShmAddr{Name: segmentName + "_server"}

	clientTransport, err := NewShmClientTransport(clientSeg, clientAddr, serverAddr)
	if err != nil {
		t.Fatalf("Failed to create client transport: %v", err)
	}
	defer clientTransport.Close(nil)

	serverTransport, err := NewShmServerTransport(serverSeg, serverAddr, clientAddr)
	if err != nil {
		t.Fatalf("Failed to create server transport: %v", err)
	}
	defer serverTransport.Close(nil)

	serverSaw := make(chan struct{}, 1)
	serverDone := make(chan struct{})

	serverCtx, serverCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer serverCancel()

	go serverTransport.HandleStreams(serverCtx, func(s *ServerStream) {
		defer close(serverDone)
		if got := s.ContentSubtype(); got != "proto" {
			t.Errorf("Server ContentSubtype=%q, want %q", got, "proto")
			return
		}
		if got := s.RecvCompress(); got != "gzip" {
			t.Errorf("Server RecvCompress=%q, want %q", got, "gzip")
			return
		}
		serverSaw <- struct{}{}

		// Respond with content-type and grpc-encoding.
		if err := serverTransport.writeHeader(s, metadata.Pairs("content-type", "application/grpc+proto", "grpc-encoding", "gzip")); err != nil {
			t.Errorf("Server writeHeader failed: %v", err)
			return
		}
		_ = serverTransport.writeStatus(s, status.New(codes.OK, ""))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := clientTransport.NewStream(ctx, &CallHdr{Host: "testhost", Method: "/test.Service/Negotiation", ContentSubtype: "proto", SendCompress: "gzip"})
	if err != nil {
		t.Fatalf("Client: NewStream failed: %v", err)
	}

	select {
	case <-serverSaw:
	case <-ctx.Done():
		t.Fatalf("Timed out waiting for server to receive stream: %v", ctx.Err())
	}

	if _, err := stream.Header(); err != nil {
		t.Fatalf("Client Header() error: %v", err)
	}
	if got := stream.RecvCompress(); got != "gzip" {
		t.Fatalf("Client RecvCompress=%q, want %q", got, "gzip")
	}
	<-stream.Done()

	// Wait for server handler to finish before closing transport
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatalf("Timed out waiting for server handler to complete: %v", ctx.Err())
	}
}

func TestShmServerDrain(t *testing.T) {
	segmentName := fmt.Sprintf("test_server_drain_%d", time.Now().UnixNano())
	defer RemoveSegment(segmentName)

	serverSeg, err := CreateSegment(segmentName, 128*1024, 128*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	serverSeg.H.SetServerReady(true)

	clientSeg, err := OpenSegment(segmentName)
	if err != nil {
		t.Fatalf("Failed to open segment for client: %v", err)
	}

	clientAddr := &ShmAddr{Name: segmentName + "_client"}
	serverAddr := &ShmAddr{Name: segmentName + "_server"}

	clientTransport, err := NewShmClientTransport(clientSeg, clientAddr, serverAddr)
	if err != nil {
		t.Fatalf("Failed to create client transport: %v", err)
	}
	defer clientTransport.Close(nil)

	serverTransport, err := NewShmServerTransport(serverSeg, serverAddr, clientAddr)
	if err != nil {
		t.Fatalf("Failed to create server transport: %v", err)
	}
	defer serverTransport.Close(nil)

	serverSaw := make(chan *ServerStream, 1)
	allowReply := make(chan struct{})

	serverCtx, serverCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer serverCancel()

	go serverTransport.HandleStreams(serverCtx, func(s *ServerStream) {
		serverSaw <- s
		<-allowReply

		if err := serverTransport.writeHeader(s, metadata.Pairs("x-drain", "ok")); err != nil {
			t.Errorf("Server writeHeader failed: %v", err)
			return
		}
		_ = serverTransport.writeStatus(s, status.New(codes.OK, ""))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := clientTransport.NewStream(ctx, &CallHdr{Host: "testhost", Method: "/test.Service/Drain"})
	if err != nil {
		t.Fatalf("Client: NewStream failed: %v", err)
	}

	select {
	case <-serverSaw:
	case <-ctx.Done():
		t.Fatalf("Timed out waiting for server to receive stream: %v", ctx.Err())
	}

	serverTransport.Drain("test")

	select {
	case <-clientTransport.GoAway():
	case <-ctx.Done():
		t.Fatalf("Timed out waiting for GOAWAY after Drain: %v", ctx.Err())
	}
	reason, debugMsg := clientTransport.GetGoAwayReason()
	if reason != GoAwayNoReason {
		t.Fatalf("GetGoAwayReason()=%v, want %v (debug=%q)", reason, GoAwayNoReason, debugMsg)
	}
	if debugMsg == "" {
		t.Fatalf("GetGoAwayReason() debug message empty")
	}

	if _, err := clientTransport.NewStream(ctx, &CallHdr{Host: "testhost", Method: "/test.Service/AfterDrain"}); err == nil {
		t.Fatalf("NewStream succeeded after Drain, want error")
	}

	close(allowReply)

	md, err := stream.Header()
	if err != nil {
		t.Fatalf("Client Header() error: %v", err)
	}
	if got := md.Get("x-drain"); len(got) != 1 || got[0] != "ok" {
		t.Fatalf("Client header x-drain=%v, want [ok]", got)
	}

	select {
	case <-stream.Done():
	case <-ctx.Done():
		t.Fatalf("Timed out waiting for drained stream to finish: %v", ctx.Err())
	}
}

func TestShmTrailerMetadataPropagation(t *testing.T) {
	segmentName := fmt.Sprintf("test_trailer_%d", time.Now().UnixNano())
	defer RemoveSegment(segmentName)

	serverSeg, err := CreateSegment(segmentName, 128*1024, 128*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	serverSeg.H.SetServerReady(true)

	clientSeg, err := OpenSegment(segmentName)
	if err != nil {
		t.Fatalf("Failed to open segment for client: %v", err)
	}

	clientAddr := &ShmAddr{Name: segmentName + "_client"}
	serverAddr := &ShmAddr{Name: segmentName + "_server"}

	clientTransport, err := NewShmClientTransport(clientSeg, clientAddr, serverAddr)
	if err != nil {
		t.Fatalf("Failed to create client transport: %v", err)
	}
	defer clientTransport.Close(nil)

	serverTransport, err := NewShmServerTransport(serverSeg, serverAddr, clientAddr)
	if err != nil {
		t.Fatalf("Failed to create server transport: %v", err)
	}
	defer serverTransport.Close(nil)

	serverSaw := make(chan struct{}, 1)
	serverDone := make(chan struct{})

	serverCtx, serverCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer serverCancel()

	go serverTransport.HandleStreams(serverCtx, func(s *ServerStream) {
		defer close(serverDone)
		serverSaw <- struct{}{}
		if err := s.SetTrailer(metadata.Pairs("x-trailer", "tv")); err != nil {
			t.Errorf("Server SetTrailer failed: %v", err)
			return
		}
		_ = serverTransport.writeStatus(s, status.New(codes.OK, ""))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := clientTransport.NewStream(ctx, &CallHdr{Host: "testhost", Method: "/test.Service/Trailer"})
	if err != nil {
		t.Fatalf("Client: NewStream failed: %v", err)
	}

	select {
	case <-serverSaw:
	case <-ctx.Done():
		t.Fatalf("Timed out waiting for server to receive stream: %v", ctx.Err())
	}

	select {
	case <-stream.Done():
	case <-ctx.Done():
		t.Fatalf("Timed out waiting for stream to finish: %v", ctx.Err())
	}

	// Wait for server handler to complete writing before checking trailer
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatalf("Timed out waiting for server handler to complete: %v", ctx.Err())
	}

	md := stream.Trailer()
	if got := md.Get("x-trailer"); len(got) != 1 || got[0] != "tv" {
		t.Fatalf("Client trailer x-trailer=%v, want [tv]", got)
	}
}

func TestShmServerCloseTerminatesActiveStreams(t *testing.T) {
	segmentName := fmt.Sprintf("test_server_close_%d", time.Now().UnixNano())
	defer RemoveSegment(segmentName)

	serverSeg, err := CreateSegment(segmentName, 128*1024, 128*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	serverSeg.H.SetServerReady(true)

	clientSeg, err := OpenSegment(segmentName)
	if err != nil {
		t.Fatalf("Failed to open segment for client: %v", err)
	}

	clientAddr := &ShmAddr{Name: segmentName + "_client"}
	serverAddr := &ShmAddr{Name: segmentName + "_server"}

	clientTransport, err := NewShmClientTransport(clientSeg, clientAddr, serverAddr)
	if err != nil {
		t.Fatalf("Failed to create client transport: %v", err)
	}
	defer clientTransport.Close(nil)

	serverTransport, err := NewShmServerTransport(serverSeg, serverAddr, clientAddr)
	if err != nil {
		t.Fatalf("Failed to create server transport: %v", err)
	}
	defer serverTransport.Close(nil)

	started := make(chan struct{})
	readErrCh := make(chan error, 1)

	serverCtx, serverCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer serverCancel()

	go serverTransport.HandleStreams(serverCtx, func(s *ServerStream) {
		close(started)
		_, err := s.Read(1)
		readErrCh <- err
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = clientTransport.NewStream(ctx, &CallHdr{Host: "testhost", Method: "/test.Service/Close"})
	if err != nil {
		t.Fatalf("Client: NewStream failed: %v", err)
	}

	select {
	case <-started:
	case <-ctx.Done():
		t.Fatalf("Timed out waiting for server handler to start: %v", ctx.Err())
	}

	serverTransport.Close(status.Error(codes.Unavailable, "server closing"))

	select {
	case err := <-readErrCh:
		if err == nil {
			t.Fatalf("Server stream Read returned nil error after Close")
		}
	case <-ctx.Done():
		t.Fatalf("Timed out waiting for server stream Read to unblock: %v", ctx.Err())
	}
}
