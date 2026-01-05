#!/bin/bash
# Copyright 2024 gRPC authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Run comprehensive benchmarks comparing SHM, TCP, and Unix domain sockets

set -e

echo "========================================="
echo "gRPC Shared Memory Transport Benchmarks"
echo "========================================="
echo ""

cd "$(dirname "$0")"

OUTPUT_DIR="benchmark_results"
mkdir -p "$OUTPUT_DIR"

TIMESTAMP=$(date +%Y%m%d_%H%M%S)
RESULTS_FILE="$OUTPUT_DIR/results_$TIMESTAMP.txt"

echo "Running benchmarks... (this may take several minutes)"
echo "Results will be saved to: $RESULTS_FILE"
echo ""

# Run unary RPC benchmarks
echo "=== Unary RPC Benchmarks ===" | tee -a "$RESULTS_FILE"
go test -bench=BenchmarkUnaryRPC -benchmem -benchtime=3s ./internal/transport | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

# Run streaming RPC benchmarks
echo "=== Streaming RPC Benchmarks ===" | tee -a "$RESULTS_FILE"
go test -bench=BenchmarkStreamingRPC -benchmem -benchtime=3s ./internal/transport | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

# Run throughput benchmarks
echo "=== Throughput Benchmarks ===" | tee -a "$RESULTS_FILE"
go test -bench=BenchmarkThroughput -benchmem -benchtime=5s ./internal/transport | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

# Run latency benchmarks
echo "=== Latency Benchmarks ===" | tee -a "$RESULTS_FILE"
go test -bench=BenchmarkLatency -benchmem -benchtime=10000x ./internal/transport | tee -a "$RESULTS_FILE"
echo "" | tee -a "$RESULTS_FILE"

echo "========================================="
echo "Benchmarks complete!"
echo "Results saved to: $RESULTS_FILE"
echo "========================================="

# Generate summary
echo ""
echo "Generating summary..."
python3 - <<EOF
import re
import sys

def parse_benchmark_results(filename):
    with open(filename, 'r') as f:
        content = f.read()
    
    results = {}
    pattern = r'Benchmark(\w+)/size=(\d+)B/(shm|tcp|unix)\s+\d+\s+(\d+\.?\d*)\s+ns/op'
    
    for match in re.finditer(pattern, content):
        bench_type, size, transport, ns_per_op = match.groups()
        key = (bench_type, int(size))
        if key not in results:
            results[key] = {}
        results[key][transport] = float(ns_per_op)
    
    print("\n=== Performance Summary ===\n")
    print("Speedup of SHM vs TCP and Unix sockets:\n")
    
    for (bench_type, size), transports in sorted(results.items()):
        if 'shm' in transports and 'tcp' in transports:
            speedup_tcp = transports['tcp'] / transports['shm']
            print(f"{bench_type} ({size}B):")
            print(f"  SHM vs TCP:  {speedup_tcp:.2f}x faster")
            if 'unix' in transports:
                speedup_unix = transports['unix'] / transports['shm']
                print(f"  SHM vs Unix: {speedup_unix:.2f}x faster")
            print()

if __name__ == '__main__':
    parse_benchmark_results('$RESULTS_FILE')
EOF

echo ""
echo "Done!"
