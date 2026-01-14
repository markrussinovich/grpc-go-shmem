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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
	payload := make([]byte, 4)
	binary.LittleEndian.PutUint32(payload, delta)
	_ = writeFrame(srvTransport.serverToClient, FrameHeader{Type: FrameTypeWindowUpdate}, payload, ctx)
	_ = writeFrame(srvTransport.serverToClient, FrameHeader{Type: FrameTypeWindowUpdate, StreamID: cs.id}, payload, ctx)

	select {
	case err := <-writeErr:
		if err != nil {
			t.Fatalf("write returned error after WINDOW_UPDATE: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("write did not unblock after WINDOW_UPDATE")
	}
}
