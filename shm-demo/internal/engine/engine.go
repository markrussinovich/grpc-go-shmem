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

// Package engine runs the Go benchmark for one or more transports and streams
// NDJSON events to stdout. For each transport it spawns the server as a child
// process (so SHM is exercised cross-process), drives a single-stream latency
// and throughput measurement from this process, samples CPU of both processes,
// and emits a result event.
package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"shmdemo/internal/bench"
	"shmdemo/internal/childguard"
	"shmdemo/internal/protocol"
	"shmdemo/internal/transportx"
	pb "shmdemo/proto/shmdemobench"
)

// Options configure an engine run.
type Options struct {
	Payload    int                // payload size in bytes
	Profile    transportx.Profile // flow-control profile (fair | max)
	Warmup     time.Duration      // warmup per phase
	Measure    time.Duration      // measurement window per phase
	Reps       int                // measurement rounds per transport; median wins (default 1)
	Transports []string           // subset of protocol.Transports; empty => all
}

// Run executes the benchmark for the configured transports, emitting NDJSON
// events to stdout.
func Run(ctx context.Context, opts Options) error {
	enc := json.NewEncoder(os.Stdout)
	emit := func(ev protocol.Event) {
		ev.Lang = "go"
		_ = enc.Encode(ev)
	}

	transports := opts.Transports
	if len(transports) == 0 {
		transports = protocol.Transports
	}
	// Measure in the fixed protocol order (tcp -> uds -> shm). This is the most
	// intuitive order to present: results read as a clear progression from the
	// general-purpose transport to the specialized one.
	if opts.Warmup <= 0 {
		opts.Warmup = time.Second
	}
	if opts.Measure <= 0 {
		opts.Measure = 5 * time.Second
	}
	if opts.Reps <= 0 {
		opts.Reps = 1
	}

	runID := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)

	for _, t := range transports {
		kind := transportx.Kind(t)
		res, err := runOne(ctx, kind, runID, opts, emit)
		if err != nil {
			emit(protocol.Event{Type: "error", Transport: t, PayloadBytes: opts.Payload, Error: err.Error()})
			continue
		}
		res.Type = "result"
		res.Transport = t
		res.PayloadBytes = opts.Payload
		emit(res)
	}

	emit(protocol.Event{Type: "done"})
	return nil
}

func runOne(ctx context.Context, kind transportx.Kind, runID string, opts Options, emit func(protocol.Event)) (protocol.Event, error) {
	endpoint := endpointFor(kind, runID)

	emit(protocol.Event{Type: "progress", Transport: string(kind), Phase: "connect"})

	srv, dialTarget, err := startServer(ctx, kind, endpoint, opts.Profile)
	if err != nil {
		return protocol.Event{}, fmt.Errorf("start server: %w", err)
	}
	defer srv.stop()

	conn, err := transportx.Dial(kind, dialTarget, opts.Profile)
	if err != nil {
		return protocol.Event{}, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	client := pb.NewBenchmarkServiceClient(conn)

	// Establish the connection and warm code paths. Scale the number of warmup
	// calls down for large payloads so a 256 MiB run doesn't move tens of GiB
	// before measuring; give the window proportionally more time.
	warmCalls := 50
	if perCall := 16 * 1024 * 1024 / max(opts.Payload, 1); perCall < warmCalls {
		warmCalls = max(perCall, 3)
	}
	warmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := bench.Warmup(warmCtx, client, opts.Payload, warmCalls); err != nil {
		return protocol.Event{}, fmt.Errorf("warmup: %w", err)
	}

	// Measure the latency and throughput phases opts.Reps times and combine the
	// rounds per metric. Repeating guards against the occasional bad sample on a
	// noisy/thermally-throttled host (notably ARM laptops): a single unlucky
	// round cannot drag the headline number. The server and client are reused
	// across rounds — only the measurement windows repeat — so the extra rounds
	// cost measurement time, not setup time.
	//
	// Per-phase warmup is paid only on the FIRST round. The connection, JIT,
	// and OS buffers are warmed once per transport; by rounds 2 and 3 the
	// process is already hot, so repeating the warmup would just burn wall-clock
	// (warmup × 2 phases × every round) without changing the numbers. Skipping
	// it on later rounds is what keeps the 3-round total near a single long run
	// instead of 3x as long.
	//
	// Each phase is given a generous per-call deadline (4x the measurement
	// window): a single 256 MiB echo round-trip is itself bandwidth-bound and
	// can take a second or more, so the deadline only fires on a genuine hang.
	// A round that exceeds the deadline is dropped, not fatal — only if every
	// round times out is the transport reported as failed.
	callTimeout := 4 * opts.Measure
	var latP50s, latP99s, msgPerSecs, mbPerSecs, cpuPer1Ms []float64
	for rep := 1; rep <= opts.Reps; rep++ {
		warmup := opts.Warmup
		if rep > 1 {
			warmup = 0 // already hot after round 1; measure straight away
		}

		emit(protocol.Event{Type: "progress", Transport: string(kind), Phase: "latency", Round: rep, Rounds: opts.Reps})
		latCtx, latCancel := context.WithTimeout(ctx, callTimeout)
		lat, err := bench.RunLatency(latCtx, client, opts.Payload, warmup, opts.Measure)
		latTimedOut := latCtx.Err() == context.DeadlineExceeded
		latCancel()
		if err != nil && !latTimedOut {
			return protocol.Event{}, fmt.Errorf("latency: %w", err)
		}
		if latTimedOut {
			continue // round hung: drop it and try the next one
		}

		// Throughput phase with CPU sampling across the measurement window only.
		// Sampling is bracketed via the onMeasure hook so the warmup the
		// throughput run performs internally is excluded — otherwise the
		// warmup's CPU time would be charged against the measure-only message
		// count and inflate the per-million-message figure.
		emit(protocol.Event{Type: "progress", Transport: string(kind), Phase: "throughput", Round: rep, Rounds: opts.Reps})
		var selfStart, srvStart, selfEnd, srvEnd time.Duration
		onMeasure := func() func() {
			selfStart, _ = bench.SelfCPU()
			srvStart, _ = bench.ProcessCPU(srv.pid())
			return func() {
				selfEnd, _ = bench.SelfCPU()
				srvEnd, _ = bench.ProcessCPU(srv.pid())
			}
		}
		tpCtx, tpCancel := context.WithTimeout(ctx, callTimeout)
		tp, err := bench.RunThroughput(tpCtx, client, opts.Payload, warmup, opts.Measure, onMeasure)
		tpTimedOut := tpCtx.Err() == context.DeadlineExceeded
		tpCancel()
		if err != nil && !tpTimedOut {
			return protocol.Event{}, fmt.Errorf("throughput: %w", err)
		}
		if tpTimedOut {
			continue // round hung: drop the whole round (latency included)
		}

		cpuSec := (selfEnd - selfStart + srvEnd - srvStart).Seconds()
		cpuPer1M := 0.0
		if tp.Msgs > 0 {
			cpuPer1M = cpuSec / (float64(tp.Msgs) / 1_000_000.0)
		}

		latP50s = append(latP50s, lat.P50Us)
		latP99s = append(latP99s, lat.P99Us)
		msgPerSecs = append(msgPerSecs, tp.MsgPerSec)
		mbPerSecs = append(mbPerSecs, tp.MBPerSec)
		cpuPer1Ms = append(cpuPer1Ms, cpuPer1M)
	}

	if len(latP50s) == 0 {
		return protocol.Event{}, fmt.Errorf("all %d round(s) timed out", opts.Reps)
	}

	return protocol.Event{
		LatencyP50Us: combineRounds(latP50s, true),     // lower is better
		LatencyP99Us: combineRounds(latP99s, true),     // lower is better
		MsgPerSec:    combineRounds(msgPerSecs, false), // higher is better
		MBPerSec:     combineRounds(mbPerSecs, false),  // higher is better
		CPUSecPer1M:  combineRounds(cpuPer1Ms, true),   // lower is better
	}, nil
}

// combineRounds reduces the surviving per-round samples of one metric to the
// single reported value. With three samples it returns the median (middle);
// with two it returns the worse (more conservative) of the pair so a lucky
// round cannot flatter the result; with one it returns that lone sample.
// worseIsMax selects the direction of "worse": true for lower-is-better metrics
// (latency, CPU cost), false for higher-is-better metrics (throughput). It
// sorts a copy so the caller's slice is untouched.
func combineRounds(vs []float64, worseIsMax bool) float64 {
	switch len(vs) {
	case 0:
		return 0
	case 1:
		return vs[0]
	case 2:
		if worseIsMax {
			return math.Max(vs[0], vs[1])
		}
		return math.Min(vs[0], vs[1])
	default:
		c := append([]float64(nil), vs...)
		sort.Float64s(c)
		return c[(len(c)-1)/2]
	}
}

func endpointFor(kind transportx.Kind, runID string) string {
	switch kind {
	case transportx.UDS:
		return filepath.Join(os.TempDir(), fmt.Sprintf("shmdemo_%s_uds.sock", runID))
	case transportx.SHM:
		return fmt.Sprintf("shmdemo_%s", runID)
	default: // TCP picks its own free port
		return ""
	}
}

// serverProc wraps the spawned server child process.
type serverProc struct {
	cmd     *exec.Cmd
	release func() // releases the childguard (kill-on-parent-death) resources
}

func (s *serverProc) pid() int { return s.cmd.Process.Pid }

func (s *serverProc) stop() {
	if s.release != nil {
		s.release()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
}

// startServer spawns "<self> --role server ..." and waits for its READY line.
func startServer(ctx context.Context, kind transportx.Kind, endpoint string, profile transportx.Profile) (*serverProc, string, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, "", err
	}
	args := []string{"--role", "server", "--transport", string(kind), "--profile", string(profile)}
	if endpoint != "" {
		args = append(args, "--endpoint", endpoint)
	}
	cmd := exec.Command(self, args...)
	cmd.Stderr = os.Stderr // surface server logs on the engine's stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	// Tie the server's lifetime to this engine so an abrupt engine kill (which
	// skips deferred cleanup) cannot orphan the server holding its SHM ring.
	childguard.Prepare(cmd)
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	release, _ := childguard.Guard(cmd)

	proc := &serverProc{cmd: cmd, release: release}

	// Wait for the READY line (with a timeout).
	type readyMsg struct {
		target string
		err    error
	}
	ch := make(chan readyMsg, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "READY ") {
				ch <- readyMsg{target: strings.TrimPrefix(line, "READY ")}
				return
			}
		}
		if err := sc.Err(); err != nil {
			ch <- readyMsg{err: err}
			return
		}
		ch <- readyMsg{err: fmt.Errorf("server exited before READY")}
	}()

	select {
	case <-ctx.Done():
		proc.stop()
		return nil, "", ctx.Err()
	case <-time.After(15 * time.Second):
		proc.stop()
		return nil, "", fmt.Errorf("timed out waiting for server READY")
	case m := <-ch:
		if m.err != nil {
			proc.stop()
			return nil, "", m.err
		}
		// Drain remaining server stdout so it never blocks.
		go func() { _, _ = io.Copy(io.Discard, stdout) }()
		return proc, m.target, nil
	}
}
