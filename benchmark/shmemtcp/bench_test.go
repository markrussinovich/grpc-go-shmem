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
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/benchmark"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/experimental"
	"google.golang.org/grpc/experimental/shm"
	imem "google.golang.org/grpc/internal/mem"
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

// TestMain wraps `go test` so we can defensively sweep stale SHM
// segment files from prior crashed / killed bench runs. On Linux the
// segments live under /dev/shm and stale entries are usually harmless,
// but on Windows the segments are backed by regular files in TEMP and
// accumulate to tens of GB if benches are killed (e.g. timeout, ^C,
// IDE process kill). A large pool of stale files also makes Defender
// scans dominate filesystem syscalls and can stall fresh segment
// creates during multi-iteration bench runs.
//
// This sweeper is conservative: it only deletes files matching
// `grpc_shm_*` in /dev/shm and the OS temp dir, leaves anything else
// alone, and runs once at startup and once on exit.
func TestMain(m *testing.M) {
	// BENCH_DIRTY_DEFAULT_POOL=1 swaps grpc-go's process-wide default
	// buffer pool to a dirty (no-memclr-on-Get) variant via the
	// already-public experimental.SetDefaultBufferPool API. This is
	// NOT a SHM-specific optimisation -- it changes the pool used by
	// codec.Marshal / codec.Unmarshal / MaterializeToBuffer for ALL
	// transports in this test binary (SHM, TCP, UDS, in-proc).
	//
	// Why the toggle exists: pprof of SHM bench showed memclr
	// dominating CPU. Root cause is grpc-go's stock
	// BinaryTieredBufferPool.sizedBufferPool with shouldZero=true:
	// it clears the ENTIRE tier capacity on every Get, so a 64 KiB
	// request from the 1 MiB tier memclrs 1 MiB. At 1000 concurrent
	// streams × 64 KiB this becomes ~1 GiB of memclr per round per
	// direction (~46% of samples). The zeroing is pure waste because
	// every grpc caller (proto.MarshalAppend, mem.Copy,
	// MaterializeToBuffer, decompress) overwrites the returned buffer
	// before any reader observes it. shouldZero=true is grpc-go's
	// defensive default for multi-tenant deployments where a bug
	// could leak stale bytes across trust boundaries; single-tenant
	// applications can safely opt out.
	//
	// Why default off: the swap is process-wide so it speeds up TCP
	// and UDS too. Leaving it off keeps the reference numbers grounded
	// in stock grpc-go behaviour every user pays. Flip the env to 1
	// to A/B compare and observe that the speedup is cross-transport,
	// not SHM-only.
	if os.Getenv("BENCH_DIRTY_DEFAULT_POOL") == "1" {
		// Mirror grpc-go's default pool tier list (256 B, 4 KiB,
		// 16 KiB, 32 KiB, 1 MiB) but on the dirty constructor so
		// per-Get clear() is skipped.
		dirty, err := imem.NewDirtyBinaryTieredBufferPool(8, 12, 14, 15, 20)
		if err != nil {
			panic(fmt.Sprintf("BENCH_DIRTY_DEFAULT_POOL init failed: %v", err))
		}
		experimental.SetDefaultBufferPool(dirty)
	}

	// The bench harness measures the production-equivalent SHM path:
	// no-WU is on by default; the per-data-segment eventfd waker is
	// OFF by default for cross-process safety (see internal/transport
	// shmDataSegWakerEnabledAtomic doc). Same-process benchmarks
	// running here reproduce the v3.4 baseline numbers, which were
	// captured with eventfd ON, by enabling it explicitly. Use
	// ConfigureShmEventfdWakerForBench(false) in an individual bench
	// to compare against the futex fallback.
	transport.ConfigureShmEventfdWakerForBench(true)

	sweepStaleShmSegments()
	code := m.Run()
	sweepStaleShmSegments()
	os.Exit(code)
}

// sweepStaleShmSegments removes leftover grpc_shm_* files from prior
// bench runs. Errors are ignored: the cleanup is best-effort.
//
// Safety: files modified within the last sweepStaleAge are considered
// "live" and skipped. Without this guard, two bench binaries running
// concurrently (e.g. the same package from two terminals, or another
// shm test package in a parallel CI shard) would race: binary B's
// TestMain on startup could unlink the freshly-created segment of
// binary A between A's CreateSegment and the peer's OpenSegment,
// breaking A. Already-mapped segments survive unlink on Linux but the
// missing inode causes lookups in tests that re-open by name to fail,
// and on Windows the unlink may itself fail.
func sweepStaleShmSegments() {
	const sweepStaleAge = 5 * time.Minute
	cutoff := time.Now().Add(-sweepStaleAge)
	dirs := []string{"/dev/shm", os.TempDir()}
	seen := map[string]bool{}
	for _, dir := range dirs {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		matches, err := filepath.Glob(filepath.Join(dir, "grpc_shm_*"))
		if err != nil {
			continue
		}
		for _, p := range matches {
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			if info.ModTime().After(cutoff) {
				continue
			}
			_ = os.Remove(p)
		}
	}
}

// logBenchEnvOnce prints the env vars and resolved settings that
// determine the bench harness's transport configuration. Called from
// the first newShmEnv / newTCPEnv / newUnixEnv invocation so the
// chosen profile, wake mode, spin state, and HTTP/2 windows are
// visible at the top of the bench output. Reviewers (Doug, Mark)
// can verify spin=0 and HTTP-settings parity from the log without
// reading the harness source.
var logBenchEnvOnceOnce sync.Once

func logBenchEnvOnce(b *testing.B) {
	logBenchEnvOnceOnce.Do(func() {
		prof := loadBenchProfile()
		spin := os.Getenv("SHM_SPIN_ITERS")
		if spin == "" {
			spin = "0 (default, no spin)"
		}
		dirtyPool := os.Getenv("BENCH_DIRTY_DEFAULT_POOL")
		if dirtyPool == "" {
			dirtyPool = "0 (off, grpc-go stock pool: shouldZero=true clears tier on every Get -- applies to ALL transports)"
		} else if dirtyPool == "1" {
			dirtyPool = "1 (grpc-go process-wide default pool swapped to dirty -- speeds up ALL transports, not SHM only)"
		}
		bProf := os.Getenv("BENCH_PROFILE")
		if bProf == "" {
			bProf = "shm-tuned (SHM keeps 2 GiB quota, TCP/UDS HTTP/2 defaults)"
		}
		// Note: no-WU flow control and eventfd waker are now ON by
		// default (v3.4 baseline). Tests that want to compare against
		// the futex / HTTP/2-WU path can call
		// transport.ConfigureShmNoWindowUpdate(false) or
		// transport.ConfigureShmEventfdWakerForBench(false).
		b.Logf("SHM bench env: BENCH_PROFILE=%s BENCH_DIRTY_DEFAULT_POOL=%s SHM_SPIN_ITERS=%s initialWindowSize=%d maxFrameSize=%d applyToShm=%v",
			bProf, dirtyPool, spin,
			prof.initialWindowSize, prof.maxFrameSize, prof.applyToShm,
		)
	})
}

// benchProfile controls HTTP/2 flow-control window sizes applied
// uniformly across SHM, TCP, and UDS so reviewers can compare them
// under matching settings. The original asymmetry that prompted
// adding this knob:
//
//   - SHM transport used `shmInitialWindowSize = 32 MiB` BDP target
//     internally; producer / receive limits were pinned at 2 GiB i.e.
//     flow control effectively disabled.
//   - TCP / UDS use HTTP/2's spec default 65535-byte initial window.
//
// So large-message streaming numbers on SHM were partly an artifact
// of unlimited flow control, not just transport efficiency. Set
// BENCH_PROFILE to one of:
//
//   - shm-tuned    : (default) SHM keeps its production 2 GiB quota;
//     TCP / UDS use HTTP/2 default 65535. Matches the
//     historical numbers in the repo. Shows SHM's
//     upper bound when tuned for local IPC.
//
//   - fair-default : All three transports use HTTP/2 default 65535 B.
//     Doug's preferred comparison. Tests SHM purely on
//     transport mechanics, no flow-control advantage.
//
//   - fair-32mb    : All three transports use 32 MiB windows. Tests
//     SHM against TCP / UDS when operators tune
//     grpc.WithInitialWindowSize for streaming.
type benchProfile struct {
	// initialWindowSize, when > 0, is passed as
	// grpc.WithInitialWindowSize on the client and
	// grpc.InitialWindowSize on the server. The SHM transport reads
	// it from ConnectOptions and applies it to per-stream send /
	// receive quota; TCP / UDS pass it directly to the HTTP/2
	// transport.
	initialWindowSize int32

	// initialConnWindowSize: same, for the connection-level window.
	initialConnWindowSize int32

	// maxFrameSize, when > 0, caps the body of each H2 DATA frame
	// the SHM producer emits. HTTP/2 over TCP / UDS in grpc-go uses
	// the spec default 16384 (= http2MaxFrameLen). Matching it on
	// SHM ensures all three transports emit the same DATA-frame
	// cadence under fair profiles. The receiver always accepts up
	// to the RFC ceiling regardless.
	maxFrameSize int

	// applyToShm is false when the profile wants SHM to stay on its
	// native 2 GiB quota even when overriding TCP / UDS (i.e. the
	// "shm-tuned" profile). True for fair-* profiles.
	applyToShm bool
}

func loadBenchProfile() benchProfile {
	switch os.Getenv("BENCH_PROFILE") {
	case "fair-default":
		return benchProfile{
			initialWindowSize:     65535,
			initialConnWindowSize: 65535,
			maxFrameSize:          16384,
			applyToShm:            true,
		}
	case "fair-32mb":
		return benchProfile{
			initialWindowSize:     32 * 1024 * 1024,
			initialConnWindowSize: 32 * 1024 * 1024,
			// 32 MiB profile leaves frame size at the SHM default
			// (h2MaxFramePayload) so DATA frames are large enough to
			// amortise per-frame overhead. The reviewer-requested
			// strict fairness is covered by fair-default.
			applyToShm: true,
		}
	case "", "shm-tuned":
		return benchProfile{}
	default:
		panic(fmt.Sprintf("BENCH_PROFILE %q not recognised; use shm-tuned | fair-default | fair-32mb",
			os.Getenv("BENCH_PROFILE")))
	}
}

func (p benchProfile) dialOpts(transport string) []grpc.DialOption {
	apply := true
	if transport == "shm" && !p.applyToShm {
		apply = false
	}
	if !apply || p.initialWindowSize == 0 {
		return nil
	}
	return []grpc.DialOption{
		grpc.WithInitialWindowSize(p.initialWindowSize),
		grpc.WithInitialConnWindowSize(p.initialConnWindowSize),
	}
}

func (p benchProfile) serverOpts(transport string) []grpc.ServerOption {
	apply := true
	if transport == "shm" && !p.applyToShm {
		apply = false
	}
	if !apply || p.initialWindowSize == 0 {
		return nil
	}
	return []grpc.ServerOption{
		grpc.InitialWindowSize(p.initialWindowSize),
		grpc.InitialConnWindowSize(p.initialConnWindowSize),
	}
}

// newShmEnv creates a full gRPC server+client over shared memory transport.
func newShmEnv(b *testing.B) *grpcBenchEnv {
	profile := loadBenchProfile()
	logBenchEnvOnce(b)
	// Apply the bench profile's window size to the SHM-specific
	// flow-control knobs (shmInitialWindowSize, shmWindowUpdateThreshold)
	// BEFORE constructing any transport. The dial-option plumbing
	// (grpc.WithInitialWindowSize → ConnectOptions.InitialWindowSize →
	// DialOptions.InitialWindowSize → conn/stream send quota) handles
	// the QUOTA side; this call separately handles the WindowUpdate
	// BATCHING THRESHOLD side which is also package-global. Without
	// it the threshold stays at the default 8 MiB so a small-window
	// stream takes thousands of iterations before its first
	// WindowUpdate fires, which deadlocks the producer once the
	// window drains. ResetShmFlowControlForBench is registered in
	// cleanups below so subsequent tests don't inherit the override.
	if profile.applyToShm && profile.initialWindowSize > 0 {
		transport.ConfigureShmFlowControlForBench(int(profile.initialWindowSize))
	}
	if profile.applyToShm && profile.maxFrameSize > 0 {
		transport.ConfigureShmMaxFrameSizeForBench(profile.maxFrameSize)
	}
	// SHM_MAX_FRAME_SIZE env var overrides whatever BENCH_PROFILE set
	// (or the default). Lets a reviewer isolate the H2 DATA frame
	// cadence variable from BENCH_PROFILE's other levers
	// (initialWindowSize, applyToShm) when investigating per-frame
	// chunking effects, e.g. why shm-tuned + 1000 streams x 64 KiB
	// underperforms fair-default at the same concurrency.
	if v := os.Getenv("SHM_MAX_FRAME_SIZE"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n <= 0 {
			b.Fatalf("SHM_MAX_FRAME_SIZE=%q invalid: %v", v, perr)
		}
		transport.ConfigureShmMaxFrameSizeForBench(n)
	}
	// SHM_SPIN_ITERS lets reviewers compare SHM under "no spin" (the
	// default — matches UDS behaviour by paying a futex syscall per
	// wake) vs operator-tuned "spin opted-in" (skips both sides'
	// syscalls when the spin window catches the data). 0 disables
	// spin. Typical opt-in values: 500–2000 on Linux. See
	// transport.ConfigureShmSpinIterations GoDoc for guidance.
	if v := os.Getenv("SHM_SPIN_ITERS"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 0 {
			b.Fatalf("SHM_SPIN_ITERS=%q invalid: %v", v, perr)
		}
		transport.ConfigureShmSpinIterations(n)
	}
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
	srvOpts = append(srvOpts, profile.serverOpts("shm")...)
	stop := benchmark.StartServer(benchmark.ServerInfo{Type: "protobuf", Listener: lis}, srvOpts...)

	dialOpts := []grpc.DialOption{
		shm.WithTransport(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(benchMaxMsg),
			grpc.MaxCallSendMsgSize(benchMaxMsg),
		),
	}
	dialOpts = append(dialOpts, profile.dialOpts("shm")...)
	conn, err := grpc.NewClient("shm://"+name, dialOpts...)
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
			// Restore SHM flow-control defaults so subsequent bench
			// envs in the same `go test` invocation don't inherit
			// this profile's overrides. No-op if we didn't override.
			transport.ResetShmFlowControlForBench,
			transport.ResetShmSpinIterationsForBench,
		},
	}
}

// newTCPEnv creates a full gRPC server+client over TCP loopback.
func newTCPEnv(b *testing.B) *grpcBenchEnv {
	profile := loadBenchProfile()
	logBenchEnvOnce(b)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("Listen: %v", err)
	}

	srvOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(benchMaxMsg),
		grpc.MaxSendMsgSize(benchMaxMsg),
	}
	srvOpts = append(srvOpts, profile.serverOpts("tcp")...)
	stop := benchmark.StartServer(benchmark.ServerInfo{Type: "protobuf", Listener: lis}, srvOpts...)

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(benchMaxMsg),
			grpc.MaxCallSendMsgSize(benchMaxMsg),
		),
	}
	dialOpts = append(dialOpts, profile.dialOpts("tcp")...)
	conn, err := grpc.NewClient(lis.Addr().String(), dialOpts...)
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
	profile := loadBenchProfile()
	logBenchEnvOnce(b)
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
	srvOpts = append(srvOpts, profile.serverOpts("uds")...)
	stop := benchmark.StartServer(benchmark.ServerInfo{Type: "protobuf", Listener: lis}, srvOpts...)

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(benchMaxMsg),
			grpc.MaxCallSendMsgSize(benchMaxMsg),
		),
	}
	dialOpts = append(dialOpts, profile.dialOpts("uds")...)
	conn, err := grpc.NewClient("unix:"+sockPath, dialOpts...)
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

// benchStream performs streaming ping-pong Send/Recv for b.N iterations on a
// single persistent stream. This measures per-message latency on an already-
// established stream rather than per-stream setup cost. The stream is closed
// at function exit; the server handler returns when it sees io.EOF on its
// receive side.
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

	// Warm up the stream so the first measured iteration doesn't include
	// per-stream setup cost.
	if err := stream.Send(req); err != nil {
		b.Fatalf("warm-up Send: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		b.Fatalf("warm-up Recv: %v", err)
	}

	b.SetBytes(int64(size))
	b.ResetTimer()
	endCPU := startCPUProbe(b)
	endZC := startZCProbe(b)

	for i := 0; i < b.N; i++ {
		if err := stream.Send(req); err != nil {
			b.Fatalf("Send: %v", err)
		}
		if _, err := stream.Recv(); err != nil {
			b.Fatalf("Recv: %v", err)
		}
	}
	b.StopTimer()
	endCPU()
	endZC()

	// Close the stream cleanly so the next benchmark size starts fresh.
	_ = stream.CloseSend()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
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
	endCPU := startCPUProbe(b)
	endZC := startZCProbe(b)

	for i := 0; i < b.N; i++ {
		if _, err := client.UnaryCall(ctx, req); err != nil {
			b.Fatalf("UnaryCall: %v", err)
		}
	}
	b.StopTimer()
	endCPU()
	endZC()
}

// benchPayloadSizes is the unified payload-size table used by all
// non-concurrent bench functions. Stream and Unary share the same
// table so the matrix is symmetric and reviewers do not have to
// cross-reference two name-spaces. The earlier split into
// BenchmarkGRPC*Stream / BenchmarkGRPC*LargeStream (and the Unary
// twin) was a historical artefact of a since-removed Custom16
// MORE-flag chunking path; the underlying transport code is
// identical for every entry below.
//
// Labels keep the previous repo convention so existing bench
// histories continue to match: raw byte counts for < 1 MiB, MB
// suffix for >= 1 MiB.
var benchPayloadSizes = []struct {
	bytes int
	label string
}{
	{64, "64"},
	{256, "256"},
	{1024, "1024"},
	{4096, "4096"},
	{16 << 10, "16384"},
	{64 << 10, "65536"},
	{256 << 10, "262144"},
	{1 << 20, "1MB"},
	{4 << 20, "4MB"},
	{16 << 20, "16MB"},
	{64 << 20, "64MB"},
	{256 << 20, "256MB"},
}

// =============================================================================
// SHM Transport — Full gRPC Stack
// =============================================================================

// BenchmarkGRPCShmStream measures streaming ping-pong through the full gRPC
// stack over the shared memory transport, sweeping every entry of
// benchPayloadSizes (64 B through 256 MiB). One env is created and reused
// across all sizes so we measure steady-state per-message throughput rather
// than connection-setup amortised into the first size.
func BenchmarkGRPCShmStream(b *testing.B) {
	env := newShmEnv(b)
	defer env.close()
	for _, p := range benchPayloadSizes {
		p := p
		b.Run(fmt.Sprintf("size=%s", p.label), func(b *testing.B) {
			benchStream(b, env.client, p.bytes)
		})
	}
}

// BenchmarkGRPCShmUnary measures unary RPC latency through the full gRPC
// stack over the shared memory transport, sweeping every entry of
// benchPayloadSizes (64 B through 256 MiB). Like the Stream variant the
// env is reused across sizes.
func BenchmarkGRPCShmUnary(b *testing.B) {
	env := newShmEnv(b)
	defer env.close()
	for _, p := range benchPayloadSizes {
		p := p
		b.Run(fmt.Sprintf("size=%s", p.label), func(b *testing.B) {
			benchUnary(b, env.client, p.bytes)
		})
	}
}

// =============================================================================
// TCP Transport — Full gRPC Stack
// =============================================================================

// BenchmarkGRPCTCPStream measures streaming ping-pong through the full gRPC
// stack over TCP loopback, covering the same size range as the SHM variant.
func BenchmarkGRPCTCPStream(b *testing.B) {
	env := newTCPEnv(b)
	defer env.close()
	for _, p := range benchPayloadSizes {
		p := p
		b.Run(fmt.Sprintf("size=%s", p.label), func(b *testing.B) {
			benchStream(b, env.client, p.bytes)
		})
	}
}

// BenchmarkGRPCTCPUnary measures unary RPC latency through the full gRPC
// stack over TCP loopback, covering the same size range as the SHM variant.
func BenchmarkGRPCTCPUnary(b *testing.B) {
	env := newTCPEnv(b)
	defer env.close()
	for _, p := range benchPayloadSizes {
		p := p
		b.Run(fmt.Sprintf("size=%s", p.label), func(b *testing.B) {
			benchUnary(b, env.client, p.bytes)
		})
	}
}

// =============================================================================
// Unix Socket Transport — Full gRPC Stack
// =============================================================================

// BenchmarkGRPCUnixStream measures streaming ping-pong through the full gRPC
// stack over a Unix domain socket, covering the same size range as the SHM
// variant.
func BenchmarkGRPCUnixStream(b *testing.B) {
	env := newUnixEnv(b)
	defer env.close()
	for _, p := range benchPayloadSizes {
		p := p
		b.Run(fmt.Sprintf("size=%s", p.label), func(b *testing.B) {
			benchStream(b, env.client, p.bytes)
		})
	}
}

// BenchmarkGRPCUnixUnary measures unary RPC latency through the full gRPC
// stack over a Unix domain socket, covering the same size range as the SHM
// variant.
func BenchmarkGRPCUnixUnary(b *testing.B) {
	env := newUnixEnv(b)
	defer env.close()
	for _, p := range benchPayloadSizes {
		p := p
		b.Run(fmt.Sprintf("size=%s", p.label), func(b *testing.B) {
			benchUnary(b, env.client, p.bytes)
		})
	}
}
