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

// Full gRPC stack benchmarks comparing SHM, TCP, and Unix socket transports.
//
// Unlike the raw transport-level micro-benchmarks in internal/transport/shm_bench_test.go,
// these benchmarks exercise the complete gRPC path:
//
//   client.UnaryCall / stream.Send+Recv
//     → protobuf serialization
//       → gRPC framing
//         → transport (SHM ring buffer / TCP / Unix socket)
//           → gRPC de-framing
//             → protobuf deserialization
//               → server handler
//                 → (reverse path for response)
//
// This makes them directly comparable to the .NET gRPC shared-memory benchmarks.

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/benchmark"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/internal/transport"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	testpb "google.golang.org/grpc/interop/grpc_testing"
)

const (
	// benchMaxMsg is the maximum gRPC message size allowed for large payload tests.
	benchMaxMsg = 512 * 1024 * 1024 // 512 MiB
	// benchRing is the SHM ring buffer size.
	benchRing = 64 * 1024 * 1024 // 64 MiB
	// benchSeg is the SHM segment size (2× ring).
	benchSeg = 2 * benchRing
)

// grpcBenchEnv holds a gRPC server and client pair for benchmarking.
type grpcBenchEnv struct {
	stopSrv  func()
	conn     *grpc.ClientConn
	client   testgrpc.BenchmarkServiceClient
	cleanups []func()
}

func (e *grpcBenchEnv) close() {
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

// newShmEnv creates a full gRPC server+client over shared memory transport.
func newShmEnv(b *testing.B) *grpcBenchEnv {
	name := fmt.Sprintf("bench_grpc_shm_%d", time.Now().UnixNano())
	lis, err := transport.NewShmListener(
		&transport.ShmAddr{Name: name},
		uint64(benchSeg), uint64(benchRing), uint64(benchRing),
	)
	if err != nil {
		b.Fatalf("NewShmListener: %v", err)
	}

	srvOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(benchMaxMsg),
		grpc.MaxSendMsgSize(benchMaxMsg),
	}
	stop := benchmark.StartServer(benchmark.ServerInfo{Type: "protobuf", Listener: lis}, srvOpts...)

	conn, err := grpc.NewClient("shm://"+name,
		grpc.WithShmTransport(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(benchMaxMsg),
			grpc.MaxCallSendMsgSize(benchMaxMsg),
		),
	)
	if err != nil {
		stop()
		lis.Close()
		transport.RemoveSegment(name)
		b.Fatalf("NewClient: %v", err)
	}

	client := testgrpc.NewBenchmarkServiceClient(conn)
	warmUpGRPC(b, client)

	return &grpcBenchEnv{
		stopSrv: stop,
		conn:    conn,
		client:  client,
		cleanups: []func(){
			func() { lis.Close() },
			func() { transport.RemoveSegment(name) },
		},
	}
}

// newTCPEnv creates a full gRPC server+client over TCP loopback.
func newTCPEnv(b *testing.B) *grpcBenchEnv {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("Listen: %v", err)
	}

	srvOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(benchMaxMsg),
		grpc.MaxSendMsgSize(benchMaxMsg),
	}
	stop := benchmark.StartServer(benchmark.ServerInfo{Type: "protobuf", Listener: lis}, srvOpts...)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(benchMaxMsg),
			grpc.MaxCallSendMsgSize(benchMaxMsg),
		),
	)
	if err != nil {
		stop()
		lis.Close()
		b.Fatalf("NewClient: %v", err)
	}

	client := testgrpc.NewBenchmarkServiceClient(conn)
	warmUpGRPC(b, client)

	return &grpcBenchEnv{
		stopSrv:  stop,
		conn:     conn,
		client:   client,
		cleanups: []func(){func() { lis.Close() }},
	}
}

// newUnixEnv creates a full gRPC server+client over a Unix domain socket.
func newUnixEnv(b *testing.B) *grpcBenchEnv {
	sockPath := filepath.Join(os.TempDir(), fmt.Sprintf("bench_grpc_%d.sock", time.Now().UnixNano()))
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		if runtime.GOOS == "windows" {
			b.Skipf("unix domain sockets unavailable: %v", err)
		}
		b.Fatalf("Listen unix: %v", err)
	}

	srvOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(benchMaxMsg),
		grpc.MaxSendMsgSize(benchMaxMsg),
	}
	stop := benchmark.StartServer(benchmark.ServerInfo{Type: "protobuf", Listener: lis}, srvOpts...)

	conn, err := grpc.NewClient("unix:"+sockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(benchMaxMsg),
			grpc.MaxCallSendMsgSize(benchMaxMsg),
		),
	)
	if err != nil {
		stop()
		lis.Close()
		os.Remove(sockPath)
		b.Fatalf("NewClient: %v", err)
	}

	client := testgrpc.NewBenchmarkServiceClient(conn)
	warmUpGRPC(b, client)

	return &grpcBenchEnv{
		stopSrv: stop,
		conn:    conn,
		client:  client,
		cleanups: []func(){
			func() { lis.Close() },
			func() { os.Remove(sockPath) },
		},
	}
}

// warmUpGRPC makes one unary call to ensure the gRPC connection is fully established
// before benchmark timing begins.
func warmUpGRPC(b *testing.B, client testgrpc.BenchmarkServiceClient) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := &testpb.SimpleRequest{
		ResponseType: testpb.PayloadType_COMPRESSABLE,
		ResponseSize: 0,
		Payload:      benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, 0),
	}
	if _, err := client.UnaryCall(ctx, req); err != nil {
		b.Fatalf("warm-up UnaryCall: %v", err)
	}
}

// warmUpStream sends several large streaming ping-pongs to fault in ring buffer
// pages and warm CPU caches before the timed benchmark sub-tests. Without this,
// the first sub-test (especially at large payload sizes) pays page-fault and TLB
// miss costs that distort results vs later sub-tests on the same connection.
func warmUpStream(b *testing.B, client testgrpc.BenchmarkServiceClient, size int) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := client.StreamingCall(ctx)
	if err != nil {
		b.Fatalf("warm-up StreamingCall: %v", err)
	}

	req := &testpb.SimpleRequest{
		ResponseType: testpb.PayloadType_COMPRESSABLE,
		ResponseSize: int32(size),
		Payload:      benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, size),
	}

	// Send enough data to touch all ring buffer pages (~128 MiB total for a
	// 64 MiB ring with both directions). 200 round-trips of the target size
	// ensures full coverage even for small payloads.
	n := 200
	if size > 0 {
		// At least enough to fill the ring twice in each direction.
		minIters := (2 * benchRing) / size
		if minIters > n {
			n = minIters
		}
	}
	for i := 0; i < n; i++ {
		if err := stream.Send(req); err != nil {
			b.Fatalf("warm-up Send: %v", err)
		}
		if _, err := stream.Recv(); err != nil {
			b.Fatalf("warm-up Recv: %v", err)
		}
	}
	_ = stream.CloseSend()
}

// benchStream opens a StreamingCall and performs ping-pong Send/Recv for b.N iterations.
// Each operation sends a request of `size` bytes and receives a response of `size` bytes.
func benchStream(b *testing.B, client testgrpc.BenchmarkServiceClient, size int) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.StreamingCall(ctx)
	if err != nil {
		b.Fatalf("StreamingCall: %v", err)
	}

	req := &testpb.SimpleRequest{
		ResponseType: testpb.PayloadType_COMPRESSABLE,
		ResponseSize: int32(size),
		Payload:      benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, size),
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
}

// benchUnary performs individual UnaryCall RPCs for b.N iterations.
// Each call sends a request of `size` bytes and receives a response of `size` bytes.
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
}

// Standard payload sizes (64 B to 1 MiB).
var benchStreamSizes = []int{64, 256, 1024, 4096, 16384, 65536, 262144, 1048576}

// Unary payload sizes (64 B to 4 KiB) — small payloads where per-call overhead dominates.
var benchUnarySizes = []int{64, 256, 1024, 4096}

// Large payload sizes (1 MiB to 16 MiB).
// NOTE: Payloads approaching the ring buffer size (64 MiB) may corrupt data
// during gRPC frame reassembly over SHM. Cap at 16 MiB for reliable results.
var benchLargeSizes = []struct {
	bytes int
	label string
}{
	{1 * 1024 * 1024, "1MB"},
	{4 * 1024 * 1024, "4MB"},
	{16 * 1024 * 1024, "16MB"},
}

// =============================================================================
// SHM Transport — Full gRPC Stack
// =============================================================================

// BenchmarkGRPCShmStream measures streaming ping-pong through the full gRPC stack
// over the shared memory transport.
func BenchmarkGRPCShmStream(b *testing.B) {
	env := newShmEnv(b)
	defer env.close()
	for _, size := range benchStreamSizes {
		size := size
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			benchStream(b, env.client, size)
		})
	}
}

// BenchmarkGRPCShmUnary measures unary RPC latency through the full gRPC stack
// over the shared memory transport.
func BenchmarkGRPCShmUnary(b *testing.B) {
	env := newShmEnv(b)
	defer env.close()
	for _, size := range benchUnarySizes {
		size := size
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			benchUnary(b, env.client, size)
		})
	}
}

// BenchmarkGRPCShmLargeStream measures streaming with large payloads (1–256 MiB)
// through the full gRPC stack over shared memory.
func BenchmarkGRPCShmLargeStream(b *testing.B) {
	env := newShmEnv(b)
	defer env.close()
	warmUpStream(b, env.client, 1*1024*1024)
	for _, ls := range benchLargeSizes {
		ls := ls
		b.Run(fmt.Sprintf("size=%dMB", ls.bytes/(1024*1024)), func(b *testing.B) {
			benchStream(b, env.client, ls.bytes)
		})
	}
}

// BenchmarkGRPCShmLargeUnary measures unary RPC with large payloads (1–256 MiB)
// through the full gRPC stack over shared memory.
func BenchmarkGRPCShmLargeUnary(b *testing.B) {
	env := newShmEnv(b)
	defer env.close()
	warmUpStream(b, env.client, 1*1024*1024)
	for _, ls := range benchLargeSizes {
		ls := ls
		b.Run(fmt.Sprintf("size=%dMB", ls.bytes/(1024*1024)), func(b *testing.B) {
			benchUnary(b, env.client, ls.bytes)
		})
	}
}

// =============================================================================
// TCP Transport — Full gRPC Stack
// =============================================================================

// BenchmarkGRPCTCPStream measures streaming ping-pong through the full gRPC stack
// over TCP loopback.
func BenchmarkGRPCTCPStream(b *testing.B) {
	env := newTCPEnv(b)
	defer env.close()
	for _, size := range benchStreamSizes {
		size := size
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			benchStream(b, env.client, size)
		})
	}
}

// BenchmarkGRPCTCPUnary measures unary RPC latency through the full gRPC stack
// over TCP loopback.
func BenchmarkGRPCTCPUnary(b *testing.B) {
	env := newTCPEnv(b)
	defer env.close()
	for _, size := range benchUnarySizes {
		size := size
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			benchUnary(b, env.client, size)
		})
	}
}

// BenchmarkGRPCTCPLargeStream measures streaming with large payloads (1–256 MiB)
// through the full gRPC stack over TCP loopback.
func BenchmarkGRPCTCPLargeStream(b *testing.B) {
	env := newTCPEnv(b)
	defer env.close()
	warmUpStream(b, env.client, 1*1024*1024)
	for _, ls := range benchLargeSizes {
		ls := ls
		b.Run(fmt.Sprintf("size=%dMB", ls.bytes/(1024*1024)), func(b *testing.B) {
			benchStream(b, env.client, ls.bytes)
		})
	}
}

// BenchmarkGRPCTCPLargeUnary measures unary RPC with large payloads (1–256 MiB)
// through the full gRPC stack over TCP loopback.
func BenchmarkGRPCTCPLargeUnary(b *testing.B) {
	env := newTCPEnv(b)
	defer env.close()
	warmUpStream(b, env.client, 1*1024*1024)
	for _, ls := range benchLargeSizes {
		ls := ls
		b.Run(fmt.Sprintf("size=%dMB", ls.bytes/(1024*1024)), func(b *testing.B) {
			benchUnary(b, env.client, ls.bytes)
		})
	}
}

// =============================================================================
// Unix Socket Transport — Full gRPC Stack
// =============================================================================

// BenchmarkGRPCUnixStream measures streaming ping-pong through the full gRPC stack
// over a Unix domain socket.
func BenchmarkGRPCUnixStream(b *testing.B) {
	env := newUnixEnv(b)
	defer env.close()
	for _, size := range benchStreamSizes {
		size := size
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			benchStream(b, env.client, size)
		})
	}
}

// BenchmarkGRPCUnixUnary measures unary RPC latency through the full gRPC stack
// over a Unix domain socket.
func BenchmarkGRPCUnixUnary(b *testing.B) {
	env := newUnixEnv(b)
	defer env.close()
	for _, size := range benchUnarySizes {
		size := size
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			benchUnary(b, env.client, size)
		})
	}
}

// BenchmarkGRPCUnixLargeStream measures streaming with large payloads (1–256 MiB)
// through the full gRPC stack over a Unix domain socket.
func BenchmarkGRPCUnixLargeStream(b *testing.B) {
	env := newUnixEnv(b)
	defer env.close()
	for _, ls := range benchLargeSizes {
		ls := ls
		b.Run(fmt.Sprintf("size=%dMB", ls.bytes/(1024*1024)), func(b *testing.B) {
			benchStream(b, env.client, ls.bytes)
		})
	}
}

// BenchmarkGRPCUnixLargeUnary measures unary RPC with large payloads (1–256 MiB)
// through the full gRPC stack over a Unix domain socket.
func BenchmarkGRPCUnixLargeUnary(b *testing.B) {
	env := newUnixEnv(b)
	defer env.close()
	for _, ls := range benchLargeSizes {
		ls := ls
		b.Run(fmt.Sprintf("size=%dMB", ls.bytes/(1024*1024)), func(b *testing.B) {
			benchUnary(b, env.client, ls.bytes)
		})
	}
}
