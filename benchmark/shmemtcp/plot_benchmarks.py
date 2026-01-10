#!/usr/bin/env python3
"""
Generate benchmark comparison plots for SHM vs TCP transport performance.
"""

import matplotlib.pyplot as plt
import numpy as np
import os

# Benchmark data from actual test runs
sizes = ['64B', '256B', '1KB', '4KB', '16KB', '64KB']
sizes_bytes = [64, 256, 1024, 4096, 16384, 65536]

# One-way latency (ns/op)
shm_latency = [126.3, 144.5, 147.3, 198.3, 499.2, 1788]
tcp_latency = [7663, 8026, 6404, 7538, 11195, 25351]

# Throughput (MB/s)
shm_throughput = [506.86, 1771.83, 6949.56, 20655.31, 32822.56, 36652.51]
tcp_throughput = [8.35, 31.90, 159.89, 543.41, 1463.50, 2585.14]

# Calculate speedup
speedup = [tcp/shm for tcp, shm in zip(tcp_latency, shm_latency)]

# Output directory
out_dir = os.path.join(os.path.dirname(__file__), 'out')
os.makedirs(out_dir, exist_ok=True)

# Set style
plt.style.use('seaborn-v0_8-whitegrid')
colors = {'shm': '#2ecc71', 'tcp': '#e74c3c', 'speedup': '#3498db'}

# Plot 1: Latency Comparison (log scale)
fig, ax = plt.subplots(figsize=(10, 6))
x = np.arange(len(sizes))
width = 0.35

bars1 = ax.bar(x - width/2, shm_latency, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
bars2 = ax.bar(x + width/2, tcp_latency, width, label='TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)

ax.set_ylabel('Latency (ns/op)', fontsize=12)
ax.set_xlabel('Message Size', fontsize=12)
ax.set_title('One-Way Latency: SHM vs TCP Loopback', fontsize=14, fontweight='bold')
ax.set_xticks(x)
ax.set_xticklabels(sizes)
ax.legend(loc='upper left', fontsize=11)
ax.set_yscale('log')
ax.set_ylim(50, 100000)

# Add value labels on bars
for bar, val in zip(bars1, shm_latency):
    ax.annotate(f'{val:.0f}', xy=(bar.get_x() + bar.get_width()/2, bar.get_height()),
                xytext=(0, 3), textcoords='offset points', ha='center', va='bottom', fontsize=8)
for bar, val in zip(bars2, tcp_latency):
    ax.annotate(f'{val:.0f}', xy=(bar.get_x() + bar.get_width()/2, bar.get_height()),
                xytext=(0, 3), textcoords='offset points', ha='center', va='bottom', fontsize=8)

plt.tight_layout()
plt.savefig(os.path.join(out_dir, 'latency_comparison.png'), dpi=150, bbox_inches='tight')
plt.close()
print(f"Created: {os.path.join(out_dir, 'latency_comparison.png')}")

# Plot 2: Throughput Comparison (log scale)
fig, ax = plt.subplots(figsize=(10, 6))

bars1 = ax.bar(x - width/2, shm_throughput, width, label='SHM', color=colors['shm'], edgecolor='black', linewidth=0.5)
bars2 = ax.bar(x + width/2, tcp_throughput, width, label='TCP', color=colors['tcp'], edgecolor='black', linewidth=0.5)

ax.set_ylabel('Throughput (MB/s)', fontsize=12)
ax.set_xlabel('Message Size', fontsize=12)
ax.set_title('Throughput: SHM vs TCP Loopback', fontsize=14, fontweight='bold')
ax.set_xticks(x)
ax.set_xticklabels(sizes)
ax.legend(loc='upper left', fontsize=11)
ax.set_yscale('log')
ax.set_ylim(1, 100000)

plt.tight_layout()
plt.savefig(os.path.join(out_dir, 'throughput_comparison.png'), dpi=150, bbox_inches='tight')
plt.close()
print(f"Created: {os.path.join(out_dir, 'throughput_comparison.png')}")

# Plot 3: Speedup Factor
fig, ax = plt.subplots(figsize=(10, 6))

bars = ax.bar(x, speedup, width=0.6, color=colors['speedup'], edgecolor='black', linewidth=0.5)
ax.axhline(y=1, color='gray', linestyle='--', linewidth=1, alpha=0.7)

ax.set_ylabel('Speedup (TCP latency / SHM latency)', fontsize=12)
ax.set_xlabel('Message Size', fontsize=12)
ax.set_title('SHM Speedup vs TCP Loopback', fontsize=14, fontweight='bold')
ax.set_xticks(x)
ax.set_xticklabels(sizes)
ax.set_ylim(0, 70)

# Add value labels
for bar, val in zip(bars, speedup):
    ax.annotate(f'{val:.0f}x', xy=(bar.get_x() + bar.get_width()/2, bar.get_height()),
                xytext=(0, 3), textcoords='offset points', ha='center', va='bottom', fontsize=11, fontweight='bold')

plt.tight_layout()
plt.savefig(os.path.join(out_dir, 'speedup.png'), dpi=150, bbox_inches='tight')
plt.close()
print(f"Created: {os.path.join(out_dir, 'speedup.png')}")

# Plot 4: Latency vs Message Size (line plot)
fig, ax = plt.subplots(figsize=(10, 6))

ax.plot(sizes_bytes, shm_latency, 'o-', color=colors['shm'], linewidth=2, markersize=8, label='SHM')
ax.plot(sizes_bytes, tcp_latency, 's-', color=colors['tcp'], linewidth=2, markersize=8, label='TCP')

ax.set_ylabel('Latency (ns/op)', fontsize=12)
ax.set_xlabel('Message Size (bytes)', fontsize=12)
ax.set_title('Latency Scaling with Message Size', fontsize=14, fontweight='bold')
ax.set_xscale('log')
ax.set_yscale('log')
ax.legend(loc='upper left', fontsize=11)
ax.grid(True, alpha=0.3)

# Add annotations for key points
ax.annotate(f'61x faster', xy=(64, 126), xytext=(200, 50),
            arrowprops=dict(arrowstyle='->', color='gray'), fontsize=10)
ax.annotate(f'14x faster', xy=(65536, 1788), xytext=(20000, 500),
            arrowprops=dict(arrowstyle='->', color='gray'), fontsize=10)

plt.tight_layout()
plt.savefig(os.path.join(out_dir, 'latency_scaling.png'), dpi=150, bbox_inches='tight')
plt.close()
print(f"Created: {os.path.join(out_dir, 'latency_scaling.png')}")

# Plot 5: Throughput vs Message Size (line plot)
fig, ax = plt.subplots(figsize=(10, 6))

ax.plot(sizes_bytes, [t/1000 for t in shm_throughput], 'o-', color=colors['shm'], linewidth=2, markersize=8, label='SHM')
ax.plot(sizes_bytes, [t/1000 for t in tcp_throughput], 's-', color=colors['tcp'], linewidth=2, markersize=8, label='TCP')

ax.set_ylabel('Throughput (GB/s)', fontsize=12)
ax.set_xlabel('Message Size (bytes)', fontsize=12)
ax.set_title('Throughput Scaling with Message Size', fontsize=14, fontweight='bold')
ax.set_xscale('log')
ax.legend(loc='upper left', fontsize=11)
ax.grid(True, alpha=0.3)
ax.set_ylim(0, 40)

# Highlight peak throughput
ax.axhline(y=36.65, color=colors['shm'], linestyle='--', linewidth=1, alpha=0.5)
ax.annotate('Peak: 36.7 GB/s', xy=(65536, 36.65), xytext=(10000, 38),
            fontsize=10, color=colors['shm'])

plt.tight_layout()
plt.savefig(os.path.join(out_dir, 'throughput_scaling.png'), dpi=150, bbox_inches='tight')
plt.close()
print(f"Created: {os.path.join(out_dir, 'throughput_scaling.png')}")

# Plot 6: Summary Dashboard
fig, axes = plt.subplots(2, 2, figsize=(14, 10))

# Subplot 1: Latency bars
ax = axes[0, 0]
bars1 = ax.bar(x - width/2, shm_latency, width, label='SHM', color=colors['shm'])
bars2 = ax.bar(x + width/2, tcp_latency, width, label='TCP', color=colors['tcp'])
ax.set_ylabel('Latency (ns/op)')
ax.set_title('One-Way Latency')
ax.set_xticks(x)
ax.set_xticklabels(sizes)
ax.legend()
ax.set_yscale('log')

# Subplot 2: Throughput bars
ax = axes[0, 1]
bars1 = ax.bar(x - width/2, [t/1000 for t in shm_throughput], width, label='SHM', color=colors['shm'])
bars2 = ax.bar(x + width/2, [t/1000 for t in tcp_throughput], width, label='TCP', color=colors['tcp'])
ax.set_ylabel('Throughput (GB/s)')
ax.set_title('Throughput')
ax.set_xticks(x)
ax.set_xticklabels(sizes)
ax.legend()
ax.set_yscale('log')

# Subplot 3: Speedup
ax = axes[1, 0]
bars = ax.bar(x, speedup, width=0.6, color=colors['speedup'])
ax.axhline(y=1, color='gray', linestyle='--', linewidth=1)
ax.set_ylabel('Speedup Factor')
ax.set_title('SHM Speedup vs TCP')
ax.set_xticks(x)
ax.set_xticklabels(sizes)
for bar, val in zip(bars, speedup):
    ax.annotate(f'{val:.0f}x', xy=(bar.get_x() + bar.get_width()/2, bar.get_height()),
                xytext=(0, 3), textcoords='offset points', ha='center', va='bottom', fontsize=9, fontweight='bold')

# Subplot 4: Summary text
ax = axes[1, 1]
ax.axis('off')
summary_text = """
SHM Transport Performance Summary
═══════════════════════════════════════

Key Metrics (64B messages):
  • SHM Latency:  126 ns
  • TCP Latency:  7,663 ns
  • Speedup:      61x

Peak Throughput:
  • SHM: 36.7 GB/s (64KB messages)
  • TCP: 2.6 GB/s (64KB messages)

Optimizations Applied:
  ✓ dataWaiters counter (eliminates
    unnecessary futex wakeups)
  ✓ Zero-copy reads with pendingReadIdx
  ✓ Minimal syscalls in steady-state

Best Use Case:
  High-throughput streaming between
  local processes on the same machine.
"""
ax.text(0.1, 0.9, summary_text, transform=ax.transAxes, fontsize=11,
        verticalalignment='top', fontfamily='monospace',
        bbox=dict(boxstyle='round', facecolor='wheat', alpha=0.5))

plt.suptitle('SHM vs TCP Transport Benchmark Results', fontsize=16, fontweight='bold', y=0.98)
plt.tight_layout(rect=[0, 0, 1, 0.96])
plt.savefig(os.path.join(out_dir, 'benchmark_dashboard.png'), dpi=150, bbox_inches='tight')
plt.close()
print(f"Created: {os.path.join(out_dir, 'benchmark_dashboard.png')}")

print(f"\nAll plots saved to: {out_dir}")
