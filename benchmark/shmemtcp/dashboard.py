#!/usr/bin/env python3
"""
Comprehensive Benchmark Dashboard for gRPC Shared Memory Transport

This script generates a single-page dashboard with all benchmark comparison
results between SHM, TCP, and Unix socket transports.

Run with: python3 dashboard.py
"""

import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
import numpy as np
from matplotlib.gridspec import GridSpec

# Set style
plt.style.use('seaborn-v0_8-whitegrid')

# Color palette
COLORS = {
    'shm': '#2ecc71',      # Green - fast
    'tcp': '#e74c3c',      # Red - slowest
    'unix': '#3498db',     # Blue - medium
    'accent': '#9b59b6',   # Purple - highlights
    'dark': '#2c3e50',     # Dark blue-gray
    'light': '#ecf0f1',    # Light gray
}

# =============================================================================
# BENCHMARK DATA (Updated 2025-06-05 with spin-wait optimization)
# =============================================================================

# Message sizes tested
sizes = [64, 256, 1024, 4096]
size_labels = ['64B', '256B', '1KB', '4KB']

# Roundtrip latency (nanoseconds) - from latest benchmark run
shm_rt_latency = [622, 582, 683, 921]      # ~600-920ns with spin-wait
tcp_rt_latency = [21878, 18629, 18012, 20645]
unix_rt_latency = [9920, 10488, 9866, 13133]

# Throughput (MB/s) - from latest benchmark run  
shm_throughput = [205.84, 879.82, 2998.44, 8892.76]
tcp_throughput = [5.85, 27.48, 113.70, 396.81]
unix_throughput = [12.90, 48.82, 207.59, 623.75]

# Large message throughput (MB/s)
large_sizes = [64, 256, 1024, 4096]  # KB
large_size_labels = ['64KB', '256KB', '1MB', '4MB']
shm_large_throughput = [27592, 16206, 15117, 18414]  # MB/s

# Latency percentiles (nanoseconds)
percentiles = ['min', 'p50', 'p90', 'p99', 'p999', 'max']
shm_percentiles = [460, 511, 571, 6573, 52268, 151853]

# Streaming throughput (MB/s) for 1024-byte messages
streaming_messages = [10, 100, 1000]
streaming_labels = ['10 msgs', '100 msgs', '1000 msgs']
client_streaming = [4234, 4734, 5699]
server_streaming = [3881, 4452, 5953]
bidi_streaming = [3402, 3391, 3923]

# Speedup factors
speedup_vs_tcp = [tcp/shm for tcp, shm in zip(tcp_rt_latency, shm_rt_latency)]
speedup_vs_unix = [unix/shm for unix, shm in zip(unix_rt_latency, shm_rt_latency)]

# =============================================================================
# CREATE DASHBOARD
# =============================================================================

fig = plt.figure(figsize=(20, 16))
fig.suptitle('gRPC Shared Memory Transport - Performance Dashboard', 
             fontsize=24, fontweight='bold', color=COLORS['dark'], y=0.98)

# Create grid layout
gs = GridSpec(4, 4, figure=fig, hspace=0.35, wspace=0.3,
              left=0.05, right=0.95, top=0.92, bottom=0.05)

# -----------------------------------------------------------------------------
# 1. Roundtrip Latency Comparison (top-left, 2x2)
# -----------------------------------------------------------------------------
ax1 = fig.add_subplot(gs[0:2, 0:2])

x = np.arange(len(sizes))
width = 0.25

bars1 = ax1.bar(x - width, shm_rt_latency, width, label='SHM', color=COLORS['shm'], edgecolor='white')
bars2 = ax1.bar(x, unix_rt_latency, width, label='Unix Socket', color=COLORS['unix'], edgecolor='white')
bars3 = ax1.bar(x + width, tcp_rt_latency, width, label='TCP', color=COLORS['tcp'], edgecolor='white')

ax1.set_xlabel('Message Size', fontsize=12, fontweight='bold')
ax1.set_ylabel('Roundtrip Latency (ns)', fontsize=12, fontweight='bold')
ax1.set_title('Roundtrip Latency Comparison', fontsize=14, fontweight='bold', pad=10)
ax1.set_xticks(x)
ax1.set_xticklabels(size_labels)
ax1.legend(loc='upper left', framealpha=0.9)
ax1.set_yscale('log')
ax1.set_ylim(100, 50000)

# Add value labels on bars
for bars in [bars1, bars2, bars3]:
    for bar in bars:
        height = bar.get_height()
        if height < 1000:
            label = f'{height:.0f}ns'
        else:
            label = f'{height/1000:.1f}µs'
        ax1.annotate(label,
                    xy=(bar.get_x() + bar.get_width() / 2, height),
                    xytext=(0, 3), textcoords="offset points",
                    ha='center', va='bottom', fontsize=7, rotation=45)

# -----------------------------------------------------------------------------
# 2. Speedup Factors (top-right, 2x1)
# -----------------------------------------------------------------------------
ax2 = fig.add_subplot(gs[0, 2:4])

x = np.arange(len(sizes))
width = 0.35

bars1 = ax2.bar(x - width/2, speedup_vs_tcp, width, label='vs TCP', color=COLORS['tcp'], alpha=0.8)
bars2 = ax2.bar(x + width/2, speedup_vs_unix, width, label='vs Unix', color=COLORS['unix'], alpha=0.8)

ax2.set_xlabel('Message Size', fontsize=11, fontweight='bold')
ax2.set_ylabel('Speedup Factor (×)', fontsize=11, fontweight='bold')
ax2.set_title('SHM Speedup vs Other Transports', fontsize=13, fontweight='bold', pad=10)
ax2.set_xticks(x)
ax2.set_xticklabels(size_labels)
ax2.legend(loc='upper right', framealpha=0.9)
ax2.axhline(y=1, color='gray', linestyle='--', alpha=0.5)

# Add value labels
for bars in [bars1, bars2]:
    for bar in bars:
        height = bar.get_height()
        ax2.annotate(f'{height:.0f}×',
                    xy=(bar.get_x() + bar.get_width() / 2, height),
                    xytext=(0, 3), textcoords="offset points",
                    ha='center', va='bottom', fontsize=9, fontweight='bold')

# -----------------------------------------------------------------------------
# 3. Throughput Comparison (second row right)
# -----------------------------------------------------------------------------
ax3 = fig.add_subplot(gs[1, 2:4])

x = np.arange(len(sizes))
width = 0.25

ax3.bar(x - width, shm_throughput, width, label='SHM', color=COLORS['shm'])
ax3.bar(x, unix_throughput, width, label='Unix Socket', color=COLORS['unix'])
ax3.bar(x + width, tcp_throughput, width, label='TCP', color=COLORS['tcp'])

ax3.set_xlabel('Message Size', fontsize=11, fontweight='bold')
ax3.set_ylabel('Throughput (MB/s)', fontsize=11, fontweight='bold')
ax3.set_title('Throughput Comparison (Log Scale)', fontsize=13, fontweight='bold', pad=10)
ax3.set_xticks(x)
ax3.set_xticklabels(size_labels)
ax3.legend(loc='upper left', framealpha=0.9)
ax3.set_yscale('log')

# -----------------------------------------------------------------------------
# 4. Large Message Throughput (third row left)
# -----------------------------------------------------------------------------
ax4 = fig.add_subplot(gs[2, 0:2])

x = np.arange(len(large_sizes))
bars = ax4.bar(x, shm_large_throughput, color=COLORS['shm'], edgecolor='white', linewidth=2)

ax4.set_xlabel('Message Size', fontsize=11, fontweight='bold')
ax4.set_ylabel('Throughput (MB/s)', fontsize=11, fontweight='bold')
ax4.set_title('SHM Large Message Throughput', fontsize=13, fontweight='bold', pad=10)
ax4.set_xticks(x)
ax4.set_xticklabels(large_size_labels)

# Add value labels
for bar in bars:
    height = bar.get_height()
    ax4.annotate(f'{height/1000:.1f} GB/s',
                xy=(bar.get_x() + bar.get_width() / 2, height),
                xytext=(0, 3), textcoords="offset points",
                ha='center', va='bottom', fontsize=10, fontweight='bold')

# Add horizontal line at 10 GB/s
ax4.axhline(y=10000, color=COLORS['accent'], linestyle='--', alpha=0.7, label='10 GB/s')
ax4.legend(loc='lower right')

# -----------------------------------------------------------------------------
# 5. Latency Percentiles (third row right)
# -----------------------------------------------------------------------------
ax5 = fig.add_subplot(gs[2, 2:4])

x = np.arange(len(percentiles))
colors = [COLORS['shm'] if p not in ['p99', 'p999', 'max'] else COLORS['accent'] 
          for p in percentiles]
bars = ax5.bar(x, shm_percentiles, color=colors, edgecolor='white', linewidth=2)

ax5.set_xlabel('Percentile', fontsize=11, fontweight='bold')
ax5.set_ylabel('Latency (ns)', fontsize=11, fontweight='bold')
ax5.set_title('SHM Latency Distribution (1KB Messages)', fontsize=13, fontweight='bold', pad=10)
ax5.set_xticks(x)
ax5.set_xticklabels(percentiles)
ax5.set_yscale('log')

# Add value labels
for bar in bars:
    height = bar.get_height()
    if height < 1000:
        label = f'{height:.0f}ns'
    else:
        label = f'{height/1000:.1f}µs'
    ax5.annotate(label,
                xy=(bar.get_x() + bar.get_width() / 2, height),
                xytext=(0, 3), textcoords="offset points",
                ha='center', va='bottom', fontsize=9, fontweight='bold')

# -----------------------------------------------------------------------------
# 6. Streaming Throughput (bottom row)
# -----------------------------------------------------------------------------
ax6 = fig.add_subplot(gs[3, 0:2])

x = np.arange(len(streaming_messages))
width = 0.25

bars1 = ax6.bar(x - width, client_streaming, width, label='Client Streaming', 
                color=COLORS['shm'], alpha=0.9)
bars2 = ax6.bar(x, server_streaming, width, label='Server Streaming', 
                color=COLORS['unix'], alpha=0.9)
bars3 = ax6.bar(x + width, bidi_streaming, width, label='Bidirectional', 
                color=COLORS['accent'], alpha=0.9)

ax6.set_xlabel('Messages per Stream', fontsize=11, fontweight='bold')
ax6.set_ylabel('Throughput (MB/s)', fontsize=11, fontweight='bold')
ax6.set_title('SHM Streaming Patterns Performance', fontsize=13, fontweight='bold', pad=10)
ax6.set_xticks(x)
ax6.set_xticklabels(streaming_labels)
ax6.legend(loc='upper left', framealpha=0.9)

# Add value labels
for bars in [bars1, bars2, bars3]:
    for bar in bars:
        height = bar.get_height()
        ax6.annotate(f'{height/1000:.1f}',
                    xy=(bar.get_x() + bar.get_width() / 2, height),
                    xytext=(0, 3), textcoords="offset points",
                    ha='center', va='bottom', fontsize=8)

ax6.set_ylabel('Throughput (MB/s)  [labels: GB/s]', fontsize=11, fontweight='bold')

# -----------------------------------------------------------------------------
# 7. Summary Stats Panel (bottom right)
# -----------------------------------------------------------------------------
ax7 = fig.add_subplot(gs[3, 2:4])
ax7.axis('off')

# Calculate summary stats
avg_speedup_tcp = np.mean(speedup_vs_tcp)
avg_speedup_unix = np.mean(speedup_vs_unix)
min_latency = min(shm_rt_latency)
max_throughput = max(shm_large_throughput)
p99_latency = shm_percentiles[3]  # p99

summary_text = f"""
+-------------------------------------------------------------+
|            PERFORMANCE SUMMARY                              |
+-------------------------------------------------------------+
|                                                             |
|   * SHM vs TCP:     {avg_speedup_tcp:.0f}x faster (avg)                     |
|   * SHM vs Unix:    {avg_speedup_unix:.0f}x faster (avg)                     |
|                                                             |
|   > Min Latency:     {min_latency} ns  ({min_latency/1000:.2f} us)                      |
|   > P99 Latency:     {p99_latency} ns ({p99_latency/1000:.1f} us)                      |
|                                                             |
|   # Peak Throughput: {max_throughput/1000:.1f} GB/s (large messages)            |
|   # Streaming:       3.4 - 6.0 GB/s                         |
|                                                             |
|   [x] Zero kernel transitions in data path                  |
|   [x] Spin-wait optimization enabled                        |
|   [x] Lock-free ring buffer design                          |
|                                                             |
+-------------------------------------------------------------+
"""

ax7.text(0.5, 0.5, summary_text, transform=ax7.transAxes, fontsize=11,
        verticalalignment='center', horizontalalignment='center',
        fontfamily='monospace', 
        bbox=dict(boxstyle='round,pad=0.5', facecolor=COLORS['light'], 
                  edgecolor=COLORS['dark'], linewidth=2))

# -----------------------------------------------------------------------------
# Add footer
# -----------------------------------------------------------------------------
fig.text(0.5, 0.01, 
         'Benchmark Environment: AMD EPYC 7763 • Linux • Go 1.24 • gRPC-Go with SHM Transport',
         ha='center', va='bottom', fontsize=10, style='italic', color='gray')

fig.text(0.98, 0.01, 'Updated: 2025-06-05', ha='right', va='bottom', 
         fontsize=9, color='gray')

# Save dashboard
plt.savefig('benchmark_dashboard.png', dpi=150, bbox_inches='tight',
            facecolor='white', edgecolor='none')
plt.savefig('benchmark_dashboard.svg', bbox_inches='tight',
            facecolor='white', edgecolor='none')

print("Dashboard saved to:")
print("  - benchmark_dashboard.png")
print("  - benchmark_dashboard.svg")

# Also save a high-res version for printing
plt.savefig('benchmark_dashboard_highres.png', dpi=300, bbox_inches='tight',
            facecolor='white', edgecolor='none')
print("  - benchmark_dashboard_highres.png (300 DPI)")

# Close to avoid interactive display in headless environment
plt.close()
