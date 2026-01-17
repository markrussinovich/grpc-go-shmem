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

// Command shmemtcp runs side-by-side TCP vs shared-memory gRPC benchmarks for
// unary ping-pong and bidirectional streaming. It sweeps payload sizes from 0
// to 2MiB, records latency and throughput, and writes CSV/JSON plus SVG plots.
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/benchmark"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/internal/transport"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	testpb "google.golang.org/grpc/interop/grpc_testing"
)

type transportKind string

const (
	transportTCP transportKind = "tcp"
	transportSHM transportKind = "shm"

	defaultRingSize   = 64 * 1024 * 1024 // 64 MiB per ring to minimize contention
	defaultSegmentCap = 2 * defaultRingSize
)

type unaryResult struct {
	Transport        transportKind `json:"transport"`
	SizeBytes        int           `json:"size_bytes"`
	Iterations       int           `json:"iterations"`
	AvgLatencyMicros float64       `json:"avg_latency_us"`
}

type streamingResult struct {
	Transport        transportKind `json:"transport"`
	SizeBytes        int           `json:"size_bytes"`
	Iterations       int           `json:"iterations"`
	AvgLatencyMicros float64       `json:"avg_latency_us"`
	ThroughputMBps   float64       `json:"throughput_mb_per_s"`
}

type suiteResults struct {
	Timestamp time.Time         `json:"timestamp"`
	Sizes     []int             `json:"sizes_bytes"`
	Unary     []unaryResult     `json:"unary"`
	Streaming []streamingResult `json:"streaming"`
	Notes     string            `json:"notes"`
}

type benchEnv struct {
	kind     transportKind
	name     string
	listener net.Listener
	stopSrv  func()
	conn     *grpc.ClientConn
	client   testgrpc.BenchmarkServiceClient
	cleanup  func()
}

func main() {
	outDir := flag.String("out", filepath.Join("benchmark", "shmemtcp", "out"), "output directory for results and plots")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create output dir: %v\n", err)
		os.Exit(1)
	}

	sizes := []int{0, 1, 1024, 4 * 1024, 16 * 1024, 64 * 1024, 256 * 1024, 512 * 1024, 1024 * 1024, 2 * 1024 * 1024}

	ctx := context.Background()
	results, err := runSuite(ctx, sizes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchmark failed: %v\n", err)
		os.Exit(1)
	}

	if err := writeJSON(filepath.Join(*outDir, "results.json"), results); err != nil {
		fmt.Fprintf(os.Stderr, "write json: %v\n", err)
		os.Exit(1)
	}
	if err := writeCSV(filepath.Join(*outDir, "results.csv"), results); err != nil {
		fmt.Fprintf(os.Stderr, "write csv: %v\n", err)
		os.Exit(1)
	}
	if err := renderPlots(*outDir, results); err != nil {
		fmt.Fprintf(os.Stderr, "render plots: %v\n", err)
		os.Exit(1)
	}

	summarize(results, *outDir)
}

func runSuite(ctx context.Context, sizes []int) (suiteResults, error) {
	envs := make([]*benchEnv, 0, 2)
	defer func() {
		for _, e := range envs {
			if e != nil && e.cleanup != nil {
				e.cleanup()
			}
		}
	}()

	transports := []transportKind{transportTCP, transportSHM}
	for _, kind := range transports {
		env, err := startBenchEnv(kind, defaultRingSize, defaultSegmentCap)
		if err != nil {
			return suiteResults{}, fmt.Errorf("start %s env: %w", kind, err)
		}
		envs = append(envs, env)
	}

	var unary []unaryResult
	var streaming []streamingResult

	for _, env := range envs {
		for _, sz := range sizes {
			iter := iterationsForSize(sz)

			unaryLat, err := measureUnary(ctx, env.client, sz, iter)
			if err != nil {
				return suiteResults{}, fmt.Errorf("%s unary size %d: %w", env.kind, sz, err)
			}
			unary = append(unary, unaryResult{
				Transport:        env.kind,
				SizeBytes:        sz,
				Iterations:       iter,
				AvgLatencyMicros: durationMicros(unaryLat),
			})

			streamLat, streamThroughput, err := measureStreaming(ctx, env.client, sz, iter)
			if err != nil {
				return suiteResults{}, fmt.Errorf("%s streaming size %d: %w", env.kind, sz, err)
			}
			streaming = append(streaming, streamingResult{
				Transport:        env.kind,
				SizeBytes:        sz,
				Iterations:       iter,
				AvgLatencyMicros: durationMicros(streamLat),
				ThroughputMBps:   streamThroughput,
			})
		}
	}

	sort.Slice(unary, func(i, j int) bool {
		if unary[i].Transport == unary[j].Transport {
			return unary[i].SizeBytes < unary[j].SizeBytes
		}
		return unary[i].Transport < unary[j].Transport
	})
	sort.Slice(streaming, func(i, j int) bool {
		if streaming[i].Transport == streaming[j].Transport {
			return streaming[i].SizeBytes < streaming[j].SizeBytes
		}
		return streaming[i].Transport < streaming[j].Transport
	})

	return suiteResults{
		Timestamp: time.Now(),
		Sizes:     sizes,
		Unary:     unary,
		Streaming: streaming,
		Notes:     "BenchmarkService protobuf payloads; client and server on same host",
	}, nil
}

func startBenchEnv(kind transportKind, ringSize, segmentSize uint64) (*benchEnv, error) {
	env := &benchEnv{kind: kind}

	switch kind {
	case transportTCP:
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		env.listener = lis
		env.name = lis.Addr().String()
	case transportSHM:
		name := fmt.Sprintf("bench_shm_%d", time.Now().UnixNano())
		lis, err := transport.NewShmListener(&transport.ShmAddr{Name: name}, segmentSize, ringSize, ringSize)
		if err != nil {
			return nil, err
		}
		env.listener = lis
		env.name = name
	default:
		return nil, fmt.Errorf("unknown transport %q", kind)
	}

	env.stopSrv = benchmark.StartServer(benchmark.ServerInfo{Type: "protobuf", Listener: env.listener})

	target := env.listener.Addr().String()
	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if kind == transportSHM {
		target = fmt.Sprintf("shm://%s", env.name)
		dialOpts = append([]grpc.DialOption{grpc.WithShmTransport()}, dialOpts...)
	}

	conn, err := grpc.NewClient(target, dialOpts...)
	if err != nil {
		env.stopSrv()
		env.listener.Close()
		return nil, err
	}

	env.conn = conn
	env.client = testgrpc.NewBenchmarkServiceClient(conn)
	env.cleanup = func() {
		_ = env.conn.Close()
		if env.stopSrv != nil {
			env.stopSrv()
		}
		if env.listener != nil {
			_ = env.listener.Close()
		}
		if env.kind == transportSHM {
			_ = transport.RemoveSegment(env.name)
		}
	}

	return env, nil
}

func measureUnary(ctx context.Context, client testgrpc.BenchmarkServiceClient, payloadSize, iterations int) (time.Duration, error) {
	if iterations <= 0 {
		iterations = 1
	}

	payload := benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, payloadSize)
	req := &testpb.SimpleRequest{
		ResponseType: payload.Type,
		ResponseSize: int32(payloadSize),
		Payload:      payload,
	}

	deadline := 5*time.Second + time.Duration(iterations)*50*time.Millisecond
	if deadline > 120*time.Second {
		deadline = 120 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	start := time.Now()
	for i := 0; i < iterations; i++ {
		if _, err := client.UnaryCall(runCtx, req); err != nil {
			return 0, err
		}
	}
	elapsed := time.Since(start)
	return elapsed / time.Duration(iterations), nil
}

func measureStreaming(ctx context.Context, client testgrpc.BenchmarkServiceClient, payloadSize, iterations int) (time.Duration, float64, error) {
	if iterations <= 0 {
		iterations = 1
	}

	payload := benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, payloadSize)
	req := &testpb.SimpleRequest{
		ResponseType: payload.Type,
		ResponseSize: int32(payloadSize),
		Payload:      payload,
	}

	deadline := 5*time.Second + time.Duration(iterations)*50*time.Millisecond
	if deadline > 120*time.Second {
		deadline = 120 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	stream, err := client.StreamingCall(runCtx)
	if err != nil {
		return 0, 0, err
	}

	start := time.Now()
	for i := 0; i < iterations; i++ {
		if err := stream.Send(req); err != nil {
			return 0, 0, err
		}
		if _, err := stream.Recv(); err != nil {
			return 0, 0, err
		}
	}
	elapsed := time.Since(start)
	_ = stream.CloseSend()

	perMsg := elapsed / time.Duration(iterations)
	totalBytes := float64(iterations) * float64(payloadSize*2) // request + response
	throughputMBps := 0.0
	if elapsed > 0 && totalBytes > 0 {
		throughputMBps = totalBytes / (1024 * 1024) / elapsed.Seconds()
	}

	return perMsg, throughputMBps, nil
}

func iterationsForSize(size int) int {
	switch {
	case size <= 0:
		return 2000
	case size <= 1024:
		return 2000
	case size <= 16*1024:
		return 1200
	case size <= 64*1024:
		return 800
	case size <= 256*1024:
		return 400
	case size <= 512*1024:
		return 250
	case size <= 1024*1024:
		return 150
	default:
		return 80
	}
}

func writeJSON(path string, res suiteResults) error {
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func writeCSV(path string, res suiteResults) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	if err := w.Write([]string{"type", "transport", "size_bytes", "iterations", "avg_latency_us", "throughput_mb_per_s"}); err != nil {
		return err
	}

	for _, u := range res.Unary {
		rec := []string{"unary", string(u.Transport), fmt.Sprintf("%d", u.SizeBytes), fmt.Sprintf("%d", u.Iterations), fmt.Sprintf("%.3f", u.AvgLatencyMicros), ""}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	for _, s := range res.Streaming {
		rec := []string{"streaming", string(s.Transport), fmt.Sprintf("%d", s.SizeBytes), fmt.Sprintf("%d", s.Iterations), fmt.Sprintf("%.3f", s.AvgLatencyMicros), fmt.Sprintf("%.3f", s.ThroughputMBps)}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	return nil
}

// Simple SVG plotting helpers to avoid external deps.
type plotPoint struct {
	X float64
	Y float64
}

type plotSeries struct {
	Label  string
	Color  string
	Points []plotPoint
}

func renderPlots(outDir string, res suiteResults) error {
	unaryPath := filepath.Join(outDir, "unary_latency.svg")
	streamingLatencyPath := filepath.Join(outDir, "streaming_latency.svg")
	streamingThroughputPath := filepath.Join(outDir, "streaming_throughput.svg")

	if err := writeSVGPlot(unaryPath, "Unary ping-pong latency", "Payload size", "Avg latency (µs)", groupUnary(res.Unary)); err != nil {
		return err
	}
	if err := writeSVGPlot(streamingLatencyPath, "Streaming ping-pong latency", "Payload size", "Avg latency (µs)", groupStreamingLatency(res.Streaming)); err != nil {
		return err
	}
	if err := writeSVGPlot(streamingThroughputPath, "Streaming throughput", "Payload size", "Throughput (MiB/s)", groupStreamingThroughput(res.Streaming)); err != nil {
		return err
	}
	return nil
}

func groupUnary(results []unaryResult) []plotSeries {
	series := map[transportKind][]plotPoint{}
	for _, r := range results {
		series[r.Transport] = append(series[r.Transport], plotPoint{X: float64(r.SizeBytes), Y: r.AvgLatencyMicros})
	}
	return toSeries(series)
}

func groupStreamingLatency(results []streamingResult) []plotSeries {
	series := map[transportKind][]plotPoint{}
	for _, r := range results {
		series[r.Transport] = append(series[r.Transport], plotPoint{X: float64(r.SizeBytes), Y: r.AvgLatencyMicros})
	}
	return toSeries(series)
}

func groupStreamingThroughput(results []streamingResult) []plotSeries {
	series := map[transportKind][]plotPoint{}
	for _, r := range results {
		series[r.Transport] = append(series[r.Transport], plotPoint{X: float64(r.SizeBytes), Y: r.ThroughputMBps})
	}
	return toSeries(series)
}

func toSeries(m map[transportKind][]plotPoint) []plotSeries {
	keys := make([]transportKind, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	colors := map[transportKind]string{
		transportTCP: "#1f77b4",
		transportSHM: "#d62728",
	}

	var res []plotSeries
	for _, k := range keys {
		pts := m[k]
		sort.Slice(pts, func(i, j int) bool { return pts[i].X < pts[j].X })
		res = append(res, plotSeries{Label: string(k), Color: colors[k], Points: pts})
	}
	return res
}

func writeSVGPlot(path, title, xLabel, yLabel string, series []plotSeries) error {
	if len(series) == 0 {
		return fmt.Errorf("no data for plot %s", title)
	}

	width := 960.0
	height := 560.0
	marginLeft := 80.0
	marginRight := 40.0
	marginTop := 60.0
	marginBottom := 70.0

	xMin := 0.0
	xMax := 0.0
	yMax := 0.0
	for _, s := range series {
		for _, p := range s.Points {
			if p.X > xMax {
				xMax = p.X
			}
			if p.Y > yMax {
				yMax = p.Y
			}
		}
	}
	if xMax == 0 {
		xMax = 1
	}
	if yMax == 0 {
		yMax = 1
	}

	xTicks := niceTicks(xMin, xMax, 6)
	yTicks := niceTicks(0, yMax, 6)

	chartW := width - marginLeft - marginRight
	chartH := height - marginTop - marginBottom

	scaleX := chartW / (xMax - xMin)
	scaleY := chartH / (yMax - 0)

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">`, int(width), int(height), int(width), int(height))
	fmt.Fprintf(&b, `<style>text{font-family:Arial, sans-serif;font-size:12px;} .title{font-size:16px;font-weight:bold;}</style>`)
	fmt.Fprintf(&b, `<rect width="100%%" height="100%%" fill="white"/>`)

	// Title and axis labels
	fmt.Fprintf(&b, `<text x="%f" y="%f" class="title">%s</text>`, width/2, marginTop/2, title)
	fmt.Fprintf(&b, `<text x="%f" y="%f" text-anchor="middle">%s</text>`, width/2, height-20, xLabel)
	fmt.Fprintf(&b, `<text x="%f" y="%f" transform="rotate(-90 %f,%f)" text-anchor="middle">%s</text>`, 15.0, height/2, 15.0, height/2, yLabel)

	// Axes
	x0 := marginLeft
	y0 := height - marginBottom
	fmt.Fprintf(&b, `<line x1="%f" y1="%f" x2="%f" y2="%f" stroke="black" stroke-width="1"/>`, x0, y0, width-marginRight, y0)
	fmt.Fprintf(&b, `<line x1="%f" y1="%f" x2="%f" y2="%f" stroke="black" stroke-width="1"/>`, x0, y0, x0, marginTop)

	// Ticks
	for _, t := range xTicks {
		px := x0 + (t-xMin)*scaleX
		fmt.Fprintf(&b, `<line x1="%f" y1="%f" x2="%f" y2="%f" stroke="black"/>`, px, y0, px, y0+6)
		fmt.Fprintf(&b, `<text x="%f" y="%f" text-anchor="middle">%s</text>`, px, y0+20, formatBytes(int(t)))
	}
	for _, t := range yTicks {
		py := y0 - (t-0)*scaleY
		fmt.Fprintf(&b, `<line x1="%f" y1="%f" x2="%f" y2="%f" stroke="black"/>`, x0-6, py, x0, py)
		fmt.Fprintf(&b, `<text x="%f" y="%f" text-anchor="end">%s</text>`, x0-8, py+4, formatNumber(t))
	}

	// Grid lines
	for _, t := range xTicks {
		px := x0 + (t-xMin)*scaleX
		fmt.Fprintf(&b, `<line x1="%f" y1="%f" x2="%f" y2="%f" stroke="#dddddd" stroke-width="1"/>`, px, y0, px, marginTop)
	}
	for _, t := range yTicks {
		py := y0 - (t-0)*scaleY
		fmt.Fprintf(&b, `<line x1="%f" y1="%f" x2="%f" y2="%f" stroke="#ededed" stroke-width="1"/>`, x0, py, width-marginRight, py)
	}

	// Series
	for _, s := range series {
		fmt.Fprintf(&b, `<polyline fill="none" stroke="%s" stroke-width="2" points="`, s.Color)
		for _, p := range s.Points {
			px := x0 + (p.X-xMin)*scaleX
			py := y0 - (p.Y-0)*scaleY
			fmt.Fprintf(&b, "%f,%f ", px, py)
		}
		fmt.Fprintf(&b, `"/>`)

		for _, p := range s.Points {
			px := x0 + (p.X-xMin)*scaleX
			py := y0 - (p.Y-0)*scaleY
			fmt.Fprintf(&b, `<circle cx="%f" cy="%f" r="3" fill="%s"/>`, px, py, s.Color)
		}
	}

	// Legend
	legendX := width - marginRight - 150
	legendY := marginTop + 10
	fmt.Fprintf(&b, `<rect x="%f" y="%f" width="140" height="%f" fill="white" stroke="#ccc"/>`, legendX, legendY, float64(len(series))*22+10)
	for i, s := range series {
		y := legendY + 20 + float64(i)*22
		fmt.Fprintf(&b, `<line x1="%f" y1="%f" x2="%f" y2="%f" stroke="%s" stroke-width="3"/>`, legendX+10, y-5, legendX+30, y-5, s.Color)
		fmt.Fprintf(&b, `<text x="%f" y="%f">%s</text>`, legendX+40, y-2, s.Label)
	}

	fmt.Fprintf(&b, `</svg>`)

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func niceTicks(min, max float64, count int) []float64 {
	if max <= min {
		max = min + 1
	}
	rawStep := (max - min) / float64(count)
	step := niceStep(rawStep)
	start := math.Floor(min/step) * step
	end := math.Ceil(max/step) * step
	var ticks []float64
	for v := start; v <= end+step/2; v += step {
		if v < 0 && min >= 0 {
			continue
		}
		ticks = append(ticks, v)
	}
	return ticks
}

func niceStep(step float64) float64 {
	if step == 0 {
		return 1
	}
	pow := math.Pow(10, math.Floor(math.Log10(step)))
	scaled := step / pow
	var nice float64
	switch {
	case scaled < 1.5:
		nice = 1
	case scaled < 3:
		nice = 2
	case scaled < 7:
		nice = 5
	default:
		nice = 10
	}
	return nice * pow
}

func formatBytes(n int) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KiB", "MiB"}
	val := float64(n)
	idx := 0
	for val >= 1024 && idx < len(units)-1 {
		val /= 1024
		idx++
	}
	if idx == 0 {
		return fmt.Sprintf("%d %s", n, units[idx])
	}
	return fmt.Sprintf("%.1f %s", val, units[idx])
}

func formatNumber(v float64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.1fM", v/1_000_000)
	case v >= 1_000:
		return fmt.Sprintf("%.1fk", v/1_000)
	case v >= 10:
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

func durationMicros(d time.Duration) float64 {
	return float64(d) / float64(time.Microsecond)
}

func summarize(res suiteResults, outDir string) {
	fmt.Println("Benchmark completed")
	fmt.Printf("Results saved to %s\n", outDir)

	fmt.Println("\nUnary ping-pong latency (µs per call):")
	printUnaryTable(res.Unary)

	fmt.Println("\nStreaming ping-pong latency (µs per message) and throughput (MiB/s):")
	printStreamingTable(res.Streaming)
}

func printUnaryTable(rows []unaryResult) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].SizeBytes == rows[j].SizeBytes {
			return rows[i].Transport < rows[j].Transport
		}
		return rows[i].SizeBytes < rows[j].SizeBytes
	})

	fmt.Printf("%-12s %-12s %-12s %-18s\n", "payload", "transport", "iters", "avg_latency_us")
	for _, r := range rows {
		fmt.Printf("%-12s %-12s %-12d %-18.3f\n", formatBytes(r.SizeBytes), r.Transport, r.Iterations, r.AvgLatencyMicros)
	}
}

func printStreamingTable(rows []streamingResult) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].SizeBytes == rows[j].SizeBytes {
			return rows[i].Transport < rows[j].Transport
		}
		return rows[i].SizeBytes < rows[j].SizeBytes
	})

	fmt.Printf("%-12s %-12s %-12s %-18s %-18s\n", "payload", "transport", "iters", "avg_latency_us", "throughput_mib_s")
	for _, r := range rows {
		fmt.Printf("%-12s %-12s %-12d %-18.3f %-18.3f\n", formatBytes(r.SizeBytes), r.Transport, r.Iterations, r.AvgLatencyMicros, r.ThroughputMBps)
	}
}
