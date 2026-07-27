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
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/benchmark"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	testpb "google.golang.org/grpc/interop/grpc_testing"
)

// Throughput benchmarks.
//
// The latency benchmarks in bench_test.go keep exactly one message in flight,
// so their MB/s is a restatement of round-trip latency, not a measure of what
// the transport can carry. These benchmarks keep the transport busy instead,
// in the two ways that matter for a shared-memory ring:
//
//   - pipelined: one stream, sender and receiver running concurrently, so
//     depth is bounded by flow control rather than by the application waiting
//     for each reply. This is the single-stream ceiling.
//
//   - concurrent: N streams on one connection, each doing ping-pong. This is
//     the multiplexing path -- stream scheduling, per-stream flow-control
//     accounting and ring contention, none of which a single stream exercises.
//
// Both report duplex-MB/s (payload bytes crossing the transport in both
// directions per second) and kmsg/s (messages per second, both directions).
// ns/op is retained but is per-round wall clock, which under concurrency is
// per-stream latency under load, not a throughput figure.

// throughputSizes runs up to 16 MiB, well past the point where per-message
// overhead matters, so the tail of the sweep shows the transports' copy and
// flow-control ceilings rather than their framing costs. Note that the top of
// the sweep is expensive: concurrent traffic scales as streams x size x b.N,
// so streams=100/size=16MB alone has gigabytes in flight.
var throughputSizes = []struct {
	bytes int
	label string
}{
	{64, "64"},
	{4096, "4096"},
	{64 << 10, "65536"},
	{1 << 20, "1MB"},
	{4 << 20, "4MB"},
	{16 << 20, "16MB"},
}

// throughputConcurrency is the stream-count dimension for the multiplexing
// benchmark.
var throughputConcurrency = []int{10, 100}

// reportThroughput converts a completed run into duplex bandwidth and message
// rate. rounds is the number of request/response exchanges completed across
// all streams; each round moves size bytes in each direction.
func reportThroughput(b *testing.B, rounds int64, size int) {
	elapsed := b.Elapsed().Seconds()
	if elapsed <= 0 {
		return
	}
	duplexBytes := float64(rounds) * float64(size) * 2
	b.ReportMetric(duplexBytes/elapsed/(1024*1024), "duplex-MB/s")
	b.ReportMetric(float64(rounds)*2/elapsed/1000, "kmsg/s")
}

// benchPipelinedStream measures single-stream throughput with the send and
// receive halves decoupled. The sender never waits for a reply, so the number
// of messages in flight is whatever flow control allows -- which is the point:
// it measures the transport's carrying capacity rather than its round-trip
// latency.
func benchPipelinedStream(b *testing.B, client testgrpc.BenchmarkServiceClient, size int) {
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

	b.ResetTimer()

	var sendErr, recvErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < b.N; i++ {
			if err := stream.Send(req); err != nil {
				sendErr = fmt.Errorf("Send %d: %w", i, err)
				return
			}
		}
	}()
	for i := 0; i < b.N; i++ {
		if _, err := stream.Recv(); err != nil {
			recvErr = fmt.Errorf("Recv %d: %w", i, err)
			break
		}
	}
	wg.Wait()
	b.StopTimer()

	if sendErr != nil {
		b.Fatalf("pipelined stream: %v", sendErr)
	}
	if recvErr != nil {
		b.Fatalf("pipelined stream: %v", recvErr)
	}
	reportThroughput(b, int64(b.N), size)

	_ = stream.CloseSend()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
}

// benchConcurrentStreams measures aggregate throughput across numStreams
// concurrent ping-pong streams sharing one connection.
func benchConcurrentStreams(b *testing.B, client testgrpc.BenchmarkServiceClient, numStreams, size int) {
	req := &testpb.SimpleRequest{
		ResponseType: testpb.PayloadType_COMPRESSABLE,
		ResponseSize: int32(size),
		Payload:      benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, size),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Open and warm every stream before the timer starts so per-stream setup
	// is not charged to the measurement.
	streams := make([]testgrpc.BenchmarkService_StreamingCallClient, numStreams)
	for i := range streams {
		s, err := client.StreamingCall(ctx)
		if err != nil {
			b.Fatalf("StreamingCall #%d: %v", i, err)
		}
		if err := s.Send(req); err != nil {
			b.Fatalf("warm-up Send #%d: %v", i, err)
		}
		if _, err := s.Recv(); err != nil {
			b.Fatalf("warm-up Recv #%d: %v", i, err)
		}
		streams[i] = s
	}

	b.ResetTimer()

	var failed atomic.Bool
	var errMu sync.Mutex
	var firstErr error
	fail := func(err error) {
		failed.Store(true)
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
	}

	var rounds atomic.Int64
	var wg sync.WaitGroup
	wg.Add(numStreams)
	for i, s := range streams {
		go func(idx int, s testgrpc.BenchmarkService_StreamingCallClient) {
			defer wg.Done()
			done := 0
			defer func() { rounds.Add(int64(done)) }()
			for j := 0; j < b.N; j++ {
				if failed.Load() {
					return
				}
				if err := s.Send(req); err != nil {
					fail(fmt.Errorf("stream %d Send round %d: %w", idx, j, err))
					return
				}
				if _, err := s.Recv(); err != nil {
					fail(fmt.Errorf("stream %d Recv round %d: %w", idx, j, err))
					return
				}
				done++
			}
		}(i, s)
	}
	wg.Wait()
	b.StopTimer()

	if firstErr != nil {
		b.Fatalf("concurrent streams: %v", firstErr)
	}
	reportThroughput(b, rounds.Load(), size)
	b.ReportMetric(float64(numStreams), "streams")

	for _, s := range streams {
		_ = s.CloseSend()
		for {
			if _, err := s.Recv(); err != nil {
				break
			}
		}
	}
}

func sweepPipelined(b *testing.B, newEnv func(*testing.B) *benchEnv) {
	env := newEnv(b)
	defer env.close()
	for _, p := range throughputSizes {
		b.Run(fmt.Sprintf("size=%s", p.label), func(b *testing.B) {
			benchPipelinedStream(b, env.client, p.bytes)
		})
	}
}

func sweepConcurrent(b *testing.B, newEnv func(*testing.B) *benchEnv) {
	env := newEnv(b)
	defer env.close()
	for _, n := range throughputConcurrency {
		for _, p := range throughputSizes {
			b.Run(fmt.Sprintf("streams=%d/size=%s", n, p.label), func(b *testing.B) {
				benchConcurrentStreams(b, env.client, n, p.bytes)
			})
		}
	}
}

func BenchmarkMonoPipelined(b *testing.B)    { sweepPipelined(b, newMonoEnv) }
func BenchmarkPluginPipelined(b *testing.B)  { sweepPipelined(b, newPluginEnv) }
func BenchmarkUDSPipelined(b *testing.B)     { sweepPipelined(b, newUDSEnv) }
func BenchmarkMonoConcurrent(b *testing.B)   { sweepConcurrent(b, newMonoEnv) }
func BenchmarkPluginConcurrent(b *testing.B) { sweepConcurrent(b, newPluginEnv) }
func BenchmarkUDSConcurrent(b *testing.B)    { sweepConcurrent(b, newUDSEnv) }
