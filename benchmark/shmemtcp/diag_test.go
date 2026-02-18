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

// Diagnostic benchmarks to identify root causes of SHM vs UDS performance gaps
// at the 1 MiB payload size. These benchmarks isolate specific factors:
//
//   - Theory 1 (Cold vs Warm): Adaptive spin cutoffs start at 32 and need warm-up
//   - Theory 2 (Frame Overhead): Unary RPCs have 6 frames vs 2 for streaming
//   - Theory 3 (Benchmark Noise): Run interleaved SHM/UDS to reduce ordering effects

package main

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/benchmark"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	testpb "google.golang.org/grpc/interop/grpc_testing"
)

const diagPayload = 1024 * 1024 // 1 MiB

// =============================================================================
// Theory 1: Cold-Start vs Warm-Start
// =============================================================================
//
// The original data showed SHM winning at 1MB in the regular Stream benchmark
// (run after 7 smaller sizes) but losing in the Large Stream benchmark (run
// first). This tests whether prior warm-up changes SHM performance.

// BenchmarkDiagShmColdStream runs 1MB streaming on a freshly created SHM
// transport with no prior warm-up beyond the single RPC handshake.
func BenchmarkDiagShmColdStream(b *testing.B) {
	env := newShmEnv(b)
	defer env.close()
	// No additional warm-up — jump straight to 1MB
	b.Run("1MB", func(b *testing.B) {
		benchStream(b, env.client, diagPayload)
	})
}

// BenchmarkDiagShmWarmStream runs 1MB streaming after warming up with 1000
// iterations of 4KB messages to let adaptive spin cutoffs converge and caches
// fill.
func BenchmarkDiagShmWarmStream(b *testing.B) {
	env := newShmEnv(b)
	defer env.close()

	// Warm up with 1000 iterations of small messages to adapt spin cutoffs
	warmUpStreaming(b, env.client, 4096, 1000)

	b.Run("1MB", func(b *testing.B) {
		benchStream(b, env.client, diagPayload)
	})
}

// BenchmarkDiagUdsColdStream is the UDS equivalent of the cold-start test.
func BenchmarkDiagUdsColdStream(b *testing.B) {
	env := newUnixEnv(b)
	defer env.close()
	b.Run("1MB", func(b *testing.B) {
		benchStream(b, env.client, diagPayload)
	})
}

// BenchmarkDiagUdsWarmStream is the UDS equivalent of the warm-start test.
func BenchmarkDiagUdsWarmStream(b *testing.B) {
	env := newUnixEnv(b)
	defer env.close()
	warmUpStreaming(b, env.client, 4096, 1000)
	b.Run("1MB", func(b *testing.B) {
		benchStream(b, env.client, diagPayload)
	})
}

// =============================================================================
// Theory 2: Per-RPC Frame Overhead in Unary Mode
// =============================================================================
//
// Unary RPCs require HEADERS + MESSAGE + HALFCLOSE (client→server) and
// HEADERS + MESSAGE + TRAILERS (server→client) = 6 frames per call.
// Streaming reuses one open stream, requiring only MESSAGE frames.
// This compares unary vs streaming at 1MB to quantify per-RPC overhead.

// BenchmarkDiagShmUnaryVsStream compares unary and streaming at 1MB on the
// same SHM transport to quantify per-RPC frame overhead.
func BenchmarkDiagShmUnaryVsStream(b *testing.B) {
	env := newShmEnv(b)
	defer env.close()
	b.Run("Unary-1MB", func(b *testing.B) {
		benchUnary(b, env.client, diagPayload)
	})
	b.Run("Stream-1MB", func(b *testing.B) {
		benchStream(b, env.client, diagPayload)
	})
}

// BenchmarkDiagUdsUnaryVsStream is the UDS equivalent.
func BenchmarkDiagUdsUnaryVsStream(b *testing.B) {
	env := newUnixEnv(b)
	defer env.close()
	b.Run("Unary-1MB", func(b *testing.B) {
		benchUnary(b, env.client, diagPayload)
	})
	b.Run("Stream-1MB", func(b *testing.B) {
		benchStream(b, env.client, diagPayload)
	})
}

// =============================================================================
// Theory 3: Interleaved Comparison to Reduce Ordering Effects
// =============================================================================
//
// Run SHM and UDS benchmarks at multiple sizes in alternating fashion to
// minimize environmental drift between measurements.

// BenchmarkDiagInterleaved1MBStream runs SHM and UDS streaming at 1MB
// in alternating pairs. Each pair creates fresh environments to avoid
// cross-contamination. The -count flag controls repetitions.
func BenchmarkDiagInterleaved1MBStream(b *testing.B) {
	b.Run("SHM", func(b *testing.B) {
		env := newShmEnv(b)
		defer env.close()
		benchStream(b, env.client, diagPayload)
	})
	b.Run("UDS", func(b *testing.B) {
		env := newUnixEnv(b)
		defer env.close()
		benchStream(b, env.client, diagPayload)
	})
}

// BenchmarkDiagInterleaved1MBUnary runs SHM and UDS unary at 1MB.
func BenchmarkDiagInterleaved1MBUnary(b *testing.B) {
	b.Run("SHM", func(b *testing.B) {
		env := newShmEnv(b)
		defer env.close()
		benchUnary(b, env.client, diagPayload)
	})
	b.Run("UDS", func(b *testing.B) {
		env := newUnixEnv(b)
		defer env.close()
		benchUnary(b, env.client, diagPayload)
	})
}

// =============================================================================
// Additional: Payload scaling to find crossover point
// =============================================================================

// BenchmarkDiagShmStreamScaling measures SHM streaming throughput across
// payload sizes from 256KB to 4MB to find where SHM's advantage appears.
func BenchmarkDiagShmStreamScaling(b *testing.B) {
	env := newShmEnv(b)
	defer env.close()
	for _, size := range []int{256 * 1024, 512 * 1024, 1024 * 1024, 2 * 1024 * 1024, 4 * 1024 * 1024} {
		size := size
		b.Run(fmt.Sprintf("size=%dKB", size/1024), func(b *testing.B) {
			benchStream(b, env.client, size)
		})
	}
}

// BenchmarkDiagUdsStreamScaling is the UDS equivalent.
func BenchmarkDiagUdsStreamScaling(b *testing.B) {
	env := newUnixEnv(b)
	defer env.close()
	for _, size := range []int{256 * 1024, 512 * 1024, 1024 * 1024, 2 * 1024 * 1024, 4 * 1024 * 1024} {
		size := size
		b.Run(fmt.Sprintf("size=%dKB", size/1024), func(b *testing.B) {
			benchStream(b, env.client, size)
		})
	}
}

// warmUpStreaming runs n iterations of streaming ping-pong with the given
// payload size. This warms up the transport's adaptive spin cutoffs, CPU
// caches, and Go runtime state without affecting benchmark timing.
func warmUpStreaming(b *testing.B, client testgrpc.BenchmarkServiceClient, size, n int) {
	b.Helper()
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
