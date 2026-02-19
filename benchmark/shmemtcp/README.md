# Shared Memory Transport Benchmarks

This directory contains benchmarks comparing the performance of gRPC's shared memory (SHM) transport against traditional TCP loopback and Unix domain sockets.

## Quick Start

### Local Execution

```bash
# Generate plots from cached results (or run benchmarks if no cache exists)
python3 benchmark_runner.py

# Force rerun benchmarks and regenerate plots
python3 benchmark_runner.py --run

# Only regenerate plots from cached results
python3 benchmark_runner.py --plot-only
```

### Cloud Execution (GitHub Actions)

The benchmarks can also be run automatically in the cloud using GitHub Actions:

1. **Manual Trigger**: Go to the "Actions" tab in the GitHub repository, select "Benchmarks" workflow, and click "Run workflow"
2. **Automatic Schedule**: Benchmarks run weekly on Sundays at midnight UTC
3. **Results**: Download benchmark results and plots from the workflow's artifacts section

The cloud benchmarks run on `ubuntu-latest` GitHub-hosted runners and produce the same outputs as local execution.

## Output Files

Outputs are segmented per-platform under `out/<platform>/` (e.g., `out/linux/` and `out/windows/`):

| File | Description |
|------|-------------|
| `benchmark_results.json` | Cached benchmark results in JSON format |
| `benchmark_patterns.png` | Main dashboard showing performance by communication pattern |
| `benchmark_summary.png` | Summary comparison with speedup factors |

## Benchmarks

The benchmark suite measures transport performance across different message sizes (64B to 64KB) and communication patterns.

### Transport Types

| Transport | Description |
|-----------|-------------|
| **SHM** | Shared memory ring buffer with futex synchronization |
| **TCP** | TCP loopback (127.0.0.1) |
| **Unix** | Unix domain socket |

### Benchmark Categories

#### 1. One-Way Streaming (`BenchmarkShmRingWriteRead`, `BenchmarkTCPLoopback`, `BenchmarkUnixSocketLoopback`)

Measures the latency and throughput of unidirectional data transfer:
- Producer writes messages to the transport
- Consumer reads messages from the transport
- Metrics: nanoseconds per operation (ns/op), megabytes per second (MB/s)

**Message Sizes:** 64B, 256B, 1KB, 4KB, 16KB, 64KB

#### 2. Roundtrip / Unary RPC (`BenchmarkShmRingRoundtrip`, `BenchmarkTCPLoopbackRoundtrip`, `BenchmarkUnixSocketRoundtrip`)

Measures request-response latency (ping-pong pattern):
- Client sends a request
- Server echoes the response
- Total roundtrip time measured

**Message Sizes:** 64B, 256B, 1KB, 4KB

### Implementation Details

#### Shared Memory Transport

- **Ring Buffer Size:** 64 MiB per direction
- **Synchronization:** Linux futex for cross-process signaling
- **Memory Layout:** Lock-free SPSC (single-producer, single-consumer) ring buffer
- **Zero-Copy:** Data is written directly to shared memory, avoiding kernel transitions

#### Benchmark Configuration

```go
-bench=BenchmarkShmRingWriteRead|BenchmarkShmRingRoundtrip|BenchmarkTCPLoopback|BenchmarkUnixSocket
-benchtime=500ms
-cpu=2
```

## Results

### Latest Benchmark Results (2026-01-11)

**CPU:** AMD EPYC 7763 64-Core Processor

#### One-Way Streaming Latency (1KB messages)

| Transport | Latency | Throughput | Speedup vs TCP |
|-----------|---------|------------|----------------|
| SHM | 156 ns | 6.6 GB/s | **40x** |
| Unix | 2,265 ns | 452 MB/s | 2.8x |
| TCP | 6,260 ns | 164 MB/s | 1x |

#### Roundtrip Latency (1KB messages)

| Transport | Latency | Speedup vs TCP |
|-----------|---------|----------------|
| SHM | 671 ns | **27x** |
| Unix | 11,044 ns | 1.7x |
| TCP | 18,377 ns | 1x |

#### Peak Throughput (64KB messages)

| Transport | Throughput |
|-----------|------------|
| SHM | **35.9 GB/s** |
| Unix | 4.1 GB/s |
| TCP | 2.7 GB/s |

## Plot Descriptions

### benchmark_patterns.png

A 3x2 grid showing performance organized by communication pattern:

| Row | Pattern | Description |
|-----|---------|-------------|
| 1 | **Unary RPC** | Request-response latency and ops/sec |
| 2 | **Unidirectional Streaming** | One-way latency and throughput |
| 3 | **Bidirectional Streaming** | Estimated bidi performance (2x unidir + 15% overhead) |

Each column shows:
- **Left:** Latency comparison (lower is better)
- **Right:** Throughput comparison (higher is better)

### benchmark_summary.png

A 2x2 summary dashboard:
- Latency comparison at 1KB message size
- Peak throughput comparison
- Speedup factors (SHM vs TCP/Unix)
- Text summary with key results

## Running Individual Benchmarks

To run specific benchmarks manually:

```bash
# All SHM benchmarks
go test -bench=BenchmarkShm -benchtime=1s google.golang.org/grpc/internal/transport

# Roundtrip comparison only
go test -bench=Roundtrip -benchtime=1s google.golang.org/grpc/internal/transport

# With memory allocation stats
go test -bench=BenchmarkShmRingWriteRead -benchmem google.golang.org/grpc/internal/transport
```

## Dependencies

- Python 3.6+
- matplotlib
- numpy

Install dependencies:
```bash
pip install matplotlib numpy
```

## Architecture

```
benchmark/shmemtcp/
├── README.md                     # This file
├── benchmark_runner.py           # Full benchmark runner (runs Go tests + plots)
├── generate_benchmark_plots.py   # Static visualization from cached data
├── main.go                       # Go benchmark harness
└── out/
    └── linux/
        ├── benchmark_results.json          # Cached results from benchmark_runner.py
        ├── benchmark_results.txt           # Raw Go benchmark output
        ├── benchmark_comprehensive.png     # 6-panel comprehensive dashboard
        ├── benchmark_latency_distribution.png  # Latency histograms/CDFs
        ├── benchmark_use_cases.png         # Use case recommendation matrix
        ├── benchmark_patterns.png          # Pattern-based comparison plots
        └── benchmark_summary.png           # Summary dashboard
    └── windows/
        └── ... # Windows-generated artifacts
```

## Scripts

| Script | Purpose | Usage |
|--------|---------|-------|
| `benchmark_runner.py` | Run Go benchmarks and generate plots | `python3 benchmark_runner.py [--run\|--plot-only]` |
| `generate_benchmark_plots.py` | Generate static plots from hardcoded data | `python3 generate_benchmark_plots.py` |

## Interpreting Results

### Why is SHM so much faster?

1. **No Kernel Transitions:** SHM keeps data in user space; TCP/Unix require syscalls for every send/recv
2. **No Data Copying:** SHM uses zero-copy semantics; kernel sockets copy data between user and kernel buffers
3. **Efficient Synchronization:** Futex provides low-overhead cross-process signaling
4. **Cache Locality:** Shared memory stays in CPU cache; network stack has poor cache behavior

### Caveats

- Benchmarks run on a single machine (localhost)
- SHM only works for same-machine communication
- Results may vary based on CPU, memory speed, and system load
- Bidirectional streaming numbers are estimated (2x unidirectional + 15% overhead)
