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

// ShmDemo.Engine is the .NET benchmark engine for the gRPC Transport Showdown
// demo. It mirrors the Go engine one-to-one: for each transport (tcp, uds,
// shm) it spawns the gRPC server as a child process, drives a single-stream
// latency (ping-pong) and throughput (bounded-in-flight ping-pong) measurement, samples the
// CPU of both processes, and emits the same NDJSON event contract to stdout.
// Human-readable logs go to stderr so stdout carries only NDJSON.
//
// The application code is identical across transports — only the channel /
// server construction differs (see StartEnv / RunServerModeAsync). That is the
// whole point of the demo.

using System.Diagnostics;
using System.Globalization;
using System.Net;
using System.Net.Sockets;
using System.Text.Json;
using System.Text.Json.Serialization;
using Google.Protobuf;
using Grpc.Core;
using Grpc.Net.Client;
using Grpc.Net.SharedMemory;
using Grpc.Testing;
using Microsoft.AspNetCore.Builder;
using Microsoft.AspNetCore.Hosting;
using Microsoft.AspNetCore.Server.Kestrel.Core;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Logging;

// ---------------------------------------------------------------------------
// Argument parsing
// ---------------------------------------------------------------------------
bool serverMode = false;
string transport = "";
int payload = 4096;
int warmupMs = 600;
int measureMs = 2500;
int reps = 1;
int port = 0;
string? segment = null;
string? udsPath = null;
int parentPid = 0;
string profile = "max";
var transports = new List<string>();

for (int i = 0; i < args.Length; i++)
{
    switch (args[i])
    {
        case "--server": serverMode = true; break;
        case "--transport": transport = args[++i]; break;
        case "--transports": transports.AddRange(args[++i].Split(',', StringSplitOptions.RemoveEmptyEntries | StringSplitOptions.TrimEntries)); break;
        case "--payload": payload = int.Parse(args[++i], CultureInfo.InvariantCulture); break;
        case "--warmup-ms": warmupMs = int.Parse(args[++i], CultureInfo.InvariantCulture); break;
        case "--measure-ms": measureMs = int.Parse(args[++i], CultureInfo.InvariantCulture); break;
        case "--reps": reps = int.Parse(args[++i], CultureInfo.InvariantCulture); break;
        case "--port": port = int.Parse(args[++i], CultureInfo.InvariantCulture); break;
        case "--segment": segment = args[++i]; break;
        case "--uds-path": udsPath = args[++i]; break;
        case "--parent-pid": parentPid = int.Parse(args[++i], CultureInfo.InvariantCulture); break;
        case "--profile": profile = args[++i]; break;
    }
}

// Flow-control profile. "fair" pins every transport to the HTTP/2 spec
// defaults (65535-byte window, 16384-byte frame) for an apples-to-apples
// run; "max" leaves the SHM library at its high-performance local-IPC
// defaults (large window/frame). The SHM window/frame are read by the
// SHM library from these env vars, so they MUST be set here — before any
// SHM channel or server is constructed — in BOTH the engine and the
// spawned server child. Where a knob is not settable (TCP/UDS frame size
// is fixed at 16 KiB), it is simply left alone.
profile = profile is "fair" or "max" ? profile : "max";
Engine.Profile = profile;
if (profile == "fair")
{
    Environment.SetEnvironmentVariable("SHM_INITIAL_WINDOW", "65535");
    Environment.SetEnvironmentVariable("SHM_FAIR_MAX_FRAME", "16384");
}

ThreadPool.SetMinThreads(64, 64);

if (serverMode)
{
    await Engine.RunServerModeAsync(transport, port, segment, udsPath, parentPid).ConfigureAwait(false);
    return;
}

if (transports.Count == 0)
{
    transports.AddRange(new[] { "tcp", "uds", "shm" });
}

await Engine.RunAsync(payload,
    TimeSpan.FromMilliseconds(warmupMs),
    TimeSpan.FromMilliseconds(measureMs),
    reps < 1 ? 1 : reps,
    transports).ConfigureAwait(false);

// ===========================================================================
// Engine
// ===========================================================================
static class Engine
{
    // Maximum gRPC message size. Kept well above the largest payload option
    // (256 MiB) so big messages flow on every transport. Note the SHM ring is
    // intentionally only 64 MiB — payloads larger than the ring stream through
    // in frames, which is exactly the "larger-than-ring" case worth demoing.
    private const int MaxMsgBytes = 512 * 1024 * 1024;

    // Profile is the flow-control profile ("fair" | "max") set from the
    // command line. It drives the HTTP/2 window used by the Kestrel
    // TCP/UDS servers; the SHM window/frame are carried via env vars set
    // in Main before any SHM transport is constructed.
    public static string Profile = "fair";

    // H2Window is the HTTP/2 connection/stream flow-control window for the
    // current profile: the spec default under "fair", opened wide (64 MiB,
    // matching the SHM ring) under "max".
    static int H2Window => Profile == "fair" ? 65535 : 64 * 1024 * 1024;

    // RunAsync drives every transport and emits NDJSON to stdout.
    public static async Task RunAsync(int payload, TimeSpan warmup, TimeSpan measure, int reps, IReadOnlyList<string> transports)
    {
        // Clean any stale SHM segments left by a crashed run.
        try { Segment.TryRemoveSegmentsByPrefix("shmdemo_dn_"); } catch { }

        foreach (var t in transports)
        {
            try
            {
                var res = await RunOneAsync(t, payload, warmup, measure, reps).ConfigureAwait(false);
                res.Type = "result";
                res.Transport = t;
                res.PayloadBytes = payload;
                Emit(res);
            }
            catch (Exception ex)
            {
                Emit(new Event { Type = "error", Transport = t, PayloadBytes = payload, Error = ex.Message });
            }
        }

        Emit(new Event { Type = "done" });
    }

    static async Task<Event> RunOneAsync(string t, int payload, TimeSpan warmup, TimeSpan measure, int reps)
    {
        Emit(new Event { Type = "progress", Transport = t, Phase = "connect" });

        await using var env = await StartEnv(t).ConfigureAwait(false);
        var client = env.Client;

        // Warm the connection + JIT before measuring. Scale the warmup call
        // count down for large payloads so a 256 MiB run doesn't move tens of
        // GiB before measuring.
        var warmReq = NewRequest(payload, payload);
        int warmCalls = Math.Clamp(16 * 1024 * 1024 / Math.Max(payload, 1), 3, 50);
        for (int i = 0; i < warmCalls; i++)
        {
            await client.UnaryCallAsync(warmReq).ResponseAsync.ConfigureAwait(false);
        }

        // Measure latency + throughput `reps` times and combine the rounds per
        // metric. Repeating guards against the occasional bad sample on a
        // noisy/thermally-throttled host (notably ARM laptops): one unlucky
        // round cannot drag the headline number. The server and client are
        // reused across rounds — only the measurement windows repeat — so extra
        // rounds cost measurement time, not setup.
        //
        // Per-phase warmup is paid only on the FIRST round. The connection,
        // JIT, and OS buffers are warmed once per transport; by rounds 2 and 3
        // the process is already hot, so repeating the warmup would just burn
        // wall-clock (warmup x 2 phases x every round) without changing the
        // numbers. Skipping it on later rounds keeps the 3-round total near a
        // single long run instead of 3x as long.
        //
        // Each phase is given a generous per-call deadline (4x the measurement
        // window): a single 256 MiB echo round-trip is itself bandwidth-bound
        // and can take a second or more, so the deadline only fires on a genuine
        // hang. A round that exceeds the deadline is dropped, not fatal — only
        // if every round times out is the transport reported as failed.
        var callTimeout = TimeSpan.FromMilliseconds(measure.TotalMilliseconds * 4);
        var latP50 = new List<double>();
        var latP99 = new List<double>();
        var msgPS = new List<double>();
        var mbPS = new List<double>();
        var cpuP1M = new List<double>();
        for (int rep = 1; rep <= reps; rep++)
        {
            var warm = rep > 1 ? TimeSpan.Zero : warmup; // already hot after round 1

            // Latency phase: single bidi stream ping-pong, one in-flight message.
            Emit(new Event { Type = "progress", Transport = t, Phase = "latency", Round = rep, Rounds = reps });
            double p50Us = 0, p99Us = 0;
            using (var latCts = new CancellationTokenSource(callTimeout))
            {
                try { (p50Us, p99Us) = await RunLatency(client, payload, warm, measure, latCts.Token).ConfigureAwait(false); }
                catch (Exception) when (latCts.IsCancellationRequested) { continue; } // round hung: drop it
            }

            // Throughput phase: bounded-in-flight ping-pong (response_size==payload => echo).
            Emit(new Event { Type = "progress", Transport = t, Phase = "throughput", Round = rep, Rounds = reps });
            double msgPerSec = 0, mbPerSec = 0, cpuBeforeUs = 0, cpuAfterUs = 0;
            long msgs = 0;
            using (var tpCts = new CancellationTokenSource(callTimeout))
            {
                cpuBeforeUs = CaptureCpu(env.ServerProcess);
                try { (msgPerSec, mbPerSec, msgs) = await RunThroughput(client, payload, warm, measure, tpCts.Token).ConfigureAwait(false); }
                catch (Exception) when (tpCts.IsCancellationRequested) { continue; } // round hung: drop the whole round
                cpuAfterUs = CaptureCpu(env.ServerProcess);
            }

            latP50.Add(p50Us);
            latP99.Add(p99Us);
            msgPS.Add(msgPerSec);
            mbPS.Add(mbPerSec);
            cpuP1M.Add(msgs > 0 ? (cpuAfterUs - cpuBeforeUs) / msgs : 0);
        }

        if (latP50.Count == 0)
            throw new TimeoutException($"all {reps} round(s) timed out");

        return new Event
        {
            LatencyP50Us = Combine(latP50, true),   // lower is better
            LatencyP99Us = Combine(latP99, true),   // lower is better
            MsgPerSec = Combine(msgPS, false),       // higher is better
            MBPerSec = Combine(mbPS, false),         // higher is better
            CpuSecPer1M = Combine(cpuP1M, true),     // lower is better
        };
    }

    // Combine reduces the surviving per-round samples of one metric to the
    // single reported value. With three samples it returns the median (middle);
    // with two it returns the worse (more conservative) of the pair so a lucky
    // round cannot flatter the result; with one it returns that lone sample.
    // worseIsMax selects the direction of "worse": true for lower-is-better
    // metrics (latency, CPU cost), false for higher-is-better (throughput).
    static double Combine(List<double> vs, bool worseIsMax)
    {
        switch (vs.Count)
        {
            case 0: return 0;
            case 1: return vs[0];
            case 2: return worseIsMax ? Math.Max(vs[0], vs[1]) : Math.Min(vs[0], vs[1]);
            default:
                var c = new List<double>(vs);
                c.Sort();
                return c[(c.Count - 1) / 2];
        }
    }

    // RunLatency measures round-trip latency on a single bidi stream using
    // ping-pong with exactly one in-flight message.
    static async Task<(double p50Us, double p99Us)> RunLatency(
        BenchmarkService.BenchmarkServiceClient client, int payload, TimeSpan warmup, TimeSpan measure, CancellationToken ct)
    {
        var req = NewRequest(payload, payload); // response_size>0 => echo
        using var call = client.StreamingCall(cancellationToken: ct);
        var reqStream = call.RequestStream;
        var respStream = call.ResponseStream;

        async Task PingPong()
        {
            await reqStream.WriteAsync(req).ConfigureAwait(false);
            if (!await respStream.MoveNext(ct).ConfigureAwait(false))
                throw new InvalidOperationException("stream closed during latency ping-pong");
        }

        var warmDeadline = Stopwatch.GetTimestamp() + (long)(warmup.TotalSeconds * Stopwatch.Frequency);
        while (Stopwatch.GetTimestamp() < warmDeadline)
            await PingPong().ConfigureAwait(false);

        var samples = new List<double>(1 << 16);
        double usPerTick = 1_000_000.0 / Stopwatch.Frequency;
        var measDeadline = Stopwatch.GetTimestamp() + (long)(measure.TotalSeconds * Stopwatch.Frequency);
        while (Stopwatch.GetTimestamp() < measDeadline)
        {
            long t0 = Stopwatch.GetTimestamp();
            await PingPong().ConfigureAwait(false);
            samples.Add((Stopwatch.GetTimestamp() - t0) * usPerTick);
        }

        await reqStream.CompleteAsync().ConfigureAwait(false);
        try { while (await respStream.MoveNext(CancellationToken.None).ConfigureAwait(false)) { } } catch { }

        return (Percentile(samples, 50), Percentile(samples, 99));
    }

    // RunThroughput measures streaming throughput on a single bidi stream using
    // bounded-in-flight ping-pong: send one message, await its echo, repeat.
    // Keeping exactly one message in flight means the workload always stays
    // within the flow-control window for every transport and profile — an
    // unbounded one-way blast would overrun a small fair window. This matches
    // the published SHM benchmark suite, so TCP, UDS, and SHM are compared on
    // equal footing. Bytes are counted in both directions (request + echoed
    // response), matching the benchmark's throughput formula.
    static async Task<(double msgPerSec, double mbPerSec, long msgs)> RunThroughput(
        BenchmarkService.BenchmarkServiceClient client, int payload, TimeSpan warmup, TimeSpan measure, CancellationToken ct)
    {
        var req = NewRequest(payload, payload); // response_size>0 => echo (one in-flight)
        using var call = client.StreamingCall(cancellationToken: ct);
        var reqStream = call.RequestStream;
        var respStream = call.ResponseStream;

        var warmDeadline = Stopwatch.GetTimestamp() + (long)(warmup.TotalSeconds * Stopwatch.Frequency);
        while (Stopwatch.GetTimestamp() < warmDeadline)
        {
            await reqStream.WriteAsync(req).ConfigureAwait(false);
            if (!await respStream.MoveNext(ct).ConfigureAwait(false)) break;
        }

        long msgs = 0;
        long start = Stopwatch.GetTimestamp();
        long measDeadline = start + (long)(measure.TotalSeconds * Stopwatch.Frequency);
        while (Stopwatch.GetTimestamp() < measDeadline)
        {
            await reqStream.WriteAsync(req).ConfigureAwait(false);
            if (!await respStream.MoveNext(ct).ConfigureAwait(false)) break;
            msgs++;
        }
        double secs = (Stopwatch.GetTimestamp() - start) / (double)Stopwatch.Frequency;

        await reqStream.CompleteAsync().ConfigureAwait(false);
        try { while (await respStream.MoveNext(CancellationToken.None).ConfigureAwait(false)) { } } catch { }

        double msgPerSec = secs > 0 ? msgs / secs : 0;
        // Count both directions (request + echoed response), matching the
        // benchmark suite's throughput definition.
        double mbPerSec = secs > 0 ? msgs * (double)payload * 2 / (1024 * 1024) / secs : 0;
        return (msgPerSec, mbPerSec, msgs);
    }

    static double Percentile(List<double> samples, double p)
    {
        if (samples.Count == 0) return 0;
        samples.Sort();
        int idx = (int)((samples.Count - 1) * p / 100.0);
        if (idx < 0) idx = 0;
        if (idx >= samples.Count) idx = samples.Count - 1;
        return samples[idx];
    }

    // CaptureCpu returns the combined CPU time (microseconds) of this process
    // plus the server child, sampled around the throughput window.
    static double CaptureCpu(Process? server)
    {
        double clientUs = Process.GetCurrentProcess().TotalProcessorTime.TotalMicroseconds;
        double serverUs = 0;
        if (server != null && !server.HasExited)
        {
            try { server.Refresh(); serverUs = server.TotalProcessorTime.TotalMicroseconds; }
            catch { }
        }
        return clientUs + serverUs;
    }

    static SimpleRequest NewRequest(int payload, int responseSize) => new()
    {
        ResponseSize = responseSize,
        Payload = payload > 0 ? new Payload { Body = UnsafeByteOperations.UnsafeWrap(new byte[payload]) } : new Payload(),
    };

    // -----------------------------------------------------------------------
    // NDJSON emission
    // -----------------------------------------------------------------------
    static readonly JsonSerializerOptions JsonOpts = new() { DefaultIgnoreCondition = JsonIgnoreCondition.Never };
    static readonly object EmitLock = new();

    static void Emit(Event ev)
    {
        ev.Lang = "dotnet";
        string line = JsonSerializer.Serialize(ev, JsonOpts);
        lock (EmitLock)
        {
            Console.Out.WriteLine(line);
            Console.Out.Flush();
        }
    }

    // =======================================================================
    // Environment setup (client + server child) per transport
    // =======================================================================
    sealed class BenchEnv : IAsyncDisposable
    {
        public required BenchmarkService.BenchmarkServiceClient Client { get; init; }
        public required GrpcChannel Channel { get; init; }
        public Process? ServerProcess { get; init; }
        public required Func<Task> Cleanup { get; init; }
        public async ValueTask DisposeAsync() => await Cleanup().ConfigureAwait(false);
    }

    static async Task<BenchEnv> StartEnv(string t) => t switch
    {
        "tcp" => await StartTcpEnv().ConfigureAwait(false),
        "uds" => await StartUdsEnv().ConfigureAwait(false),
        "shm" => await StartShmEnv().ConfigureAwait(false),
        _ => throw new InvalidOperationException($"unknown transport '{t}'"),
    };

    static async Task<BenchEnv> StartTcpEnv()
    {
        int p = GetAvailablePort();
        var server = StartServerProcess("tcp", port: p);
        try
        {
            var channel = GrpcChannel.ForAddress($"http://127.0.0.1:{p}", new GrpcChannelOptions
            {
                MaxReceiveMessageSize = MaxMsgBytes,
                MaxSendMessageSize = MaxMsgBytes,
            });
            var client = new BenchmarkService.BenchmarkServiceClient(channel);
            await WaitForServerReadyAsync(client, TimeSpan.FromSeconds(20)).ConfigureAwait(false);
            return new BenchEnv
            {
                Client = client,
                Channel = channel,
                ServerProcess = server,
                Cleanup = async () => { channel.Dispose(); await StopServerAsync(server).ConfigureAwait(false); },
            };
        }
        catch { await StopServerAsync(server).ConfigureAwait(false); throw; }
    }

    static async Task<BenchEnv> StartUdsEnv()
    {
        string sock = Path.Combine(Path.GetTempPath(), $"shmdemo_dn_{Environment.ProcessId}_{Guid.NewGuid():N}.sock");
        try { if (File.Exists(sock)) File.Delete(sock); } catch { }
        var server = StartServerProcess("uds", udsPath: sock);
        try
        {
            var deadline = DateTime.UtcNow + TimeSpan.FromSeconds(15);
            while (!File.Exists(sock) && DateTime.UtcNow < deadline)
                await Task.Delay(20).ConfigureAwait(false);

            var handler = new SocketsHttpHandler
            {
                ConnectCallback = async (ctx, ct) =>
                {
                    var s = new Socket(AddressFamily.Unix, SocketType.Stream, ProtocolType.Unspecified);
                    await s.ConnectAsync(new UnixDomainSocketEndPoint(sock), ct).ConfigureAwait(false);
                    return new NetworkStream(s, ownsSocket: true);
                },
            };
            var channel = GrpcChannel.ForAddress("http://localhost", new GrpcChannelOptions
            {
                HttpHandler = handler,
                DisposeHttpClient = true,
                MaxReceiveMessageSize = MaxMsgBytes,
                MaxSendMessageSize = MaxMsgBytes,
            });
            var client = new BenchmarkService.BenchmarkServiceClient(channel);
            await WaitForServerReadyAsync(client, TimeSpan.FromSeconds(20)).ConfigureAwait(false);
            return new BenchEnv
            {
                Client = client,
                Channel = channel,
                ServerProcess = server,
                Cleanup = async () =>
                {
                    channel.Dispose();
                    await StopServerAsync(server).ConfigureAwait(false);
                    try { if (File.Exists(sock)) File.Delete(sock); } catch { }
                },
            };
        }
        catch
        {
            await StopServerAsync(server).ConfigureAwait(false);
            try { if (File.Exists(sock)) File.Delete(sock); } catch { }
            throw;
        }
    }

    static async Task<BenchEnv> StartShmEnv()
    {
        string seg = $"shmdemo_dn_{Environment.ProcessId}_{Guid.NewGuid():N}";
        Segment.TryRemoveSegment(seg);
        Segment.TryRemoveSegment(seg + "_ctl");
        var server = StartServerProcess("shm", segmentName: seg);
        try
        {
            var channel = GrpcChannel.ForAddress("http://localhost", new GrpcChannelOptions
            {
                HttpHandler = new ShmControlHandler(seg, new ShmClientTransportOptions
                {
                    SingleStreamMode = true,
                    // 2026-06-01: now safe — ShmReaderThreadContext +
                    // WouldBlockSendQuota pre-flight in SendMessageAsync
                    // hop off the reader thread when a flow-controlled
                    // outbound would deadlock. Saves the ~17µs Windows
                    // ThreadPool wake hop per RX frame in single-stream
                    // pingpong (where _bypassStriper=true would otherwise
                    // force async-continuations on the inbound channel).
                    InlineReceiveContinuations = true,
                }),
                DisposeHttpClient = true,
                MaxReceiveMessageSize = MaxMsgBytes,
                MaxSendMessageSize = MaxMsgBytes,
            });
            var client = new BenchmarkService.BenchmarkServiceClient(channel);
            await WaitForServerReadyAsync(client, TimeSpan.FromSeconds(25)).ConfigureAwait(false);
            return new BenchEnv
            {
                Client = client,
                Channel = channel,
                ServerProcess = server,
                Cleanup = async () =>
                {
                    channel.Dispose();
                    await StopServerAsync(server).ConfigureAwait(false);
                    Segment.TryRemoveSegment(seg);
                    Segment.TryRemoveSegment(seg + "_ctl");
                },
            };
        }
        catch
        {
            await StopServerAsync(server).ConfigureAwait(false);
            Segment.TryRemoveSegment(seg);
            Segment.TryRemoveSegment(seg + "_ctl");
            throw;
        }
    }

    static int GetAvailablePort()
    {
        using var l = new TcpListener(IPAddress.Loopback, 0);
        l.Start();
        return ((IPEndPoint)l.LocalEndpoint).Port;
    }

    // StartServerProcess spawns this same executable in --server mode so SHM is
    // exercised across two processes, exactly like the Go engine.
    static Process StartServerProcess(string t, int? port = null, string? segmentName = null, string? udsPath = null)
    {
        var (fileName, prefixArgs) = HostInvocation();
        var psi = new ProcessStartInfo
        {
            FileName = fileName,
            UseShellExecute = false,
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            CreateNoWindow = true,
        };
        foreach (var a in prefixArgs) psi.ArgumentList.Add(a);
        psi.ArgumentList.Add("--server");
        psi.ArgumentList.Add("--transport");
        psi.ArgumentList.Add(t);
        psi.ArgumentList.Add("--profile");
        psi.ArgumentList.Add(Profile);
        psi.ArgumentList.Add("--parent-pid");
        psi.ArgumentList.Add(Environment.ProcessId.ToString(CultureInfo.InvariantCulture));
        if (port.HasValue) { psi.ArgumentList.Add("--port"); psi.ArgumentList.Add(port.Value.ToString(CultureInfo.InvariantCulture)); }
        if (!string.IsNullOrWhiteSpace(segmentName)) { psi.ArgumentList.Add("--segment"); psi.ArgumentList.Add(segmentName!); }
        if (!string.IsNullOrWhiteSpace(udsPath)) { psi.ArgumentList.Add("--uds-path"); psi.ArgumentList.Add(udsPath!); }

        var proc = new Process { StartInfo = psi, EnableRaisingEvents = true };
        // Forward child output to our stderr so stdout carries only NDJSON.
        proc.OutputDataReceived += (_, e) => { if (!string.IsNullOrWhiteSpace(e.Data)) Console.Error.WriteLine($"[srv] {e.Data}"); };
        proc.ErrorDataReceived += (_, e) => { if (!string.IsNullOrWhiteSpace(e.Data)) Console.Error.WriteLine($"[srv] {e.Data}"); };
        if (!proc.Start()) throw new InvalidOperationException($"failed to start {t} server");
        proc.BeginOutputReadLine();
        proc.BeginErrorReadLine();
        return proc;
    }

    static (string fileName, IReadOnlyList<string> prefixArgs) HostInvocation()
    {
        string exe = Environment.ProcessPath ?? "dotnet";
        string name = Path.GetFileNameWithoutExtension(exe);
        if (name.Equals("dotnet", StringComparison.OrdinalIgnoreCase))
        {
            string dll = System.Reflection.Assembly.GetEntryAssembly()!.Location;
            return (exe, new[] { dll });
        }
        return (exe, Array.Empty<string>());
    }

    static async Task StopServerAsync(Process proc)
    {
        try
        {
            if (!proc.HasExited)
            {
                proc.Kill(entireProcessTree: true);
                await proc.WaitForExitAsync().ConfigureAwait(false);
            }
        }
        catch { }
        finally { proc.Dispose(); }
    }

    static async Task WaitForServerReadyAsync(BenchmarkService.BenchmarkServiceClient client, TimeSpan timeout)
    {
        var sw = Stopwatch.StartNew();
        Exception? last = null;
        while (sw.Elapsed < timeout)
        {
            using var cts = new CancellationTokenSource(TimeSpan.FromSeconds(2));
            try
            {
                using var call = client.UnaryCallAsync(new SimpleRequest { ResponseSize = 0 }, cancellationToken: cts.Token);
                await call.ResponseAsync.ConfigureAwait(false);
                return;
            }
            catch (Exception ex)
            {
                last = ex;
                await Task.Delay(150).ConfigureAwait(false);
            }
        }
        throw new TimeoutException($"server not ready within {timeout.TotalSeconds:F0}s", last);
    }

    // =======================================================================
    // Server mode (child process)
    // =======================================================================
    public static async Task RunServerModeAsync(string transport, int port, string? segmentName, string? udsPath, int parentPid)
    {
        using var cts = new CancellationTokenSource();
        Console.CancelKeyPress += (_, e) => { e.Cancel = true; cts.Cancel(); };
        WatchParent(parentPid, cts);

        if (transport.Equals("tcp", StringComparison.OrdinalIgnoreCase))
        {
            await RunKestrelServerAsync(k => k.Listen(IPAddress.Loopback, port, lo => lo.Protocols = HttpProtocols.Http2),
                $"TCP 127.0.0.1:{port}", cts.Token).ConfigureAwait(false);
            return;
        }
        if (transport.Equals("uds", StringComparison.OrdinalIgnoreCase))
        {
            try { if (File.Exists(udsPath)) File.Delete(udsPath); } catch { }
            await RunKestrelServerAsync(k => k.ListenUnixSocket(udsPath!, lo => lo.Protocols = HttpProtocols.Http2),
                $"UDS {udsPath}", cts.Token).ConfigureAwait(false);
            try { if (File.Exists(udsPath)) File.Delete(udsPath); } catch { }
            return;
        }
        if (!transport.Equals("shm", StringComparison.OrdinalIgnoreCase))
            throw new InvalidOperationException($"unknown transport '{transport}'");

        Segment.TryRemoveSegment(segmentName!);
        Segment.TryRemoveSegment(segmentName + "_ctl");
        var server = new ShmGrpcServer(segmentName!, ringCapacity: 64UL * 1024 * 1024, singleStreamMode: true,
            // 2026-06-01: safe inline-RX (see client-side comment above).
            pooledDeserialization: true, maxReceiveMessageSize: 0, inlineReceiveContinuations: true);
        server.MapUnary<SimpleRequest, SimpleResponse>(
            "/grpc.testing.BenchmarkService/UnaryCall",
            (req, _) => Task.FromResult(new SimpleResponse { Payload = MakePayload(req.ResponseSize) }));
        server.MapDuplexStreaming<SimpleRequest, SimpleResponse>(
            "/grpc.testing.BenchmarkService/StreamingCall",
            async (reader, writer, ctx) =>
            {
                while (await reader.MoveNext(ctx.CancellationToken).ConfigureAwait(false))
                {
                    var req = reader.Current;
                    // response_size>0 => echo (latency); ==0 => drain (throughput).
                    if (req.ResponseSize > 0)
                        await writer.WriteAsync(new SimpleResponse { Payload = MakePayload(req.ResponseSize) }).ConfigureAwait(false);
                }
            });
        Console.Error.WriteLine($"[SERVER] SHM ready on segment {segmentName}");
        try { await server.RunAsync(cts.Token).ConfigureAwait(false); }
        catch (OperationCanceledException) { }
        finally
        {
            server.Shutdown();
            await server.DisposeAsync().ConfigureAwait(false);
            Segment.TryRemoveSegment(segmentName!);
            Segment.TryRemoveSegment(segmentName + "_ctl");
        }
    }

    static async Task RunKestrelServerAsync(Action<KestrelServerOptions> listen, string label, CancellationToken ct)
    {
        var builder = WebApplication.CreateBuilder(Array.Empty<string>());
        builder.Logging.ClearProviders();
        builder.Services.AddGrpc(o =>
        {
            o.MaxReceiveMessageSize = MaxMsgBytes;
            o.MaxSendMessageSize = MaxMsgBytes;
        });
        builder.WebHost.ConfigureKestrel(k =>
        {
            listen(k);
            k.Limits.MaxRequestBodySize = MaxMsgBytes;
            k.Limits.Http2.InitialConnectionWindowSize = H2Window;
            k.Limits.Http2.InitialStreamWindowSize = H2Window;
        });
        var app = builder.Build();
        app.MapGrpcService<BenchmarkServiceImpl>();
        await app.StartAsync(ct).ConfigureAwait(false);
        Console.Error.WriteLine($"[SERVER] {label} ready");
        try { await app.WaitForShutdownAsync(ct).ConfigureAwait(false); }
        catch (OperationCanceledException) { }
        await app.StopAsync().ConfigureAwait(false);
        await app.DisposeAsync().ConfigureAwait(false);
    }

    static void WatchParent(int parentPid, CancellationTokenSource cts)
    {
        if (parentPid <= 0) return;
        _ = Task.Run(async () =>
        {
            while (!cts.IsCancellationRequested)
            {
                try
                {
                    using var parent = Process.GetProcessById(parentPid);
                    if (parent.HasExited) { cts.Cancel(); return; }
                }
                catch { cts.Cancel(); return; }
                try { await Task.Delay(500, cts.Token).ConfigureAwait(false); }
                catch (OperationCanceledException) { return; }
            }
        });
    }

    static readonly System.Collections.Concurrent.ConcurrentDictionary<int, byte[]> PayloadCache = new();
    internal static Payload MakePayload(int size)
    {
        if (size <= 0) return new Payload();
        var bytes = PayloadCache.GetOrAdd(size, s => new byte[s]);
        return new Payload { Body = UnsafeByteOperations.UnsafeWrap(bytes) };
    }
}

// ===========================================================================
// Kestrel gRPC service (tcp + uds). Mirrors the SHM handler: echo when
// response_size>0, drain when response_size==0.
// ===========================================================================
sealed class BenchmarkServiceImpl : BenchmarkService.BenchmarkServiceBase
{
    public override Task<SimpleResponse> UnaryCall(SimpleRequest request, ServerCallContext context)
        => Task.FromResult(new SimpleResponse { Payload = Engine.MakePayload(request.ResponseSize) });

    public override async Task StreamingCall(
        IAsyncStreamReader<SimpleRequest> requestStream,
        IServerStreamWriter<SimpleResponse> responseStream,
        ServerCallContext context)
    {
        while (await requestStream.MoveNext(context.CancellationToken).ConfigureAwait(false))
        {
            var req = requestStream.Current;
            if (req.ResponseSize > 0)
                await responseStream.WriteAsync(new SimpleResponse { Payload = Engine.MakePayload(req.ResponseSize) }).ConfigureAwait(false);
            // response_size==0 => one-way blast: drain without replying.
        }
    }
}

// ===========================================================================
// NDJSON event contract — identical field names/semantics to the Go engine
// (internal/protocol/event.go).
// ===========================================================================
sealed class Event
{
    [JsonPropertyName("type")]
    public string Type { get; set; } = "";

    [JsonPropertyName("lang")]
    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)]
    public string? Lang { get; set; }

    [JsonPropertyName("transport")]
    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)]
    public string? Transport { get; set; }

    [JsonPropertyName("phase")]
    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)]
    public string? Phase { get; set; }

    [JsonPropertyName("round")]
    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingDefault)]
    public int Round { get; set; }

    [JsonPropertyName("rounds")]
    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingDefault)]
    public int Rounds { get; set; }

    [JsonPropertyName("payloadBytes")]
    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingDefault)]
    public int PayloadBytes { get; set; }

    [JsonPropertyName("latencyP50Us")]
    public double LatencyP50Us { get; set; }

    [JsonPropertyName("latencyP99Us")]
    public double LatencyP99Us { get; set; }

    [JsonPropertyName("msgPerSec")]
    public double MsgPerSec { get; set; }

    [JsonPropertyName("mbPerSec")]
    public double MBPerSec { get; set; }

    [JsonPropertyName("cpuSecPer1M")]
    public double CpuSecPer1M { get; set; }

    [JsonPropertyName("error")]
    [JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)]
    public string? Error { get; set; }
}
