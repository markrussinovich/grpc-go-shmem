#!/usr/bin/env python3
"""
Generate benchmark comparison plots for SHM vs TCP vs Unix socket transport performance.
"""

import matplotlib.pyplot as plt
import numpy as np
import os
import sys

# Benchmark data from actual test runs
sizes = ['64B', '256B', '1KB', '4KB', '16KB', '64KB']
sizes_bytes = [64, 256, 1024, 4096, 16384, 65536]

# One-way latency (ns/op)
shm_latency = [126.3, 144.5, 147.3, 198.3, 499.2, 1788]
tcp_latency = [7663, 8026, 6404, 7538, 11195, 25351]
unix_latency = [2187, 2220, 2646, 3223, 5213, 13012]

# Throughput (MB/s) - one-way
shm_throughput = [506.86, 1771.83, 6949.56, 20655.31, 32822.56, 36652.51]
tcp_throughput = [8.35, 31.90, 159.89, 543.41, 1463.50, 2585.14]
unix_throughput = [29.26, 115.29, 387.05, 1270.92, 3142.86, 5036.76]

# Roundtrip latency (ns/op) - for unary RPC comparison
# Updated with spin-wait optimization (2026-01-10)
rt_sizes = ['64B', '256B', '1KB', '4KB']
shm_rt_latency = [650, 637, 679, 925]  # With spin-wait optimization (avg of 3 runs)
tcp_rt_latency = [18500, 18100, 18600, 20100]  # Average of 3 runs
unix_rt_latency = [9500, 9600, 9650, 11400]    # Average of 3 runs

# Output directory (segmented per-platform)
platform_dir = "windows" if os.name == "nt" else ("linux" if sys.platform.startswith("linux") else sys.platform)
out_dir = os.path.join(os.path.dirname(__file__), 'out', platform_dir)
os.makedirs(out_dir, exist_ok=True)

# Set style
plt.style.use('seaborn-v0_8-whitegrid')
colors = {'shm': '#2ecc71', 'tcp': '#e74c3c', 'unix': '#3498db', 'speedup': '#9b59b6'}

# Plot 1: One-Way Latency Comparison (log scale)
fig, ax = plt.subplots(figsize=(12, 6))
x = np.arange(len(sizes))
width = 0.25

bars1 = ax.bar(x - width, shm_latency, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
bars2 = ax.bar(x, unix_latency, width, label='Unix Socket', color=colors['unix'], edgecolor='black', linewidth=0.5)
bars3 = ax.bar(x + width, tcp_latency, width, label='TCP Loopback', color=colors['tcp'], edgecolor='black', linewidth=0.5)

ax.set_ylabel('Latency (ns/op)', fontsize=12)
ax.set_xlabel('Message Size', fontsize=12)
ax.set_title('One-Way Streaming Latency: SHM vs Unix Socket vs TCP', fontsize=14, fontweight='bold')
ax.set_xticks(x)
ax.set_xticklabels(sizes)
ax.legend(loc='upper left', fontsize=11)
ax.set_yscale('log')
ax.set_ylim(50, 100000)

plt.tight_layout()
plt.savefig(os.path.join(out_dir, 'latency_comparison.png'), dpi=150, bbox_inches='tight')
plt.close()
print(f"Created: {os.path.join(out_dir, 'latency_comparison.png')}")

# Plot 2: Roundtrip (Unary RPC) Latency Comparison
fig, ax = plt.subplots(figsize=(10, 6))
x = np.arange(len(rt_sizes))
width = 0.25

# Convert to microseconds for readability
bars1 = ax.bar(x - width, [l/1000 for l in shm_rt_latency], width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
bars2 = ax.bar(x, [l/1000 for l in unix_rt_latency], width, label='Unix Socket', color=colors['unix'], edgecolor='black', linewidth=0.5)
bars3 = ax.bar(x + width, [l/1000 for l in tcp_rt_latency], width, label='TCP Loopback', color=colors['tcp'], edgecolor='black', linewidth=0.5)

ax.set_ylabel('Roundtrip Latency (µs)', fontsize=12)
ax.set_xlabel('Message Size', fontsize=12)
ax.set_title('Unary RPC Roundtrip Latency Comparison', fontsize=14, fontweight='bold')
ax.set_xticks(x)
ax.set_xticklabels(rt_sizes)
ax.legend(loc='upper left', fontsize=11)

# Add value labels
for bar, val in zip(bars1, shm_rt_latency):
    ax.annotate(f'{val/1000:.1f}', xy=(bar.get_x() + bar.get_width()/2, bar.get_height()),
                xytext=(0, 3), textcoords='offset points', ha='center', va='bottom', fontsize=9)
for bar, val in zip(bars2, unix_rt_latency):
    ax.annotate(f'{val/1000:.1f}', xy=(bar.get_x() + bar.get_width()/2, bar.get_height()),
                xytext=(0, 3), textcoords='offset points', ha='center', va='bottom', fontsize=9)
for bar, val in zip(bars3, tcp_rt_latency):
    ax.annotate(f'{val/1000:.1f}', xy=(bar.get_x() + bar.get_width()/2, bar.get_height()),
                xytext=(0, 3), textcoords='offset points', ha='center', va='bottom', fontsize=9)

plt.tight_layout()
plt.savefig(os.path.join(out_dir, 'roundtrip_comparison.png'), dpi=150, bbox_inches='tight')
plt.close()
print(f"Created: {os.path.join(out_dir, 'roundtrip_comparison.png')}")

# Plot 3: Throughput Comparison (log scale)
fig, ax = plt.subplots(figsize=(12, 6))
x = np.arange(len(sizes))

bars1 = ax.bar(x - width, shm_throughput, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
bars2 = ax.bar(x, unix_throughput, width, label='Unix Socket', color=colors['unix'], edgecolor='black', linewidth=0.5)
bars3 = ax.bar(x + width, tcp_throughput, width, label='TCP Loopback', color=colors['tcp'], edgecolor='black', linewidth=0.5)

ax.set_ylabel('Throughput (MB/s)', fontsize=12)
ax.set_xlabel('Message Size', fontsize=12)
ax.set_title('One-Way Streaming Throughput', fontsize=14, fontweight='bold')
ax.set_xticks(x)
ax.set_xticklabels(sizes)
ax.legend(loc='upper left', fontsize=11)
ax.set_yscale('log')
ax.set_ylim(1, 100000)

plt.tight_layout()
plt.savefig(os.path.join(out_dir, 'throughput_comparison.png'), dpi=150, bbox_inches='tight')
plt.close()
print(f"Created: {os.path.join(out_dir, 'throughput_comparison.png')}")

# Plot 4: Speedup Factor for One-Way (SHM vs others)
fig, ax = plt.subplots(figsize=(10, 6))
x = np.arange(len(sizes))

speedup_vs_tcp = [tcp/shm for tcp, shm in zip(tcp_latency, shm_latency)]
speedup_vs_unix = [unix/shm for unix, shm in zip(unix_latency, shm_latency)]

width = 0.35
bars1 = ax.bar(x - width/2, speedup_vs_tcp, width, label='vs TCP Loopback', color=colors['tcp'], edgecolor='black', linewidth=0.5)
bars2 = ax.bar(x + width/2, speedup_vs_unix, width, label='vs Unix Socket', color=colors['unix'], edgecolor='black', linewidth=0.5)
ax.axhline(y=1, color='gray', linestyle='--', linewidth=1, alpha=0.7)

ax.set_ylabel('SHM Speedup Factor', fontsize=12)
ax.set_xlabel('Message Size', fontsize=12)
ax.set_title('SHM One-Way Streaming Speedup', fontsize=14, fontweight='bold')
ax.set_xticks(x)
ax.set_xticklabels(sizes)
ax.legend(loc='upper right', fontsize=11)
ax.set_ylim(0, 75)

# Add value labels
for bar, val in zip(bars1, speedup_vs_tcp):
    ax.annotate(f'{val:.0f}x', xy=(bar.get_x() + bar.get_width()/2, bar.get_height()),
                xytext=(0, 3), textcoords='offset points', ha='center', va='bottom', fontsize=9, fontweight='bold')
for bar, val in zip(bars2, speedup_vs_unix):
    ax.annotate(f'{val:.0f}x', xy=(bar.get_x() + bar.get_width()/2, bar.get_height()),
                xytext=(0, 3), textcoords='offset points', ha='center', va='bottom', fontsize=9, fontweight='bold')

plt.tight_layout()
plt.savefig(os.path.join(out_dir, 'speedup.png'), dpi=150, bbox_inches='tight')
plt.close()
print(f"Created: {os.path.join(out_dir, 'speedup.png')}")

# Plot 5: Latency vs Message Size (line plot)
fig, ax = plt.subplots(figsize=(10, 6))

ax.plot(sizes_bytes, shm_latency, 'o-', color=colors['shm'], linewidth=2, markersize=8, label='SHM')
ax.plot(sizes_bytes, unix_latency, 's-', color=colors['unix'], linewidth=2, markersize=8, label='Unix Socket')
ax.plot(sizes_bytes, tcp_latency, '^-', color=colors['tcp'], linewidth=2, markersize=8, label='TCP Loopback')

ax.set_ylabel('Latency (ns/op)', fontsize=12)
ax.set_xlabel('Message Size (bytes)', fontsize=12)
ax.set_title('One-Way Latency Scaling with Message Size', fontsize=14, fontweight='bold')
ax.set_xscale('log')
ax.set_yscale('log')
ax.legend(loc='upper left', fontsize=11)
ax.grid(True, alpha=0.3)

plt.tight_layout()
plt.savefig(os.path.join(out_dir, 'latency_scaling.png'), dpi=150, bbox_inches='tight')
plt.close()
print(f"Created: {os.path.join(out_dir, 'latency_scaling.png')}")

# Plot 6: Throughput vs Message Size (line plot)
fig, ax = plt.subplots(figsize=(10, 6))

ax.plot(sizes_bytes, [t/1000 for t in shm_throughput], 'o-', color=colors['shm'], linewidth=2, markersize=8, label='SHM')
ax.plot(sizes_bytes, [t/1000 for t in unix_throughput], 's-', color=colors['unix'], linewidth=2, markersize=8, label='Unix Socket')
ax.plot(sizes_bytes, [t/1000 for t in tcp_throughput], '^-', color=colors['tcp'], linewidth=2, markersize=8, label='TCP Loopback')

ax.set_ylabel('Throughput (GB/s)', fontsize=12)
ax.set_xlabel('Message Size (bytes)', fontsize=12)
ax.set_title('One-Way Throughput Scaling with Message Size', fontsize=14, fontweight='bold')
ax.set_xscale('log')
ax.legend(loc='upper left', fontsize=11)
ax.grid(True, alpha=0.3)
ax.set_ylim(0, 40)

# Highlight peak throughput
ax.axhline(y=36.65, color=colors['shm'], linestyle='--', linewidth=1, alpha=0.5)
ax.annotate('SHM Peak: 36.7 GB/s', xy=(65536, 36.65), xytext=(10000, 38),
            fontsize=10, color=colors['shm'])

plt.tight_layout()
plt.savefig(os.path.join(out_dir, 'throughput_scaling.png'), dpi=150, bbox_inches='tight')
plt.close()
print(f"Created: {os.path.join(out_dir, 'throughput_scaling.png')}")

# Plot 7: Summary Dashboard
fig, axes = plt.subplots(2, 2, figsize=(14, 10))

# Subplot 1: One-way latency bars
ax = axes[0, 0]
x = np.arange(len(sizes))
bars1 = ax.bar(x - width, shm_latency, width, label='SHM', color=colors['shm'])
bars2 = ax.bar(x, unix_latency, width, label='Unix', color=colors['unix'])
bars3 = ax.bar(x + width, tcp_latency, width, label='TCP', color=colors['tcp'])
ax.set_ylabel('Latency (ns/op)')
ax.set_title('One-Way Streaming Latency')
ax.set_xticks(x)
ax.set_xticklabels(sizes)
ax.legend()
ax.set_yscale('log')

# Subplot 2: Roundtrip latency bars
ax = axes[0, 1]
x = np.arange(len(rt_sizes))
bars1 = ax.bar(x - width, [l/1000 for l in shm_rt_latency], width, label='SHM', color=colors['shm'])
bars2 = ax.bar(x, [l/1000 for l in unix_rt_latency], width, label='Unix', color=colors['unix'])
bars3 = ax.bar(x + width, [l/1000 for l in tcp_rt_latency], width, label='TCP', color=colors['tcp'])
ax.set_ylabel('Latency (µs)')
ax.set_title('Unary RPC Roundtrip Latency')
ax.set_xticks(x)
ax.set_xticklabels(rt_sizes)
ax.legend()

# Subplot 3: Throughput
ax = axes[1, 0]
x = np.arange(len(sizes))
bars1 = ax.bar(x - width, [t/1000 for t in shm_throughput], width, label='SHM', color=colors['shm'])
bars2 = ax.bar(x, [t/1000 for t in unix_throughput], width, label='Unix', color=colors['unix'])
bars3 = ax.bar(x + width, [t/1000 for t in tcp_throughput], width, label='TCP', color=colors['tcp'])
ax.set_ylabel('Throughput (GB/s)')
ax.set_title('One-Way Throughput')
ax.set_xticks(x)
ax.set_xticklabels(sizes)
ax.legend()
ax.set_yscale('log')

# Subplot 4: Summary text
ax = axes[1, 1]
ax.axis('off')
summary_text = """
SHM Transport Performance Summary
═══════════════════════════════════════

One-Way Streaming (best case):
  • SHM: 126 ns (64B) → 36.7 GB/s peak
  • Unix: 2,187 ns    → 5.0 GB/s peak
  • TCP:  7,663 ns    → 2.6 GB/s peak

  SHM is 17x faster than Unix, 61x faster than TCP

Unary RPC Roundtrip (64B):
  • Unix: 9.5 µs  ← Fastest (kernel optimized)
  • TCP:  20.1 µs
  • SHM:  23.0 µs ← Needs spin-wait optimization

Key Insight:
  SHM excels at streaming (no syscalls in
  steady-state). For unary RPC, futex wakes
  on every message add latency. Future work:
  spin-wait before futex to reduce latency.
"""
ax.text(0.1, 0.9, summary_text, transform=ax.transAxes, fontsize=11,
        verticalalignment='top', fontfamily='monospace',
        bbox=dict(boxstyle='round', facecolor='wheat', alpha=0.5))

plt.suptitle('SHM vs Unix Socket vs TCP Benchmark Results', fontsize=16, fontweight='bold', y=0.98)
plt.tight_layout(rect=[0, 0, 1, 0.96])
plt.savefig(os.path.join(out_dir, 'benchmark_dashboard.png'), dpi=150, bbox_inches='tight')
plt.close()
print(f"Created: {os.path.join(out_dir, 'benchmark_dashboard.png')}")

print(f"\nAll plots saved to: {out_dir}")

