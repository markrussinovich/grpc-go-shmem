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
	"testing"
	"time"

	"google.golang.org/grpc/mem"
)

// TestClientTransportNewStreamAndWrite tests that NewStream creates a stream
// and that the stream's Write method works via the transport.
func TestClientTransportNewStreamAndWrite(t *testing.T) {
	segmentName := fmt.Sprintf("test-client-write-%d", time.Now().UnixNano())
	defer RemoveSegment(segmentName)

	// Create a segment for testing
	seg, err := CreateSegment(segmentName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}

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
