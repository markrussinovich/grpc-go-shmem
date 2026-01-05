//go:build linux

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

package shm

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestBidirectionalStreamingNoDeadlock verifies that bidirectional streaming
// works without deadlocking even when both sides are actively sending and receiving.
func TestBidirectionalStreamingNoDeadlock(t *testing.T) {
	// Create segment
	seg, err := CreateSegment("test_bidir", DefaultRingASize, DefaultRingBSize)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer RemoveSegment("test_bidir")
	defer seg.Close()

	// Create client and server
	client := NewShmStreamingClient(seg)
	defer client.Close()

	server := NewShmStreamingServer(seg)
	defer server.Close()

	// Server handler: echo messages back to client
	serverDone := make(chan struct{})
	server.handler = func(stream *streamingServerStream) {
		defer close(serverDone)

		// Send headers
		if err := stream.SendHeaders(nil); err != nil {
			t.Errorf("Server failed to send headers: %v", err)
			return
		}

		// Echo loop: receive and send back
		for {
			msg, err := stream.RecvMsg()
			if err != nil {
				// End of stream
				break
			}
			
			// Echo back
			if err := stream.SendMsg(msg); err != nil {
				t.Errorf("Server failed to send message: %v", err)
				break
			}
		}

		// Send trailers
		if err := stream.SendTrailers(0, "OK", nil); err != nil {
			t.Errorf("Server failed to send trailers: %v", err)
		}
	}

	// Start server (this starts its reader)
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	go server.Serve(serverCtx, server.handler)

	// Give server a moment to start
	time.Sleep(50 * time.Millisecond)

	// Start client reader
	client.Start()

	// Create stream (this will send HEADERS)
	stream, err := client.NewStream(context.Background(), "/test.Service/Echo", "localhost", nil)
	if err != nil {
		t.Fatalf("Failed to create stream: %v", err)
	}

	// Wait for headers
	_, err = stream.RecvHeaders()
	if err != nil {
		t.Fatalf("Failed to receive headers: %v", err)
	}

	// Send multiple messages concurrently
	numMessages := 100
	var wg sync.WaitGroup
	
	// Sender goroutine
	wg.Add(1)
	sendErrors := make(chan error, numMessages)
	go func() {
		defer wg.Done()
		for i := 0; i < numMessages; i++ {
			msg := []byte(fmt.Sprintf("message-%d", i))
			if err := stream.SendMsg(msg); err != nil {
				sendErrors <- err
				return
			}
		}
		stream.CloseSend()
	}()

	// Receiver goroutine
	wg.Add(1)
	recvErrors := make(chan error, numMessages)
	receivedCount := 0
	go func() {
		defer wg.Done()
		for i := 0; i < numMessages; i++ {
			_, err := stream.RecvMsg()
			if err != nil {
				recvErrors <- err
				return
			}
			receivedCount++
		}
	}()

	// Wait for completion with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success
		t.Logf("Successfully sent and received %d messages", receivedCount)
	case <-time.After(10 * time.Second):
		t.Fatal("Test timed out - possible deadlock!")
	case err := <-sendErrors:
		t.Fatalf("Send error: %v", err)
	case err := <-recvErrors:
		t.Fatalf("Receive error: %v", err)
	}

	if receivedCount != numMessages {
		t.Errorf("Expected to receive %d messages, got %d", numMessages, receivedCount)
	}
}

// TestBidirectionalStreamingFullBuffers tests that the system doesn't deadlock
// when both ring buffers become full simultaneously.
func TestBidirectionalStreamingFullBuffers(t *testing.T) {
	// Create segment with smaller rings to trigger buffer full condition
	smallRingSize := uint64(32 * 1024) // 32KB
	seg, err := CreateSegment("test_full_buffers", smallRingSize, smallRingSize)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer RemoveSegment("test_full_buffers")
	defer seg.Close()

	client := NewShmStreamingClient(seg)
	defer client.Close()

	server := NewShmStreamingServer(seg)
	defer server.Close()

	// Server handler: sends large messages while receiving
	server.handler = func(stream *streamingServerStream) {
		if err := stream.SendHeaders(nil); err != nil {
			t.Errorf("Server failed to send headers: %v", err)
			return
		}

		// Concurrently send and receive
		var wg sync.WaitGroup
		
		// Receiver
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				_, err := stream.RecvMsg()
				if err != nil {
					break
				}
			}
		}()

		// Sender - send large messages to fill buffer
		wg.Add(1)
		go func() {
			defer wg.Done()
			largeMsg := make([]byte, 8*1024) // 8KB messages
			for i := 0; i < 10; i++ {
				if err := stream.SendMsg(largeMsg); err != nil {
					break
				}
			}
		}()

		wg.Wait()
		stream.SendTrailers(0, "OK", nil)
	}

	// Start server
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	go server.Serve(serverCtx, server.handler)

	// Start client
	client.Start()

	// Create stream
	stream, err := client.NewStream(context.Background(), "/test.Service/BulkTransfer", "localhost", nil)
	if err != nil {
		t.Fatalf("Failed to create stream: %v", err)
	}

	// Wait for headers
	_, err = stream.RecvHeaders()
	if err != nil {
		t.Fatalf("Failed to receive headers: %v", err)
	}

	// Concurrently send and receive large messages
	var wg sync.WaitGroup
	
	// Sender
	wg.Add(1)
	go func() {
		defer wg.Done()
		largeMsg := make([]byte, 8*1024) // 8KB messages
		for i := 0; i < 10; i++ {
			if err := stream.SendMsg(largeMsg); err != nil {
				t.Logf("Client send error (expected during full buffer): %v", err)
				break
			}
		}
		stream.CloseSend()
	}()

	// Receiver
	wg.Add(1)
	receivedCount := 0
	go func() {
		defer wg.Done()
		for {
			_, err := stream.RecvMsg()
			if err != nil {
				break
			}
			receivedCount++
		}
	}()

	// Wait for completion with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("Test completed successfully - no deadlock! Received %d messages", receivedCount)
	case <-time.After(15 * time.Second):
		t.Fatal("Test timed out - DEADLOCK DETECTED when both buffers are full!")
	}
}

// TestConcurrentStreams verifies that multiple concurrent streams work correctly
func TestConcurrentStreams(t *testing.T) {
	seg, err := CreateSegment("test_concurrent", DefaultRingASize, DefaultRingBSize)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer RemoveSegment("test_concurrent")
	defer seg.Close()

	client := NewShmStreamingClient(seg)
	defer client.Close()

	server := NewShmStreamingServer(seg)
	defer server.Close()

	// Server handler: echo with stream ID in response
	streamCount := 0
	var streamCountMu sync.Mutex
	server.handler = func(stream *streamingServerStream) {
		streamCountMu.Lock()
		streamCount++
		streamCountMu.Unlock()

		if err := stream.SendHeaders(nil); err != nil {
			return
		}

		for {
			msg, err := stream.RecvMsg()
			if err != nil {
				break
			}
			response := append(msg, []byte(fmt.Sprintf("-stream-%d", stream.id))...)
			if err := stream.SendMsg(response); err != nil {
				break
			}
		}

		stream.SendTrailers(0, "OK", nil)
	}

	// Start server
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	go server.Serve(serverCtx, server.handler)

	// Start client
	client.Start()

	// Create multiple concurrent streams
	numStreams := 5
	var wg sync.WaitGroup
	errors := make(chan error, numStreams)

	for i := 0; i < numStreams; i++ {
		wg.Add(1)
		go func(streamNum int) {
			defer wg.Done()

			stream, err := client.NewStream(context.Background(), fmt.Sprintf("/test.Service/Method%d", streamNum), "localhost", nil)
			if err != nil {
				errors <- fmt.Errorf("stream %d: failed to create: %w", streamNum, err)
				return
			}

			if _, err := stream.RecvHeaders(); err != nil {
				errors <- fmt.Errorf("stream %d: failed to receive headers: %w", streamNum, err)
				return
			}

			// Send a message
			msg := []byte(fmt.Sprintf("hello-from-stream-%d", streamNum))
			if err := stream.SendMsg(msg); err != nil {
				errors <- fmt.Errorf("stream %d: failed to send: %w", streamNum, err)
				return
			}
			stream.CloseSend()

			// Receive response
			_, err = stream.RecvMsg()
			if err != nil {
				errors <- fmt.Errorf("stream %d: failed to receive: %w", streamNum, err)
				return
			}
		}(i)
	}

	// Wait with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("Successfully completed %d concurrent streams", numStreams)
	case <-time.After(10 * time.Second):
		t.Fatal("Concurrent streams test timed out!")
	case err := <-errors:
		t.Fatalf("Stream error: %v", err)
	}

	streamCountMu.Lock()
	finalCount := streamCount
	streamCountMu.Unlock()

	if finalCount != numStreams {
		t.Errorf("Expected %d streams to be handled, got %d", numStreams, finalCount)
	}
}
