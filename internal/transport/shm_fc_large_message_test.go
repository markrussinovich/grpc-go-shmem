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

// TestShmEnsureStreamWindow covers the window-growth primitive that keeps
// oversized messages off the inFlow.delta pre-credit ledger.
//
// Background, since the invariants below are not obvious in isolation:
// inFlow.delta is a loan pool. onMessageStart lends a sender the capacity
// an in-flight LPM needs but the window does not currently have, and
// inFlow.onRead repays the loan by withholding an equal number of bytes
// from the WINDOW_UPDATE it would otherwise emit. That ledger balances
// only while loans are transient. A message larger than the window can
// never be admitted by the window alone, so it needs a loan every single
// time it is sent, and the repayments fall behind until the peer's send
// quota is exhausted and the stream wedges. See
// TestShmPipelinedOversizedMessages in benchmark/shmsccmp for the
// end-to-end reproduction.
//
// shmEnsureStreamWindow breaks that cycle by raising the window itself, so
// oversized messages stop needing a loan at all. Hence the assertions:
// growth is exactly the shortfall, it is monotonic and idempotent so a
// steady state is reached after the first message of a new size, it is
// clamped to the 31-bit HTTP/2 window, and it never touches delta.
func TestShmEnsureStreamWindow(t *testing.T) {
	tests := []struct {
		name      string
		limit     uint32
		n         uint32
		wantGrant uint32
		wantLimit uint32
	}{
		{"fits exactly", 1000, 1000, 0, 1000},
		{"fits under", 1000, 999, 0, 1000},
		{"one byte over", 1000, 1001, 1, 1001},
		{"well over", 1000, 5000, 4000, 5000},
		{"zero", 1000, 0, 0, 1000},
		{"saturates at max window", maxWindowSize - 10, maxWindowSize, 10, maxWindowSize},
		{"clamps past max window", maxWindowSize - 10, maxWindowSize + 500, 10, maxWindowSize},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &inFlow{limit: tc.limit}
			if got := f.shmEnsureStreamWindow(tc.n); got != tc.wantGrant {
				t.Errorf("shmEnsureStreamWindow(%d) grant = %d, want %d", tc.n, got, tc.wantGrant)
			}
			if f.limit != tc.wantLimit {
				t.Errorf("limit = %d, want %d", f.limit, tc.wantLimit)
			}
			// The point of growing the window is to stay off the loan
			// ledger, so delta must be left alone.
			if f.delta != 0 {
				t.Errorf("delta = %d, want 0 (window growth must not touch the delta loan pool)", f.delta)
			}
			// Idempotent: repeating the same size grants nothing more,
			// so a stream that keeps sending one size pays for one
			// WINDOW_UPDATE, not one per message.
			if got := f.shmEnsureStreamWindow(tc.n); got != 0 {
				t.Errorf("second shmEnsureStreamWindow(%d) grant = %d, want 0", tc.n, got)
			}
			// Monotonic: a smaller message never shrinks the window.
			if tc.n > 0 {
				f.shmEnsureStreamWindow(tc.n / 2)
				if f.limit != tc.wantLimit {
					t.Errorf("after smaller request, limit = %d, want %d (must be monotonic)", f.limit, tc.wantLimit)
				}
			}
		})
	}
}

// TestShmOversizedMessageRoundTrip is a transport-level smoke test for
// messages larger than the stream flow-control window.
//
// Scope, stated plainly so this is not mistaken for the regression test:
// this exercises the shmEnsureStreamWindow path (the window does grow here)
// and proves oversized messages round-trip over raw transports, but it does
// NOT reproduce the deadlock it was written alongside — it passes with the
// fix reverted.
//
// The reason is worth recording. The loan taken by onMessageStart is
// maybeAdjustAdditive's need = n - (limit + delta - pendingData -
// pendingUpdate), so its size depends on how far the *reader* is behind,
// not on the message size alone. This test's reader consumes each message
// before the next one's header is parsed, so the shortfall is only the
// 5 bytes of LPM header and the decay is too slow to wedge the stream. To
// make it fatal the receiving application has to lag by roughly a whole
// message, which is what a real handler does when it unmarshals a request
// and marshals a large reply. That needs the full stack, so the
// discriminating test lives in benchmark/shmsccmp.
func TestShmOversizedMessageRoundTrip(t *testing.T) {
	// Deliberately runs at the production window and frame size: the
	// behaviour under test only appears above shmInitialWindowSize.
	const msgSize = 32 * 1024 * 1024
	const rounds = 4
	const ringSize = 64 * 1024 * 1024

	if msgSize < shmInitialWindowSize {
		t.Fatalf("test is meaningless unless the message exceeds the window: msgSize=%d window=%d",
			msgSize, shmInitialWindowSize)
	}

	testCtx, testCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer testCancel()

	segName := fmt.Sprintf("test-oversized-roundtrip-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)

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

	payload := make([]byte, msgSize)
	for i := range payload {
		payload[i] = byte(i & 0xFF)
	}
	hdr := make([]byte, 5)
	binary.BigEndian.PutUint32(hdr[1:5], uint32(msgSize))

	// readMessage pulls one whole LPM: the 5-byte header, then the body it
	// advertises. Reading the body is what drives inFlow.onRead, so every
	// message has to be consumed in full for the accounting to be
	// exercised at all.
	readMessage := func(read func(int) (mem.BufferSlice, error)) (int, error) {
		h, err := read(5)
		if err != nil {
			return 0, err
		}
		var hb [5]byte
		h.CopyTo(hb[:])
		h.Free()
		n := int(binary.BigEndian.Uint32(hb[1:5]))
		body, err := read(n)
		if err != nil {
			return 0, err
		}
		got := body.Len()
		body.Free()
		return got, nil
	}

	srvErr := make(chan error, 1)
	go srvTransport.HandleStreams(testCtx, func(si ServerStreamIface) {
		s := si.(*ServerStream)
		for i := 0; i < rounds; i++ {
			n, err := readMessage(s.Read)
			if err != nil {
				srvErr <- fmt.Errorf("server read %d: %w", i, err)
				return
			}
			if n != msgSize {
				srvErr <- fmt.Errorf("server read %d: got %d bytes, want %d", i, n, msgSize)
				return
			}
			// Reply in kind so the server->client direction gets the
			// same treatment, not just client->server.
			if err := s.Write(hdr, mem.BufferSlice{mem.Copy(payload, mem.DefaultBufferPool())}, &WriteOptions{}); err != nil {
				srvErr <- fmt.Errorf("server write %d: %w", i, err)
				return
			}
		}
		_ = s.WriteStatus(status.New(codes.OK, ""))
		srvErr <- nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	csI, err := cliTransport.NewStream(ctx, &CallHdr{Method: "/test/OversizedRoundTrip"}, nil)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	cs := csI.(*ClientStream)

	grownBefore := shmStreamWindowGrown.Load()

	// Sender and receiver are decoupled so writes are not serialised
	// behind reads.
	done := make(chan error, 2)
	go func() {
		for i := 0; i < rounds; i++ {
			err := cs.Write(hdr,
				mem.BufferSlice{mem.Copy(payload, mem.DefaultBufferPool())},
				&WriteOptions{Last: i == rounds-1})
			if err != nil {
				done <- fmt.Errorf("client write %d: %w", i, err)
				return
			}
		}
		done <- nil
	}()
	go func() {
		for i := 0; i < rounds; i++ {
			n, err := readMessage(cs.Read)
			if err != nil {
				done <- fmt.Errorf("client read %d: %w", i, err)
				return
			}
			if n != msgSize {
				done <- fmt.Errorf("client read %d: got %d bytes, want %d", i, n, msgSize)
				return
			}
		}
		done <- nil
	}()

	timeout := time.After(40 * time.Second)
	for pending := 2; pending > 0; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			pending--
		case err := <-srvErr:
			if err != nil {
				t.Fatal(err)
			}
		case <-timeout:
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			t.Logf("STALLED; goroutine dump (%d bytes):", n)
			for _, line := range strings.Split(string(buf[:n]), "\n") {
				if strings.Contains(line, "transport.") ||
					strings.Contains(line, "Shm") ||
					strings.Contains(line, "goroutine ") {
					t.Logf("  %s", line)
				}
			}
			t.Fatalf("%d pipelined %d-byte messages stalled (window is %d bytes)",
				rounds, msgSize, shmInitialWindowSize)
		}
	}

	// Guard the premise: if the window never grew, the messages were not
	// actually oversized relative to the window and this test has
	// silently stopped testing anything.
	if grown := shmStreamWindowGrown.Load() - grownBefore; grown == 0 {
		t.Errorf("shmStreamWindowGrown did not advance; the oversized-message path was not exercised")
	}
}
