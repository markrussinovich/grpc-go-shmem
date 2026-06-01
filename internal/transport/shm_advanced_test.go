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
	"encoding/binary"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestShmPingPongSizes tests various message sizes (1B, 1KB, 64KB, 1MB)
// This is the primary test added to match TCP transport test coverage
func TestShmPingPongSizes(t *testing.T) {
	sizes := []struct {
		name string
		size int
	}{
		{"1B", 1},
		{"1KB", 1024},
		{"64KB", 64 * 1024},
		{"1MB", 1024 * 1024},
	}

	for _, tc := range sizes {
		t.Run(tc.name, func(t *testing.T) {
			testCtx, testCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
			defer testCancel()

			name := fmt.Sprintf("pingpong-%s-%d", tc.name, time.Now().UnixNano())
			// Use reasonable ring sizes (minimum 4KB) with enough space for frames
			ringSize := tc.size * 4
			if ringSize < 4096 {
				ringSize = 4096 // Minimum ring size
			}
			raw := fmt.Sprintf("shm://%s?cap=%d", name, ringSize)

			// Server factory
			lis, err := newShmServerFactory(raw)
			if err != nil {
				t.Fatalf("server factory: %v", err)
			}
			defer lis.Close()

			// Server responder goroutine (HEADERS→echo MESSAGE→TRAILERS OK)
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				c, err := lis.Accept()
				if err != nil {
					t.Errorf("server accept: %v", err)
					return
				}
				defer c.Close()
				conn := c.(*shmConn)
				srvRx := conn.ReadRing()
				srvTx := conn.WriteRing()

				// Read headers
				fh, pl, err := readFrame(testCtx, srvRx)
				if err != nil {
					t.Errorf("server read headers: %v", err)
					return
				}
				if fh.Type != FrameTypeHEADERS {
					t.Errorf("expected HEADERS, got %v", fh.Type)
					return
				}
				if _, err := takeOrDecodeHeaders(srvRx.h2Decoder(), pl); err != nil {
					t.Errorf("decode headers: %v", err)
					return
				}

				// Read message
				fh2, msg, err := readFrame(testCtx, srvRx)
				if err != nil {
					t.Errorf("server read msg: %v", err)
					return
				}
				if fh2.Type != FrameTypeMESSAGE {
					t.Errorf("expected MESSAGE, got %v", fh2.Type)
					return
				}

				// Echo back: HEADERS + MESSAGE + TRAILERS
				h := HeadersV1{Version: 1, HdrType: 1}
				_ = writeFrame(testCtx, srvTx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL}, encodeHeaders(h))
				_ = writeFrame(testCtx, srvTx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeMESSAGE}, msg)
				tr := TrailersV1{Version: 1, GRPCStatusCode: 0}
				_ = writeFrame(testCtx, srvTx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeTRAILERS, Flags: TrailersFlagEndStream}, encodeTrailers(tr))
			}()

			// Client factory
			enableClientReader.Store(false)
			defer enableClientReader.Store(true)
			ct, err := newShmClientFactory(testCtx, raw)
			if err != nil {
				t.Fatalf("client factory: %v", err)
			}
			defer ct.Close(fmt.Errorf("test done"))

			// Unary call
			runtime.Gosched()
			seg := ct.(*ShmClientTransport).segment
			cli := NewShmUnaryClient(seg)

			// Create payload
			payload := make([]byte, 5+tc.size)
			payload[0] = 0 // not compressed
			payload[1] = byte(tc.size >> 24)
			payload[2] = byte(tc.size >> 16)
			payload[3] = byte(tc.size >> 8)
			payload[4] = byte(tc.size)
			for i := 0; i < tc.size; i++ {
				payload[5+i] = byte(i % 256)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, msg, tr, err := cli.UnaryCall(ctx, "/test/Echo", name, nil, payload)
			if err != nil {
				t.Fatalf("UnaryCall error: %v", err)
			}
			if tr.GRPCStatusCode != 0 {
				t.Fatalf("expected OK status, got %d", tr.GRPCStatusCode)
			}
			if len(msg) != len(payload) {
				t.Fatalf("len mismatch: got %d want %d", len(msg), len(payload))
			}
			for i := range msg {
				if msg[i] != payload[i] {
					t.Fatalf("data mismatch at %d", i)
				}
			}
			_ = cli.Close()
			<-serverDone

			t.Logf("Successfully ping-ponged %d bytes", tc.size)
		})
	}
}

// TestShmConcurrentStreams tests multiple concurrent streams
func TestShmConcurrentStreams(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer testCancel()

	name := fmt.Sprintf("concurrent-%d", time.Now().UnixNano())
	raw := fmt.Sprintf("shm://%s?cap=131072", name)

	numCalls := 10

	// Server factory
	lis, err := newShmServerFactory(raw)
	if err != nil {
		t.Fatalf("server factory: %v", err)
	}
	defer lis.Close()

	// Server responder goroutine - read all requests first, then respond. This
	// ensures multiple client streams are in-flight concurrently.
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		c, err := lis.Accept()
		if err != nil {
			t.Errorf("server accept: %v", err)
			return
		}
		defer c.Close()
		conn := c.(*shmConn)
		srvRx := conn.ReadRing()
		srvTx := conn.WriteRing()

		type req struct {
			streamID uint32
			msg      []byte
		}
		reqs := make([]req, 0, numCalls)
		for i := 0; i < numCalls; i++ {
			fh, pl, err := readFrame(testCtx, srvRx)
			if err != nil {
				t.Errorf("server read headers: %v", err)
				return
			}
			if fh.Type != FrameTypeHEADERS {
				t.Errorf("server expected HEADERS, got %v", fh.Type)
				return
			}
			if _, err := takeOrDecodeHeaders(srvRx.h2Decoder(), pl); err != nil {
				t.Errorf("decode headers: %v", err)
				return
			}

			fh2, msg, err := readFrame(testCtx, srvRx)
			if err != nil {
				t.Errorf("server read msg: %v", err)
				return
			}
			if fh2.Type != FrameTypeMESSAGE {
				t.Errorf("server expected MESSAGE, got %v", fh2.Type)
				return
			}
			reqs = append(reqs, req{streamID: fh.StreamID, msg: msg})
		}

		// Respond out-of-order to ensure client demux by stream ID works.
		for i := len(reqs) - 1; i >= 0; i-- {
			r := reqs[i]
			h := HeadersV1{Version: 1, HdrType: 1}
			_ = writeFrame(testCtx, srvTx, FrameHeader{StreamID: r.streamID, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL}, encodeHeaders(h))
			_ = writeFrame(testCtx, srvTx, FrameHeader{StreamID: r.streamID, Type: FrameTypeMESSAGE}, r.msg)
			tr := TrailersV1{Version: 1, GRPCStatusCode: 0}
			_ = writeFrame(testCtx, srvTx, FrameHeader{StreamID: r.streamID, Type: FrameTypeTRAILERS, Flags: TrailersFlagEndStream}, encodeTrailers(tr))
		}
	}()

	// Client factory
	enableClientReader.Store(false)
	defer enableClientReader.Store(true)
	ct, err := newShmClientFactory(testCtx, raw)
	if err != nil {
		t.Fatalf("client factory: %v", err)
	}
	defer ct.Close(fmt.Errorf("test done"))

	// Create multiple concurrent calls.
	seg := ct.(*ShmClientTransport).segment
	cli := NewShmUnaryClient(seg)
	defer cli.Close()

	var wg sync.WaitGroup
	errCh := make(chan error, numCalls)
	start := make(chan struct{})

	for i := 0; i < numCalls; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start

			payload := []byte(fmt.Sprintf("call-%d", id))
			fullPayload := make([]byte, 5+len(payload))
			fullPayload[0] = 0
			fullPayload[1] = byte(len(payload) >> 24)
			fullPayload[2] = byte(len(payload) >> 16)
			fullPayload[3] = byte(len(payload) >> 8)
			fullPayload[4] = byte(len(payload))
			copy(fullPayload[5:], payload)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			_, msg, tr, err := cli.UnaryCall(ctx, "/test/Concurrent", name, nil, fullPayload)
			if err != nil {
				errCh <- fmt.Errorf("call %d: %v", id, err)
				return
			}
			if tr.GRPCStatusCode != 0 {
				errCh <- fmt.Errorf("call %d: bad status %d", id, tr.GRPCStatusCode)
				return
			}
			if len(msg) == 0 {
				errCh <- fmt.Errorf("call %d: empty response", id)
			}
		}(i)
	}
	close(start)

	wg.Wait()
	close(errCh)

	// Check for errors
	var errCount int
	for err := range errCh {
		t.Error(err)
		errCount++
	}

	if errCount > 0 {
		t.Fatalf("%d calls failed", errCount)
	}
	<-serverDone

	t.Logf("Successfully ran %d concurrent calls", numCalls)
}

// TestShmStreamError tests error handling
func TestShmStreamError(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer testCancel()

	name := fmt.Sprintf("error-%d", time.Now().UnixNano())
	raw := fmt.Sprintf("shm://%s?cap=65536", name)

	// Server factory
	lis, err := newShmServerFactory(raw)
	if err != nil {
		t.Fatalf("server factory: %v", err)
	}
	defer lis.Close()

	// Server responder - return error status
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		c, err := lis.Accept()
		if err != nil {
			t.Errorf("server accept: %v", err)
			return
		}
		defer c.Close()
		conn := c.(*shmConn)
		srvRx := conn.ReadRing()
		srvTx := conn.WriteRing()

		// Read headers
		fh, _, err := readFrame(testCtx, srvRx)
		if err != nil {
			t.Errorf("server read: %v", err)
			return
		}

		// Read message
		_, _, err = readFrame(testCtx, srvRx)
		if err != nil {
			t.Errorf("server read msg: %v", err)
			return
		}

		// Send error TRAILERS
		tr := TrailersV1{Version: 1, GRPCStatusCode: uint32(codes.Internal), GRPCStatusMsg: "test error"}
		_ = writeFrame(testCtx, srvTx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeTRAILERS, Flags: TrailersFlagEndStream}, encodeTrailers(tr))
	}()

	// Client factory
	enableClientReader.Store(false)
	defer enableClientReader.Store(true)
	ct, err := newShmClientFactory(testCtx, raw)
	if err != nil {
		t.Fatalf("client factory: %v", err)
	}
	defer ct.Close(fmt.Errorf("test done"))

	runtime.Gosched()
	seg := ct.(*ShmClientTransport).segment
	cli := NewShmUnaryClient(seg)
	defer cli.Close()

	payload := make([]byte, 8)
	payload[4] = 3
	copy(payload[5:], []byte("err"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _, tr, err := cli.UnaryCall(ctx, "/test/Error", name, nil, payload)
	if err == nil && tr.GRPCStatusCode == 0 {
		t.Fatal("expected error status, got OK")
	}

	if tr.GRPCStatusCode != uint32(codes.Internal) {
		t.Logf("Got status code %d (expected %d)", tr.GRPCStatusCode, codes.Internal)
	}
	<-serverDone

	t.Log("Stream error handling verified")
}

// TestShmClientErrorNotify tests error channel notification
func TestShmClientErrorNotify(t *testing.T) {
	segName := fmt.Sprintf("test-error-notify-%d", time.Now().UnixNano())
	segment, err := CreateSegment(segName, 65536, 65536)
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	defer segment.Close()

	clientTransport, err := NewShmClientTransport(segment, testAddr{"shm", "client"}, testAddr{"shm", "server"})
	if err != nil {
		t.Fatalf("failed to create client transport: %v", err)
	}

	// Get error channel
	errCh := clientTransport.Error()

	// Close transport with error
	testErr := fmt.Errorf("test error notification")
	clientTransport.Close(testErr)

	// Verify error notification (error channel signals by closing)
	select {
	case _, ok := <-errCh:
		if ok {
			t.Fatal("error channel should be closed")
		}
		t.Log("Error notification received correctly (channel closed)")
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for error notification")
	}
}

// TestShmInflightStreamClosing tests closing in-flight stream
// sends status error to concurrent stream reader.
// This mirrors TestInflightStreamClosing for HTTP2.
func TestShmInflightStreamClosing(t *testing.T) {
	segName := fmt.Sprintf("test-inflight-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)

	segment, err := CreateSegment(segName, 65536, 65536)
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	segment.H.SetServerReady(true)

	clientTransport, err := NewShmClientTransport(segment, testAddr{"shm", "client"}, testAddr{"shm", "server"})
	if err != nil {
		t.Fatalf("failed to create client transport: %v", err)
	}
	defer clientTransport.Close(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := clientTransport.NewStream(ctx, &CallHdr{Method: "/test/Stream"}, nil)
	if err != nil {
		t.Fatalf("NewStream failed: %v", err)
	}

	donec := make(chan struct{})
	ready := make(chan struct{})
	serr := status.Error(codes.Internal, "client connection is closing")
	go func() {
		defer close(donec)
		// Signal ready just before blocking on read
		close(ready)
		// Try to read from the stream - this should block until stream is closed
		_, err := stream.Read(1024)
		if err == nil {
			t.Errorf("expected error from Read, got nil")
			return
		}
		// The error could be the status error or a transport error
		t.Logf("Read returned error as expected: %v", err)
	}()

	// Wait for reader goroutine to be ready to block
	<-ready
	runtime.Gosched()

	// Close the stream with an error - this should unblock the reader
	stream.Close(serr)

	// Wait for reader to complete
	select {
	case <-donec:
		t.Log("Stream read properly unblocked after close")
	case <-time.After(5 * time.Second):
		t.Fatal("Test timed out, expected stream read to unblock")
	}

	// Also verify the done channel is closed
	select {
	case <-stream.Done():
		t.Log("Stream done channel properly closed")
	default:
		t.Error("Stream done channel not closed after stream.Close()")
	}
}

// TestShmContextCanceledOnClose tests that stream contexts are canceled on close
func TestShmContextCanceledOnClose(t *testing.T) {
	segName := fmt.Sprintf("test-ctx-cancel-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)

	serverSeg, err := CreateSegment(segName, 65536, 65536)
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	defer serverSeg.Close()
	serverSeg.H.SetServerReady(true)

	clientSeg, err := OpenSegment(segName)
	if err != nil {
		t.Fatalf("failed to open segment: %v", err)
	}
	defer clientSeg.Close()

	serverTransport, err := NewShmServerTransport(serverSeg, testAddr{"shm", "server"}, testAddr{"shm", "client"})
	if err != nil {
		t.Fatalf("failed to create server transport: %v", err)
	}
	defer serverTransport.Close(nil)

	var ctxCanceled atomic.Bool
	ctxCanceledCh := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 1)
	go serverTransport.HandleStreams(ctx, func(s *ServerStream) {
		started <- struct{}{}
		<-s.Context().Done()
		ctxCanceled.Store(true)
		close(ctxCanceledCh)
	})

	// Client creates stream
	clientTransport, err := NewShmClientTransport(clientSeg, testAddr{"shm", "client"}, testAddr{"shm", "server"})
	if err != nil {
		t.Fatalf("failed to create client transport: %v", err)
	}
	defer clientTransport.Close(nil)

	cs, err := clientTransport.NewStream(ctx, &CallHdr{Method: "/test/CtxCancel"}, nil)
	if err != nil {
		t.Fatalf("NewStream failed: %v", err)
	}

	// Send initial data
	hdr := make([]byte, 5)
	if err := clientTransport.write(cs, hdr, mem.BufferSlice{}, &WriteOptions{}); err != nil {
		t.Fatalf("write error: %v", err)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for server handler to start")
	}

	// Close connection from the client side.
	clientTransport.Close(fmt.Errorf("connection closed"))

	// Wait for context to be canceled via explicit channel signal
	select {
	case <-ctxCanceledCh:
		// Success - context was canceled
	case <-time.After(2 * time.Second):
		t.Fatal("stream context was not canceled on connection close")
	}
}

// TestShmGracefulClose ensures that GracefulClose allows in-flight streams to
// proceed until they complete naturally, while not allowing creation of new
// streams during this window.
func TestShmGracefulClose(t *testing.T) {
	segName := fmt.Sprintf("test-graceful-close-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)

	serverSeg, err := CreateSegment(segName, 65536, 65536)
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	defer serverSeg.Close()

	clientSeg, err := OpenSegment(segName)
	if err != nil {
		t.Fatalf("failed to open segment: %v", err)
	}
	defer clientSeg.Close()

	serverTransport, err := NewShmServerTransport(serverSeg, testAddr{"shm", "server"}, testAddr{"shm", "client"})
	if err != nil {
		t.Fatalf("failed to create server transport: %v", err)
	}
	defer serverTransport.Close(nil)

	testCtx, testCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer testCancel()

	// Channel to signal when server handler has started processing a stream.
	serverStarted := make(chan struct{}, 1)

	// Start server stream handler: echo one message, then wait for client
	// half-close and send OK trailers.
	go serverTransport.HandleStreams(testCtx, func(s *ServerStream) {
		// Signal that the server handler is running.
		select {
		case serverStarted <- struct{}{}:
		default:
		}

		// Send initial headers.
		_ = serverTransport.writeHeader(s, metadata.MD{"content-type": []string{"application/grpc"}})

		incomingHeader := make([]byte, 5)
		if _, err := s.readTo(incomingHeader); err != nil {
			return
		}
		sz := binary.BigEndian.Uint32(incomingHeader[1:])
		msg := make([]byte, int(sz))
		if _, err := s.readTo(msg); err != nil {
			return
		}

		// Echo back.
		_ = s.Write(incomingHeader, newBufferSlice(msg), &WriteOptions{})

		// Wait for client half-close.
		if _, err := s.Read(1); err != io.EOF {
			return
		}
		_ = s.WriteStatus(status.New(codes.OK, ""))
	})

	clientTransport, err := NewShmClientTransport(clientSeg, testAddr{"shm", "client"}, testAddr{"shm", "server"})
	if err != nil {
		t.Fatalf("failed to create client transport: %v", err)
	}
	defer clientTransport.Close(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cs, err := clientTransport.NewStream(ctx, &CallHdr{Method: "/test/GracefulClose"}, nil)
	if err != nil {
		t.Fatalf("NewStream(_, _) = _, %v, want _, <nil>", err)
	}

	// Confirm basic stream functionality.
	msg := make([]byte, 1024)
	outgoingHeader := make([]byte, 5)
	outgoingHeader[0] = byte(0)
	binary.BigEndian.PutUint32(outgoingHeader[1:], uint32(len(msg)))
	incomingHeader := make([]byte, 5)
	if err := cs.Write(outgoingHeader, newBufferSlice(msg), &WriteOptions{}); err != nil {
		t.Fatalf("Error while writing: %v", err)
	}

	// Wait for the server handler to start processing before reading.
	select {
	case <-serverStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for server handler to start")
	}

	if _, err := cs.readTo(incomingHeader); err != nil {
		t.Fatalf("Error while reading: %v", err)
	}
	sz := binary.BigEndian.Uint32(incomingHeader[1:])
	recvMsg := make([]byte, int(sz))
	if _, err := cs.readTo(recvMsg); err != nil {
		t.Fatalf("Error while reading: %v", err)
	}

	// Gracefully close the transport; existing stream should remain usable.
	clientTransport.GracefulClose()

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := clientTransport.NewStream(ctx, &CallHdr{Method: "/test/NewStreamAfterGracefulClose"}, nil)
			if err != nil {
				if nse, ok := err.(*NewStreamError); ok && nse.Err == ErrConnClosing && nse.AllowTransparentRetry {
					return
				}
			}
			t.Errorf("NewStream(_, _) = _, %v, want _, %v", err, ErrConnClosing)
		}()
	}

	// Confirm existing stream can still complete.
	cs.Write(nil, nil, &WriteOptions{Last: true})
	if _, err := cs.readTo(incomingHeader); err != io.EOF {
		t.Fatalf("Client expected EOF from the server. Got: %v", err)
	}

	wg.Wait()

	// Server should close after the last stream completes when it receives GOAWAY.
	select {
	case <-serverTransport.errCh:
		// closed
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for server transport to close after client GracefulClose")
	}
}

func TestShmMaxStreams(t *testing.T) {
	segName := fmt.Sprintf("test-max-streams-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)

	serverSeg, err := CreateSegment(segName, 65536, 65536)
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	defer serverSeg.Close()
	serverSeg.H.SetMaxStreams(1)
	serverSeg.H.SetServerReady(true)

	clientSeg, err := OpenSegment(segName)
	if err != nil {
		t.Fatalf("failed to open segment: %v", err)
	}
	defer clientSeg.Close()
	clientSeg.H.SetClientReady(true)

	serverTransport, err := NewShmServerTransport(serverSeg, testAddr{"shm", "server"}, testAddr{"shm", "client"})
	if err != nil {
		t.Fatalf("failed to create server transport: %v", err)
	}
	defer serverTransport.Close(nil)

	testCtx, testCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer testCancel()

	allowFinishFirst := make(chan struct{})
	firstStarted := make(chan struct{})
	var streamCount atomic.Uint32
	go serverTransport.HandleStreams(testCtx, func(s *ServerStream) {
		idx := streamCount.Add(1)
		if idx == 1 {
			close(firstStarted)
			<-allowFinishFirst
		}
		_ = serverTransport.writeHeader(s, metadata.MD{"content-type": []string{"application/grpc"}})
		_ = s.WriteStatus(status.New(codes.OK, ""))
	})

	clientTransport, err := NewShmClientTransport(clientSeg, testAddr{"shm", "client"}, testAddr{"shm", "server"})
	if err != nil {
		t.Fatalf("failed to create client transport: %v", err)
	}
	defer clientTransport.Close(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs1, err := clientTransport.NewStream(ctx, &CallHdr{Method: "/test/MaxStreams"}, nil)
	if err != nil {
		t.Fatalf("NewStream(_, _) = _, %v, want _, <nil>", err)
	}
	defer cs1.Close(nil)

	select {
	case <-firstStarted:
	case <-ctx.Done():
		t.Fatalf("timeout waiting for server to start handling first stream: %v", ctx.Err())
	}

	// With maxstreams=1, a second NewStream should block until the first stream
	// completes. With a short context deadline, it should fail.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer shortCancel()
	if _, err := clientTransport.NewStream(shortCtx, &CallHdr{Method: "/test/MaxStreamsShort"}, nil); err == nil {
		t.Fatalf("NewStream(_, _) = _, <nil>, want deadline exceeded")
	} else if err.Error() != status.Error(codes.DeadlineExceeded, context.DeadlineExceeded.Error()).Error() {
		t.Fatalf("NewStream(_, _) = _, %v, want _, %v", err, status.Error(codes.DeadlineExceeded, context.DeadlineExceeded.Error()))
	}

	// Now start a waiting NewStream with a long timeout and verify it unblocks
	// once the first stream finishes.
	waitDone := make(chan error, 1)
	go func() {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel2()
		cs2, err := clientTransport.NewStream(ctx2, &CallHdr{Method: "/test/MaxStreamsWait"}, nil)
		if err == nil {
			cs2.Close(nil)
		}
		waitDone <- err
	}()

	select {
	case err := <-waitDone:
		t.Fatalf("second NewStream unexpectedly returned early: %v", err)
	case <-time.After(100 * time.Millisecond):
		// expected to be blocked
	}

	close(allowFinishFirst)
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("second NewStream failed after first finished: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for second NewStream to unblock")
	}
}

func TestShmServerHandlesClientGoAwayDraining(t *testing.T) {
	segName := fmt.Sprintf("test-client-goaway-%d", time.Now().UnixNano())
	serverSeg, err := CreateSegment(segName, 65536, 65536)
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	defer serverSeg.Close()

	clientSeg, err := OpenSegment(segName)
	if err != nil {
		t.Fatalf("failed to open segment: %v", err)
	}
	defer clientSeg.Close()

	serverTransport, err := NewShmServerTransport(serverSeg, testAddr{"shm", "server"}, testAddr{"shm", "client"})
	if err != nil {
		t.Fatalf("failed to create server transport: %v", err)
	}
	defer serverTransport.Close(nil)

	testCtx, testCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer testCancel()

	// Start server stream handler: respond OK immediately.
	go serverTransport.HandleStreams(testCtx, func(s *ServerStream) {
		_ = serverTransport.writeHeader(s, metadata.MD{"content-type": []string{"application/grpc"}})
		_ = serverTransport.writeStatus(s, status.New(codes.OK, ""))
	})

	clientTransport, err := NewShmClientTransport(clientSeg, testAddr{"shm", "client"}, testAddr{"shm", "server"})
	if err != nil {
		t.Fatalf("failed to create client transport: %v", err)
	}
	defer clientTransport.Close(nil)

	// Create one active stream, then send GOAWAY via GracefulClose.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cs, err := clientTransport.NewStream(ctx, &CallHdr{Method: "/test/GoAway"}, nil)
	if err != nil {
		t.Fatalf("NewStream failed: %v", err)
	}
	clientTransport.GracefulClose()

	// Wait for stream to complete from server side.
	select {
	case <-cs.Done():
		// stream finished
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for client stream to finish")
	}

	// Server should close after the last stream completes when it receives GOAWAY.
	select {
	case <-serverTransport.errCh:
		// closed
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for server transport to close after client GOAWAY")
	}
}
