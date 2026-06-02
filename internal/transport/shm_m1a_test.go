//go:build linux || windows

// Copyright 2026 gRPC authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package transport

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Note: these tests intentionally do NOT call t.Parallel() because they
// share the global shmM1aBatchFire counter. Parallel execution with any
// other M1a-counter test (or any future test that triggers WriteProto)
// would race the delta assertions. Keep them sequential.

// TestShmM1aBatchFiresOnFirstResponse verifies that
// ShmServerTransport.writeProto's M1a HEADERS+DATA wake-coalesce
// branch (BeginBatch/EndBatch around the first server response)
// actually enters on the M1a path.
//
// Uses the dedicated shmM1aBatchFire counter (incremented only
// inside the M1a branch in shm_server_transport.go around L1891)
// for a clean exact-count assertion that doesn't rely on signalData
// timing (signalData is gated on DataWaiters>0 and is flaky to
// assert in tight in-test loops).
//
// What we assert:
//   - After one server response sent via WriteProto (the M1a code
//     path), shmM1aBatchFire incremented by exactly 1.
//   - End-to-end response still arrives correctly with OK status.
//
// A regression that removes M1a's BeginBatch/EndBatch scope, or that
// flips emitHeader to false incorrectly, would leave the counter at 0
// and the test fails.
//
// This test was rewritten after a round-2 review caught that the
// original brittle wake-count assertion was both wrong (calling
// s.Write which routes through the byte path, not writeProto) and
// too loose (delta <= 4 also satisfied by an unfused HEADERS+DATA
// at 4).
func TestShmM1aBatchFiresOnFirstResponse(t *testing.T) {
	ct, st, _, cleanup := setupShmTransportPair(t, 256*1024)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		st.HandleStreams(ctx, func(s *ServerStream) {
			if _, err := s.Read(1024); err != nil && err != io.EOF {
				return
			}
			// Use WriteProto — exercises ShmServerTransport.writeProto
			// where M1a lives. s.Write (byte path) would NOT hit M1a.
			resp := &wrapperspb.BytesValue{Value: []byte("M1A-OK")}
			if _, err := s.WriteProto(resp, &WriteOptions{}); err != nil {
				return
			}
			_ = s.WriteStatus(status.New(codes.OK, ""))
		})
	}()

	callHdr := &CallHdr{Host: "localhost", Method: "/test/M1aBatch"}
	cs, err := ct.NewStream(ctx, callHdr, nil)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	beforeBatch := atomic.LoadUint64(&shmM1aBatchFire)

	req := make([]byte, 16)
	reqHdr := make([]byte, 5)
	binaryBE32(reqHdr[1:5], uint32(len(req)))
	if err := cs.Write(reqHdr, mem.BufferSlice{mem.SliceBuffer(req)}, &WriteOptions{Last: true}); err != nil && err != io.EOF {
		t.Fatalf("cs.Write request: %v", err)
	}

	if _, err := cs.Read(1024); err != nil && err != io.EOF {
		t.Fatalf("cs.Read response: %v", err)
	}
	select {
	case <-cs.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("stream Done timed out")
	}

	delta := atomic.LoadUint64(&shmM1aBatchFire) - beforeBatch
	if delta != 1 {
		t.Errorf("M1a BeginBatch fired %d times for one unary RPC, want 1 "+
			"(regression: M1a branch did not engage on the first server response)", delta)
	}

	if st := cs.Status(); st.Code() != codes.OK {
		t.Errorf("stream status = %s, want OK", st.Code())
	}

	ct.Close(nil)
	st.Close(nil)
	<-serverDone
}

// TestShmM1aHeaderDedupAcrossExplicitSendHeader verifies the
// headerSent CAS dedup: if the handler calls SendHeader explicitly
// BEFORE WriteProto, buildServerInitialHeaderPayload returns
// emitHeader=false, so M1a does NOT enter the BeginBatch branch.
// A duplicate HEADERS would surface as a PROTOCOL_ERROR on the
// client decoder or as a non-OK final status.
//
// What we assert:
//   - After SendHeader+WriteProto: shmM1aBatchFire stays at 0
//     (emitHeader=false skips the batch entirely).
//   - Response still arrives correctly with OK status — proves no
//     duplicate HEADERS was sent.
func TestShmM1aHeaderDedupAcrossExplicitSendHeader(t *testing.T) {
	ct, st, _, cleanup := setupShmTransportPair(t, 256*1024)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		st.HandleStreams(ctx, func(s *ServerStream) {
			if _, err := s.Read(1024); err != nil && err != io.EOF {
				return
			}
			// Explicit SendHeader -> headerSent CAS to 1
			if err := s.SendHeader(nil); err != nil {
				return
			}
			// WriteProto -> buildServerInitialHeaderPayload returns
			// emitHeader=false because headerSent is already 1.
			// M1a batch path MUST NOT enter; shmM1aBatchFire stays 0.
			resp := &wrapperspb.BytesValue{Value: []byte("DEDUP-OK")}
			if _, err := s.WriteProto(resp, &WriteOptions{}); err != nil {
				return
			}
			_ = s.WriteStatus(status.New(codes.OK, ""))
		})
	}()

	callHdr := &CallHdr{Host: "localhost", Method: "/test/M1aDedup"}
	cs, err := ct.NewStream(ctx, callHdr, nil)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	beforeBatch := atomic.LoadUint64(&shmM1aBatchFire)

	req := make([]byte, 4)
	reqHdr := make([]byte, 5)
	binaryBE32(reqHdr[1:5], uint32(len(req)))
	if err := cs.Write(reqHdr, mem.BufferSlice{mem.SliceBuffer(req)}, &WriteOptions{Last: true}); err != nil && err != io.EOF {
		t.Fatalf("cs.Write request: %v", err)
	}

	if _, err := cs.Read(1024); err != nil && err != io.EOF {
		t.Fatalf("cs.Read response: %v (a duplicate HEADERS would surface here as PROTOCOL_ERROR)", err)
	}
	select {
	case <-cs.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("stream Done timed out")
	}

	delta := atomic.LoadUint64(&shmM1aBatchFire) - beforeBatch
	if delta != 0 {
		t.Errorf("M1a BeginBatch fired %d times after explicit SendHeader, want 0 "+
			"(regression: headerSent dedup broken, M1a would emit duplicate HEADERS)", delta)
	}

	if st := cs.Status(); st.Code() != codes.OK {
		t.Errorf("stream status = %s, want OK (a duplicate HEADERS would corrupt the response)", st.Code())
	}

	ct.Close(nil)
	st.Close(nil)
	<-serverDone
}

// TestShmM1aOnlyOnFirstMessageInStream verifies that for a server-
// streaming RPC sending N response messages, the M1a batch fires
// EXACTLY ONCE (on the first message, where HEADERS hasn't been
// sent yet). Subsequent messages must take the emitHeader=false
// path (no batch).
func TestShmM1aOnlyOnFirstMessageInStream(t *testing.T) {
	ct, st, _, cleanup := setupShmTransportPair(t, 256*1024)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverDone := make(chan struct{})
	const numResponses = 5
	go func() {
		defer close(serverDone)
		st.HandleStreams(ctx, func(s *ServerStream) {
			if _, err := s.Read(1024); err != nil && err != io.EOF {
				return
			}
			for i := 0; i < numResponses; i++ {
				resp := &wrapperspb.BytesValue{Value: []byte("STREAMING-RESP")}
				if _, err := s.WriteProto(resp, &WriteOptions{}); err != nil {
					return
				}
			}
			_ = s.WriteStatus(status.New(codes.OK, ""))
		})
	}()

	callHdr := &CallHdr{Host: "localhost", Method: "/test/M1aStream"}
	cs, err := ct.NewStream(ctx, callHdr, nil)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	beforeBatch := atomic.LoadUint64(&shmM1aBatchFire)

	req := make([]byte, 8)
	reqHdr := make([]byte, 5)
	binaryBE32(reqHdr[1:5], uint32(len(req)))
	if err := cs.Write(reqHdr, mem.BufferSlice{mem.SliceBuffer(req)}, &WriteOptions{Last: true}); err != nil && err != io.EOF {
		t.Fatalf("cs.Write request: %v", err)
	}

	for i := 0; i < numResponses; i++ {
		if _, err := cs.Read(1024); err != nil && err != io.EOF {
			t.Fatalf("cs.Read response[%d]: %v", i, err)
		}
	}
	select {
	case <-cs.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("stream Done timed out")
	}

	delta := atomic.LoadUint64(&shmM1aBatchFire) - beforeBatch
	if delta != 1 {
		t.Errorf("M1a BeginBatch fired %d times for %d-message stream, want exactly 1 "+
			"(regression: M1a batch firing on subsequent messages, or not firing on first)",
			delta, numResponses)
	}

	if st := cs.Status(); st.Code() != codes.OK {
		t.Errorf("stream status = %s, want OK", st.Code())
	}

	ct.Close(nil)
	st.Close(nil)
	<-serverDone
}

// binaryBE32 writes v as a big-endian uint32 into b[0:4].
func binaryBE32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}
