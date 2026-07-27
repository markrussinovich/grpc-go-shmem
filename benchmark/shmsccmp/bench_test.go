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

// Package shmsccmp benchmarks the in-tree ("monolithic") shared-memory
// transport against the self-contained plugin transport, through the full
// gRPC stack, with stock HTTP/2 over a unix domain socket ("UDS") as the
// baseline both are trying to beat.
//
// All three arms are configured identically:
//
//   - same segment / ring geometry for the two SHM arms (136 MiB segment,
//     64 MiB rings, which is the default for both implementations),
//   - same server and dial options (no transport-specific buffer-pool or
//     flow-control tuning on any side; UDS runs stock HTTP/2 with gRPC's
//     default BDP-estimated flow-control windows),
//   - same payload sweep, same ping-pong / unary loops.
//
// Run with:
//
//	go test -run '^$' -bench . -benchtime=500ms -count=1 ./...
package shmsccmp

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/benchmark"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/experimental/shm"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	shmsc "google.golang.org/grpc/plugin/shmsc"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
)

// benchMaxMsg is the maximum gRPC message size allowed, sized above the
// largest payload in the sweep.
const benchMaxMsg = 64 * 1024 * 1024

// benchPayloadSizes is the payload sweep shared by both transports.
var benchPayloadSizes = []struct {
	bytes int
	label string
}{
	{64, "64"},
	{1024, "1024"},
	{16 << 10, "16384"},
	{64 << 10, "65536"},
	{256 << 10, "262144"},
	{1 << 20, "1MB"},
}

type benchEnv struct {
	stopSrv  func()
	conn     *grpc.ClientConn
	client   testgrpc.BenchmarkServiceClient
	cleanups []func()
}

func (e *benchEnv) close() {
	if e.conn != nil {
		e.conn.Close()
	}
	if e.stopSrv != nil {
		e.stopSrv()
	}
	for i := len(e.cleanups) - 1; i >= 0; i-- {
		e.cleanups[i]()
	}
}

// serverOpts and dialOpts are deliberately transport-agnostic: the point of
// this comparison is the transport implementation, so neither side gets
// tuning the other cannot express.
func serverOpts() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.MaxRecvMsgSize(benchMaxMsg),
		grpc.MaxSendMsgSize(benchMaxMsg),
	}
}

func dialOpts() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(benchMaxMsg),
			grpc.MaxCallSendMsgSize(benchMaxMsg),
		),
	}
}

// removeSegment best-effort unlinks the backing segment file so repeated
// bench runs do not accumulate 136 MiB files in /dev/shm.
func removeSegment(name string) {
	os.Remove("/dev/shm/grpc_shm_" + name)
}

// newMonoEnv builds a server+client pair over the in-tree SHM transport,
// selected through the shm dial option and the shm:// target.
func newMonoEnv(b *testing.B) *benchEnv {
	name := fmt.Sprintf("bench_mono_%d", time.Now().UnixNano())
	lis, err := shm.NewListener(name, nil)
	if err != nil {
		b.Fatalf("shm.NewListener: %v", err)
	}
	stop := benchmark.StartServer(benchmark.ServerInfo{Type: "protobuf", Listener: lis}, serverOpts()...)

	conn, err := grpc.NewClient("shm://"+name, append(dialOpts(), shm.WithTransport())...)
	if err != nil {
		stop()
		lis.Close()
		removeSegment(name)
		b.Fatalf("grpc.NewClient: %v", err)
	}

	client := testgrpc.NewBenchmarkServiceClient(conn)
	warmUp(b, client)

	return &benchEnv{
		stopSrv: stop,
		conn:    conn,
		client:  client,
		cleanups: []func(){
			func() { lis.Close() },
			func() { removeSegment(name) },
		},
	}
}

// newPluginEnv builds a server+client pair over the self-contained plugin
// transport, selected through resolver.Address.TransportType on the client
// and the tagged listener on the server.
func newPluginEnv(b *testing.B) *benchEnv {
	name := fmt.Sprintf("bench_plugin_%d", time.Now().UnixNano())
	lis, err := shmsc.Listen(name)
	if err != nil {
		b.Fatalf("shmsc.Listen: %v", err)
	}
	stop := benchmark.StartServer(benchmark.ServerInfo{Type: "protobuf", Listener: lis}, serverOpts()...)

	r := manual.NewBuilderWithScheme("shmsccmp")
	r.InitialState(resolver.State{
		Addresses: []resolver.Address{{Addr: name, TransportType: shmsc.Name}},
	})

	conn, err := grpc.NewClient("shmsccmp:///"+name, append(dialOpts(), grpc.WithResolvers(r))...)
	if err != nil {
		stop()
		lis.Close()
		removeSegment(name)
		b.Fatalf("grpc.NewClient: %v", err)
	}

	client := testgrpc.NewBenchmarkServiceClient(conn)
	warmUp(b, client)

	return &benchEnv{
		stopSrv: stop,
		conn:    conn,
		client:  client,
		cleanups: []func(){
			func() { lis.Close() },
			func() { removeSegment(name) },
		},
	}
}

// newUDSEnv builds a server+client pair over stock HTTP/2 carried on a unix
// domain socket. This is the baseline: it is the fastest transport a gRPC
// user gets today without any shared-memory work, so it is what the two SHM
// implementations have to be measured against.
func newUDSEnv(b *testing.B) *benchEnv {
	// The socket lives under TempDir rather than /dev/shm so it is a plain
	// AF_UNIX rendezvous point and nothing about it is shared-memory backed.
	path := filepath.Join(b.TempDir(), "bench.sock")
	lis, err := net.Listen("unix", path)
	if err != nil {
		b.Fatalf("net.Listen(unix): %v", err)
	}
	stop := benchmark.StartServer(benchmark.ServerInfo{Type: "protobuf", Listener: lis}, serverOpts()...)

	conn, err := grpc.NewClient("unix://"+path, dialOpts()...)
	if err != nil {
		stop()
		lis.Close()
		b.Fatalf("grpc.NewClient: %v", err)
	}

	client := testgrpc.NewBenchmarkServiceClient(conn)
	warmUp(b, client)

	return &benchEnv{
		stopSrv: stop,
		conn:    conn,
		client:  client,
		cleanups: []func(){
			func() { lis.Close() },
		},
	}
}

// warmUp forces connection establishment and the first handler dispatch so
// neither is charged to the first measured iteration.
func warmUp(b *testing.B, client testgrpc.BenchmarkServiceClient) {
	b.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req := &testpb.SimpleRequest{
		ResponseType: testpb.PayloadType_COMPRESSABLE,
		ResponseSize: 1,
		Payload:      benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, 1),
	}
	if _, err := client.UnaryCall(ctx, req); err != nil {
		b.Fatalf("warm-up UnaryCall: %v", err)
	}
}

// benchStream measures streaming ping-pong on a single established stream.
func benchStream(b *testing.B, client testgrpc.BenchmarkServiceClient, size int) {
	req := &testpb.SimpleRequest{
		ResponseType: testpb.PayloadType_COMPRESSABLE,
		ResponseSize: int32(size),
		Payload:      benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, size),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.StreamingCall(ctx)
	if err != nil {
		b.Fatalf("StreamingCall: %v", err)
	}
	if err := stream.Send(req); err != nil {
		b.Fatalf("warm-up Send: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		b.Fatalf("warm-up Recv: %v", err)
	}

	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := stream.Send(req); err != nil {
			b.Fatalf("Send: %v", err)
		}
		if _, err := stream.Recv(); err != nil {
			b.Fatalf("Recv: %v", err)
		}
	}
	b.StopTimer()

	_ = stream.CloseSend()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
}

// benchUnary measures individual unary RPC latency.
func benchUnary(b *testing.B, client testgrpc.BenchmarkServiceClient, size int) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := &testpb.SimpleRequest{
		ResponseType: testpb.PayloadType_COMPRESSABLE,
		ResponseSize: int32(size),
		Payload:      benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, size),
	}

	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := client.UnaryCall(ctx, req); err != nil {
			b.Fatalf("UnaryCall: %v", err)
		}
	}
	b.StopTimer()
}

func sweep(b *testing.B, newEnv func(*testing.B) *benchEnv, run func(*testing.B, testgrpc.BenchmarkServiceClient, int)) {
	env := newEnv(b)
	defer env.close()
	for _, p := range benchPayloadSizes {
		b.Run(fmt.Sprintf("size=%s", p.label), func(b *testing.B) {
			run(b, env.client, p.bytes)
		})
	}
}

func BenchmarkMonoUnary(b *testing.B)   { sweep(b, newMonoEnv, benchUnary) }
func BenchmarkPluginUnary(b *testing.B) { sweep(b, newPluginEnv, benchUnary) }
func BenchmarkUDSUnary(b *testing.B)    { sweep(b, newUDSEnv, benchUnary) }
func BenchmarkMonoStream(b *testing.B)  { sweep(b, newMonoEnv, benchStream) }
func BenchmarkPluginStream(b *testing.B) {
	sweep(b, newPluginEnv, benchStream)
}
func BenchmarkUDSStream(b *testing.B) { sweep(b, newUDSEnv, benchStream) }
