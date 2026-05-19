#!/bin/bash
# Summarize resource snapshots
RES=~/bench_out/v34_fair/resources
if [ ! -d "$RES" ]; then
    echo "No resource snapshots found at $RES."
    echo "Run tools/fair_bench_with_resources_linux.sh or tools/sample_resources_linux.sh first."
    exit 1
fi
cd "$RES" || exit 1
printf "%-25s %-7s %-9s %-7s %-7s %-7s %-9s %-10s %-12s\n" "CELL" "fd_tot" "fd_evfd" "fd_sock" "fd_anon" "fd_shm" "mmaps" "shm_kib" "rss"
for dir in */; do
    label="${dir%/}"
    snap="$dir/snap2.txt"
    if [ ! -f "$snap" ]; then
        printf "%-25s NO_SNAP\n" "$label"
        continue
    fi
    ft=$(awk -F= '/^fd_total=/{print $2}' "$snap")
    fe=$(awk -F= '/^fd_eventfd=/{print $2}' "$snap")
    fs=$(awk -F= '/^fd_socket=/{print $2}' "$snap")
    fa=$(awk -F= '/^fd_anon_inode=/{print $2}' "$snap")
    fsh=$(awk -F= '/^fd_shm=/{print $2}' "$snap")
    mc=$(awk -F= '/^mmap_shm_count=/{print $2}' "$snap")
    mk=$(awk -F= '/^mmap_shm_size_kb=/{print $2}' "$snap")
    rss=$(awk '/^VmRSS:/{print $2 $3}' "$snap")
    printf "%-25s %-7s %-9s %-7s %-7s %-7s %-9s %-10s %-12s\n" \
        "$label" "$ft" "$fe" "$fs" "$fa" "$fsh" "$mc" "$mk" "$rss"
done

echo ""
echo "=== Bench MB/s per cell (from each cell's bench.log) ==="
for dir in */; do
    label="${dir%/}"
    log="$dir/bench.log"
    if [ -f "$log" ]; then
        line=$(grep '^Benchmark' "$log" | tail -1)
        printf "%-25s %s\n" "$label" "$line"
    fi
done
