// Copyright 2026 gRPC SHM Demo authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bench

import (
	"context"
	"sort"
	"time"

	pb "shmdemo/proto/shmdemobench"
)

// LatencyResult holds single-stream ping-pong latency percentiles.
type LatencyResult struct {
	P50Us float64
	P99Us float64
	Ops   int64
}

// ThroughputResult holds single-stream bounded-in-flight ping-pong throughput.
type ThroughputResult struct {
	MsgPerSec float64
	MBPerSec  float64
	Msgs      int64
}

func newRequest(payload int, responseSize int) *pb.SimpleRequest {
	return &pb.SimpleRequest{
		ResponseSize: int32(responseSize),
		Payload:      &pb.Payload{Body: make([]byte, payload)},
	}
}

// RunLatency measures round-trip latency on a single bidi stream using
// ping-pong with exactly one in-flight message (send -> await echo -> repeat).
func RunLatency(ctx context.Context, client pb.BenchmarkServiceClient, payload int, warmup, measure time.Duration) (LatencyResult, error) {
	stream, err := client.StreamingCall(ctx)
	if err != nil {
		return LatencyResult{}, err
	}
	req := newRequest(payload, payload) // response_size>0 => echo

	pingPong := func() error {
		if err := stream.Send(req); err != nil {
			return err
		}
		_, err := stream.Recv()
		return err
	}

	// Warmup.
	for deadline := time.Now().Add(warmup); time.Now().Before(deadline); {
		if err := pingPong(); err != nil {
			return LatencyResult{}, err
		}
	}

	// Measure.
	samples := make([]int64, 0, 1<<16)
	for deadline := time.Now().Add(measure); time.Now().Before(deadline); {
		t0 := nowTick()
		if err := pingPong(); err != nil {
			return LatencyResult{}, err
		}
		samples = append(samples, tickNanos(nowTick()-t0))
	}
	_ = stream.CloseSend()

	return LatencyResult{
		P50Us: percentileUs(samples, 50),
		P99Us: percentileUs(samples, 99),
		Ops:   int64(len(samples)),
	}, nil
}

// RunThroughput measures streaming throughput on a single bidi stream using
// bounded-in-flight ping-pong: send one message, await its echo, repeat. Keeping
// exactly one message in flight means the workload always stays within the
// flow-control window for every transport and every profile — an unbounded
// one-way blast would overrun a small fair window. This is the identical
// methodology used by the published SHM benchmark suite, so TCP, UDS, and SHM
// are compared on equal footing. Bytes are counted in both directions (request
// + echoed response), matching the benchmark's throughput formula.
//
// onMeasure, if non-nil, is invoked immediately before the measurement loop
// begins (after warmup) and must return a stop func that is invoked immediately
// after the loop ends. This lets the caller sample external counters (e.g. CPU
// time) over exactly the measured window, excluding warmup.
func RunThroughput(ctx context.Context, client pb.BenchmarkServiceClient, payload int, warmup, measure time.Duration, onMeasure func() (stop func())) (ThroughputResult, error) {
	stream, err := client.StreamingCall(ctx)
	if err != nil {
		return ThroughputResult{}, err
	}
	req := newRequest(payload, payload) // response_size>0 => echo (one in-flight)

	pingPong := func() error {
		if err := stream.Send(req); err != nil {
			return err
		}
		_, err := stream.Recv()
		return err
	}

	// Warmup.
	for deadline := time.Now().Add(warmup); time.Now().Before(deadline); {
		if err := pingPong(); err != nil {
			return ThroughputResult{}, err
		}
	}

	// Measure. Bracket the loop with the caller's hook so external sampling
	// (CPU time) covers exactly the measured window, not the warmup above.
	var stop func()
	if onMeasure != nil {
		stop = onMeasure()
	}
	var msgs int64
	start := time.Now()
	for deadline := start.Add(measure); time.Now().Before(deadline); {
		if err := pingPong(); err != nil {
			if stop != nil {
				stop()
			}
			return ThroughputResult{}, err
		}
		msgs++
	}
	elapsed := time.Since(start)
	if stop != nil {
		stop()
	}
	_ = stream.CloseSend()
	_, _ = stream.Recv() // drain server EOF

	secs := elapsed.Seconds()
	res := ThroughputResult{Msgs: msgs}
	if secs > 0 {
		res.MsgPerSec = float64(msgs) / secs
		// Count both directions (request + echoed response), matching the
		// benchmark suite's throughput definition.
		res.MBPerSec = float64(msgs) * float64(payload) * 2 / (1024 * 1024) / secs
	}
	return res, nil
}

// Warmup issues a few unary calls to force the connection to establish and
// the code paths to JIT/allocate before measurement begins.
func Warmup(ctx context.Context, client pb.BenchmarkServiceClient, payload int, calls int) error {
	req := newRequest(payload, payload)
	for i := 0; i < calls; i++ {
		if _, err := client.UnaryCall(ctx, req); err != nil {
			return err
		}
	}
	return nil
}

func percentileUs(samplesNs []int64, p float64) float64 {
	if len(samplesNs) == 0 {
		return 0
	}
	sort.Slice(samplesNs, func(i, j int) bool { return samplesNs[i] < samplesNs[j] })
	idx := int(float64(len(samplesNs)-1) * p / 100.0)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(samplesNs) {
		idx = len(samplesNs) - 1
	}
	return float64(samplesNs[idx]) / 1000.0
}
