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

// Package shell hosts the demo web UI and orchestrates benchmark engine
// children. It serves the embedded SPA, and exposes /api/run as a
// Server-Sent Events stream that forwards NDJSON events from the engine.
package shell

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"shmdemo/internal/childguard"
	"shmdemo/internal/web"
)

// emptyGrace is how long the shell keeps running after the last open page
// disconnects. It must comfortably exceed a page refresh (sub-second gap) and
// the EventSource auto-reconnect window (~3s) so neither trips shutdown, while
// a genuinely closed last tab does.
const emptyGrace = 8 * time.Second

// startupGrace bounds how long the shell waits for any page to connect at all.
// If no presence connection ever arrives (e.g. the browser failed to launch),
// the process exits instead of lingering forever.
const startupGrace = 60 * time.Second

// presence tracks how many browser pages currently hold an open presence
// connection. Liveness is keyed on the *existence* of these connections rather
// than on elapsed time, so a locked screen, hibernation, or background-tab
// timer throttling cannot trip shutdown: the underlying TCP connection freezes
// with the machine and is still there on wake. Shutdown only happens once every
// page is genuinely gone (each closed connection sends a TCP FIN), with a short
// grace so a refresh — which briefly drops to zero — survives.
type presence struct {
	mu        sync.Mutex
	count     int       // number of open presence connections
	ever      bool      // true once any page has ever connected
	zeroSince time.Time // when count last became 0 (zero value = currently >0)
}

// add registers a new open page and returns a release func to call on
// disconnect. The release is idempotent-safe for a single connection.
func (p *presence) add() func() {
	p.mu.Lock()
	p.count++
	p.ever = true
	p.zeroSince = time.Time{}
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		p.count--
		if p.count <= 0 {
			p.count = 0
			p.zeroSince = time.Now()
		}
		p.mu.Unlock()
	}
}

// expired reports whether every page has been gone for longer than emptyGrace.
// It only consults elapsed time while the count is genuinely 0 (all TCP
// connections closed), so sleep/lock — which keep connections open — never
// reach this path.
func (p *presence) expired() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ever && p.count == 0 && !p.zeroSince.IsZero() && time.Since(p.zeroSince) > emptyGrace
}

// everConnected reports whether any page has ever connected.
func (p *presence) everConnected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ever
}

// children tracks live engine subprocesses so they can be killed on shutdown.
var (
	childMu  sync.Mutex
	children = map[*exec.Cmd]struct{}{}
)

// runGate serializes benchmark runs. Only one engine may run at a time: two
// concurrent engines (e.g. two open tabs both pressing Run) would contend for
// CPU and produce meaningless numbers, so a second run is rejected rather than
// allowed to corrupt the measurement.
var runGate sync.Mutex

func trackChild(c *exec.Cmd)   { childMu.Lock(); children[c] = struct{}{}; childMu.Unlock() }
func untrackChild(c *exec.Cmd) { childMu.Lock(); delete(children, c); childMu.Unlock() }

func killChildren() {
	childMu.Lock()
	defer childMu.Unlock()
	for c := range children {
		if c.Process != nil {
			_ = c.Process.Kill()
		}
		delete(children, c)
	}
}

// Run starts the web shell, opens the browser, and serves until ctx is done.
func Run(ctx context.Context, port int) error {
	cleanupStaleResources()

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	live := &presence{}

	// runCtx lets the watchdog trigger a clean shutdown; presence connections
	// also unblock on it.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	mux := http.NewServeMux()
	staticFS, err := fs.Sub(web.Assets, ".")
	if err != nil {
		return err
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/api/run", handleRun)
	// /api/health is a cheap liveness probe the frontend hits before a run or
	// reset, so a dead backend is reported instead of failing silently.
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	// /api/presence is a long-lived SSE connection that every open page holds
	// for its entire lifetime. The shell counts these connections and shuts
	// down only once they are all gone (every tab closed). Because liveness is
	// the connection's existence rather than a timer, a locked screen or
	// hibernation does not trip shutdown: the connection freezes with the
	// machine and is still present on wake.
	mux.HandleFunc("/api/presence", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		release := live.add()
		defer release()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		_, _ = io.WriteString(w, "retry: 2000\n: connected\n\n")
		flusher.Flush()
		// Block until the page closes its tab (TCP FIN cancels the request
		// context) or the shell shuts down.
		select {
		case <-r.Context().Done():
		case <-runCtx.Done():
		}
	})

	srv := &http.Server{Handler: mux}
	go func() {
		<-runCtx.Done()
		killChildren()
		_ = srv.Close()
	}()

	// Watchdog: exit once every page's presence connection is gone for longer
	// than the grace period (all tabs closed). A refresh only drops the count
	// to zero briefly, so it survives. If no page ever connects within
	// startupGrace, exit too (nothing is driving us).
	go func() {
		start := time.Now()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				if live.expired() {
					fmt.Fprintln(os.Stderr, "no open pages; shutting down")
					cancel()
					return
				}
				if !live.everConnected() && time.Since(start) > startupGrace {
					fmt.Fprintln(os.Stderr, "browser never connected; shutting down")
					cancel()
					return
				}
			}
		}
	}()

	url := fmt.Sprintf("http://%s/", ln.Addr().String())
	fmt.Fprintf(os.Stderr, "gRPC Transport Showdown running at %s\n", url)
	openBrowser(url)

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// handleRun spawns the requested engine and streams its NDJSON output as SSE.
func handleRun(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	lang := r.URL.Query().Get("lang")
	payload := clampPayload(r.URL.Query().Get("payload"))
	profile := r.URL.Query().Get("profile")
	if profile != "fair" && profile != "max" {
		profile = "max"
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func(line string) {
		fmt.Fprintf(w, "data: %s\n\n", line)
		flusher.Flush()
	}
	sendErr := func(msg string) {
		send(fmt.Sprintf(`{"type":"error","error":%q}`, msg))
		send(`{"type":"done"}`)
	}

	// Reject overlapping runs: a single machine can only meaningfully benchmark
	// one engine at a time, so a concurrent request is refused rather than
	// allowed to skew both measurements.
	if !runGate.TryLock() {
		sendErr("a benchmark run is already in progress; please wait for it to finish")
		return
	}
	defer runGate.Unlock()

	cmd, err := engineCommand(r.Context(), lang, payload, profile)
	if err != nil {
		sendErr(err.Error())
		return
	}
	// Capture stderr (tail) so a crash or missing runtime can be surfaced to
	// the UI, while still echoing it to the console for local debugging.
	var errTail tailWriter
	cmd.Stderr = io.MultiWriter(os.Stderr, &errTail)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		sendErr(err.Error())
		return
	}
	// Tie the engine's lifetime to this shell so the engine — and, transitively,
	// its own grpc server children — is killed if the shell dies abruptly.
	childguard.Prepare(cmd)
	if err := cmd.Start(); err != nil {
		sendErr(err.Error())
		return
	}
	release, _ := childguard.Guard(cmd)
	defer release()
	trackChild(cmd)
	defer untrackChild(cmd)

	// Kill the engine (and let it kill its server children) if the client
	// disconnects mid-run.
	go func() {
		<-r.Context().Done()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	sawDone := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, `"type":"done"`) {
			sawDone = true
		}
		send(line)
	}
	waitErr := cmd.Wait()
	// If the engine never reported completion (crashed, missing runtime, etc.)
	// surface a clean error instead of leaving the UI hanging.
	if !sawDone {
		msg := "engine exited without producing results"
		if waitErr != nil {
			msg = fmt.Sprintf("engine failed: %v", waitErr)
		}
		if tail := strings.TrimSpace(errTail.String()); tail != "" {
			msg = msg + " — " + tail
		}
		sendErr(msg)
	}
}

// tailWriter keeps the last maxTail bytes written to it, used to capture the
// tail of a child process's stderr for error reporting.
type tailWriter struct {
	buf [4096]byte
	n   int
}

func (t *tailWriter) Write(p []byte) (int, error) {
	if len(p) >= len(t.buf) {
		copy(t.buf[:], p[len(p)-len(t.buf):])
		t.n = len(t.buf)
		return len(p), nil
	}
	if t.n+len(p) > len(t.buf) {
		shift := t.n + len(p) - len(t.buf)
		copy(t.buf[:], t.buf[shift:t.n])
		t.n -= shift
	}
	copy(t.buf[t.n:], p)
	t.n += len(p)
	return len(p), nil
}

func (t *tailWriter) String() string { return string(t.buf[:t.n]) }

// maxPayloadBytes bounds the requested payload size. The upper bound matches the
// largest option offered by the UI (256 MiB); the lower bound prevents a zero or
// negative size from reaching the engine, where it would otherwise allocate a
// degenerate buffer or stall a ping-pong stream.
const maxPayloadBytes = 256 * 1024 * 1024

// clampPayload parses the requested payload size and clamps it to a safe range,
// falling back to the 4 KiB default when the value is missing or unparseable.
func clampPayload(raw string) string {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return "4096"
	}
	if n < 1 {
		n = 1
	}
	if n > maxPayloadBytes {
		n = maxPayloadBytes
	}
	return strconv.Itoa(n)
}

// engineCommand builds the child process command for the requested language.
//
// Each transport is measured benchRounds times with a benchMeasureMs window per
// phase; the engine reports the median across rounds (or the worse of two
// survivors / the lone survivor when rounds time out). Repeating absorbs the
// occasional bad sample on a noisy/thermally-throttled host (notably the ARM
// demo laptop) without inflating total runtime.
//
// Wall-clock budget: warmup is paid only on round 1 (the engine skips it on
// later rounds once the process is hot), so each transport spends roughly
// 2×warmup + benchRounds×2×measure in fixed time windows, plus a few seconds of
// server-start/connect overhead. With the values below that is
// 3 × (2×0.6 + 3×2×1.3) ≈ 27s of windows + overhead ≈ 30s total across the
// three transports.
func engineCommand(ctx context.Context, lang, payload, profile string) (*exec.Cmd, error) {
	const (
		benchWarmupMs  = "600"
		benchMeasureMs = "1300"
		benchRounds    = "3"
	)
	switch lang {
	case "", "go":
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return exec.CommandContext(ctx, self,
			"--role", "engine", "--transport", "all", "--payload", payload,
			"--profile", profile,
			"--warmup-ms", benchWarmupMs, "--measure-ms", benchMeasureMs,
			"--reps", benchRounds), nil
	case "dotnet":
		exe := dotnetEnginePath()
		if exe == "" {
			return nil, fmt.Errorf(".NET engine not bundled in this build")
		}
		return exec.CommandContext(ctx, exe, "--payload", payload, "--profile", profile,
			"--warmup-ms", benchWarmupMs, "--measure-ms", benchMeasureMs,
			"--reps", benchRounds), nil
	default:
		return nil, fmt.Errorf("unknown language %q", lang)
	}
}

// dotnetEnginePath returns the bundled .NET engine executable path, or "" if
// it is not present (the Go-only build).
func dotnetEnginePath() string {
	dir, err := os.Executable()
	if err != nil {
		return ""
	}
	base := filepath.Dir(dir)
	candidates := []string{
		filepath.Join(base, "dotnet-engine", "ShmDemo.Engine.exe"),
		filepath.Join(base, "dotnet-engine", "ShmDemo.Engine"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// cleanupStaleResources removes leftover UDS sockets and shm segment files from
// previous (possibly crashed) runs so a fresh demo starts clean.
func cleanupStaleResources() {
	tmp := os.TempDir()
	for _, pattern := range []string{"shmdemo_*", "*shmdemo*"} {
		matches, _ := filepath.Glob(filepath.Join(tmp, pattern))
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
}

// openBrowser best-effort launches the default browser at url.
func openBrowser(url string) {
	if os.Getenv("SHMDEMO_NO_BROWSER") != "" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
