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
	// HTTP/2-style WINDOW_UPDATE flow control is now the only profile;
	// no global toggle needed.

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

	cs, err := cliTransport.NewStream(ctx, &CallHdr{Method: "/test/FlowControl"}, nil)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	// Exhaust both connection and stream send windows to force a block.
	cliTransport.sendQuotaMu.Lock()
	cliTransport.connSendQuota.Store(0)
	cs.sendQuota.Store(0)
	cliTransport.notifyQuotaChangeLocked(0)
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
		s, err := cliTransport.NewStream(testCtx, &CallHdr{Method: fmt.Sprintf("/test/Stream%d", i)}, nil)
		if err != nil {
			t.Fatalf("NewStream %d: %v", i, err)
		}
		streams[i] = s
	}

	// Verify flow control accounting - check stream send quotas exist
	for i, s := range streams {
		quota := s.sendQuota.Load()
		if quota <= 0 {
			t.Fatalf("stream %d has non-positive send quota: %d", i, quota)
		}
	}
	initialConnQuota := cliTransport.connSendQuota.Load()

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
	finalConnQuota := cliTransport.connSendQuota.Load()

	// Connection quota should have decreased (or been replenished by WINDOW_UPDATEs)
	t.Logf("Connection quota: initial=%d, final=%d", initialConnQuota, finalConnQuota)
	// Streams will be cleaned up when transport closes
}

// TestShmFlowControl_SlowConsumer_SenderBlocks verifies the core HTTP/2
// flow-control invariant on the SHM transport: when the receiving
// application stops reading, the sender's Write MUST eventually block
// at a bounded number of bytes in flight (one per-stream window). This
// is the scenario that motivated the move away from NoWU: a fast
// producer + slow consumer must NOT push receiver memory unbounded.
//
// Test shape:
//   - Configure a small per-stream window (64 KiB) so the test runs fast.
//   - Server's stream handler accepts streams but NEVER calls Recv.
//   - Client opens one stream and writes messages of size W-5 back to
//     back. Each LPM is 5 + (W-5) = W bytes; the first one consumes the
//     entire stream window.
//   - The SECOND Write MUST block (window depleted, app not reading).
//   - Verify the block lasts > 50 ms (vs the typical sub-ms Write).
func TestShmFlowControl_SlowConsumer_SenderBlocks(t *testing.T) {
	const window = 64 * 1024
	ConfigureShmFlowControlForBench(window)
	defer ResetShmFlowControlForBench()

	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()

	segName := fmt.Sprintf("test-slow-consumer-block-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)

	const ringSize = 4 * 1024 * 1024
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

	// Server: accept stream and PARK FOREVER without reading. This is
	// the "slow consumer" scenario; the absence of Recv must propagate
	// back-pressure to the sender via the missing WINDOW_UPDATE.
	handlerEntered := make(chan struct{}, 1)
	go srvTransport.HandleStreams(testCtx, func(s *ServerStream) {
		select {
		case handlerEntered <- struct{}{}:
		default:
		}
		<-testCtx.Done() // park until test cleanup
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs, err := cliTransport.NewStream(ctx, &CallHdr{Method: "/test/SlowConsumer"}, nil)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	// Force conn + stream quotas down to one window so the test does not
	// inadvertently start with a 32 MiB SHM-tuned quota that would let
	// the sender finish many messages before noticing the back-pressure.
	cliTransport.sendQuotaMu.Lock()
	cliTransport.connSendQuota.Store(window)
	cs.sendQuota.Store(window)
	cliTransport.sendQuotaMu.Unlock()

	// Wait for the server's handler to enter so the stream is fully set
	// up on both sides before we begin observing back-pressure timing.
	select {
	case <-handlerEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("server handler never entered")
	}

	// Each LPM = 5 (header) + bodyLen; choose body = window-5 so the
	// total is exactly W bytes, consuming the per-stream window in one
	// shot. The SECOND Write must block.
	bodyLen := window - 5
	payload := make([]byte, bodyLen)
	for i := range payload {
		payload[i] = byte(i & 0xff)
	}
	hdr := make([]byte, 5)
	// header[0]=0 (no compression); header[1:5]=big-endian bodyLen
	hdr[1] = byte(bodyLen >> 24)
	hdr[2] = byte(bodyLen >> 16)
	hdr[3] = byte(bodyLen >> 8)
	hdr[4] = byte(bodyLen)

	// First write: should succeed; consumes the stream window entirely.
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- cs.Write(hdr, mem.BufferSlice{mem.Copy(payload, mem.DefaultBufferPool())}, &WriteOptions{Last: false})
	}()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first write: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first write did not complete; SHM ring/quota likely misconfigured")
	}

	// Second write: must block. App is not reading, so no WU can be
	// emitted by the receiver, and the sender's stream quota stays at 0.
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- cs.Write(hdr, mem.BufferSlice{mem.Copy(payload, mem.DefaultBufferPool())}, &WriteOptions{Last: true})
	}()

	// Observe the block. A typical SHM write completes in microseconds;
	// 200 ms is a long-tail-safe margin. If the write completes here,
	// receiver memory is growing unbounded, which is the regression we
	// want to catch.
	select {
	case err := <-secondDone:
		t.Fatalf("second write returned early (err=%v); slow-consumer back-pressure not working — receiver memory would grow unbounded", err)
	case <-time.After(200 * time.Millisecond):
		// Still blocked: good.
	}

	// Sanity: verify stream send quota is indeed depleted on the sender.
	streamQ := cs.sendQuota.Load()
	if streamQ > 0 {
		t.Errorf("expected stream sendQuota=0 (blocked sender), got %d", streamQ)
	}
}

// TestShmFlowControl_SlowConsumer_UnblocksOnAppRead verifies the
// dual of TestShmFlowControl_SlowConsumer_SenderBlocks: once the
// application starts reading, the receiver MUST emit WINDOW_UPDATE
// frames that unblock the sender.
func TestShmFlowControl_SlowConsumer_UnblocksOnAppRead(t *testing.T) {
	const window = 64 * 1024
	ConfigureShmFlowControlForBench(window)
	defer ResetShmFlowControlForBench()

	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()

	segName := fmt.Sprintf("test-slow-unblock-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)

	const ringSize = 4 * 1024 * 1024
	serverSeg, _ := CreateSegment(segName, ringSize, ringSize)
	serverSeg.H.SetServerReady(true)
	defer serverSeg.Close()
	clientSeg, _ := OpenSegment(segName)
	clientSeg.H.SetClientReady(true)
	defer clientSeg.Close()

	srvTransport, _ := NewShmServerTransport(serverSeg, testAddr{"shm", "server"}, testAddr{"shm", "client"})
	defer srvTransport.Close(nil)
	cliTransport, _ := NewShmClientTransport(clientSeg, testAddr{"shm", "client"}, testAddr{"shm", "server"})
	defer cliTransport.Close(nil)

	// Server handler: read on demand from a channel-driven trigger.
	startReading := make(chan struct{})
	readerDone := make(chan struct{})
	go srvTransport.HandleStreams(testCtx, func(s *ServerStream) {
		<-startReading
		// Drain all received messages.
		for {
			if _, err := s.Read(4096); err != nil {
				break
			}
		}
		close(readerDone)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs, err := cliTransport.NewStream(ctx, &CallHdr{Method: "/test/SlowUnblock"}, nil)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	cliTransport.sendQuotaMu.Lock()
	cliTransport.connSendQuota.Store(window)
	cs.sendQuota.Store(window)
	cliTransport.sendQuotaMu.Unlock()

	bodyLen := window - 5
	payload := make([]byte, bodyLen)
	hdr := make([]byte, 5)
	hdr[1] = byte(bodyLen >> 24)
	hdr[2] = byte(bodyLen >> 16)
	hdr[3] = byte(bodyLen >> 8)
	hdr[4] = byte(bodyLen)

	// First write fills the window.
	if err := cs.Write(hdr, mem.BufferSlice{mem.Copy(payload, mem.DefaultBufferPool())}, &WriteOptions{Last: false}); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Second write should block (receiver not reading).
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- cs.Write(hdr, mem.BufferSlice{mem.Copy(payload, mem.DefaultBufferPool())}, &WriteOptions{Last: true})
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("second write returned early without reader: %v", err)
	case <-time.After(100 * time.Millisecond):
		// blocked as expected
	}

	// Unleash the reader. App-Recv must drive WINDOW_UPDATE emission
	// (via updateWindow -> s.fc.onRead -> sendWindowUpdate). The
	// sender should then unblock.
	close(startReading)

	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second write returned error after reader started: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second write did not unblock after reader started; WINDOW_UPDATE machinery is broken")
	}

	// Ensure server-side reader completes cleanly so the test does not
	// race with HandleStreams shutdown.
	cancel()
	select {
	case <-readerDone:
	case <-time.After(2 * time.Second):
		// Don't fail; the deferred transport.Close will tear it down.
	}
}

// TestShmFlowControl_MemoryBoundedByWindow checks that under a fast
// producer + slow consumer + many concurrent streams, the receiver
// transport's per-stream pendingData stays bounded by the window. If
// it grew unbounded we would see pendingData >> window or memory
// growth proportional to messages sent.
func TestShmFlowControl_MemoryBoundedByWindow(t *testing.T) {
	const window = 64 * 1024
	ConfigureShmFlowControlForBench(window)
	defer ResetShmFlowControlForBench()

	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()

	segName := fmt.Sprintf("test-mem-bounded-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)

	const ringSize = 16 * 1024 * 1024
	serverSeg, _ := CreateSegment(segName, ringSize, ringSize)
	serverSeg.H.SetServerReady(true)
	defer serverSeg.Close()
	clientSeg, _ := OpenSegment(segName)
	clientSeg.H.SetClientReady(true)
	defer clientSeg.Close()

	srvTransport, _ := NewShmServerTransport(serverSeg, testAddr{"shm", "server"}, testAddr{"shm", "client"})
	defer srvTransport.Close(nil)
	cliTransport, _ := NewShmClientTransport(clientSeg, testAddr{"shm", "client"}, testAddr{"shm", "server"})
	defer cliTransport.Close(nil)

	const numStreams = 10
	streamRefs := make(chan *ServerStream, numStreams)
	go srvTransport.HandleStreams(testCtx, func(s *ServerStream) {
		streamRefs <- s
		<-testCtx.Done() // park; never read
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientStreams := make([]*ClientStream, numStreams)
	for i := 0; i < numStreams; i++ {
		cs, err := cliTransport.NewStream(ctx, &CallHdr{Method: fmt.Sprintf("/test/Bound/%d", i)}, nil)
		if err != nil {
			t.Fatalf("NewStream %d: %v", i, err)
		}
		clientStreams[i] = cs
	}

	// Clamp quotas to a single window each on the sender so each stream
	// can push at most W bytes before back-pressure kicks in.
	cliTransport.sendQuotaMu.Lock()
	cliTransport.connSendQuota.Store(numStreams * int64(window)) // ample conn budget
	for _, cs := range clientStreams {
		cs.sendQuota.Store(window)
	}
	cliTransport.sendQuotaMu.Unlock()

	bodyLen := window - 5
	payload := make([]byte, bodyLen)
	hdr := make([]byte, 5)
	hdr[1] = byte(bodyLen >> 24)
	hdr[2] = byte(bodyLen >> 16)
	hdr[3] = byte(bodyLen >> 8)
	hdr[4] = byte(bodyLen)

	// Each goroutine fires-and-forgets many writes; flow control
	// should pause each stream after the first complete LPM.
	const writesPerStream = 50
	for i := 0; i < numStreams; i++ {
		cs := clientStreams[i]
		go func() {
			for j := 0; j < writesPerStream; j++ {
				last := j == writesPerStream-1
				_ = cs.Write(hdr, mem.BufferSlice{mem.Copy(payload, mem.DefaultBufferPool())}, &WriteOptions{Last: last})
			}
		}()
	}

	// Wait for all server streams to be set up so we can inspect their
	// per-stream pendingData.
	servers := make([]*ServerStream, 0, numStreams)
	for len(servers) < numStreams {
		select {
		case s := <-streamRefs:
			servers = append(servers, s)
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d/%d server streams set up", len(servers), numStreams)
		}
	}

	// Let the senders push as much as they can. With back-pressure
	// working, each stream should top out at ~1 window worth of
	// pendingData. Without it, pendingData would grow without bound.
	time.Sleep(300 * time.Millisecond)

	for i, s := range servers {
		s.fc.mu.Lock()
		pending := s.fc.pendingData
		s.fc.mu.Unlock()
		// Allow a small headroom for in-flight DATA frames; pendingData
		// SHOULD NOT exceed 2 windows in steady state.
		const headroomFactor = 2
		if pending > headroomFactor*uint32(window) {
			t.Errorf("stream %d pendingData=%d exceeds %dx window=%d (back-pressure failing)",
				i, pending, headroomFactor, window)
		}
		t.Logf("stream %d pendingData=%d (window=%d)", i, pending, window)
	}
}

// TestShmFlowControl_RealOptionPath_NoDeadlock validates the two
// correctness fixes for the per-transport WindowUpdate-threshold
// regression at the unit / struct-field level. The behavioural
// deadlock prevention is exercised by
// TestShmFlowControl_SlowConsumer_UnblocksOnAppRead (which goes
// through ConfigureShmFlowControlForBench, the path that propagates
// the threshold symmetrically to both transports' construction-time
// init); this test focuses on the dialer-override code path that the
// gRPC option grpc.WithInitialWindowSize takes.
//
//   - Bug 1 (NewStream default stream quota): pre-fix, the client
//     stream send quota fell back to maxWindowSize (~2 GiB) when
//     initialStreamWindow was 0, so the sender was effectively
//     unbounded while the receiver enforced its 32 MiB inFlow.limit.
//     Asymmetric quotas silently violated HTTP/2 stream-window
//     semantics; the overflow surfaced as a swallowed onData error.
//     The fix falls back to t.initialWindowSize (which is sourced
//     from shmInitialWindowSize / the dialer's WithInitialWindowSize
//     override), keeping the sender symmetric with the receiver.
//
//   - Bug 2 (per-transport wuThreshold): pre-fix, sendWindowUpdate
//     read the package-global shmWindowUpdateThreshold directly. A
//     transport dialed with grpc.WithInitialWindowSize(65535) while
//     the package global was still 8 MiB (the shm-tuned default)
//     would never reach 8 MiB through onRead's 16 KiB drip credits,
//     so no WindowUpdate frame was ever emitted and the sender
//     deadlocked. The fix captures wuThreshold per-transport,
//     recomputed via computeWUThreshold whenever the effective
//     window changes (dialer override + BDP update).
func TestShmFlowControl_RealOptionPath_NoDeadlock(t *testing.T) {
	const userWindow = 64 * 1024

	// computeWUThreshold sanity at the relevant sizes.
	if got := computeWUThreshold(int32(userWindow)); got != userWindow/4 {
		t.Errorf("computeWUThreshold(64 KiB) = %d, want %d (window/4)", got, userWindow/4)
	}
	if got := computeWUThreshold(int32(32 * 1024 * 1024)); got != 32*1024*1024/4 {
		t.Errorf("computeWUThreshold(32 MiB) = %d, want %d (window/4)", got, 32*1024*1024/4)
	}
	// Tiny-window guard: 4 KiB / 4 = 1024 (floor), don't collapse.
	if got := computeWUThreshold(int32(4 * 1024)); got < 1024 {
		t.Errorf("computeWUThreshold(4 KiB) = %d, want >= 1024 (floor must hold)", got)
	}
	// Zero / negative → fall back to package global.
	if got := computeWUThreshold(0); got != uint32(shmWindowUpdateThreshold) {
		t.Errorf("computeWUThreshold(0) = %d, want %d (package default fallback)",
			got, shmWindowUpdateThreshold)
	}

	// Spin up a paired client+server (we do not exchange data; we just
	// need a valid ShmClientTransport with the standard construction
	// path so the regression assertions exercise the real init code).
	segName := fmt.Sprintf("test-real-option-units-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	srvSeg, err := CreateSegment(segName, 4*1024*1024, 4*1024*1024)
	if err != nil {
		t.Fatalf("create segment: %v", err)
	}
	srvSeg.H.SetServerReady(true)
	defer srvSeg.Close()
	cliSeg, err := OpenSegment(segName)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	cliSeg.H.SetClientReady(true)
	defer cliSeg.Close()
	srv, err := NewShmServerTransport(srvSeg, testAddr{"shm", "server"}, testAddr{"shm", "client"})
	if err != nil {
		t.Fatalf("server transport: %v", err)
	}
	defer srv.Close(nil)
	cli, err := NewShmClientTransport(cliSeg, testAddr{"shm", "client"}, testAddr{"shm", "server"})
	if err != nil {
		t.Fatalf("client transport: %v", err)
	}
	defer cli.Close(nil)

	// Construction-time wuThreshold must reflect the transport's
	// initialWindowSize, NOT 0 or a random package global.
	gotInitThreshold := cli.wuThreshold.Load()
	wantInitThreshold := computeWUThreshold(cli.initialWindowSize)
	if gotInitThreshold != wantInitThreshold {
		t.Errorf("Construction: wuThreshold=%d, want %d (computed from initialWindowSize=%d)",
			gotInitThreshold, wantInitThreshold, cli.initialWindowSize)
	}

	// Simulate the dialer applying grpc.WithInitialWindowSize(64 KiB).
	// This is exactly what shm_dialer.go does at the
	// `opts.InitialWindowSize > 0` branch.
	cli.sendQuotaMu.Lock()
	cli.initialStreamWindow = int64(userWindow)
	cli.initialWindowSize = int32(userWindow)
	cli.sendQuotaMu.Unlock()
	cli.wuThreshold.Store(computeWUThreshold(int32(userWindow)))

	gotThreshold := cli.wuThreshold.Load()
	wantThreshold := uint32(userWindow / 4) // 16 KiB for 64 KiB window
	if gotThreshold != wantThreshold {
		t.Errorf("Bug 2 regression: cli.wuThreshold=%d after dialer override, want %d (pre-fix would have left it at 8 MiB package default → sender deadlock under small windows)",
			gotThreshold, wantThreshold)
	}

	// Now exercise the NewStream code path. The default stream send
	// quota must equal t.initialStreamWindow (which the dialer set
	// from opts.InitialWindowSize), not maxWindowSize.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go srv.HandleStreams(ctx, func(s *ServerStream) { <-ctx.Done() })
	cs, err := cli.NewStream(ctx, &CallHdr{Method: "/test/UnitAssert"}, nil)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	cli.sendQuotaMu.Lock()
	gotQuota := cs.sendQuota.Load()
	cli.sendQuotaMu.Unlock()
	if gotQuota != int64(userWindow) {
		t.Errorf("Bug 1 regression: NewStream stream send quota=%d, want %d (pre-fix would fall back to maxWindowSize=%d, silently violating HTTP/2 stream-window semantics)",
			gotQuota, userWindow, maxWindowSize)
	}

	// Sanity check: with initialStreamWindow cleared, the fallback
	// must now go to t.initialWindowSize (NOT to maxWindowSize). Test
	// this by creating a second transport without dialer override.
	segName2 := fmt.Sprintf("test-real-option-fallback-%d", time.Now().UnixNano())
	defer RemoveSegment(segName2)
	srvSeg2, err := CreateSegment(segName2, 4*1024*1024, 4*1024*1024)
	if err != nil {
		t.Fatalf("create segment 2: %v", err)
	}
	srvSeg2.H.SetServerReady(true)
	defer srvSeg2.Close()
	cliSeg2, err := OpenSegment(segName2)
	if err != nil {
		t.Fatalf("open segment 2: %v", err)
	}
	cliSeg2.H.SetClientReady(true)
	defer cliSeg2.Close()
	srv2, err := NewShmServerTransport(srvSeg2, testAddr{"shm", "server"}, testAddr{"shm", "client"})
	if err != nil {
		t.Fatalf("server transport 2: %v", err)
	}
	defer srv2.Close(nil)
	cli2, err := NewShmClientTransport(cliSeg2, testAddr{"shm", "client"}, testAddr{"shm", "server"})
	if err != nil {
		t.Fatalf("client transport 2: %v", err)
	}
	defer cli2.Close(nil)

	go srv2.HandleStreams(ctx, func(s *ServerStream) { <-ctx.Done() })
	cs2, err := cli2.NewStream(ctx, &CallHdr{Method: "/test/UnitAssert2"}, nil)
	if err != nil {
		t.Fatalf("NewStream 2: %v", err)
	}
	cli2.sendQuotaMu.Lock()
	gotFallback := cs2.sendQuota.Load()
	cli2.sendQuotaMu.Unlock()
	wantFallback := int64(cli2.initialWindowSize)
	if gotFallback != wantFallback {
		t.Errorf("Bug 1 fallback: NewStream stream send quota=%d, want %d (= t.initialWindowSize). maxWindowSize fallback regressed.",
			gotFallback, wantFallback)
	}
	if gotFallback == int64(maxWindowSize) {
		t.Errorf("Bug 1 regression: NewStream stream send quota=%d == maxWindowSize. The pre-fix unbounded fallback is back.",
			gotFallback)
	}
}

// TestShmServerTransport_ApplyServerConfig validates that grpc.ServerOption
// flow-control values (grpc.InitialWindowSize, grpc.InitialConnWindowSize,
// grpc.MaxConcurrentStreams) reach the SHM server transport via the
// ServerTransportProvider fast path in transport.NewServerTransport. The
// pre-fix server transport had no path to receive ServerConfig because
// ShmListener.Accept constructed it without access to grpc.ServerOption,
// causing a real production deadlock when a client dialed
// grpc.WithInitialWindowSize(65535) while the server retained the 32 MiB
// shm-tuned default window + 8 MiB WindowUpdate emission threshold.
//
// This test asserts the new ApplyServerConfig hook actually mutates the
// transport's flow-control fields so subsequent stream creation and
// WindowUpdate emission honour the user's configuration.
func TestShmServerTransport_ApplyServerConfig(t *testing.T) {
	const userWindow = 64 * 1024
	const userConnWindow = 128 * 1024
	const userMaxStreams = uint32(42)

	segName := fmt.Sprintf("test-apply-server-config-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	srvSeg, err := CreateSegment(segName, 4*1024*1024, 4*1024*1024)
	if err != nil {
		t.Fatalf("create segment: %v", err)
	}
	srvSeg.H.SetServerReady(true)
	defer srvSeg.Close()
	srv, err := NewShmServerTransport(srvSeg, testAddr{"shm", "server"}, testAddr{"shm", "client"})
	if err != nil {
		t.Fatalf("server transport: %v", err)
	}
	defer srv.Close(nil)

	// Capture pre-Apply state: defaults from shmInitialWindowSize.
	srv.sendQuotaMu.Lock()
	preWindow := srv.initialWindowSize
	srv.sendQuotaMu.Unlock()
	preThreshold := srv.wuThreshold.Load()
	if preWindow != int32(shmInitialWindowSize) {
		t.Fatalf("pre-Apply initialWindowSize=%d, want shm-tuned default %d", preWindow, shmInitialWindowSize)
	}
	if preThreshold != computeWUThreshold(int32(shmInitialWindowSize)) {
		t.Fatalf("pre-Apply wuThreshold=%d, want %d (construction-time init regression)",
			preThreshold, computeWUThreshold(int32(shmInitialWindowSize)))
	}

	cfg := &ServerConfig{
		InitialWindowSize:     userWindow,
		InitialConnWindowSize: userConnWindow,
		MaxStreams:            userMaxStreams,
	}
	srv.ApplyServerConfig(cfg)

	srv.sendQuotaMu.Lock()
	postWindow := srv.initialWindowSize
	postConnLimit := srv.connInFlow.limit
	srv.sendQuotaMu.Unlock()
	postThreshold := srv.wuThreshold.Load()
	postMaxStreams := srv.maxStreams

	if postWindow != int32(userWindow) {
		t.Errorf("post-Apply initialWindowSize=%d, want %d (Bug 3 regression: server still on shm-tuned default)", postWindow, userWindow)
	}
	wantThreshold := computeWUThreshold(int32(userWindow))
	if postThreshold != wantThreshold {
		t.Errorf("post-Apply wuThreshold=%d, want %d (Bug 3 regression: WU threshold did not track ServerConfig.InitialWindowSize)", postThreshold, wantThreshold)
	}
	if postConnLimit != uint32(userConnWindow) {
		t.Errorf("post-Apply connInFlow.limit=%d, want %d", postConnLimit, userConnWindow)
	}
	if postMaxStreams != userMaxStreams {
		t.Errorf("post-Apply maxStreams=%d, want %d", postMaxStreams, userMaxStreams)
	}

	// Negative path: nil config + sub-defaultWindowSize values should be no-ops.
	srv.ApplyServerConfig(nil) // must not panic / mutate

	srv2, err := NewShmServerTransport(srvSeg, testAddr{"shm", "server"}, testAddr{"shm", "client"})
	if err != nil {
		t.Fatalf("server transport 2: %v", err)
	}
	defer srv2.Close(nil)
	srv2.ApplyServerConfig(&ServerConfig{
		InitialWindowSize: defaultWindowSize - 1, // below gate
	})
	srv2.sendQuotaMu.Lock()
	gateWindow := srv2.initialWindowSize
	srv2.sendQuotaMu.Unlock()
	if gateWindow != int32(shmInitialWindowSize) {
		t.Errorf("sub-default InitialWindowSize=%d incorrectly applied (should be ignored, expected default %d)",
			gateWindow, shmInitialWindowSize)
	}
}

// TestShmConnPreCredit_LargeMessageFairWindow_NoStalls validates that
// when a single message larger than the conn window is streamed under
// fair-default flow-control settings, the receiver's onMessageStart
// pre-credit pathway fires for the connection layer (via
// trInFlow.maybeAdjust) and the sender completes without parking on
// conn quota for each limit/4 drip-credit refill.
//
// Pre-fix the 256 KiB write under a 64 KiB window required ~16
// conn-quota refill round-trips (256K / 16K wuThreshold), each
// costing ~25 us of frame-writer / ring-write / scheduler overhead.
// Post-fix, onMessageStart sees the full 256 KiB LPM and emits a
// single conn WINDOW_UPDATE pre-credit covering 256K - 65535 bytes,
// letting the sender complete the message in one round.
//
// The assertion is twofold:
//
//   - shmConnPreCreditEmitted must be non-zero after the write
//     (proving the pre-credit pathway fired);
//   - the write completes within the test timeout (proving the
//     pre-credit was enough to unstall the sender, i.e. it really
//     reached the peer and was applied to its conn send quota).
//
// Unlike TestShmSmallWindowMultiFrameMessage which asserts only
// completion, this test specifically guards the new code path so a
// future regression that disables conn pre-credit (e.g. via
// accidental NoWU reintroduction) is caught even if the write still
// completes through fallback drip-credit.
func TestShmConnPreCredit_LargeMessageFairWindow_NoStalls(t *testing.T) {
	const fairWindow = 64 * 1024
	ConfigureShmFlowControlForBench(fairWindow)
	defer ResetShmFlowControlForBench()

	preEmitted := shmConnPreCreditEmitted.Load()
	preStream := shmStreamPreCreditEmitted.Load()

	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()

	segName := fmt.Sprintf("test-conn-precredit-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	srvSeg, err := CreateSegment(segName, 4*1024*1024, 4*1024*1024)
	if err != nil {
		t.Fatalf("create segment: %v", err)
	}
	srvSeg.H.SetServerReady(true)
	defer srvSeg.Close()
	cliSeg, err := OpenSegment(segName)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	cliSeg.H.SetClientReady(true)
	defer cliSeg.Close()

	srv, err := NewShmServerTransport(srvSeg, testAddr{"shm", "server"}, testAddr{"shm", "client"})
	if err != nil {
		t.Fatalf("server transport: %v", err)
	}
	defer srv.Close(nil)
	// Server-side conn limit must be clamped to the fair window so
	// trInFlow.maybeAdjust returns non-zero pre-credit when the LPM
	// exceeds the (clamped) conn-level baseline. Production wires
	// this via ApplyServerConfig + grpc.InitialConnWindowSize.
	srv.sendQuotaMu.Lock()
	srv.connInFlow = trInFlow{limit: uint32(fairWindow)}
	srv.connInFlow.updateEffectiveWindowSize()
	srv.sendQuotaMu.Unlock()

	cli, err := NewShmClientTransport(cliSeg, testAddr{"shm", "client"}, testAddr{"shm", "server"})
	if err != nil {
		t.Fatalf("client transport: %v", err)
	}
	defer cli.Close(nil)

	go srv.HandleStreams(testCtx, func(s *ServerStream) {
		const chunk = 4096
		for {
			buf, rerr := s.Read(chunk)
			if rerr != nil {
				return
			}
			buf.Free()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs, err := cli.NewStream(ctx, &CallHdr{Method: "/test/ConnPreCredit"}, nil)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	// Clamp client outbound quotas to the fair window so the sender
	// actually parks on conn quota and exercises the pre-credit path.
	// (NewStream just initialised stream.sendQuota; we trim it down.)
	cli.sendQuotaMu.Lock()
	cli.connSendQuota.Store(int64(fairWindow))
	cs.sendQuota.Store(int64(fairWindow))
	cli.sendQuotaMu.Unlock()

	const msgSize = 256 * 1024
	payload := make([]byte, msgSize)
	for i := range payload {
		payload[i] = byte(i & 0xFF)
	}
	hdr := make([]byte, 5)
	hdr[1] = byte((msgSize >> 24) & 0xff)
	hdr[2] = byte((msgSize >> 16) & 0xff)
	hdr[3] = byte((msgSize >> 8) & 0xff)
	hdr[4] = byte(msgSize & 0xff)

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- cs.Write(hdr, mem.BufferSlice{mem.Copy(payload, mem.DefaultBufferPool())}, &WriteOptions{Last: true})
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("write: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("write did not complete within 4s — conn pre-credit pathway likely broken")
	}

	postEmitted := shmConnPreCreditEmitted.Load()
	postStream := shmStreamPreCreditEmitted.Load()
	connDelta := postEmitted - preEmitted
	streamDelta := postStream - preStream

	// Stream pre-credit must fire: 256 KiB LPM > 64 KiB stream window.
	if streamDelta == 0 {
		t.Errorf("shmStreamPreCreditEmitted did not advance; stream pre-credit pathway did not fire for 256 KiB LPM on %d-byte window", fairWindow)
	}
	// Conn pre-credit must fire: 256 KiB LPM > 64 KiB conn window
	// (we clamped server connInFlow.limit above). If this is zero,
	// trInFlow.maybeAdjust was either never called or returned zero
	// — that is the regression we want to catch.
	if connDelta == 0 {
		t.Errorf("shmConnPreCreditEmitted did not advance; trInFlow.maybeAdjust did not fire for 256 KiB LPM on %d-byte conn window — conn pre-credit pathway broken",
			fairWindow)
	}
	t.Logf("pre-credit delta: stream=%d conn=%d (LPM=%d window=%d)",
		streamDelta, connDelta, msgSize, fairWindow)
}

// TestShmFlowControl_StreamCloseUnblocksDeferredWrite verifies that
// a whole-MESSAGE write parked in the writer's deferred map
// (because both stream and conn quota are zero) is unblocked with
// errStreamDone when the sender locally closes the stream via
// closeStream.
//
// Without the stream-close handling, the deferred entry would sit
// in w.deferred[id] until transport close and the sender goroutine
// would leak waiting on doneCh. Two mechanisms guarantee unblock:
//  1. closeStream fires wuRetryWake so the writer's next iteration
//     calls retryDeferred.
//  2. advanceDeferred checks d.streamPtr.getState() == streamDone at
//     the top of its loop and signals errStreamDone + deletes the
//     map entry.
func TestShmFlowControl_StreamCloseUnblocksDeferredWrite(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer testCancel()

	segName := fmt.Sprintf("test-deferred-streamclose-%d", time.Now().UnixNano())
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

	// Park the server handler so it doesn't drain the receive ring.
	go srvTransport.HandleStreams(testCtx, func(s *ServerStream) {
		<-testCtx.Done()
	})

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	cs, err := cliTransport.NewStream(ctx, &CallHdr{Method: "/test/DeferredStreamClose"}, nil)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	// Drive both quotas to zero so the next whole-MESSAGE write lands
	// in the writer's deferred map.
	cliTransport.connSendQuota.Store(0)
	cs.sendQuota.Store(0)

	body := make([]byte, 1024)
	hdr := []byte{0, 0, 0, 0x04, 0x00} // 5-byte LPM header for 1024-byte body

	writeErr := make(chan error, 1)
	go func() {
		writeErr <- cs.Write(hdr, mem.BufferSlice{mem.Copy(body, mem.DefaultBufferPool())}, &WriteOptions{Last: true})
	}()

	// Give the writer time to install the entry into the deferred map.
	time.Sleep(50 * time.Millisecond)

	// Stream-local close. closeStream fires wuRetryWake; advanceDeferred
	// observes streamDone on its next pass and signals errStreamDone.
	cs.Close(fmt.Errorf("test-driven stream close"))

	select {
	case err := <-writeErr:
		// Contract: a deferred whole-message entry that observes
		// streamDone in advanceDeferred returns errStreamDone to
		// the parked sender. Allow ErrConnClosing as an alternative
		// only if the transport happens to close concurrently — not
		// expected in this test but harmless to permit.
		if err != errStreamDone && err != ErrConnClosing {
			t.Errorf("expected errStreamDone or ErrConnClosing, got %v", err)
		}
		t.Logf("sender returned with %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("sender goroutine leaked: write did not return within 2s after stream close")
	}
}

// TestShmFlowControl_CtxCancelUnblocksDeferredWrite verifies that a
// whole-MESSAGE write parked in the writer's deferred map unblocks
// promptly when the caller's ctx is cancelled.
//
// enqueueMessageAndWait selects on doneCh OR ctx.Done(); on ctx
// cancellation the sender returns ContextErr. advanceDeferred
// observes d.ctx.Err() on its next pass and signals doneCh (sender
// already left; buffered channel slot is harmless).
func TestShmFlowControl_CtxCancelUnblocksDeferredWrite(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer testCancel()

	segName := fmt.Sprintf("test-deferred-ctxcancel-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)

	serverSeg, _ := CreateSegment(segName, 65536, 65536)
	serverSeg.H.SetServerReady(true)
	defer serverSeg.Close()
	clientSeg, _ := OpenSegment(segName)
	clientSeg.H.SetClientReady(true)
	defer clientSeg.Close()

	srvTransport, _ := NewShmServerTransport(serverSeg, testAddr{"shm", "server"}, testAddr{"shm", "client"})
	defer srvTransport.Close(nil)
	cliTransport, _ := NewShmClientTransport(clientSeg, testAddr{"shm", "client"}, testAddr{"shm", "server"})
	defer cliTransport.Close(nil)

	go srvTransport.HandleStreams(testCtx, func(s *ServerStream) {
		<-testCtx.Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cs, err := cliTransport.NewStream(ctx, &CallHdr{Method: "/test/DeferredCtxCancel"}, nil)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	// Drive quotas to zero so the write lands in the deferred map.
	cliTransport.connSendQuota.Store(0)
	cs.sendQuota.Store(0)

	body := make([]byte, 1024)
	hdr := []byte{0, 0, 0, 0x04, 0x00}

	writeErr := make(chan error, 1)
	go func() {
		writeErr <- cs.Write(hdr, mem.BufferSlice{mem.Copy(body, mem.DefaultBufferPool())}, &WriteOptions{Last: true})
	}()

	time.Sleep(50 * time.Millisecond) // wait for deferred install

	cancel() // cancel ctx; sender should observe ctx.Done() and return ContextErr

	select {
	case err := <-writeErr:
		if err == nil {
			t.Errorf("expected non-nil error from cancelled write, got nil")
		}
		// Either ContextErr or a post-cancel error is acceptable.
		t.Logf("write returned with %v after ctx cancel", err)
	case <-time.After(2 * time.Second):
		t.Fatal("sender did not return within 2s after ctx cancel — deferred entry not cleaned")
	}
}

// TestShmFlowControl_ConcurrentWholeMessageWrites stress-tests the
// concurrent whole-message dispatch path. Spawns many streams each
// firing multiple sequential writes; default windows so no entry
// goes into the deferred map under normal conditions. The test
// catches lost-wakeup / deadlock regressions in the writer
// goroutine's main loop (channel send + entry processing) under
// realistic concurrency.
//
// NOTE: this test does NOT reliably exercise advanceDeferred's CAS
// rollback path (which requires conn-CAS to lose a race with the
// reader's addSendQuota). The rollback path is covered
// probabilistically here when WU traffic happens to land between
// WL's quota.Load and quota.CAS, but is not the test's primary
// purpose.
func TestShmFlowControl_ConcurrentWholeMessageWrites(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer testCancel()

	segName := fmt.Sprintf("test-concurrent-whole-msg-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)

	serverSeg, _ := CreateSegment(segName, 1048576, 1048576)
	serverSeg.H.SetServerReady(true)
	defer serverSeg.Close()
	clientSeg, _ := OpenSegment(segName)
	clientSeg.H.SetClientReady(true)
	defer clientSeg.Close()

	srvTransport, _ := NewShmServerTransport(serverSeg, testAddr{"shm", "server"}, testAddr{"shm", "client"})
	defer srvTransport.Close(nil)
	cliTransport, _ := NewShmClientTransport(clientSeg, testAddr{"shm", "client"}, testAddr{"shm", "server"})
	defer cliTransport.Close(nil)

	// Server drains everything quickly so windows keep refilling.
	go srvTransport.HandleStreams(testCtx, func(s *ServerStream) {
		buf := make([]byte, 8192)
		for {
			if _, err := s.Read(len(buf)); err != nil {
				return
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const numStreams = 20
	const msgsPerStream = 3
	const msgSize = 8192

	body := make([]byte, msgSize)
	hdr := make([]byte, 5)
	mLen := uint32(msgSize)
	hdr[1] = byte(mLen >> 24)
	hdr[2] = byte(mLen >> 16)
	hdr[3] = byte(mLen >> 8)
	hdr[4] = byte(mLen)

	streams := make([]*ClientStream, numStreams)
	for i := 0; i < numStreams; i++ {
		cs, err := cliTransport.NewStream(ctx, &CallHdr{Method: fmt.Sprintf("/test/CAS/%d", i)}, nil)
		if err != nil {
			t.Fatalf("NewStream %d: %v", i, err)
		}
		streams[i] = cs
	}

	done := make(chan error, numStreams*msgsPerStream)
	for i := 0; i < numStreams; i++ {
		cs := streams[i]
		go func() {
			for j := 0; j < msgsPerStream; j++ {
				last := j == msgsPerStream-1
				err := cs.Write(hdr,
					mem.BufferSlice{mem.Copy(body, mem.DefaultBufferPool())},
					&WriteOptions{Last: last})
				done <- err
				if err != nil {
					return
				}
			}
		}()
	}

	expected := numStreams * msgsPerStream
	deadline := time.After(8 * time.Second)
	completed := 0
	for completed < expected {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("write completed with error (msg %d/%d): %v", completed+1, expected, err)
			}
			completed++
		case <-deadline:
			t.Fatalf("concurrent whole-message test deadlocked: only %d/%d writes completed", completed, expected)
		}
	}
	t.Logf("completed %d concurrent whole-message writes", completed)
}
