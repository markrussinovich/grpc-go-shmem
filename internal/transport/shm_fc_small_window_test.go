//go:build linux || windows

/*
 *
 * Copyright 2026 gRPC authors.
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
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
)

// TestShmSmallWindowMultiFrameMessage is a regression test for a
// flow-control deadlock that triggers whenever a single gRPC MESSAGE
// is larger than the per-stream HTTP/2 initial window. The bug is
// architectural rather than a one-line oversight, so this test is the
// canonical reproducer; any future change that touches the SHM
// transport's flow-control accounting MUST keep it green.
//
// # Failure mode (before fix)
//
//  1. Producer's ShmClientTransport.write computes payloadLen and
//     calls acquireSendQuota(payloadLen). acquireSendQuota waits
//     atomically for `payloadLen` bytes of stream+conn quota.
//  2. Consumer reads the first DATA frame (window-sized chunk) into
//     the per-stream lpmAccumulator. lpmAccumulator buffers it
//     because the LPM header declares more bytes are coming.
//  3. ShmServerTransport.handleMessage is NOT called until the
//     lpmAccumulator emits a complete LPM, so neither
//     connInFlow.onData nor s.fc.onData are called for the bytes
//     already received.
//  4. No WindowUpdate is sent back to the producer. The producer's
//     send window stays at zero. The producer waits forever for the
//     window to grow; the consumer waits forever for the rest of the
//     LPM to arrive. Deadlock.
//
// HTTP/2 over TCP doesn't have this bug because http2Client.handleData
// credits connection flow control per-DATA-frame, decoupled from
// message reassembly (see http2_client.go's handleData "Decouple
// connection's flow control from application's read" comment).
//
// # Production impact (pre-fix)
//
// The SHM transport hardcodes shmInitialWindowSize = 32 MiB which
// hides this bug from the existing test suite and bench (32 MiB is
// larger than every message they send). The bug becomes a customer-
// visible deadlock whenever:
//
//   - A user passes grpc.WithInitialWindowSize(N) with N smaller than
//     a sent message, OR
//   - A reviewer asks for an apples-to-apples comparison against
//     HTTP/2-default 64 KiB windows (Doug's request that motivated
//     finding this).
//
// # Test design
//
// Sends a 256 KiB MESSAGE under a 64 KiB initial window. The producer
// must drain 4 × the window via WindowUpdates to complete the send.
// On deadlock the test dumps every goroutine's stack to t.Log so the
// failure is actionable (the original investigation needed the dump
// to identify the lpmAccumulator → handleMessage coupling).
func TestShmSmallWindowMultiFrameMessage(t *testing.T) {
	// Configure the SHM flow-control knobs BEFORE any transport is
	// constructed. Both transports capture the values at construction;
	// mutating them mid-test does nothing.
	ConfigureShmFlowControlForBench(64 * 1024)
	defer ResetShmFlowControlForBench()

	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()

	segName := fmt.Sprintf("test-small-window-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)

	// 1 MiB rings — well above message size so the ring itself never
	// applies backpressure. The only constraint being tested is the
	// HTTP/2 flow-control window.
	const ringSize = 1 << 20
	serverSeg, err := CreateSegment(segName, ringSize, ringSize)
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

	// Server reads the whole message and replies OK. Reads in 4 KiB
	// chunks so the app-level read calls update the stream window
	// incrementally (this is the path through s.fc.onRead → WindowUpdate
	// that, in the buggy implementation, never gets exercised because
	// handleMessage is never called).
	go srvTransport.HandleStreams(testCtx, func(s *ServerStream) {
		const chunk = 4096
		for {
			_, err := s.Read(chunk)
			if err != nil {
				break
			}
		}
		_ = s.WriteStatus(status.New(codes.OK, ""))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cs, err := cliTransport.NewStream(ctx, &CallHdr{Method: "/test/SmallWindow"}, nil)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	// Force the producer's send quota down to 64 KiB to simulate what
	// `grpc.WithInitialWindowSize(64*1024)` would do once that option
	// is wired through to the SHM transport. Without this the SHM
	// transport defaults to a 2 GiB quota (maxWindowSize) regardless
	// of ConfigureShmFlowControlForBench, hiding the bug behind a
	// massive over-allocation. The send-quota init code lives in
	// NewShmClientTransport (conn quota) and NewStream (stream quota);
	// once both have run we clamp them down here, before the first
	// write call exercises the flow-control path.
	const fairWindow = 64 * 1024
	cliTransport.sendQuotaMu.Lock()
	cliTransport.connSendQuota.Store(fairWindow)
	cs.sendQuota.Store(fairWindow)
	cliTransport.sendQuotaMu.Unlock()

	// 256 KiB payload — 4× the 64 KiB initial window. Forces at least
	// three WindowUpdate round-trips on the client→server path to fully
	// drain. Pre-fix this loops on the first iteration forever; the
	// fix's correctness criterion is that the write completes.
	const msgSize = 256 * 1024
	payload := make([]byte, msgSize)
	for i := range payload {
		payload[i] = byte(i & 0xFF)
	}
	hdr := make([]byte, 5)
	binary.BigEndian.PutUint32(hdr[1:5], uint32(msgSize))

	writeErr := make(chan error, 1)
	go func() {
		writeErr <- cs.Write(hdr, mem.BufferSlice{mem.Copy(payload, mem.DefaultBufferPool())}, &WriteOptions{Last: true})
	}()

	select {
	case err := <-writeErr:
		if err != nil {
			t.Fatalf("write returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		// On deadlock dump all goroutine state so the failure is
		// actionable instead of just "test hung".
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Logf("DEADLOCK; goroutine dump (%d bytes):", n)
		// Filter to stacks that mention the transport package so the
		// log isn't drowned by testing framework / runtime frames.
		for _, line := range strings.Split(string(buf[:n]), "\n") {
			if strings.Contains(line, "transport.") ||
				strings.Contains(line, "Shm") ||
				strings.Contains(line, "goroutine ") {
				t.Logf("  %s", line)
			}
		}
		t.Fatal("write of 256 KiB under 64 KiB window deadlocked after 5 s")
	}
}
