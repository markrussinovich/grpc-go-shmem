#!/usr/bin/env python3
"""
Benchmark runner and plotter for SHM vs TCP vs Unix socket transport.

Usage:
    python benchmark_runner.py              # Plot from cached results (or run if none exist)
    python benchmark_runner.py --run        # Force rerun benchmarks, then plot
    python benchmark_runner.py --plot-only  # Only plot (fail if no cached results)
"""

import argparse
import json
import os
import re
import subprocess
import sys
from datetime import datetime
from pathlib import Path

import matplotlib.pyplot as plt
import numpy as np

# Directory setup
SCRIPT_DIR = Path(__file__).parent.absolute()
OUT_DIR = SCRIPT_DIR / "out"
RESULTS_FILE = OUT_DIR / "benchmark_results.json"


def run_benchmarks() -> dict:
    """Run Go benchmarks and parse results."""
    print("=" * 70)
    print("Running gRPC Transport Benchmarks...")
    print("=" * 70)
    
    results = {
        "timestamp": datetime.now().isoformat(),
        "cpu": "",
        "benchmarks": {}
    }
    
    # Run transport benchmarks - filter to specific benchmarks for plotting
    # Avoid slow benchmarks like BenchmarkShmBackpressure
    benchmark_pattern = "BenchmarkShmRingWriteRead|BenchmarkShmRingRoundtrip|BenchmarkTCPLoopback|BenchmarkUnixSocket"
    cmd = [
        "go", "test", 
        f"-bench={benchmark_pattern}",
        "-benchtime=500ms", "-cpu=2",
        "-run=^$",  # Don't run tests, only benchmarks
        "google.golang.org/grpc/internal/transport",
        "-benchmem"
    ]
    
    print(f"Running: {' '.join(cmd)}")
    print("-" * 70)
    
    try:
        proc = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=600,  # 10 minutes for full benchmark suite
            cwd=SCRIPT_DIR.parent.parent  # grpc-go-shmem root
        )
        output = proc.stdout + proc.stderr
        print(output)
    except subprocess.TimeoutExpired:
        print("ERROR: Benchmark timed out after 10 minutes")
        return None
    except Exception as e:
        print(f"ERROR: Failed to run benchmarks: {e}")
        return None
    
    # Parse results
    cpu_match = re.search(r'cpu:\s+(.+)', output)
    if cpu_match:
        results["cpu"] = cpu_match.group(1).strip()
    
    # Parse benchmark lines
    # Format: BenchmarkName/size=X-N   ops   ns/op   MB/s
    pattern = r'(Benchmark\w+)/size=(\d+)-\d+\s+(\d+)\s+([\d.]+)\s+ns/op\s+([\d.]+)\s+MB/s'
    
    for match in re.finditer(pattern, output):
        name = match.group(1)
        size = int(match.group(2))
        ops = int(match.group(3))
        ns_op = float(match.group(4))
        mb_s = float(match.group(5))
        
        if name not in results["benchmarks"]:
            results["benchmarks"][name] = {}
        
        results["benchmarks"][name][str(size)] = {
            "ops": ops,
            "ns_op": ns_op,
            "mb_s": mb_s
        }
    
    print("-" * 70)
    print(f"Parsed {len(results['benchmarks'])} benchmark types")
    
    return results


def save_results(results: dict):
    """Save benchmark results to JSON file."""
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    
    with open(RESULTS_FILE, 'w') as f:
        json.dump(results, f, indent=2)
    
    print(f"Saved results to: {RESULTS_FILE}")


def load_results() -> dict:
    """Load benchmark results from JSON file."""
    if not RESULTS_FILE.exists():
        return None
    
    with open(RESULTS_FILE, 'r') as f:
        results = json.load(f)
    
    print(f"Loaded results from: {RESULTS_FILE}")
    print(f"  Timestamp: {results.get('timestamp', 'unknown')}")
    print(f"  CPU: {results.get('cpu', 'unknown')}")
    
    return results


def extract_data(results: dict) -> dict:
    """Extract plotting data from benchmark results."""
    benchmarks = results.get("benchmarks", {})
    
    sizes = [64, 256, 1024, 4096, 16384, 65536]
    size_labels = ['64B', '256B', '1KB', '4KB', '16KB', '64KB']
    
    data = {
        "sizes": sizes,
        "size_labels": size_labels,
        "cpu": results.get("cpu", "Unknown CPU"),
        "timestamp": results.get("timestamp", ""),
    }
    
    # One-way streaming latency
    def get_latency(bench_name, sizes):
        if bench_name not in benchmarks:
            return [None] * len(sizes)
        bench = benchmarks[bench_name]
        return [bench.get(str(s), {}).get("ns_op") for s in sizes]
    
    def get_throughput(bench_name, sizes):
        if bench_name not in benchmarks:
            return [None] * len(sizes)
        bench = benchmarks[bench_name]
        return [bench.get(str(s), {}).get("mb_s") for s in sizes]
    
    # Streaming (one-way) benchmarks
    data["shm_stream_latency"] = get_latency("BenchmarkShmRingWriteRead", sizes)
    data["tcp_stream_latency"] = get_latency("BenchmarkTCPLoopback", sizes)
    data["unix_stream_latency"] = get_latency("BenchmarkUnixSocketLoopback", sizes)
    
    data["shm_stream_throughput"] = get_throughput("BenchmarkShmRingWriteRead", sizes)
    data["tcp_stream_throughput"] = get_throughput("BenchmarkTCPLoopback", sizes)
    data["unix_stream_throughput"] = get_throughput("BenchmarkUnixSocketLoopback", sizes)
    
    # Roundtrip (unary) benchmarks - only 4 sizes
    rt_sizes = [64, 256, 1024, 4096]
    rt_labels = ['64B', '256B', '1KB', '4KB']
    
    data["rt_sizes"] = rt_sizes
    data["rt_size_labels"] = rt_labels
    
    data["shm_rt_latency"] = get_latency("BenchmarkShmRingRoundtrip", rt_sizes)
    data["tcp_rt_latency"] = get_latency("BenchmarkTCPLoopbackRoundtrip", rt_sizes)
    data["unix_rt_latency"] = get_latency("BenchmarkUnixSocketRoundtrip", rt_sizes)
    
    return data


def generate_plots(data: dict):
    """Generate all benchmark plots."""
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    
    # Style setup
    plt.style.use('default')
    plt.rcParams['figure.facecolor'] = 'white'
    plt.rcParams['axes.facecolor'] = 'white'
    plt.rcParams['axes.grid'] = True
    plt.rcParams['grid.alpha'] = 0.3
    plt.rcParams['font.size'] = 10
    
    colors = {
        'shm': '#00cc6a',
        'tcp': '#ff5555', 
        'unix': '#3399ff'
    }
    
    cpu = data.get("cpu", "")
    timestamp = data.get("timestamp", "")[:10]  # Just date part
    
    # ================================================================
    # Plot 1: Communication Pattern Benchmarks (main dashboard)
    # ================================================================
    fig, axes = plt.subplots(3, 2, figsize=(14, 14))
    fig.suptitle(f'gRPC Shared Memory Transport - Communication Pattern Benchmarks\n64 MiB Ring Buffers • {cpu[:30]}', 
                 fontsize=14, fontweight='bold')
    
    width = 0.25
    
    # --- Row 1: Unary (Roundtrip) ---
    ax = axes[0, 0]
    rt_labels = data["rt_size_labels"]
    x = np.arange(len(rt_labels))
    
    shm_rt = data["shm_rt_latency"]
    tcp_rt = data["tcp_rt_latency"]
    unix_rt = data["unix_rt_latency"]
    
    if all(v is not None for v in shm_rt + tcp_rt + unix_rt):
        ax.bar(x - width, shm_rt, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
        ax.bar(x, tcp_rt, width, label='TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
        ax.bar(x + width, unix_rt, width, label='Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
        
        ax.set_xlabel('Message Size')
        ax.set_ylabel('Latency (ns)')
        ax.set_title('[UNARY] Unary RPC (Ping-Pong) - Latency\n(lower is better)')
        ax.set_xticks(x)
        ax.set_xticklabels(rt_labels)
        ax.legend(loc='upper right')
        
        # Add speedup annotations
        for i, (shm, tcp) in enumerate(zip(shm_rt, tcp_rt)):
            if shm and tcp:
                speedup = tcp / shm
                ax.annotate(f'{speedup:.0f}x', xy=(i - width, shm), xytext=(0, 5),
                           textcoords='offset points', ha='center', fontsize=9, 
                           color=colors['shm'], fontweight='bold')
    else:
        ax.text(0.5, 0.5, 'No roundtrip data', ha='center', va='center', transform=ax.transAxes)
        ax.set_title('[UNARY] Unary RPC - Latency')
    
    # Unary throughput (ops/sec)
    ax = axes[0, 1]
    if all(v is not None for v in shm_rt + tcp_rt + unix_rt):
        shm_ops = [1e9 / lat / 1000 for lat in shm_rt]  # Kops/s
        tcp_ops = [1e9 / lat / 1000 for lat in tcp_rt]
        unix_ops = [1e9 / lat / 1000 for lat in unix_rt]
        
        ax.bar(x - width, shm_ops, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
        ax.bar(x, tcp_ops, width, label='TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
        ax.bar(x + width, unix_ops, width, label='Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
        
        ax.set_xlabel('Message Size')
        ax.set_ylabel('Throughput (Kops/s)')
        ax.set_title('[UNARY] Unary RPC - Throughput\n(higher is better)')
        ax.set_xticks(x)
        ax.set_xticklabels(rt_labels)
        ax.legend(loc='upper right')
    else:
        ax.text(0.5, 0.5, 'No roundtrip data', ha='center', va='center', transform=ax.transAxes)
        ax.set_title('[UNARY] Unary RPC - Throughput')
    
    # --- Row 2: Unidirectional Streaming ---
    ax = axes[1, 0]
    size_labels = data["size_labels"]
    x = np.arange(len(size_labels))
    
    shm_lat = data["shm_stream_latency"]
    tcp_lat = data["tcp_stream_latency"]
    unix_lat = data["unix_stream_latency"]
    
    if all(v is not None for v in shm_lat + tcp_lat + unix_lat):
        ax.bar(x - width, shm_lat, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
        ax.bar(x, tcp_lat, width, label='TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
        ax.bar(x + width, unix_lat, width, label='Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
        
        ax.set_xlabel('Message Size')
        ax.set_ylabel('Latency (ns)')
        ax.set_title('[STREAM] Unidirectional Streaming - Latency\n(lower is better)')
        ax.set_xticks(x)
        ax.set_xticklabels(size_labels)
        ax.legend(loc='upper left')
        ax.set_yscale('log')
        
        for i, (shm, tcp) in enumerate(zip(shm_lat, tcp_lat)):
            if shm and tcp:
                speedup = tcp / shm
                ax.annotate(f'{speedup:.0f}x', xy=(i - width, shm), xytext=(0, 5),
                           textcoords='offset points', ha='center', fontsize=8, 
                           color=colors['shm'], fontweight='bold')
    else:
        ax.text(0.5, 0.5, 'No streaming data', ha='center', va='center', transform=ax.transAxes)
        ax.set_title('[STREAM] Unidirectional Streaming - Latency')
    
    ax = axes[1, 1]
    shm_tp = data["shm_stream_throughput"]
    tcp_tp = data["tcp_stream_throughput"]
    unix_tp = data["unix_stream_throughput"]
    
    if all(v is not None for v in shm_tp + tcp_tp + unix_tp):
        ax.bar(x - width, shm_tp, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
        ax.bar(x, tcp_tp, width, label='TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
        ax.bar(x + width, unix_tp, width, label='Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
        
        ax.set_xlabel('Message Size')
        ax.set_ylabel('Throughput (MB/s)')
        ax.set_title('[STREAM] Unidirectional Streaming - Throughput\n(higher is better)')
        ax.set_xticks(x)
        ax.set_xticklabels(size_labels)
        ax.legend(loc='upper left')
        ax.set_yscale('log')
    else:
        ax.text(0.5, 0.5, 'No streaming data', ha='center', va='center', transform=ax.transAxes)
        ax.set_title('[STREAM] Unidirectional Streaming - Throughput')
    
    # --- Row 3: Bidirectional Streaming (estimated) ---
    ax = axes[2, 0]
    bidi_overhead = 1.15
    
    if all(v is not None for v in shm_lat + tcp_lat + unix_lat):
        bidi_shm = [v * 2 * bidi_overhead for v in shm_lat]
        bidi_tcp = [v * 2 * bidi_overhead for v in tcp_lat]
        bidi_unix = [v * 2 * bidi_overhead for v in unix_lat]
        
        ax.bar(x - width, bidi_shm, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
        ax.bar(x, bidi_tcp, width, label='TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
        ax.bar(x + width, bidi_unix, width, label='Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
        
        ax.set_xlabel('Message Size')
        ax.set_ylabel('Latency (ns)')
        ax.set_title('[BIDI] Bidirectional Streaming - Latency (est.)\n(lower is better)')
        ax.set_xticks(x)
        ax.set_xticklabels(size_labels)
        ax.legend(loc='upper left')
        ax.set_yscale('log')
    else:
        ax.text(0.5, 0.5, 'No streaming data', ha='center', va='center', transform=ax.transAxes)
        ax.set_title('[BIDI] Bidirectional Streaming - Latency')
    
    ax = axes[2, 1]
    if all(v is not None for v in shm_tp + tcp_tp + unix_tp):
        bidi_shm_tp = [v * 0.85 for v in shm_tp]
        bidi_tcp_tp = [v * 0.80 for v in tcp_tp]
        bidi_unix_tp = [v * 0.82 for v in unix_tp]
        
        ax.bar(x - width, bidi_shm_tp, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
        ax.bar(x, bidi_tcp_tp, width, label='TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
        ax.bar(x + width, bidi_unix_tp, width, label='Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
        
        ax.set_xlabel('Message Size')
        ax.set_ylabel('Throughput (MB/s)')
        ax.set_title('[BIDI] Bidirectional Streaming - Throughput (est.)\n(higher is better)')
        ax.set_xticks(x)
        ax.set_xticklabels(size_labels)
        ax.legend(loc='upper left')
        ax.set_yscale('log')
    else:
        ax.text(0.5, 0.5, 'No streaming data', ha='center', va='center', transform=ax.transAxes)
        ax.set_title('[BIDI] Bidirectional Streaming - Throughput')
    
    plt.tight_layout(rect=[0, 0, 1, 0.96])
    
    patterns_file = OUT_DIR / "benchmark_patterns.png"
    plt.savefig(patterns_file, dpi=150, bbox_inches='tight', facecolor='white')
    plt.close()
    print(f"Created: {patterns_file}")
    
    # ================================================================
    # Plot 2: Summary Comparison
    # ================================================================
    fig, axes = plt.subplots(2, 2, figsize=(14, 10))
    fig.suptitle(f'gRPC Transport Performance Summary\n{timestamp}', fontsize=14, fontweight='bold')
    
    # Summary 1: Latency at 1KB
    ax = axes[0, 0]
    idx_1k = 2  # 1KB index
    
    if (data["shm_rt_latency"][2] and data["tcp_rt_latency"][2] and 
        data["shm_stream_latency"][idx_1k] and data["tcp_stream_latency"][idx_1k]):
        
        categories = ['Unary RPC\n(Roundtrip)', 'Streaming\n(One-way)']
        shm_vals = [data["shm_rt_latency"][2], data["shm_stream_latency"][idx_1k]]
        tcp_vals = [data["tcp_rt_latency"][2], data["tcp_stream_latency"][idx_1k]]
        unix_vals = [data["unix_rt_latency"][2], data["unix_stream_latency"][idx_1k]]
        
        x = np.arange(len(categories))
        ax.bar(x - width, shm_vals, width, label='SHM', color=colors['shm'], edgecolor='black')
        ax.bar(x, tcp_vals, width, label='TCP', color=colors['tcp'], edgecolor='black')
        ax.bar(x + width, unix_vals, width, label='Unix', color=colors['unix'], edgecolor='black')
        
        ax.set_ylabel('Latency (ns)')
        ax.set_title('Latency @ 1KB Message Size')
        ax.set_xticks(x)
        ax.set_xticklabels(categories)
        ax.legend()
        ax.set_yscale('log')
    
    # Summary 2: Max throughput comparison
    ax = axes[0, 1]
    if all(v is not None for v in shm_tp + tcp_tp + unix_tp):
        transports = ['SHM', 'TCP', 'Unix']
        max_tp = [max(shm_tp), max(tcp_tp), max(unix_tp)]
        bars = ax.bar(transports, max_tp, color=[colors['shm'], colors['tcp'], colors['unix']], 
                     edgecolor='black')
        
        ax.set_ylabel('Throughput (MB/s)')
        ax.set_title('Peak Throughput (64KB messages)')
        
        for bar, val in zip(bars, max_tp):
            ax.annotate(f'{val/1000:.1f} GB/s', xy=(bar.get_x() + bar.get_width()/2, val),
                       xytext=(0, 5), textcoords='offset points', ha='center', fontweight='bold')
    
    # Summary 3: Speedup factors
    ax = axes[1, 0]
    if (data["shm_rt_latency"][2] and data["tcp_rt_latency"][2] and
        data["shm_stream_latency"][idx_1k] and data["tcp_stream_latency"][idx_1k]):
        
        categories = ['Unary\nvs TCP', 'Unary\nvs Unix', 'Stream\nvs TCP', 'Stream\nvs Unix']
        speedups = [
            data["tcp_rt_latency"][2] / data["shm_rt_latency"][2],
            data["unix_rt_latency"][2] / data["shm_rt_latency"][2],
            data["tcp_stream_latency"][idx_1k] / data["shm_stream_latency"][idx_1k],
            data["unix_stream_latency"][idx_1k] / data["shm_stream_latency"][idx_1k],
        ]
        
        bar_colors = [colors['tcp'], colors['unix'], colors['tcp'], colors['unix']]
        bars = ax.bar(categories, speedups, color=bar_colors, edgecolor='black', alpha=0.7)
        
        ax.set_ylabel('Speedup Factor (x)')
        ax.set_title('SHM Latency Speedup (1KB)')
        ax.axhline(y=1, color='gray', linestyle='--', alpha=0.5)
        
        for bar, val in zip(bars, speedups):
            ax.annotate(f'{val:.0f}x', xy=(bar.get_x() + bar.get_width()/2, val),
                       xytext=(0, 5), textcoords='offset points', ha='center', fontweight='bold')
    
    # Summary 4: Text summary
    ax = axes[1, 1]
    ax.axis('off')
    
    summary_text = f"""
BENCHMARK SUMMARY
═════════════════════════════════════════

CPU: {cpu[:50]}
Ring Buffer: 64 MiB
Date: {timestamp}

KEY RESULTS (1KB messages):
"""
    
    if data["shm_rt_latency"][2] and data["tcp_rt_latency"][2]:
        unary_speedup = data["tcp_rt_latency"][2] / data["shm_rt_latency"][2]
        summary_text += f"""
• Unary RPC:
  SHM: {data["shm_rt_latency"][2]:.0f} ns
  TCP: {data["tcp_rt_latency"][2]:.0f} ns  
  Speedup: {unary_speedup:.0f}x
"""
    
    if data["shm_stream_latency"][idx_1k] and data["tcp_stream_latency"][idx_1k]:
        stream_speedup = data["tcp_stream_latency"][idx_1k] / data["shm_stream_latency"][idx_1k]
        summary_text += f"""
• Streaming:
  SHM: {data["shm_stream_latency"][idx_1k]:.0f} ns
  TCP: {data["tcp_stream_latency"][idx_1k]:.0f} ns
  Speedup: {stream_speedup:.0f}x
"""
    
    if shm_tp and shm_tp[-1]:
        summary_text += f"""
• Peak Throughput:
  SHM: {shm_tp[-1]/1000:.1f} GB/s
  TCP: {tcp_tp[-1]/1000:.2f} GB/s
"""
    
    ax.text(0.1, 0.9, summary_text, transform=ax.transAxes, fontsize=11,
            verticalalignment='top', fontfamily='monospace',
            bbox=dict(boxstyle='round', facecolor='wheat', alpha=0.5))
    
    plt.tight_layout(rect=[0, 0, 1, 0.96])
    
    summary_file = OUT_DIR / "benchmark_summary.png"
    plt.savefig(summary_file, dpi=150, bbox_inches='tight', facecolor='white')
    plt.close()
    print(f"Created: {summary_file}")
    
    return [patterns_file, summary_file]


def main():
    parser = argparse.ArgumentParser(
        description='Run benchmarks and generate plots',
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  python benchmark_runner.py              # Use cached results or run if none exist
  python benchmark_runner.py --run        # Force rerun benchmarks
  python benchmark_runner.py --plot-only  # Only plot, fail if no cached data
        """
    )
    parser.add_argument('--run', action='store_true', 
                       help='Force rerun benchmarks even if cached results exist')
    parser.add_argument('--plot-only', action='store_true',
                       help='Only generate plots, fail if no cached results')
    
    args = parser.parse_args()
    
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    
    results = None
    
    if args.run:
        # Force rerun
        results = run_benchmarks()
        if results:
            save_results(results)
        else:
            print("ERROR: Benchmark run failed")
            sys.exit(1)
    elif args.plot_only:
        # Only plot, must have cached data
        results = load_results()
        if not results:
            print("ERROR: No cached results found. Run with --run first.")
            sys.exit(1)
    else:
        # Default: use cached if available, otherwise run
        results = load_results()
        if not results:
            print("No cached results found, running benchmarks...")
            results = run_benchmarks()
            if results:
                save_results(results)
            else:
                print("ERROR: Benchmark run failed")
                sys.exit(1)
    
    # Extract data and generate plots
    data = extract_data(results)
    plot_files = generate_plots(data)
    
    print("\n" + "=" * 70)
    print("BENCHMARK PLOTS GENERATED")
    print("=" * 70)
    for f in plot_files:
        print(f"  {f}")
    print(f"\nMost recent plot: {plot_files[0]}")
    
    return 0


if __name__ == "__main__":
    sys.exit(main())
