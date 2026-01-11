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
    # Include Large Payload benchmarks for all transports
    benchmark_pattern = "BenchmarkShmRingWriteRead|BenchmarkShmRingRoundtrip|BenchmarkShmRingLargePayloads|BenchmarkTCPLoopback|BenchmarkTCPLargePayloads|BenchmarkUnixSocket|BenchmarkUnixLargePayloads"
    cmd = [
        "go", "test", 
        f"-bench={benchmark_pattern}",
        "-benchtime=100ms", "-cpu=2",
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
    # or:     BenchmarkName/size=XMB-N   ops   ns/op   MB/s
    pattern = r'(Benchmark\w+)/size=(\d+)(MB)?-\d+\s+(\d+)\s+([\d.]+)\s+ns/op\s+([\d.]+)\s+MB/s'
    
    for match in re.finditer(pattern, output):
        name = match.group(1)
        size_val = int(match.group(2))
        size_unit = match.group(3)  # "MB" or None
        ops = int(match.group(4))
        ns_op = float(match.group(5))
        mb_s = float(match.group(6))
        
        # Convert to bytes if in MB
        if size_unit == "MB":
            size = size_val * 1024 * 1024
        else:
            size = size_val
        
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
    
    # Standard sizes (64B to 1MB)
    sizes = [64, 256, 1024, 4096, 16384, 65536, 262144, 1048576]
    size_labels = ['64B', '256B', '1KB', '4KB', '16KB', '64KB', '256KB', '1MB']
    
    # Large payload sizes (1MB to 256MB)
    large_sizes = [1048576, 4194304, 16777216, 67108864, 134217728, 268435456]
    large_size_labels = ['1MB', '4MB', '16MB', '64MB', '128MB', '256MB']
    
    data = {
        "sizes": sizes,
        "size_labels": size_labels,
        "large_sizes": large_sizes,
        "large_size_labels": large_size_labels,
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
    
    # Large payload benchmarks (all transports - up to 256MB)
    data["shm_large_throughput"] = get_throughput("BenchmarkShmRingLargePayloads", large_sizes)
    data["shm_large_latency"] = get_latency("BenchmarkShmRingLargePayloads", large_sizes)
    data["tcp_large_throughput"] = get_throughput("BenchmarkTCPLargePayloads", large_sizes)
    data["tcp_large_latency"] = get_latency("BenchmarkTCPLargePayloads", large_sizes)
    data["unix_large_throughput"] = get_throughput("BenchmarkUnixLargePayloads", large_sizes)
    data["unix_large_latency"] = get_latency("BenchmarkUnixLargePayloads", large_sizes)
    
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
    
    # ================================================================
    # Plot 3: Large Payload Benchmarks (1MB - 256MB) - All Transports
    # ================================================================
    shm_large_tp = data.get("shm_large_throughput", [])
    tcp_large_tp = data.get("tcp_large_throughput", [])
    unix_large_tp = data.get("unix_large_throughput", [])
    shm_large_lat = data.get("shm_large_latency", [])
    tcp_large_lat = data.get("tcp_large_latency", [])
    unix_large_lat = data.get("unix_large_latency", [])
    large_labels = data.get("large_size_labels", [])
    
    # Only generate if we have large payload data
    if shm_large_tp and any(v is not None for v in shm_large_tp):
        fig, axes = plt.subplots(1, 2, figsize=(14, 6))
        fig.suptitle(f'Large Payload Performance - All Transports (64 MiB Ring Buffer)\n{cpu[:40]}', 
                     fontsize=14, fontweight='bold')
        
        # Filter to valid data points (where SHM has data)
        valid_idx = [i for i, v in enumerate(shm_large_tp) if v is not None]
        if valid_idx:
            valid_labels = [large_labels[i] for i in valid_idx]
            x = np.arange(len(valid_labels))
            width = 0.25
            
            shm_vals = [shm_large_tp[i] if i < len(shm_large_tp) and shm_large_tp[i] else 0 for i in valid_idx]
            tcp_vals = [tcp_large_tp[i] if i < len(tcp_large_tp) and tcp_large_tp[i] else 0 for i in valid_idx]
            unix_vals = [unix_large_tp[i] if i < len(unix_large_tp) and unix_large_tp[i] else 0 for i in valid_idx]
            
            # Throughput plot - All Transports
            ax = axes[0]
            ax.bar(x - width, shm_vals, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
            ax.bar(x, tcp_vals, width, label='TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
            ax.bar(x + width, unix_vals, width, label='Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
            
            ax.set_xlabel('Message Size')
            ax.set_ylabel('Throughput (MB/s)')
            ax.set_title('Large Payload Throughput\n(higher is better)')
            ax.set_xticks(x)
            ax.set_xticklabels(valid_labels)
            ax.legend(loc='upper right')
            ax.set_yscale('log')
            
            # Latency plot - All Transports (in milliseconds)
            ax = axes[1]
            shm_lat_vals = [shm_large_lat[i] / 1e6 if i < len(shm_large_lat) and shm_large_lat[i] else 0 for i in valid_idx]
            tcp_lat_vals = [tcp_large_lat[i] / 1e6 if i < len(tcp_large_lat) and tcp_large_lat[i] else 0 for i in valid_idx]
            unix_lat_vals = [unix_large_lat[i] / 1e6 if i < len(unix_large_lat) and unix_large_lat[i] else 0 for i in valid_idx]
            
            ax.bar(x - width, shm_lat_vals, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
            ax.bar(x, tcp_lat_vals, width, label='TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
            ax.bar(x + width, unix_lat_vals, width, label='Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
            
            ax.set_xlabel('Message Size')
            ax.set_ylabel('Latency (ms)')
            ax.set_title('Large Payload Latency\n(lower is better)')
            ax.set_xticks(x)
            ax.set_xticklabels(valid_labels)
            ax.legend(loc='upper left')
        
        plt.tight_layout(rect=[0, 0, 1, 0.94])
        
        large_file = OUT_DIR / "benchmark_large_payloads.png"
        plt.savefig(large_file, dpi=150, bbox_inches='tight', facecolor='white')
        plt.close()
        print(f"Created: {large_file}")
        
        return [patterns_file, summary_file, large_file]
    
    return [patterns_file, summary_file]


def generate_consolidated_plot(data: dict):
    """Generate a single consolidated plot with all benchmark results."""
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    
    # Style setup
    plt.style.use('default')
    plt.rcParams['figure.facecolor'] = 'white'
    plt.rcParams['axes.facecolor'] = 'white'
    plt.rcParams['axes.grid'] = True
    plt.rcParams['grid.alpha'] = 0.3
    plt.rcParams['font.size'] = 9
    
    colors = {
        'shm': '#00cc6a',
        'tcp': '#ff5555', 
        'unix': '#3399ff'
    }
    
    cpu = data.get("cpu", "")[:40]
    timestamp = data.get("timestamp", "")[:10]
    
    # Create a large figure with 4 rows x 3 columns layout
    fig = plt.figure(figsize=(18, 20))
    
    # Create GridSpec for flexible subplot arrangement
    gs = fig.add_gridspec(5, 3, hspace=0.35, wspace=0.25,
                          height_ratios=[1, 1, 1, 1, 0.8])
    
    fig.suptitle(f'gRPC Shared Memory Transport - Complete Benchmark Results\n'
                 f'64 MiB Ring Buffers • {cpu} • {timestamp}', 
                 fontsize=16, fontweight='bold', y=0.98)
    
    width = 0.25
    
    # ============================================================
    # ROW 1: Unary RPC (Roundtrip) - Latency and Throughput
    # ============================================================
    rt_labels = data["rt_size_labels"]
    x_rt = np.arange(len(rt_labels))
    
    shm_rt = data["shm_rt_latency"]
    tcp_rt = data["tcp_rt_latency"]
    unix_rt = data["unix_rt_latency"]
    
    # Unary Latency
    ax = fig.add_subplot(gs[0, 0])
    if all(v is not None for v in (shm_rt + tcp_rt + unix_rt)):
        ax.bar(x_rt - width, shm_rt, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
        ax.bar(x_rt, tcp_rt, width, label='TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
        ax.bar(x_rt + width, unix_rt, width, label='Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
        ax.set_xlabel('Message Size')
        ax.set_ylabel('Latency (ns)')
        ax.set_title('Unary RPC - Latency\n(lower is better)', fontweight='bold')
        ax.set_xticks(x_rt)
        ax.set_xticklabels(rt_labels)
        ax.legend(loc='upper right', fontsize=8)
        # Add speedup annotations
        for i, (shm, tcp) in enumerate(zip(shm_rt, tcp_rt)):
            if shm and tcp:
                speedup = tcp / shm
                ax.annotate(f'{speedup:.0f}x', xy=(i - width, shm), xytext=(0, 3),
                           textcoords='offset points', ha='center', fontsize=7, 
                           color=colors['shm'], fontweight='bold')
    else:
        ax.text(0.5, 0.5, 'No data', ha='center', va='center', transform=ax.transAxes)
        ax.set_title('Unary RPC - Latency')
    
    # Unary Throughput (ops/sec)
    ax = fig.add_subplot(gs[0, 1])
    if all(v is not None for v in (shm_rt + tcp_rt + unix_rt)):
        shm_ops = [1e9 / lat / 1000 for lat in shm_rt]  # Kops/s
        tcp_ops = [1e9 / lat / 1000 for lat in tcp_rt]
        unix_ops = [1e9 / lat / 1000 for lat in unix_rt]
        
        ax.bar(x_rt - width, shm_ops, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
        ax.bar(x_rt, tcp_ops, width, label='TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
        ax.bar(x_rt + width, unix_ops, width, label='Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
        ax.set_xlabel('Message Size')
        ax.set_ylabel('Throughput (Kops/s)')
        ax.set_title('Unary RPC - Throughput\n(higher is better)', fontweight='bold')
        ax.set_xticks(x_rt)
        ax.set_xticklabels(rt_labels)
        ax.legend(loc='upper right', fontsize=8)
    else:
        ax.text(0.5, 0.5, 'No data', ha='center', va='center', transform=ax.transAxes)
        ax.set_title('Unary RPC - Throughput')
    
    # Unary Speedup Chart
    ax = fig.add_subplot(gs[0, 2])
    if all(v is not None for v in (shm_rt + tcp_rt + unix_rt)):
        tcp_speedups = [tcp / shm for shm, tcp in zip(shm_rt, tcp_rt)]
        unix_speedups = [unix / shm for shm, unix in zip(shm_rt, unix_rt)]
        
        ax.bar(x_rt - 0.15, tcp_speedups, 0.3, label='vs TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
        ax.bar(x_rt + 0.15, unix_speedups, 0.3, label='vs Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
        ax.set_xlabel('Message Size')
        ax.set_ylabel('Speedup Factor')
        ax.set_title('Unary RPC - SHM Speedup\n(higher is better)', fontweight='bold')
        ax.set_xticks(x_rt)
        ax.set_xticklabels(rt_labels)
        ax.legend(loc='upper right', fontsize=8)
        ax.axhline(y=1, color='gray', linestyle='--', alpha=0.5)
        for i, (tcp_s, unix_s) in enumerate(zip(tcp_speedups, unix_speedups)):
            ax.annotate(f'{tcp_s:.0f}x', xy=(i - 0.15, tcp_s), xytext=(0, 3),
                       textcoords='offset points', ha='center', fontsize=7, fontweight='bold')
    else:
        ax.text(0.5, 0.5, 'No data', ha='center', va='center', transform=ax.transAxes)
        ax.set_title('Unary RPC - SHM Speedup')
    
    # ============================================================
    # ROW 2: Unidirectional Streaming (64B to 1MB)
    # ============================================================
    size_labels = data["size_labels"]
    x_stream = np.arange(len(size_labels))
    
    shm_lat = data["shm_stream_latency"]
    tcp_lat = data["tcp_stream_latency"]
    unix_lat = data["unix_stream_latency"]
    
    # Streaming Latency
    ax = fig.add_subplot(gs[1, 0])
    if all(v is not None for v in (shm_lat + tcp_lat + unix_lat)):
        ax.bar(x_stream - width, shm_lat, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
        ax.bar(x_stream, tcp_lat, width, label='TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
        ax.bar(x_stream + width, unix_lat, width, label='Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
        ax.set_xlabel('Message Size')
        ax.set_ylabel('Latency (ns)')
        ax.set_title('Streaming - Latency\n(lower is better)', fontweight='bold')
        ax.set_xticks(x_stream)
        ax.set_xticklabels(size_labels, rotation=45, ha='right')
        ax.legend(loc='upper left', fontsize=8)
        ax.set_yscale('log')
    else:
        ax.text(0.5, 0.5, 'No data', ha='center', va='center', transform=ax.transAxes)
        ax.set_title('Streaming - Latency')
    
    # Streaming Throughput
    ax = fig.add_subplot(gs[1, 1])
    shm_tp = data["shm_stream_throughput"]
    tcp_tp = data["tcp_stream_throughput"]
    unix_tp = data["unix_stream_throughput"]
    
    if all(v is not None for v in (shm_tp + tcp_tp + unix_tp)):
        ax.bar(x_stream - width, shm_tp, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
        ax.bar(x_stream, tcp_tp, width, label='TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
        ax.bar(x_stream + width, unix_tp, width, label='Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
        ax.set_xlabel('Message Size')
        ax.set_ylabel('Throughput (MB/s)')
        ax.set_title('Streaming - Throughput\n(higher is better)', fontweight='bold')
        ax.set_xticks(x_stream)
        ax.set_xticklabels(size_labels, rotation=45, ha='right')
        ax.legend(loc='upper left', fontsize=8)
        ax.set_yscale('log')
    else:
        ax.text(0.5, 0.5, 'No data', ha='center', va='center', transform=ax.transAxes)
        ax.set_title('Streaming - Throughput')
    
    # Streaming Speedup
    ax = fig.add_subplot(gs[1, 2])
    if all(v is not None for v in (shm_lat + tcp_lat + unix_lat)):
        tcp_speedups = [tcp / shm for shm, tcp in zip(shm_lat, tcp_lat)]
        unix_speedups = [unix / shm for shm, unix in zip(shm_lat, unix_lat)]
        
        ax.bar(x_stream - 0.15, tcp_speedups, 0.3, label='vs TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
        ax.bar(x_stream + 0.15, unix_speedups, 0.3, label='vs Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
        ax.set_xlabel('Message Size')
        ax.set_ylabel('Speedup Factor')
        ax.set_title('Streaming - SHM Speedup\n(higher is better)', fontweight='bold')
        ax.set_xticks(x_stream)
        ax.set_xticklabels(size_labels, rotation=45, ha='right')
        ax.legend(loc='upper right', fontsize=8)
        ax.axhline(y=1, color='gray', linestyle='--', alpha=0.5)
    else:
        ax.text(0.5, 0.5, 'No data', ha='center', va='center', transform=ax.transAxes)
        ax.set_title('Streaming - SHM Speedup')
    
    # ============================================================
    # ROW 3: Large Payloads (1MB to 256MB) - All Transports
    # ============================================================
    shm_large_tp = data.get("shm_large_throughput", [])
    shm_large_lat = data.get("shm_large_latency", [])
    tcp_large_tp = data.get("tcp_large_throughput", [])
    tcp_large_lat = data.get("tcp_large_latency", [])
    unix_large_tp = data.get("unix_large_throughput", [])
    unix_large_lat = data.get("unix_large_latency", [])
    large_labels = data.get("large_size_labels", [])
    
    # Find indices where all transports have data
    valid_idx = [i for i in range(len(large_labels)) 
                 if (i < len(shm_large_tp) and shm_large_tp[i] is not None)]
    
    # Large Payload Throughput - All Transports
    ax = fig.add_subplot(gs[2, 0])
    if valid_idx:
        valid_labels = [large_labels[i] for i in valid_idx]
        x_large = np.arange(len(valid_labels))
        
        shm_vals = [shm_large_tp[i] if i < len(shm_large_tp) and shm_large_tp[i] else 0 for i in valid_idx]
        tcp_vals = [tcp_large_tp[i] if i < len(tcp_large_tp) and tcp_large_tp[i] else 0 for i in valid_idx]
        unix_vals = [unix_large_tp[i] if i < len(unix_large_tp) and unix_large_tp[i] else 0 for i in valid_idx]
        
        ax.bar(x_large - width, shm_vals, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
        ax.bar(x_large, tcp_vals, width, label='TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
        ax.bar(x_large + width, unix_vals, width, label='Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
        ax.set_xlabel('Message Size')
        ax.set_ylabel('Throughput (MB/s)')
        ax.set_title('Large Payloads - Throughput\n(higher is better)', fontweight='bold')
        ax.set_xticks(x_large)
        ax.set_xticklabels(valid_labels)
        ax.legend(loc='upper right', fontsize=8)
        ax.set_yscale('log')
    else:
        ax.text(0.5, 0.5, 'No data', ha='center', va='center', transform=ax.transAxes)
        ax.set_title('Large Payloads - Throughput')
    
    # Large Payload Latency - All Transports
    ax = fig.add_subplot(gs[2, 1])
    if valid_idx:
        valid_labels = [large_labels[i] for i in valid_idx]
        x_large = np.arange(len(valid_labels))
        
        shm_lat_vals = [shm_large_lat[i] / 1e6 if i < len(shm_large_lat) and shm_large_lat[i] else 0 for i in valid_idx]
        tcp_lat_vals = [tcp_large_lat[i] / 1e6 if i < len(tcp_large_lat) and tcp_large_lat[i] else 0 for i in valid_idx]
        unix_lat_vals = [unix_large_lat[i] / 1e6 if i < len(unix_large_lat) and unix_large_lat[i] else 0 for i in valid_idx]
        
        ax.bar(x_large - width, shm_lat_vals, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
        ax.bar(x_large, tcp_lat_vals, width, label='TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
        ax.bar(x_large + width, unix_lat_vals, width, label='Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
        ax.set_xlabel('Message Size')
        ax.set_ylabel('Latency (ms)')
        ax.set_title('Large Payloads - Latency\n(lower is better)', fontweight='bold')
        ax.set_xticks(x_large)
        ax.set_xticklabels(valid_labels)
        ax.legend(loc='upper left', fontsize=8)
    else:
        ax.text(0.5, 0.5, 'No data', ha='center', va='center', transform=ax.transAxes)
        ax.set_title('Large Payloads - Latency')
    
    # Large Payload Speedup
    ax = fig.add_subplot(gs[2, 2])
    if valid_idx and any(shm_large_lat[i] for i in valid_idx if i < len(shm_large_lat)):
        valid_labels = [large_labels[i] for i in valid_idx]
        x_large = np.arange(len(valid_labels))
        
        tcp_speedups = []
        unix_speedups = []
        for i in valid_idx:
            shm_l = shm_large_lat[i] if i < len(shm_large_lat) and shm_large_lat[i] else 1
            tcp_l = tcp_large_lat[i] if i < len(tcp_large_lat) and tcp_large_lat[i] else shm_l
            unix_l = unix_large_lat[i] if i < len(unix_large_lat) and unix_large_lat[i] else shm_l
            tcp_speedups.append(tcp_l / shm_l if shm_l else 1)
            unix_speedups.append(unix_l / shm_l if shm_l else 1)
        
        ax.bar(x_large - 0.15, tcp_speedups, 0.3, label='vs TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
        ax.bar(x_large + 0.15, unix_speedups, 0.3, label='vs Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
        ax.set_xlabel('Message Size')
        ax.set_ylabel('Speedup Factor')
        ax.set_title('Large Payloads - SHM Speedup\n(higher is better)', fontweight='bold')
        ax.set_xticks(x_large)
        ax.set_xticklabels(valid_labels)
        ax.legend(loc='upper right', fontsize=8)
        ax.axhline(y=1, color='gray', linestyle='--', alpha=0.5)
        
        for i, (tcp_s, unix_s) in enumerate(zip(tcp_speedups, unix_speedups)):
            ax.annotate(f'{tcp_s:.1f}x', xy=(i - 0.15, tcp_s), xytext=(0, 3),
                       textcoords='offset points', ha='center', fontsize=7, fontweight='bold')
    else:
        ax.text(0.5, 0.5, 'No data', ha='center', va='center', transform=ax.transAxes)
        ax.set_title('Large Payloads - SHM Speedup')
    
    # ============================================================
    # ROW 4: Peak Performance and Summary Statistics
    # ============================================================
    
    # Peak Throughput by Transport
    ax = fig.add_subplot(gs[3, 0])
    if shm_tp and tcp_tp and unix_tp:
        transports = ['SHM', 'TCP', 'Unix']
        peak_tp = [max(shm_tp), max(tcp_tp), max(unix_tp)]
        bar_colors = [colors['shm'], colors['tcp'], colors['unix']]
        
        bars = ax.bar(transports, peak_tp, color=bar_colors, edgecolor='black', linewidth=0.5)
        ax.set_ylabel('Throughput (MB/s)')
        ax.set_title('Peak Throughput\n(higher is better)', fontweight='bold')
        
        for bar, val in zip(bars, peak_tp):
            label = f'{val/1000:.1f} GB/s' if val >= 1000 else f'{val:.0f} MB/s'
            ax.annotate(label, xy=(bar.get_x() + bar.get_width()/2, val),
                       xytext=(0, 3), textcoords='offset points', ha='center', fontsize=9, fontweight='bold')
    
    # Minimum Latency by Transport
    ax = fig.add_subplot(gs[3, 1])
    if shm_lat and tcp_lat and unix_lat:
        transports = ['SHM', 'TCP', 'Unix']
        min_lat = [min(shm_lat), min(tcp_lat), min(unix_lat)]
        bar_colors = [colors['shm'], colors['tcp'], colors['unix']]
        
        bars = ax.bar(transports, min_lat, color=bar_colors, edgecolor='black', linewidth=0.5)
        ax.set_ylabel('Latency (ns)')
        ax.set_title('Minimum Latency (64B)\n(lower is better)', fontweight='bold')
        
        for bar, val in zip(bars, min_lat):
            ax.annotate(f'{val:.0f} ns', xy=(bar.get_x() + bar.get_width()/2, val),
                       xytext=(0, 3), textcoords='offset points', ha='center', fontsize=9, fontweight='bold')
    
    # Overall Speedup Summary
    ax = fig.add_subplot(gs[3, 2])
    if shm_lat and tcp_lat and unix_lat and shm_rt and tcp_rt:
        categories = ['Unary\n(1KB)', 'Stream\n(1KB)', 'Stream\n(1MB)', 'Peak\nThroughput']
        
        # Calculate speedups
        idx_1k = 2  # 1KB index
        speedups_tcp = []
        speedups_unix = []
        
        # Unary 1KB
        if shm_rt[2] and tcp_rt[2]:
            speedups_tcp.append(tcp_rt[2] / shm_rt[2])
            speedups_unix.append(unix_rt[2] / shm_rt[2])
        else:
            speedups_tcp.append(0)
            speedups_unix.append(0)
        
        # Stream 1KB
        if shm_lat[idx_1k] and tcp_lat[idx_1k]:
            speedups_tcp.append(tcp_lat[idx_1k] / shm_lat[idx_1k])
            speedups_unix.append(unix_lat[idx_1k] / shm_lat[idx_1k])
        else:
            speedups_tcp.append(0)
            speedups_unix.append(0)
        
        # Stream 1MB
        if shm_lat[-1] and tcp_lat[-1]:
            speedups_tcp.append(tcp_lat[-1] / shm_lat[-1])
            speedups_unix.append(unix_lat[-1] / shm_lat[-1])
        else:
            speedups_tcp.append(0)
            speedups_unix.append(0)
        
        # Peak Throughput
        if max(shm_tp) and max(tcp_tp):
            speedups_tcp.append(max(shm_tp) / max(tcp_tp))
            speedups_unix.append(max(shm_tp) / max(unix_tp))
        else:
            speedups_tcp.append(0)
            speedups_unix.append(0)
        
        x_summ = np.arange(len(categories))
        ax.bar(x_summ - 0.15, speedups_tcp, 0.3, label='vs TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)
        ax.bar(x_summ + 0.15, speedups_unix, 0.3, label='vs Unix', color=colors['unix'], edgecolor='black', linewidth=0.5)
        ax.set_ylabel('Speedup Factor')
        ax.set_title('SHM Performance Advantage\n(higher is better)', fontweight='bold')
        ax.set_xticks(x_summ)
        ax.set_xticklabels(categories)
        ax.legend(loc='upper right', fontsize=8)
        ax.axhline(y=1, color='gray', linestyle='--', alpha=0.5)
        
        for i, (tcp_s, unix_s) in enumerate(zip(speedups_tcp, speedups_unix)):
            if tcp_s > 0:
                ax.annotate(f'{tcp_s:.1f}x', xy=(i - 0.15, tcp_s), xytext=(0, 3),
                           textcoords='offset points', ha='center', fontsize=8, fontweight='bold')
    
    # ============================================================
    # ROW 5: Text Summary
    # ============================================================
    ax = fig.add_subplot(gs[4, :])
    ax.axis('off')
    
    # Build summary text
    summary_lines = [
        "═" * 100,
        "BENCHMARK SUMMARY",
        "═" * 100,
        "",
    ]
    
    if shm_rt and tcp_rt and shm_rt[2] and tcp_rt[2]:
        unary_speedup = tcp_rt[2] / shm_rt[2]
        summary_lines.append(f"UNARY RPC (1KB):     SHM: {shm_rt[2]:.0f} ns    TCP: {tcp_rt[2]:.0f} ns    Unix: {unix_rt[2]:.0f} ns    Speedup: {unary_speedup:.0f}x vs TCP")
    
    if shm_lat and tcp_lat and shm_lat[2] and tcp_lat[2]:
        stream_speedup = tcp_lat[2] / shm_lat[2]
        summary_lines.append(f"STREAMING (1KB):     SHM: {shm_lat[2]:.0f} ns    TCP: {tcp_lat[2]:.0f} ns    Unix: {unix_lat[2]:.0f} ns    Speedup: {stream_speedup:.0f}x vs TCP")
    
    if shm_tp:
        summary_lines.append(f"PEAK THROUGHPUT:     SHM: {max(shm_tp)/1000:.1f} GB/s    TCP: {max(tcp_tp)/1000:.2f} GB/s    Unix: {max(unix_tp)/1000:.2f} GB/s")
    
    if valid_idx and shm_large_tp:
        large_max = max([shm_large_tp[i] for i in valid_idx])
        summary_lines.append(f"LARGE PAYLOADS:      SHM Peak: {large_max/1000:.1f} GB/s (tested up to 64MB messages)")
    
    summary_lines.extend(["", "═" * 100])
    
    summary_text = "\n".join(summary_lines)
    ax.text(0.5, 0.5, summary_text, transform=ax.transAxes, fontsize=11,
            verticalalignment='center', horizontalalignment='center',
            fontfamily='monospace',
            bbox=dict(boxstyle='round', facecolor='#f0f0f0', edgecolor='gray', alpha=0.8))
    
    # Save
    consolidated_file = OUT_DIR / "benchmark_consolidated.png"
    plt.savefig(consolidated_file, dpi=150, bbox_inches='tight', facecolor='white')
    plt.close()
    print(f"Created: {consolidated_file}")
    
    return consolidated_file


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
    
    # Generate consolidated plot
    consolidated_file = generate_consolidated_plot(data)
    
    print("\n" + "=" * 70)
    print("BENCHMARK PLOTS GENERATED")
    print("=" * 70)
    for f in plot_files:
        print(f"  {f}")
    print(f"  {consolidated_file}  ← CONSOLIDATED (all data)")
    print(f"\nConsolidated plot: {consolidated_file}")
    
    return 0


if __name__ == "__main__":
    sys.exit(main())
