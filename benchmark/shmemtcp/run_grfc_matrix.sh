#!/usr/bin/env bash
#
# Linux VM bench-matrix runner for the gRFC review (Doug Fawley + Mark
# Russinovich). Captures the FULL deliverable in one invocation, with
# CPU / FD telemetry, so the result can be handed back as-is.
#
# Usage:   ./benchmark/shmemtcp/run_grfc_matrix.sh [output_dir]
#
# Default output_dir = ./out/grfc_$(date +%Y%m%d_%H%M%S)
#
# Produces, in $OUT:
#   - env.txt             - host info: uname, CPU, Go version, kernel
#   - settings.txt        - active env vars at run time
#   - unary_fair.txt      - SHM vs UDS vs TCP single-stream bench
#   - concurrent_fair.txt - SHM vs UDS vs TCP at N=10/100/1000
#   - cpu_pidstat.txt     - pidstat capture during one warm bench
#   - fd_lsof.txt         - FD count snapshot at peak load
#
# All bench runs use:
#   BENCH_PROFILE=fair-default     (Doug D-B2: equalize HTTP/2 settings)
#   SHM_DATASEG_WAKE=1             (per-direction eventfd waker)
#   SHM_INPROC_WAKE=1              (per-address eventfd registry; both
#                                   layers are opt-in but compatible)
#   SHM_BENCH_CPU=1                (Doug D-B3: report cpu-ns/op)
#   SHM_BENCH_ZC=1                 (zero-copy probe metrics)
#   no SHM_SPIN_ITERS              (Doug D-B5: spin OFF by default)
#
# Required tools: go (>=1.25), pidstat (sysstat package), lsof.

set -euo pipefail

OUT="${1:-./out/grfc_$(date +%Y%m%d_%H%M%S)}"
mkdir -p "$OUT"
echo "Output: $OUT"

GO="${GO:-/usr/local/go/bin/go}"

# --- env capture ----------------------------------------------------
{
  echo "=== uname ==="
  uname -a
  echo ""
  echo "=== /proc/cpuinfo (first cpu) ==="
  awk '/^processor[[:space:]]*: 0/{flag=1; next} /^processor/{flag=0} flag' /proc/cpuinfo | head -20
  echo ""
  echo "=== free -h ==="
  free -h
  echo ""
  echo "=== go version ==="
  "$GO" version
  echo ""
  echo "=== git ==="
  git -C "$(dirname "$0")/../.." rev-parse HEAD
  git -C "$(dirname "$0")/../.." log -1 --oneline
} > "$OUT/env.txt" 2>&1
echo "--> env.txt"

# Common SHM-wake flags across all profiles.
export SHM_DATASEG_WAKE=1
export SHM_INPROC_WAKE=1
export SHM_BENCH_CPU=1
export SHM_BENCH_ZC=1
unset SHM_SPIN_ITERS

# Helper: run a bench filter under a specific BENCH_PROFILE.
run_profile_bench() {
  local profile="$1"
  local out_file="$2"
  local filter="$3"
  local benchtime="${4:-3s}"
  local count="${5:-3}"
  echo "--> $out_file  (BENCH_PROFILE=$profile filter=$filter benchtime=$benchtime count=$count)"
  if [[ "$profile" == "default" ]]; then
    unset BENCH_PROFILE
  else
    export BENCH_PROFILE="$profile"
  fi
  "$GO" test -bench="$filter" -benchtime="$benchtime" -count="$count" \
    -run=^$ -timeout=30m ./benchmark/shmemtcp/ 2>&1 | tee "$out_file"
}

# --- settings capture -----------------------------------------------
{
  echo "=== bench env vars (other than BENCH_PROFILE which varies per run) ==="
  env | grep -E '^SHM_' | sort
  echo ""
  echo "=== Doug's reviewer asks status ==="
  echo "B1 latest grpc-go      : branch HEAD ($(git -C "$(dirname "$0")/../.." rev-parse --short HEAD))"
  echo "B2 HTTP-settings parity: BENCH_PROFILE=fair-default in the *-fair* runs"
  echo "B3 CPU utilization     : SHM_BENCH_CPU=1 + pidstat capture below"
  echo "B4 concurrent scaling  : BenchmarkGRPC*Concurrent at N=10/100/1000"
  echo "B5 spin off            : SHM_SPIN_ITERS unset (= 0)"
  echo "M2 FD usage            : lsof snapshot below"
  echo ""
  echo "=== profiles covered ==="
  echo "fair-default : initialWindow=65535,    maxFrame=16384      (Doug's apples-to-apples)"
  echo "fair-32mb    : initialWindow=32MiB,    maxFrame=SHM-default (relaxed-fair: same windows, native frames)"
  echo "shm-tuned    : initialWindow=2GiB,     maxFrame=SHM-default (SHM native settings)"
} > "$OUT/settings.txt" 2>&1
echo "--> settings.txt"

# ==== UNARY (single-stream) per profile =============================
# Compare SHM vs TCP vs UDS at each profile. Small payloads first.
run_profile_bench fair-default "$OUT/unary_fair_default.txt" \
  'BenchmarkGRPC(Shm|Unix|TCP)Unary$' 3s 3

run_profile_bench fair-32mb "$OUT/unary_fair_32mb.txt" \
  'BenchmarkGRPC(Shm|Unix|TCP)Unary$' 3s 3

run_profile_bench shm-tuned "$OUT/unary_shm_tuned.txt" \
  'BenchmarkGRPC(Shm|Unix|TCP)Unary$' 3s 3

# Large payloads (64 KiB and up).
run_profile_bench fair-default "$OUT/unary_large_fair_default.txt" \
  'BenchmarkGRPC(Shm|Unix|TCP)LargeUnary$' 3s 3

run_profile_bench shm-tuned "$OUT/unary_large_shm_tuned.txt" \
  'BenchmarkGRPC(Shm|Unix|TCP)LargeUnary$' 3s 3

# ==== STREAM ========================================================
run_profile_bench fair-default "$OUT/stream_fair_default.txt" \
  'BenchmarkGRPC(Shm|Unix|TCP)Stream$' 3s 3

run_profile_bench shm-tuned "$OUT/stream_shm_tuned.txt" \
  'BenchmarkGRPC(Shm|Unix|TCP)Stream$' 3s 3

# ==== CONCURRENT (D-B4) =============================================
# Three profiles to expose the scaling-with-window-size story:
# - fair-default at N=1000 hits the 64 KiB HTTP/2 connection-window
#   ceiling and serialises; this is the "apples-to-apples" answer.
# - fair-32mb shows the same equal-settings vs UDS comparison but
#   without that ceiling — closer to a "what could SHM do if Doug
#   accepted equalised-but-realistic-for-IPC windows?"
# - shm-tuned is the production-recommended setting; SHM's natural
#   advantage at high concurrency surfaces here.
run_profile_bench fair-default "$OUT/concurrent_fair_default.txt" \
  'BenchmarkGRPC(Shm|Unix|TCP)Concurrent$' 3s 1

run_profile_bench fair-32mb "$OUT/concurrent_fair_32mb.txt" \
  'BenchmarkGRPC(Shm|Unix|TCP)Concurrent$' 3s 1

run_profile_bench shm-tuned "$OUT/concurrent_shm_tuned.txt" \
  'BenchmarkGRPC(Shm|Unix|TCP)Concurrent$' 3s 1

# ==== CPU + FD telemetry during one steady-state run ===============
# Sample pidstat + lsof during a long SHM unary run.
echo "--> cpu_pidstat.txt / fd_lsof.txt (sampling steady SHM run)"
export BENCH_PROFILE=fair-default
"$GO" test -bench='BenchmarkGRPCShmUnary/size=4096-' -benchtime=30s -count=1 \
  -run=^$ -timeout=60s ./benchmark/shmemtcp/ > "$OUT/telemetry_bench.txt" 2>&1 &
BENCH_PID=$!
sleep 8  # let the bench reach steady state
# Find the actual `go test` child process (the one running the bench binary).
SHMEMTCP_PID="$(pgrep -P "$BENCH_PID" -f shmemtcp 2>/dev/null || true)"
if [[ -z "$SHMEMTCP_PID" ]]; then
  SHMEMTCP_PID="$(pgrep -f 'shmemtcp\.test' | head -1 || true)"
fi
if [[ -n "$SHMEMTCP_PID" ]]; then
  echo "Sampling pidstat on PID $SHMEMTCP_PID for 10 s"
  pidstat -p "$SHMEMTCP_PID" -u -r -d 1 10 > "$OUT/cpu_pidstat.txt" 2>&1 || true
  lsof -p "$SHMEMTCP_PID" > "$OUT/fd_lsof_full.txt" 2>&1 || true
  {
    echo "=== FD count breakdown (TYPE column) ==="
    awk 'NR>1 {print $5}' "$OUT/fd_lsof_full.txt" 2>/dev/null | sort | uniq -c | sort -rn
    echo ""
    echo "=== Total FD count ==="
    wc -l < "$OUT/fd_lsof_full.txt"
  } > "$OUT/fd_lsof.txt"
else
  echo "WARN: could not find bench PID for telemetry" | tee "$OUT/cpu_pidstat.txt"
fi
wait "$BENCH_PID" 2>/dev/null || true

echo ""
echo "=== DONE. Output captured in: $OUT ==="
ls -la "$OUT"
