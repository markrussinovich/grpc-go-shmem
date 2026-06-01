# Bench / profile / resource-collection scripts

These scripts produce the cross-platform A/B + resource-usage data
captured in `tmp/fair_results/SUMMARY_FOR_MARK.md`. They are
*reproducibility helpers* — not part of the SHM transport runtime.

## Linux (run from repo root)

| Script | What it does | Wall time |
|---|---|---|
| `fair_bench_with_resources_linux.sh` | Pass A (stock pool) + Pass B (`BENCH_DIRTY_DEFAULT_POOL=1`) full matrix (SHM/UDS/TCP × Unary/Stream/Concurrent × all sizes) + 12 resource snapshots | ~25 min |
| `sample_resources_linux.sh`          | Standalone resource snapshot (12 cells × {fd,eventfd,socket,mmap,RSS} from /proc) | ~6 min |
| `cpuprof_linux.sh`                   | `go test -cpuprofile` on 9 hot cells, top-25 via `go tool pprof` | ~5 min |
| `summarize_resources.sh`             | Print one-line summary table from snap files | <1 s |

All write outputs to `~/bench_out/v34_fair/`.

Quick start on a Linux VM after `git checkout feat/shm-v3.4-multilane`:

```bash
bash tools/fair_bench_with_resources_linux.sh
bash tools/cpuprof_linux.sh
bash tools/summarize_resources.sh
ls ~/bench_out/v34_fair/
```

## Windows (run from repo root in pwsh 7+)

| Script | What it does | Wall time |
|---|---|---|
| `bench_fair_windows.ps1 -FullMatrix` | Pass A + Pass B full matrix (SHM/UDS/TCP — Windows 10 1803+ has `AF_UNIX`) + 8 resource snapshots | ~25 min |
| `sample_resources_windows.ps1`       | Standalone resource snapshot (12 cells × HandleCount/WS/PrivateMem/TCP_Established) | ~5 min |
| `summarize_windows.ps1`              | Print snap summary table | <1 s |

Outputs go to `bench_win_fair/` under the repo root.

Quick start on a Windows host:

```powershell
pwsh -File tools\bench_fair_windows.ps1 -FullMatrix
pwsh -File tools\sample_resources_windows.ps1
pwsh -File tools\summarize_windows.ps1
```

## Cross-platform analysis (run on either OS with pwsh)

| Script | What it does |
|---|---|
| `cpu_efficiency.ps1`        | MB/s per fully-loaded CPU core, derived from raw `%cpu` and `cpu-ns/op` fields. |
| `make_full_comparison.ps1`  | Side-by-side Linux × Windows × {SHM, UDS, TCP} for Unary / Stream / Concurrent. |

Both read from `tmp/fair_results/A_fair_default*.txt` and
`tmp/fair_results/A_fair_default_windows.txt`. Copy the per-host
`A_fair_default.txt` from `~/bench_out/v34_fair/` into
`tmp/fair_results/` first.

## Environment variables the bench respects

| Var | Meaning |
|---|---|
| `BENCH_PROFILE=fair-default` | HTTP/2 stock window=65535, frame=16384 applied uniformly to SHM/UDS/TCP. |
| `BENCH_DIRTY_DEFAULT_POOL=1` | Swaps grpc-go's process-wide default buffer pool to a dirty (no-memclr-on-Get) variant. **Cross-transport** (affects SHM + UDS + TCP). Off by default in the canonical numbers. |
| `SHM_BENCH_CPU=1` | Include `%cpu` and `cpu-ns/op` columns in bench output (Linux only, ResourceUsage). |

Note: the per-data-segment eventfd waker is the default behaviour of the SHM
transport on Linux. Tests/benchmarks that want to compare against the futex
wait path can call `transport.ConfigureShmEventfdWakerForBench(false)` before
running.

## Output layout reference

```
~/bench_out/v34_fair/                  # Linux
├── A_fair_default.txt                 # 117 cells (SHM/UDS/TCP × Unary+Stream+Concurrent)
├── B_fair_dirty.txt                   # 117 cells
├── resources/<cell>/{INFO.txt,snap[1-3].txt,bench.log}
└── cpuprof/<cell>.{prof,top.txt,log}

bench_win_fair/                        # Windows
├── A_fair_default.txt                 # 117 cells (UDS too)
├── B_fair_dirty.txt
└── resources/<cell>/{INFO.txt,snap[1-3].txt,bench.log,bench.err}
```
