#!/bin/bash
# Resource snapshot v4: more precise PID detection (avoid transient go-build).
set -u
OUTROOT=~/bench_out/v34_fair
RES="$OUTROOT/resources"
mkdir -p "$RES"
REPO="${REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "$REPO" || { echo "Cannot cd to $REPO"; exit 1; }
echo "using REPO=$REPO"

clean_shm() {
    ls /dev/shm 2>/dev/null | grep -E '^grpc_shm_' | xargs -r -I{} rm -f /dev/shm/{}
    pkill -f '/shmemtcp\.test ' 2>/dev/null
    sleep 1
}

unset SHM_INPROC_WAKE
export SHM_NO_WU=1 BENCH_PROFILE=fair-default SHM_DATASEG_WAKE=1 SHM_BENCH_CPU=1
unset BENCH_DIRTY_DEFAULT_POOL

# Find the PID of the actual long-running test binary (not go test wrapper, not
# the transient go-build compile pass). Wait until same PID is seen 2x in a row
# (stability check).
find_test_pid() {
    local PREV=""
    local CUR=""
    local STABLE=0
    for i in $(seq 1 30); do
        sleep 1
        # Match the compiled binary path only, anchored on path-separator.
        CUR=$(pgrep -f '/shmemtcp\.test ' 2>/dev/null | head -1)
        if [ -n "$CUR" ] && [ "$CUR" = "$PREV" ]; then
            STABLE=$((STABLE+1))
            if [ $STABLE -ge 1 ]; then
                echo "$CUR"
                return 0
            fi
        else
            STABLE=0
        fi
        PREV="$CUR"
    done
    return 1
}

snap_one() {
    local LABEL="$1" PATTERN="$2"
    local D="$RES/$LABEL"
    rm -rf "$D"; mkdir -p "$D"
    clean_shm
    /usr/local/go/bin/go test \
        -bench="$PATTERN" \
        -benchtime=25s -count=1 -run=^$ -timeout=180s \
        ./benchmark/shmemtcp/ > "$D/bench.log" 2>&1 &
    local BENCH_PID=$!

    local TPID
    TPID=$(find_test_pid)
    if [ -z "$TPID" ]; then
        echo "$LABEL: stable test PID not found"
        wait $BENCH_PID
        return
    fi
    echo "label=$LABEL pid=$TPID pattern=$PATTERN cmd=$(cat /proc/$TPID/cmdline 2>/dev/null | tr '\0' ' ')" > "$D/INFO.txt"

    # Allow steady-state
    sleep 3

    for s in 1 2 3; do
        if [ ! -d /proc/$TPID ]; then
            echo "snap $s: pid gone" >> "$D/INFO.txt"
            break
        fi
        local F="$D/snap${s}.txt"
        : > "$F"
        echo "ts=$(date +%T) snap=$s pid=$TPID" >> "$F"
        awk '/^VmRSS|^VmData|^VmSize|^VmPeak|^Threads|^FDSize/' /proc/$TPID/status 2>/dev/null >> "$F"
        local fdtot
        fdtot=$(ls /proc/$TPID/fd 2>/dev/null | wc -l)
        echo "fd_total=$fdtot" >> "$F"
        local fdlist
        fdlist=$(ls -la /proc/$TPID/fd 2>/dev/null)
        echo "fd_eventfd=$(echo "$fdlist" | grep -c 'eventfd]')" >> "$F"
        echo "fd_eventpoll=$(echo "$fdlist" | grep -c 'eventpoll]')" >> "$F"
        echo "fd_socket=$(echo "$fdlist" | grep -c 'socket:')" >> "$F"
        echo "fd_anon_inode=$(echo "$fdlist" | grep -c 'anon_inode:')" >> "$F"
        echo "fd_shm=$(echo "$fdlist" | grep -c 'grpc_shm')" >> "$F"
        echo "mmap_shm_count=$(grep -c grpc_shm /proc/$TPID/maps 2>/dev/null || echo 0)" >> "$F"
        echo "mmap_shm_size_kb=$(awk '/grpc_shm/ {split($1,a,"-"); sz+=strtonum("0x"a[2])-strtonum("0x"a[1])} END {print (sz/1024)+0}' /proc/$TPID/maps 2>/dev/null)" >> "$F"
        echo "--- /proc/net/sockstat ---" >> "$F"
        cat /proc/net/sockstat 2>/dev/null >> "$F"
        echo "--- top fd types (sample) ---" >> "$F"
        ls -la /proc/$TPID/fd 2>/dev/null | awk 'NR>2 {print $NF}' | sed 's/\[[0-9]*\]//' | sort | uniq -c | sort -rn | head -10 >> "$F"
        sleep 2
    done

    wait $BENCH_PID
    echo "" >> "$D/INFO.txt"
    echo "=== BENCH RESULT ===" >> "$D/INFO.txt"
    grep '^Benchmark' "$D/bench.log" >> "$D/INFO.txt"
    echo "$LABEL done"
}

for trans in Shm Unix TCP; do
    snap_one "${trans}_unary_64B"      "^BenchmarkGRPC${trans}Unary\$/^size=64\$"
    snap_one "${trans}_unary_64K"      "^BenchmarkGRPC${trans}Unary\$/^size=65536\$"
    snap_one "${trans}_unary_1M"       "^BenchmarkGRPC${trans}Unary\$/^size=1MB\$"
    snap_one "${trans}_conc_1000x64K"  "^BenchmarkGRPC${trans}Concurrent\$/^streams=1000\$/^size=65536\$"
done
clean_shm

echo "ALL DONE v4" > "$OUTROOT/RESOURCES_DONE_v4"
echo "ALL DONE v4"
