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
	"encoding/binary"
	"fmt"
	"log"
	"testing"
	"time"
)

// TestSimpleCancellation tests cancellation with a simple context.WithCancel.
// It uses explicit channel-based synchronization (like TCP tests) rather than
// arbitrary sleeps to ensure deterministic behavior under load.
func TestSimpleCancellation(t *testing.T) {
	log.Printf("=== Starting TestSimpleCancellation ===")
	name := fmt.Sprintf("simple-cancel-%d", time.Now().UnixNano())
	seg, err := CreateSegment(name, 65536, 65536)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	// Synchronization channels
	headersReceived := make(chan struct{}) // Signals server got HEADERS frame
	messageReceived := make(chan struct{}) // Signals server got MESSAGE frame
	canceledSeen := make(chan struct{})    // Signals server got CANCEL frame

	// Server goroutine: read HEADERS and MESSAGE, then expect a CANCEL frame
	go func() {
		log.Printf("Server: Starting")
		srvRx := NewShmRingFromSegment(seg.A, seg.Mem)

		// Read request HEADERS with timeout
		log.Printf("Server: Reading HEADERS...")
		readCtx, cancelRead := context.WithTimeout(context.Background(), 2*time.Second)
		fh, _, err := readFrame(readCtx, srvRx)
		cancelRead()

		if err != nil {
			log.Printf("Server: Failed to read HEADERS: %v", err)
			return
		}
		if fh.Type != FrameTypeHEADERS {
			log.Printf("Server: Expected HEADERS, got type %d", fh.Type)
			return
		}
		log.Printf("Server: Got HEADERS (streamID=%d)", fh.StreamID)
		close(headersReceived) // Signal: HEADERS received

		// Read MESSAGE frame
		log.Printf("Server: Reading MESSAGE...")
		readCtx, cancelRead = context.WithTimeout(context.Background(), 2*time.Second)
		fh2, payload, err := readFrame(readCtx, srvRx)
		cancelRead()

		if err != nil {
			log.Printf("Server: Failed to read MESSAGE: %v", err)
			return
		}
		log.Printf("Server: Got frame type %d (streamID=%d, payloadLen=%d)", fh2.Type, fh2.StreamID, len(payload))
		close(messageReceived) // Signal: MESSAGE received

		// Now wait for CANCEL frame
		log.Printf("Server: Waiting for CANCEL frame...")
		readCtx, cancelRead = context.WithTimeout(context.Background(), 2*time.Second)
		fh3, _, err := readFrame(readCtx, srvRx)
		cancelRead()

		if err != nil {
			log.Printf("Server: ReadFrame error waiting for CANCEL: %v", err)
			return
		}
		if fh3.Type == FrameTypeCANCEL {
			log.Printf("Server: Saw CANCEL frame!")
			close(canceledSeen)
		} else {
			log.Printf("Server: Expected CANCEL, got type %d", fh3.Type)
		}
	}()

	client := NewShmUnaryClient(seg)
	payload := make([]byte, 5+3)
	payload[0] = 0 // not compressed
	// gRPC LPM length is big-endian (H2 wire format).
	binary.BigEndian.PutUint32(payload[1:5], 3)
	copy(payload[5:], []byte("abc"))

	// Use a simple cancellable context WITHOUT timeout
	ctx, cancel := context.WithCancel(context.Background())

	// Invoke in a goroutine to allow early cancel
	errCh := make(chan error, 1)
	go func() {
		log.Printf("Client: Starting UnaryCall")
		_, _, _, err := client.UnaryCall(ctx, "/svc.X/Unary", "example.com", nil, payload)
		log.Printf("Client: UnaryCall returned: %v", err)
		errCh <- err
	}()

	// Wait for server to confirm it received frames (explicit synchronization)
	log.Printf("Client: Waiting for server to receive HEADERS...")
	select {
	case <-headersReceived:
		log.Printf("Client: Server confirmed HEADERS received")
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for server to receive HEADERS")
	}

	select {
	case <-messageReceived:
		log.Printf("Client: Server confirmed MESSAGE received")
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for server to receive MESSAGE")
	}

	// NOW cancel - we know for certain the frames were transmitted
	log.Printf("Client: Calling cancel()...")
	cancel()
	log.Printf("Client: cancel() returned")

	// Client should return with cancellation error
	log.Printf("Client: Waiting for UnaryCall result...")
	select {
	case err := <-errCh:
		log.Printf("Client: Got result: %v", err)
		if err == nil {
			t.Fatal("expected cancellation error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not return after cancel")
	}

	// Server observed CANCEL
	log.Printf("Checking if server observed CANCEL...")
	select {
	case <-canceledSeen:
		log.Printf("Server observed CANCEL frame")
	case <-time.After(2 * time.Second):
		t.Fatal("server did not observe CANCEL frame")
	}

	// Clean up resources
	log.Printf("Cleaning up...")
	_ = client.Close()
	log.Printf("=== TestSimpleCancellation completed ===")
}
