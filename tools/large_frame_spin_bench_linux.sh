#!/bin/bash
# Large-frame + spin-wait sensitivity bench on Linux.
#
# Mark asked: "Can you do a run with large frames and spin wait to show
# how much it improves?" His follow-up feedback was: use 1 MiB frame
# (not 16 MiB) and report UDS alongside TCP so the apples-to-apples
# local-IPC baseline is included. This script runs three matched cells
# against the same fair-default baseline so the contribution of each
# tuning knob is isolated:
#
#   A  fair_default     — 16 KiB frame, no spin (current fair config)
#   B  fair_1Mframe     — 1 MiB frame, no spin
#   C  fair_1Mframe_spin— 1 MiB frame, SHM_SPIN_ITERS=2000 (light)
#
# Spin and large-frame are SHM-only operator tunings; TCP / UDS keep
# their HTTP/2 spec defaults across all three cells, so the comparison
# stays apples-to-apples.
#
# Output: ~/bench_out/v34_1Mframe_spin/{A,B,C}.txt
set -u
OUTROOT=~/bench_out/v34_1Mframe_spin
mkdir -p "$OUTROOT"
REPO="${REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "$REPO" || { echo "Cannot cd to $REPO"; exit 1; }
echo "using REPO=$REPO  HEAD=$(git rev-parse --short HEAD)  out=$OUTROOT"

clean_shm() {
    ls /dev/shm 2>/dev/null | grep -E '^grpc_shm_' | xargs -r -I{} rm -f /dev/shm/{}
    pkill -f shmemtcp.test 2>/dev/null
    sleep 1
}

# Common settings for all three passes. fair-default sets a 64 KiB
# initial window AND a 16 KiB max frame on SHM, matching TCP / UDS
# under the spec defaults; the per-pass overrides below change only
# the SHM-specific knobs.
export BENCH_PROFILE=fair-default
export SHM_BENCH_CPU=1
# Avoid stale dirty-pool state across runs in the same shell.
unset BENCH_DIRTY_DEFAULT_POOL

run_pass() {
    local LABEL="$1"
    local OUT="$OUTROOT/$LABEL.txt"
    clean_shm
    echo "[$(date +%T)] $LABEL start  SHM_MAX_FRAME_SIZE=${SHM_MAX_FRAME_SIZE:-unset} SHM_SPIN_ITERS=${SHM_SPIN_ITERS:-unset}"
    /usr/local/go/bin/go test \
        -bench='^BenchmarkGRPC(Shm|Unix|TCP)(Unary|Stream|Concurrent)$' \
        -benchtime=2s -count=1 -run=^$ -timeout=3000s \
        ./benchmark/shmemtcp/ > "$OUT" 2>&1
    echo "[$(date +%T)] $LABEL done rc=$?  cells=$(grep -c '^Benchmark' "$OUT")"
}

# ---- Cell A: baseline (16 KiB frame, no spin) ----
unset SHM_MAX_FRAME_SIZE
unset SHM_SPIN_ITERS
run_pass A_fair_default

# ---- Cell B: 1 MiB frame only ----
export SHM_MAX_FRAME_SIZE=1048576   # 1 MiB; per Mark's feedback (was 16 MiB)
unset SHM_SPIN_ITERS
run_pass B_fair_1Mframe

# ---- Cell C: 1 MiB frame + light spin ----
export SHM_MAX_FRAME_SIZE=1048576
export SHM_SPIN_ITERS=2000
run_pass C_fair_1Mframe_spin

unset SHM_MAX_FRAME_SIZE SHM_SPIN_ITERS

echo "DONE.  Three result files under $OUTROOT:"
ls -lh "$OUTROOT"/*.txt
