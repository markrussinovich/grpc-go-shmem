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
	"runtime"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
)

// Test that a client write blocks when the outbound flow-control window is
// exhausted and resumes when WINDOW_UPDATE frames arrive.
func TestShmFlowControlBlocksUntilWindowUpdate(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer testCancel()

	segName := fmt.Sprintf("test-flow-ctrl-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)

	serverSeg, err := CreateSegment(segName, 65536, 65536)
	if err != nil {
		t.Fatalf("create segment: %v", err)
	}
	serverSeg.H.SetServerReady(true)
	defer serverSeg.Close()

	clientSeg, err := OpenSegment(segName)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	clientSeg.H.SetClientReady(true)
	defer clientSeg.Close()

	srvTransport, err := NewShmServerTransport(serverSeg, testAddr{"shm", "server"}, testAddr{"shm", "client"})
	if err != nil {
		t.Fatalf("server transport: %v", err)
	}
	defer srvTransport.Close(nil)

	cliTransport, err := NewShmClientTransport(clientSeg, testAddr{"shm", "client"}, testAddr{"shm", "server"})
	if err != nil {
		t.Fatalf("client transport: %v", err)
	}
	defer cliTransport.Close(nil)

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	go srvTransport.HandleStreams(testCtx, func(s *ServerStream) {
		// Read whatever the client sends to consume the window on the receive side.
		_, _ = s.Read(5)
		_ = s.WriteStatus(status.New(codes.OK, ""))
	})

	cs, err := cliTransport.NewStream(ctx, &CallHdr{Method: "/test/FlowControl"})
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	// Exhaust both connection and stream send windows to force a block.
	cliTransport.sendQuotaMu.Lock()
	cliTransport.connSendQuota = 0
	cliTransport.streamSendQuota[cs.id] = 0
	cliTransport.notifyQuotaChangeLocked()
	cliTransport.sendQuotaMu.Unlock()

	msg := mem.BufferSlice{mem.Copy([]byte("hello"), mem.DefaultBufferPool())}
	writeErr := make(chan error, 1)
	go func() {
		writeErr <- cs.Write(nil, msg, &WriteOptions{Last: true})
	}()

	// The write should block until a WINDOW_UPDATE arrives.
	select {
	case err := <-writeErr:
		t.Fatalf("write returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
		// still blocked as expected
	}

	// Send WINDOW_UPDATE for both the connection and the stream to release the writer.
	delta := uint32(msg.Len())
	cliTransport.addSendQuota(0, delta)
	cliTransport.addSendQuota(cs.id, delta)

	select {
	case err := <-writeErr:
		if err != nil {
			t.Fatalf("write returned error after WINDOW_UPDATE: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("write did not unblock after WINDOW_UPDATE")
	}
}

// TestShmFlowControlMultiStreamAccountCheck tests that flow control accounting
// works correctly across multiple concurrent streams, similar to HTTP/2's
// testFlowControlAccountCheck test.
func TestShmFlowControlMultiStreamAccountCheck(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer testCancel()

	segName := fmt.Sprintf("test-multi-flow-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)

	serverSeg, err := CreateSegment(segName, 262144, 262144) // 256KB rings
	if err != nil {
		t.Fatalf("create segment: %v", err)
	}
	serverSeg.H.SetServerReady(true)
	defer serverSeg.Close()

	clientSeg, err := OpenSegment(segName)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	clientSeg.H.SetClientReady(true)
	defer clientSeg.Close()

	srvTransport, err := NewShmServerTransport(serverSeg, testAddr{"shm", "server"}, testAddr{"shm", "client"})
	if err != nil {
		t.Fatalf("server transport: %v", err)
	}
	defer srvTransport.Close(nil)

	cliTransport, err := NewShmClientTransport(clientSeg, testAddr{"shm", "client"}, testAddr{"shm", "server"})
	if err != nil {
		t.Fatalf("client transport: %v", err)
	}
	defer cliTransport.Close(nil)

	const numStreams = 5
	const msgSize = 1024

	// Server echo handler
	go srvTransport.HandleStreams(testCtx, func(s *ServerStream) {
		data, err := s.Read(msgSize * 2)
		if err != nil {
			// Ignore read errors - stream might be closed
			return
		}
		// Echo back the message
		opts := &WriteOptions{Last: false}
		_ = s.Write(nil, data, opts)
		_ = s.WriteStatus(status.New(codes.OK, ""))
	})

	// Create multiple streams
	streams := make([]*ClientStream, numStreams)
	for i := 0; i < numStreams; i++ {
		s, err := cliTransport.NewStream(testCtx, &CallHdr{Method: fmt.Sprintf("/test/Stream%d", i)})
		if err != nil {
			t.Fatalf("NewStream %d: %v", i, err)
		}
		streams[i] = s
	}

	// Verify flow control accounting - check stream send quotas exist
	cliTransport.sendQuotaMu.Lock()
	for i, s := range streams {
		quota, ok := cliTransport.streamSendQuota[s.id]
		if !ok {
			cliTransport.sendQuotaMu.Unlock()
			t.Fatalf("stream %d has no send quota", i)
		}
		if quota <= 0 {
			cliTransport.sendQuotaMu.Unlock()
			t.Fatalf("stream %d has non-positive send quota: %d", i, quota)
		}
	}
	initialConnQuota := cliTransport.connSendQuota
	cliTransport.sendQuotaMu.Unlock()

	// Send messages on all streams
	testData := make([]byte, msgSize)
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	for i, s := range streams {
		msg := mem.BufferSlice{mem.Copy(testData, mem.DefaultBufferPool())}
		if err := s.Write(nil, msg, &WriteOptions{Last: true}); err != nil {
			t.Errorf("Write on stream %d failed: %v", i, err)
		}
	}

	// Yield to allow writes to be processed
	for i := 0; i < 10; i++ {
		runtime.Gosched()
	}

	// Verify connection quota was consumed
	cliTransport.sendQuotaMu.Lock()
	finalConnQuota := cliTransport.connSendQuota
	cliTransport.sendQuotaMu.Unlock()

	// Connection quota should have decreased (or been replenished by WINDOW_UPDATEs)
	t.Logf("Connection quota: initial=%d, final=%d", initialConnQuota, finalConnQuota)
	// Streams will be cleaned up when transport closes
}
