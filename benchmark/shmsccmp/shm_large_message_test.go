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

package shmsccmp

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/benchmark"
	"google.golang.org/grpc/experimental/shm"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	shmsc "google.golang.org/grpc/plugin/shmsc"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
)

// largeMsgEnv is a testing.T-friendly counterpart to newMonoEnv, which is
// written against *testing.B.
type largeMsgEnv struct {
	conn   *grpc.ClientConn
	client testgrpc.BenchmarkServiceClient
	close  func()
}

func newLargeMsgEnv(t *testing.T) *largeMsgEnv {
	t.Helper()
	name := fmt.Sprintf("largemsg_mono_%d", time.Now().UnixNano())
	lis, err := shm.NewListener(name, nil)
	if err != nil {
		t.Fatalf("shm.NewListener: %v", err)
	}
	stop := benchmark.StartServer(benchmark.ServerInfo{Type: "protobuf", Listener: lis}, serverOpts()...)
	conn, err := grpc.NewClient("shm://"+name, append(dialOpts(), shm.WithTransport())...)
	if err != nil {
		stop()
		lis.Close()
		os.Remove("/dev/shm/grpc_shm_" + name)
		t.Fatalf("grpc.NewClient: %v", err)
	}
	e := &largeMsgEnv{
		conn:   conn,
		client: testgrpc.NewBenchmarkServiceClient(conn),
	}
	e.close = func() {
		conn.Close()
		stop()
		lis.Close()
		os.Remove("/dev/shm/grpc_shm_" + name)
	}
	return e
}

// newLargeMsgPluginEnv is the same for the self-contained plugin
// transport, which carries a mirrored copy of the flow-control code and so
// needs the same coverage.
func newLargeMsgPluginEnv(t *testing.T) *largeMsgEnv {
	t.Helper()
	name := fmt.Sprintf("largemsg_plugin_%d", time.Now().UnixNano())
	lis, err := shmsc.Listen(name)
	if err != nil {
		t.Fatalf("shmsc.Listen: %v", err)
	}
	stop := benchmark.StartServer(benchmark.ServerInfo{Type: "protobuf", Listener: lis}, serverOpts()...)

	r := manual.NewBuilderWithScheme("largemsg")
	r.InitialState(resolver.State{
		Addresses: []resolver.Address{{Addr: name, TransportType: shmsc.Name}},
	})
	conn, err := grpc.NewClient("largemsg:///"+name, append(dialOpts(), grpc.WithResolvers(r))...)
	if err != nil {
		stop()
		lis.Close()
		removeSegment(name)
		t.Fatalf("grpc.NewClient: %v", err)
	}
	e := &largeMsgEnv{
		conn:   conn,
		client: testgrpc.NewBenchmarkServiceClient(conn),
	}
	e.close = func() {
		conn.Close()
		stop()
		lis.Close()
		removeSegment(name)
	}
	return e
}

// largeMsgArms is both SHM transport implementations. The fix for the
// deadlock below lives in duplicated code, so a regression in either tree
// has to be caught here.
var largeMsgArms = []struct {
	name   string
	newEnv func(*testing.T) *largeMsgEnv
}{
	{"mono", newLargeMsgEnv},
	{"plugin", newLargeMsgPluginEnv},
}

// largeMsgCase is one (transport arm, message size) pair.
type largeMsgCase struct {
	n      int
	label  string
	newEnv func(*testing.T) *largeMsgEnv
}

// crossArms expands a size ladder over both transport implementations.
func crossArms(sizes []struct {
	n     int
	label string
}) []largeMsgCase {
	cases := make([]largeMsgCase, 0, len(sizes)*len(largeMsgArms))
	for _, arm := range largeMsgArms {
		for _, sz := range sizes {
			cases = append(cases, largeMsgCase{
				n:      sz.n,
				label:  arm.name + "_" + sz.label,
				newEnv: arm.newEnv,
			})
		}
	}
	return cases
}

// TestShmPipelinedOversizedMessages is the regression test for the
// oversized-message send-quota decay deadlock. Before the fix, every size at
// or above 32 MiB wedged permanently; 32 MiB stalled with 5 of 8 messages
// sent and 1 received.
//
// # The bug
//
// inFlow.delta is a loan pool. When a message arrives that the stream's
// current receive capacity cannot admit, onMessageStart lends the sender the
// shortfall out of delta, and inFlow.onRead repays that loan by withholding
// an equal number of bytes from the WINDOW_UPDATE it would otherwise emit as
// the application consumes the message.
//
// The shortfall is computed against *available* capacity, not the window:
// need = n - (limit + delta - pendingData - pendingUpdate). So the loan grows
// with how far the reader is behind. That is fine while loans are transient,
// because catching up on reads restores the capacity and the ledger settles.
//
// It stops being transient once a message is larger than the window itself.
// Such a message can never be admitted by the window alone, so it needs a
// loan every single time one is sent, no matter how promptly the application
// reads. Under pipelining the loans accumulate in the single delta pool while
// onRead drains it at the rate the application consumes bytes. Once the pool
// exceeds the message being read, that read emits no WINDOW_UPDATE at all and
// the sender is never re-credited for bytes it has already delivered. Each
// oversized message therefore erodes the peer's send quota a little further,
// and because onMessageStart is the only thing that can mint credit for such
// a message and it fires exactly once per LPM, the erosion never recovers.
// Eventually the sender parks with a handful of bytes of a message left to
// send and zero quota, while the receiver waits on an LPM that can never
// complete.
//
// The fix, shmEnsureStreamWindow, raises the window to cover the message so
// oversized messages stop needing a loan at all.
//
// # Why the test is shaped like this
//
// Three things all have to be true, which is why this went unnoticed:
//
//   - The message must exceed shmInitialWindowSize (32 MiB), so the payloads
//     have to be larger than anything the benchmarks or the transport suite
//     exercised. The 31 MiB case below is the control: it sits just under the
//     window and passed even before the fix.
//   - Messages must be pipelined. A single message in flight always
//     completes: one loan, settled by one read. Hence the decoupled sender
//     and receiver, and hence rounds.
//   - The receiving application must lag the sender by roughly a message, or
//     each loan is settled before the next is taken. A real handler does this
//     naturally by unmarshalling a large request and marshalling a large
//     reply, which is why this test drives the full stack instead of raw
//     transports. A raw-transport version of this test does not reproduce the
//     deadlock; see TestShmOversizedMessageRoundTrip in internal/transport.
func TestShmPipelinedOversizedMessages(t *testing.T) {
	sizes := []struct {
		n     int
		label string
	}{
		{31 << 20, "31MiB_control_under_window"},
		{32 << 20, "32MiB"},
		{33 << 20, "33MiB"},
		{63 << 20, "63MiB"},
	}
	// Pre-fix, 32 MiB wedged on the 5th message. Enough rounds to clear
	// that comfortably, few enough to keep the test around a second per
	// size.
	const rounds = 16

	for _, sz := range crossArms(sizes) {
		t.Run(sz.label, func(t *testing.T) {
			env := sz.newEnv(t)
			defer env.close()

			req := &testpb.SimpleRequest{
				ResponseType: testpb.PayloadType_COMPRESSABLE,
				ResponseSize: int32(sz.n),
				Payload:      benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, sz.n),
			}

			// Passing runs finish in under a second even at 63 MiB, so
			// this is only a stall detector; keep it short enough that a
			// regression fails quickly.
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			stream, err := env.client.StreamingCall(ctx)
			if err != nil {
				t.Fatalf("StreamingCall: %v", err)
			}

			done := make(chan error, 2)
			start := time.Now()

			var sent, recvd int64
			go func() {
				for i := 0; i < rounds; i++ {
					if err := stream.Send(req); err != nil {
						done <- fmt.Errorf("Send %d: %w", i, err)
						return
					}
					atomic.AddInt64(&sent, 1)
				}
				done <- nil
			}()
			go func() {
				for i := 0; i < rounds; i++ {
					if _, err := stream.Recv(); err != nil {
						done <- fmt.Errorf("Recv %d: %w", i, err)
						return
					}
					atomic.AddInt64(&recvd, 1)
				}
				done <- nil
			}()

			for i := 0; i < 2; i++ {
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("%s: %v (sent=%d recvd=%d after %v)", sz.label, err,
							atomic.LoadInt64(&sent), atomic.LoadInt64(&recvd), time.Since(start))
					}
				case <-ctx.Done():
					// The signature of the bug: progress stops partway
					// through with neither side able to continue.
					t.Fatalf("%s STALLED: sent=%d recvd=%d of %d after %v",
						sz.label, atomic.LoadInt64(&sent), atomic.LoadInt64(&recvd), rounds, time.Since(start))
				}
			}
			t.Logf("%s OK: %d rounds in %v", sz.label, rounds, time.Since(start))
		})
	}
}

// TestShmOversizedUnary is the non-pipelined control for the above. Unary
// calls at these sizes always worked, including before the fix, because a
// single message in flight means a single loan settled by a single read.
// It is kept so that a future change that breaks large messages outright is
// distinguishable from one that breaks only the pipelined case.
func TestShmOversizedUnary(t *testing.T) {
	sizes := []struct {
		n     int
		label string
	}{
		{31 << 20, "31MiB"},
		{32 << 20, "32MiB"},
		{63 << 20, "63MiB"},
	}

	for _, sz := range crossArms(sizes) {
		t.Run(sz.label, func(t *testing.T) {
			env := sz.newEnv(t)
			defer env.close()

			// Warm up with a tiny RPC so connection setup is not
			// attributed to the large call.
			wctx, wcancel := context.WithTimeout(context.Background(), 15*time.Second)
			_, err := env.client.UnaryCall(wctx, &testpb.SimpleRequest{
				ResponseType: testpb.PayloadType_COMPRESSABLE,
				ResponseSize: 1,
				Payload:      benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, 1),
			})
			wcancel()
			if err != nil {
				t.Fatalf("warmup UnaryCall: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			start := time.Now()
			resp, err := env.client.UnaryCall(ctx, &testpb.SimpleRequest{
				ResponseType: testpb.PayloadType_COMPRESSABLE,
				ResponseSize: int32(sz.n),
				Payload:      benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, sz.n),
			})
			el := time.Since(start)
			if err != nil {
				t.Fatalf("UnaryCall(%s) after %v: %v", sz.label, el, err)
			}
			if got := len(resp.GetPayload().GetBody()); got != sz.n {
				t.Fatalf("UnaryCall(%s): got %d bytes, want %d", sz.label, got, sz.n)
			}
			t.Logf("%s OK in %v", sz.label, el)
		})
	}
}
