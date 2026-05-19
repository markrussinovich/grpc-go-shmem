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

package main

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

// Concurrent-streams scaling benchmarks. Reviewer (Doug) asked for
// numbers under realistic multi-RPC workloads (10 / 100 / 1000 concurrent
// streams), not just a single ping-pong stream which under-tests the
// transport's scheduling, lock contention, and flow-control accounting
// under load.
//
// Design: open N concurrent StreamingCall streams against the SAME
// gRPC connection (a single SHM segment, a single TCP / UDS socket).
// Each stream runs in its own goroutine and does b.N ping-pong rounds.
// The benchmark reports:
//
//   ns/op:  wall-clock duration per round per stream. With N streams
//           running in parallel, this is the per-stream latency under
//           contention — smaller is better.
//
//   MB/s:   per-stream throughput. Aggregate transport throughput
//           across all N streams is N × the reported MB/s.
//
// The numStreams dimension drives the scaling table. Three sizes
// (small / mid / large) keep run time bounded while still exercising
// both latency-sensitive and bandwidth-sensitive paths.

var benchConcurrencyLevels = []int{10, 100, 1000}

// concurrentStreamSizes is the per-message payload set used by the
// concurrent benchmarks. Kept SMALLER than the single-stream benchmarks
// because concurrent total bytes scales as numStreams x size x b.N:
// for 1000 streams x 1 MiB x just b.N=10 that is already 10 GiB of ring
// traffic per sub-bench. Anything > 1 MiB makes the matrix run for
// tens of minutes without adding new information beyond what
// BenchmarkGRPC*Stream and BenchmarkGRPC*Unary already cover at the
// large end.
var concurrentStreamSizes = []int{64, 4096, 65536, 262144, 1048576}

// benchConcurrentStreams runs numStreams concurrent ping-pong streams,
// each doing b.N rounds in its own goroutine. Per-stream stream setup
// is part of the unmeasured warm-up; only the timed loop counts.
func benchConcurrentStreams(b *testing.B, client testgrpc.BenchmarkServiceClient, numStreams, size int) {
	if numStreams <= 0 {
		b.Fatalf("numStreams must be > 0")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := &testpb.SimpleRequest{
		ResponseType: testpb.PayloadType_COMPRESSABLE,
		ResponseSize: int32(size),
		Payload:      benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, size),
	}

	// Open all streams up-front and warm each one with a single
	// Send/Recv round so the timed loop measures steady-state
	// per-round latency rather than per-stream setup cost.
	streams := make([]testgrpc.BenchmarkService_StreamingCallClient, numStreams)
	for i := 0; i < numStreams; i++ {
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

	b.SetBytes(int64(size))
	b.ResetTimer()
	endCPU := startCPUProbe(b)
	endZC := startZCProbe(b)

	// One goroutine per stream. Each runs b.N ping-pong rounds. The
	// outer goroutine waits for all to finish before stopping the
	// timer. fatalErrs collects the first error from each goroutine
	// (atomic int32 ticks once any goroutine fails) so we don't keep
	// hammering a broken transport.
	var failed atomic.Int32
	var firstErrMu sync.Mutex
	var firstErr error

	var wg sync.WaitGroup
	wg.Add(numStreams)
	for i := 0; i < numStreams; i++ {
		s := streams[i]
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < b.N; j++ {
				if failed.Load() != 0 {
					return
				}
				if err := s.Send(req); err != nil {
					failed.Store(1)
					firstErrMu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("stream %d Send round %d: %w", idx, j, err)
					}
					firstErrMu.Unlock()
					return
				}
				if _, err := s.Recv(); err != nil {
					failed.Store(1)
					firstErrMu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("stream %d Recv round %d: %w", idx, j, err)
					}
					firstErrMu.Unlock()
					return
				}
			}
		}(i)
	}
	wg.Wait()
	b.StopTimer()
	endCPU()
	endZC()

	if firstErr != nil {
		b.Fatalf("concurrent stream failure: %v", firstErr)
	}

	// Aggregate throughput across all streams. SetBytes(size) reports
	// per-stream MB/s; multiply by numStreams for total wire bandwidth.
	totalBytes := float64(b.N) * float64(size) * float64(numStreams)
	elapsedSec := b.Elapsed().Seconds()
	if elapsedSec > 0 {
		b.ReportMetric(totalBytes/elapsedSec/(1024*1024), "aggregate-MB/s")
	}
	b.ReportMetric(float64(numStreams), "streams")

	// Clean up streams.
	for _, s := range streams {
		_ = s.CloseSend()
		for {
			if _, err := s.Recv(); err != nil {
				break
			}
		}
	}
}

// BenchmarkGRPCShmConcurrent runs concurrent-streams scaling over SHM.
func BenchmarkGRPCShmConcurrent(b *testing.B) {
	env := newShmEnv(b)
	defer env.close()
	for _, n := range benchConcurrencyLevels {
		for _, size := range concurrentStreamSizes {
			n, size := n, size
			b.Run(fmt.Sprintf("streams=%d/size=%d", n, size), func(b *testing.B) {
				benchConcurrentStreams(b, env.client, n, size)
			})
		}
	}
}

// BenchmarkGRPCTCPConcurrent runs concurrent-streams scaling over TCP.
func BenchmarkGRPCTCPConcurrent(b *testing.B) {
	env := newTCPEnv(b)
	defer env.close()
	for _, n := range benchConcurrencyLevels {
		for _, size := range concurrentStreamSizes {
			n, size := n, size
			b.Run(fmt.Sprintf("streams=%d/size=%d", n, size), func(b *testing.B) {
				benchConcurrentStreams(b, env.client, n, size)
			})
		}
	}
}

// BenchmarkGRPCUnixConcurrent runs concurrent-streams scaling over UDS.
func BenchmarkGRPCUnixConcurrent(b *testing.B) {
	env := newUnixEnv(b)
	defer env.close()
	for _, n := range benchConcurrencyLevels {
		for _, size := range concurrentStreamSizes {
			n, size := n, size
			b.Run(fmt.Sprintf("streams=%d/size=%d", n, size), func(b *testing.B) {
				benchConcurrentStreams(b, env.client, n, size)
			})
		}
	}
}
