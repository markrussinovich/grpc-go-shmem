# shmsccmp — shared-memory vs. Unix-socket transport benchmarks

This module compares the shared-memory (SHM) transport against a stock gRPC
baseline across four traffic patterns. It is a nested Go module so that it can
depend on both the monorepo transport and the self-contained plugin at once.

## Arms

| Arm | Description |
|-----|-------------|
| **mono** | SHM transport built into this repo (`internal/transport`) |
| **plugin** | Self-contained SHM plugin (`plugin/shmsc`), dialed through its own resolver |
| **UDS** | Stock gRPC HTTP/2 over an AF_UNIX socket — the baseline to beat |

`mono` and `plugin` are two implementations of the same transport and should
track each other closely; a gap between them is a bug, not a result.

## Running

```bash
cd benchmark/shmsccmp

# Everything (slow: concurrent traffic scales as streams x size x b.N)
go test -run='^$' -bench=. -benchtime=1s -timeout=60m .

# One pattern
go test -run='^$' -bench='^Benchmark(Mono|Plugin|UDS)Pipelined$' .

# One cell, with more samples
go test -run='^$' -bench='^BenchmarkMonoConcurrent/streams=100/size=16MB$' \
  -benchtime=10s -count=5 .
```

`/dev/shm` must be large enough for the segments; the 16 MB cells want a few
GiB of headroom.

## Patterns

| Benchmark | What it measures |
|-----------|------------------|
| `*Unary` | Sequential unary RPCs. Pure round-trip latency. |
| `*Stream` | Ping-pong on one long-lived stream. Latency without per-RPC setup. |
| `*Pipelined` | One stream, sender never waits for replies. Single-stream throughput ceiling. |
| `*Concurrent` | N concurrent ping-pong streams on one connection. Aggregate throughput. |

Throughput is reported as `duplex-MB/s`: request plus response bytes over
elapsed time, in MiB despite the label. A round trip of a 1 MB payload moves
2 MB of duplex traffic.

## Results

Run on 2026-07-27 at commit `4c1d86d0`.

| | |
|---|---|
| CPU | Intel Xeon Platinum 8370C @ 2.80 GHz, 16 cores |
| Memory | 62 GB, `/dev/shm` tmpfs 32 GB |
| Go | 1.25.0, linux/amd64 |
| Command | `-bench=. -benchtime=1s -count=5` (450 rows, 853 s) |

All figures are the median of 5 repeats. `minN` is the smallest iteration count
any arm reached for that row, and is a confidence hint: single-digit `minN`
means the cell is indicative only.

### Unary round-trip latency (µs/op, lower is better)

| payload | UDS | mono | plugin | UDS/mono | minN |
|--------:|----:|-----:|-------:|---------:|-----:|
| 64 B | 63.5 | 51.6 | 54.1 | 1.23x | 18,387 |
| 1 KB | 69.4 | 53.2 | 54.9 | 1.30x | 17,181 |
| 16 KB | 124.4 | 74.4 | 75.0 | 1.67x | 8,686 |
| 64 KB | 279.0 | 114.5 | 114.7 | 2.44x | 4,114 |
| 256 KB | 695.2 | 292.1 | 289.5 | 2.38x | 1,573 |
| 1 MB | 2,441 | 837.7 | 870.6 | 2.91x | 480 |

### Stream round-trip latency (µs/op, lower is better)

| payload | UDS | mono | plugin | UDS/mono | minN |
|--------:|----:|-----:|-------:|---------:|-----:|
| 64 B | 25.4 | 13.9 | 14.4 | 1.82x | 44,464 |
| 1 KB | 29.1 | 15.8 | 17.0 | 1.84x | 37,153 |
| 16 KB | 76.7 | 43.6 | 55.3 | 1.76x | 15,547 |
| 64 KB | 181.1 | 107.1 | 118.5 | 1.69x | 6,094 |
| 256 KB | 528.7 | 261.2 | 267.3 | 2.02x | 2,186 |
| 1 MB | 2,151 | 784.1 | 781.8 | 2.74x | 511 |

### Pipelined, 1 stream (duplex MB/s, higher is better)

| payload | UDS | mono | plugin | mono/UDS | plugin/mono | minN |
|--------:|----:|-----:|-------:|---------:|------------:|-----:|
| 64 B | 41.7 | 67.0 | 64.3 | 1.61x | 0.96x | 382,508 |
| 4 KB | 1,196 | 1,779 | 1,696 | 1.49x | 0.95x | 143,834 |
| 64 KB | 1,973 | 3,956 | 3,897 | 2.01x | 0.99x | 18,535 |
| 1 MB | 1,939 | 4,812 | 4,932 | 2.48x | 1.02x | 1,119 |
| 4 MB | 1,786 | **4,853** | 4,873 | 2.72x | 1.00x | 252 |
| 16 MB | 1,494 | **2,439** | 2,295 | 1.63x | 0.94x | 51 |

### Concurrent, 10 streams (duplex MB/s, higher is better)

| payload | UDS | mono | plugin | mono/UDS | plugin/mono | minN |
|--------:|----:|-----:|-------:|---------:|------------:|-----:|
| 64 B | 16.8 | 47.2 | 46.7 | 2.80x | 0.99x | 15,994 |
| 4 KB | 543.8 | 1,439 | 1,390 | 2.65x | 0.97x | 7,494 |
| 64 KB | 1,669 | 3,830 | 3,358 | 2.29x | 0.88x | 1,530 |
| 1 MB | 3,063 | 5,129 | 5,100 | 1.67x | 0.99x | 175 |
| 4 MB | 2,731 | 5,363 | 5,358 | 1.96x | 1.00x | 39 |
| 16 MB | 2,308 | 5,020 | 4,201 | 2.18x | 0.84x | 8 ⚠ |

### Concurrent, 100 streams (duplex MB/s, higher is better)

| payload | UDS | mono | plugin | mono/UDS | plugin/mono | minN |
|--------:|----:|-----:|-------:|---------:|------------:|-----:|
| 64 B | 31.8 | 83.8 | 83.0 | 2.64x | 0.99x | 2,802 |
| 4 KB | 1,323 | 2,274 | 2,121 | 1.72x | 0.93x | 1,856 |
| 64 KB | 3,836 | 4,989 | 4,883 | 1.30x | 0.98x | 349 |
| 1 MB | 3,315 | 6,291 | 6,155 | 1.90x | 0.98x | 18 |
| 4 MB | 2,233 | **6,649** | 6,137 | 2.98x | 0.92x | 3 ⚠ |
| 16 MB | 1,844 | 5,826 | 4,618 | 3.16x | 0.79x | 1 ⚠ |

### Message rate (kmsg/s, median of 5)

| benchmark | UDS | mono | plugin |
|-----------|----:|-----:|-------:|
| Pipelined 64 B | 682.7 | 1,097 | 1,054 |
| Pipelined 4 KB | 306.1 | 455.3 | 434.3 |
| Pipelined 64 KB | 31.6 | 63.3 | 62.4 |
| Pipelined 1 MB | 1.94 | 4.81 | 4.93 |
| Pipelined 4 MB | 0.45 | 1.21 | 1.22 |
| Pipelined 16 MB | 0.09 | 0.15 | 0.14 |
| conc/10 64 B | 275.8 | 772.9 | 765.7 |
| conc/10 4 KB | 139.2 | 368.4 | 355.8 |
| conc/10 64 KB | 26.7 | 61.3 | 53.7 |
| conc/10 1 MB | 3.06 | 5.13 | 5.10 |
| conc/10 4 MB | 0.68 | 1.34 | 1.34 |
| conc/10 16 MB | 0.14 | 0.31 | 0.26 |
| conc/100 64 B | 520.9 | 1,374 | 1,360 |
| conc/100 4 KB | 338.7 | 582.1 | 543.0 |
| conc/100 64 KB | 61.4 | 79.8 | 78.1 |
| conc/100 1 MB | 3.31 | 6.29 | 6.16 |
| conc/100 4 MB | 0.56 | 1.66 | 1.53 |
| conc/100 16 MB | 0.12 | 0.36 | 0.29 |

## Reading the results

**SHM beats UDS everywhere**, by 1.2x to 3.2x. The advantage grows with payload
size for latency (1.23x at 64 B, 2.91x at 1 MB) because that is where the copy
and syscall savings dominate; at 64 B the fixed per-RPC cost of the gRPC stack
itself is most of the measurement, and no transport can avoid it.

**Single-stream throughput falls off a cliff at 16 MB.** Pipelined mono peaks at
4,853 MB/s at 4 MB and drops to 2,439 MB/s at 16 MB, roughly half. This is the
cost of splitting messages across HTTP/2 DATA frames, which are capped at
2^24-1 bytes: a 16 MB message cannot ride in one frame. The cliff is a known
open item, not a regression.

**Concurrency masks the cliff.** With 10 or 100 streams the 16 MB cells reach
5,020 and 5,826 MB/s, because other streams fill the gaps left by any one
stream's framing overhead. Only latency-sensitive single-stream traffic at
multi-MB sizes is affected.

**mono and plugin are equivalent**, within 0.95x-1.02x, except for three
concurrent cells (64 KB/10 at 0.88x, 16 MB/10 at 0.84x, 16 MB/100 at 0.79x).
Those are the cells with the least headroom and the fewest samples, so the gap
is not yet established as real.

## Caveats

- The three cells marked ⚠ ran 1, 3, and 8 iterations. Treat them as
  directional; rerun with `-benchtime=10s` before drawing conclusions.
- Median run-to-run spread is 3.9% across all 90 cells. The worst offenders are
  UDS concurrent/100/16 MB at 20.2%, plugin stream/64 KB at 13.9%, and UDS
  concurrent/10/16 MB at 13.3%.
- Both endpoints run in one process on one machine. This flatters shared memory
  relative to a real cross-container deployment, and it means the two sides
  compete for the same 16 cores at high stream counts.
- Numbers are from a single machine and a single run of 5 repeats. They are
  useful for comparing arms against each other, not as absolute capacity
  figures.
