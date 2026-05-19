#!/bin/bash
# CPU profile on hot cells x 3 transports
set -u
OUTROOT=~/bench_out/v34_fair/cpuprof
mkdir -p "$OUTROOT"
REPO="${REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "$REPO" || { echo "Cannot cd to $REPO"; exit 1; }
echo "using REPO=$REPO"

unset SHM_INPROC_WAKE
export SHM_NO_WU=1 BENCH_PROFILE=fair-default SHM_DATASEG_WAKE=1 SHM_BENCH_CPU=1
unset BENCH_DIRTY_DEFAULT_POOL

prof_one() {
    local LABEL="$1" PATTERN="$2"
    local PROF="$OUTROOT/$LABEL.prof"
    ls /dev/shm 2>/dev/null | grep grpc_shm_ | xargs -r -I{} rm -f /dev/shm/{}
    pkill -f '/shmemtcp\.test ' 2>/dev/null
    sleep 1
    echo "[$(date +%T)] profile $LABEL"
    /usr/local/go/bin/go test \
        -bench="$PATTERN" \
        -benchtime=15s -count=1 -run=^$ -timeout=180s \
        -cpuprofile="$PROF" \
        ./benchmark/shmemtcp/ > "$OUTROOT/$LABEL.log" 2>&1
    /usr/local/go/bin/go tool pprof -top -cum -nodecount=25 "$PROF" 2>/dev/null > "$OUTROOT/$LABEL.top.txt"
    echo "$LABEL done ($(wc -c < "$PROF" 2>/dev/null) bytes prof)"
}

for trans in Shm Unix TCP; do
    prof_one "${trans}_unary_64K"      "^BenchmarkGRPC${trans}Unary\$/^size=65536\$"
    prof_one "${trans}_unary_1M"       "^BenchmarkGRPC${trans}Unary\$/^size=1MB\$"
    prof_one "${trans}_conc_1000x64K"  "^BenchmarkGRPC${trans}Concurrent\$/^streams=1000\$/^size=65536\$"
done
ls /dev/shm 2>/dev/null | grep grpc_shm_ | xargs -r -I{} rm -f /dev/shm/{}

echo "ALL DONE cpuprof" > "$OUTROOT/CPUPROF_DONE"
echo "ALL DONE cpuprof"
