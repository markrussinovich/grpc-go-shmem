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

package transport

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestHighThroughputBidirectional reproduces the flow_control corruption bug.
// This test sends 1500+ messages of 8KB bidirectionally, matching the
// flow_control example pattern that triggers data corruption.
func TestHighThroughputBidirectional(t *testing.T) {
	segmentName := fmt.Sprintf("%s_%d", t.Name(), time.Now().UnixNano())

	// Create segment
	serverSeg, err := CreateSegment(segmentName, DefaultRingASize, DefaultRingBSize)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer RemoveSegment(segmentName)

	clientSeg, err := OpenSegment(segmentName)
	if err != nil {
		serverSeg.Close()
		t.Fatalf("Failed to open segment: %v", err)
	}

	client := NewShmStreamingClient(clientSeg)
	defer client.Close()

	server := NewShmStreamingServer(serverSeg)
	defer server.Close()

	// Parameters matching flow_control example
	const messageSize = 8 * 1024 // 8KB
	const numMessages = 1500     // Send many messages like flow_control

	serverDone := make(chan struct{})
	serverErrors := make(chan error, 10)

	server.handler = func(stream *streamingServerStream) {
		defer close(serverDone)

		if err := stream.SendHeaders(nil); err != nil {
			serverErrors <- fmt.Errorf("server failed to send headers: %v", err)
			return
		}

		// Echo loop
		messagesReceived := 0
		for {
			msg, err := stream.RecvMsg()
			if err != nil {
				break
			}
			messagesReceived++

			// Verify message integrity
			for i, b := range msg {
				expected := byte(i % 256)
				if b != expected {
					serverErrors <- fmt.Errorf("DATA CORRUPTION at msg %d, offset %d: expected %d, got %d",
						messagesReceived, i, expected, b)
					return
				}
			}

			// Echo back
			if err := stream.SendMsg(msg); err != nil {
				break
			}
		}

		t.Logf("Server received and echoed %d messages", messagesReceived)
		stream.SendTrailers(0, "OK", nil)
	}

	serverCtx, serverCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer serverCancel()
	go server.Serve(serverCtx, server.handler)

	client.Start()

	stream, err := client.NewStream(serverCtx, "/test.Service/FlowControl", "localhost", nil)
	if err != nil {
		t.Fatalf("Failed to create stream: %v", err)
	}

	_, err = stream.RecvHeaders()
	if err != nil {
		t.Fatalf("Failed to receive headers: %v", err)
	}

	// Create test message with predictable pattern
	testMsg := make([]byte, messageSize)
	for i := range testMsg {
		testMsg[i] = byte(i % 256)
	}

	var wg sync.WaitGroup
	clientErrors := make(chan error, 10)

	// Sender goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numMessages; i++ {
			if err := stream.SendMsg(testMsg); err != nil {
				clientErrors <- fmt.Errorf("send error at msg %d: %v", i, err)
				return
			}
		}
		stream.CloseSend()
		t.Logf("Client finished sending %d messages", numMessages)
	}()

	// Receiver goroutine
	wg.Add(1)
	receivedCount := 0
	go func() {
		defer wg.Done()
		for i := 0; i < numMessages; i++ {
			msg, err := stream.RecvMsg()
			if err != nil {
				clientErrors <- fmt.Errorf("recv error at msg %d: %v", i, err)
				return
			}
			receivedCount++

			// Verify integrity
			for j, b := range msg {
				expected := byte(j % 256)
				if b != expected {
					clientErrors <- fmt.Errorf("CLIENT DATA CORRUPTION at msg %d, offset %d: expected %d, got %d",
						receivedCount, j, expected, b)
					return
				}
			}
		}
		t.Logf("Client received %d messages", receivedCount)
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Check for errors
		select {
		case err := <-serverErrors:
			t.Fatalf("Server error: %v", err)
		case err := <-clientErrors:
			t.Fatalf("Client error: %v", err)
		default:
			t.Logf("SUCCESS: sent and received %d messages of %d bytes each", numMessages, messageSize)
		}
	case err := <-serverErrors:
		t.Fatalf("Server error: %v", err)
	case err := <-clientErrors:
		t.Fatalf("Client error: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("Test timed out")
	}
}
