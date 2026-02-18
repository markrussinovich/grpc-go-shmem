#!/usr/bin/env python3
"""
Generate benchmark plots for SHM vs TCP vs Unix socket transport performance.

Uses benchmark_results.json (produced by benchmark_runner.py) when available,
otherwise falls back to hardcoded default data. The plotting code is shared
with benchmark_runner.py -- both scripts generate the same charts.

Usage:
    python plot_benchmarks.py                      # Auto-detect JSON or use defaults
    python plot_benchmarks.py --data results.json  # Load specific JSON file
"""

import argparse
import json
import os
import sys
from pathlib import Path

import matplotlib
matplotlib.use('Agg')

from benchmark_runner import (
    extract_data,
    generate_plots,
    generate_consolidated_plot,
    OUT_DIR,
    OUT_ROOT,
    RESULTS_FILE,
)


def default_data():
    """Return hardcoded default benchmark data as fallback.

    Used when no benchmark_results.json is available (e.g. fresh clone).
    Values are representative results from transport microbenchmarks.
    """
    return {
        "sizes": [64, 256, 1024, 4096, 16384, 65536],
        "size_labels": ['64B', '256B', '1KB', '4KB', '16KB', '64KB'],
        "large_sizes": [1048576, 4194304, 16777216],
        "large_size_labels": ['1MB', '4MB', '16MB'],
        "cpu": "Default Data (run benchmark_runner.py --run to collect live data)",
        "timestamp": "",
        "rt_sizes": [64, 256, 1024, 4096],
        "rt_size_labels": ['64B', '256B', '1KB', '4KB'],
        # One-way streaming latency (ns/op)
        "shm_stream_latency": [126.3, 144.5, 147.3, 198.3, 499.2, 1788],
        "tcp_stream_latency": [7663, 8026, 6404, 7538, 11195, 25351],
        "unix_stream_latency": [2187, 2220, 2646, 3223, 5213, 13012],
        # One-way streaming throughput (MB/s)
        "shm_stream_throughput": [506.86, 1771.83, 6949.56, 20655.31, 32822.56, 36652.51],
        "tcp_stream_throughput": [8.35, 31.90, 159.89, 543.41, 1463.50, 2585.14],
        "unix_stream_throughput": [29.26, 115.29, 387.05, 1270.92, 3142.86, 5036.76],
        # Roundtrip (unary RPC) latency (ns/op)
        "shm_rt_latency": [650, 637, 679, 925],
        "tcp_rt_latency": [18500, 18100, 18600, 20100],
        "unix_rt_latency": [9500, 9600, 9650, 11400],
        # Large payload data - not available in defaults
        "shm_large_stream_throughput": [None, None, None],
        "shm_large_stream_latency": [None, None, None],
        "tcp_large_stream_throughput": [None, None, None],
        "tcp_large_stream_latency": [None, None, None],
        "unix_large_stream_throughput": [None, None, None],
        "unix_large_stream_latency": [None, None, None],
        "shm_large_rt_throughput": [None, None, None],
        "shm_large_rt_latency": [None, None, None],
        "tcp_large_rt_throughput": [None, None, None],
        "tcp_large_rt_latency": [None, None, None],
        "unix_large_rt_throughput": [None, None, None],
        "unix_large_rt_latency": [None, None, None],
    }


def load_data(json_path=None):
    """Load benchmark data, trying JSON file first, falling back to defaults.

    Args:
        json_path: Optional path to a benchmark_results.json file.
                   If None, checks the default platform-specific location.

    Returns:
        Data dict for generate_plots() and generate_consolidated_plot().
    """
    if json_path:
        p = Path(json_path)
        if p.exists():
            with open(p) as f:
                results = json.load(f)
            print(f"Loaded benchmark data from: {p}")
            return extract_data(results)
        print(f"WARNING: {p} not found, using defaults.")
        return default_data()

    # Try default platform-specific location
    if RESULTS_FILE.exists():
        with open(RESULTS_FILE) as f:
            results = json.load(f)
        print(f"Loaded benchmark data from: {RESULTS_FILE}")
        return extract_data(results)

    # Try legacy location (no platform subdir)
    legacy = OUT_ROOT / "benchmark_results.json"
    if legacy.exists():
        with open(legacy) as f:
            results = json.load(f)
        print(f"Loaded benchmark data from: {legacy}")
        return extract_data(results)

    print("No benchmark_results.json found, using default data.")
    print("Run 'python benchmark_runner.py --run' to collect live benchmark data.")
    return default_data()


def main():
    parser = argparse.ArgumentParser(
        description='Generate benchmark plots for SHM vs TCP vs Unix transport.',
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  python plot_benchmarks.py                      # Auto-detect JSON or defaults
  python plot_benchmarks.py --data results.json  # Use specific data file
        """
    )
    parser.add_argument('--data', type=str, default=None,
                        help='Path to benchmark_results.json file')
    args = parser.parse_args()

    data = load_data(args.data)
    OUT_DIR.mkdir(parents=True, exist_ok=True)

    print("=" * 70)
    print("Generating benchmark plots...")
    print("=" * 70)

    plot_files = generate_plots(data)
    consolidated = generate_consolidated_plot(data)

    print("\n" + "=" * 70)
    print("BENCHMARK PLOTS GENERATED")
    print("=" * 70)
    for f in plot_files:
        print(f"  {f}")
    print(f"  {consolidated}  <- CONSOLIDATED (all data)")
    print(f"\nAll plots saved to: {OUT_DIR}")


if __name__ == '__main__':
    main()
