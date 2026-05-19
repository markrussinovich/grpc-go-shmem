#!/bin/bash
# Fair bench + resource sampling on Linux.
# Outputs to ~/bench_out/v34_fair/ (persistent, survives WSL restart).
set -u
OUTROOT=~/bench_out/v34_fair
mkdir -p "$OUTROOT"
# Repo root = parent of this script's tools/ dir, or override with REPO env var.
REPO="${REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "$REPO" || { echo "Cannot cd to $REPO"; exit 1; }
echo "using REPO=$REPO"

clean_shm() {
    ls /dev/shm 2>/dev/null | grep -E '^grpc_shm_' | xargs -r -I{} rm -f /dev/shm/{}
    pkill -f shmemtcp.test 2>/dev/null
    sleep 1
}

unset SHM_INPROC_WAKE
export SHM_NO_WU=1 BENCH_PROFILE=fair-default SHM_DATASEG_WAKE=1 SHM_BENCH_CPU=1

# ---- Pass A: stock pool ----
clean_shm
echo "[$(date +%T)] PASS A start"
/usr/local/go/bin/go test \
    -bench='^BenchmarkGRPC(Shm|Unix|TCP)(Unary|Stream|Concurrent)$' \
    -benchtime=2s -count=1 -run=^$ -timeout=3000s \
    ./benchmark/shmemtcp/ > "$OUTROOT/A_fair_default.txt" 2>&1
echo "[$(date +%T)] PASS A done rc=$?  cells=$(grep -c '^Benchmark' "$OUTROOT/A_fair_default.txt")"

# ---- Pass B: dirty pool ----
clean_shm
export BENCH_DIRTY_DEFAULT_POOL=1
echo "[$(date +%T)] PASS B start"
/usr/local/go/bin/go test \
    -bench='^BenchmarkGRPC(Shm|Unix|TCP)(Unary|Stream|Concurrent)$' \
    -benchtime=2s -count=1 -run=^$ -timeout=3000s \
    ./benchmark/shmemtcp/ > "$OUTROOT/B_fair_dirty.txt" 2>&1
echo "[$(date +%T)] PASS B done rc=$?  cells=$(grep -c '^Benchmark' "$OUTROOT/B_fair_dirty.txt")"
unset BENCH_DIRTY_DEFAULT_POOL

# ---- Resource snapshots ----
RES="$OUTROOT/resources"
mkdir -p "$RES"

snap_one() {
    local LABEL="$1" PATTERN="$2"
    local D="$RES/$LABEL"
    mkdir -p "$D"
    clean_shm
    /usr/local/go/bin/go test \
        -bench="$PATTERN" \
        -benchtime=12s -count=1 -run=^$ -timeout=120s \
        ./benchmark/shmemtcp/ > "$D/bench.log" 2>&1 &
    local BENCH_PID=$!
    local TPID=""
    for i in $(seq 1 30); do
        sleep 1
        TPID=$(pgrep -f shmemtcp.test 2>/dev/null | head -1)
        [ -n "$TPID" ] && break
    done
    if [ -z "$TPID" ]; then
        echo "$LABEL: failed to spawn"
        wait $BENCH_PID
        return
    fi
    echo "label=$LABEL pid=$TPID pattern=$PATTERN" > "$D/INFO.txt"
    sleep 4
    for s in 1 2 3; do
        [ ! -d /proc/$TPID ] && break
        {
            echo "ts=$(date +%T) snap=$s"
            awk '/^VmRSS|^VmData|^VmSize|^Threads|^FDSize/' /proc/$TPID/status 2>/dev/null
            echo "fd_total=$(ls /proc/$TPID/fd 2>/dev/null | wc -l)"
            echo "fd_eventfd=$(ls -la /proc/$TPID/fd 2>/dev/null | grep -c '\[eventfd:')"
            echo "fd_socket=$(ls -la /proc/$TPID/fd 2>/dev/null | grep -c 'socket:')"
            echo "fd_anon_inode=$(ls -la /proc/$TPID/fd 2>/dev/null | grep -c 'anon_inode:')"
            echo "fd_shm=$(ls -la /proc/$TPID/fd 2>/dev/null | grep -c 'grpc_shm')"
            echo "mmap_shm_count=$(grep -c grpc_shm /proc/$TPID/maps 2>/dev/null || echo 0)"
            echo "mmap_shm_size_kb=$(awk '/grpc_shm/ {split($1,a,"-"); sz+=strtonum("0x"a[2])-strtonum("0x"a[1])} END {print sz/1024+0}' /proc/$TPID/maps 2>/dev/null)"
            echo "--- sockstat ---"
            cat /proc/net/sockstat 2>/dev/null
        } > "$D/snap${s}.txt"
        sleep 2
    done
    wait $BENCH_PID
    echo "" >> "$D/INFO.txt"
    echo "=== BENCH RESULT ===" >> "$D/INFO.txt"
    grep '^Benchmark' "$D/bench.log" >> "$D/INFO.txt"
    echo "$LABEL done"
}

echo "[$(date +%T)] resource sampling start"
for trans in Shm Unix TCP; do
    snap_one "${trans}_unary_64B"        "^BenchmarkGRPC${trans}Unary$/^size=64-"
    snap_one "${trans}_unary_64K"        "^BenchmarkGRPC${trans}Unary$/^size=65536-"
    snap_one "${trans}_unary_1M"         "^BenchmarkGRPC${trans}Unary$/^size=1MB-"
    snap_one "${trans}_conc_1000x64K"    "^BenchmarkGRPC${trans}Concurrent$/^streams=1000$/^size=65536-"
done
clean_shm

echo "[$(date +%T)] ALL DONE" > "$OUTROOT/DONE"
echo "[$(date +%T)] ALL DONE"
