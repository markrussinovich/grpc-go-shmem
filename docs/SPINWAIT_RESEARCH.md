# Research: Optimizing Ping-Pong Latency in Shared Memory Transport

## Executive Summary

Our current SHM transport achieves **60x faster one-way streaming** compared to TCP (126ns vs 7,663ns), but **roundtrip/unary RPC is slower than Unix sockets** (~23µs vs ~9.5µs). This document analyzes optimization strategies based on research into high-performance concurrent queue implementations.

## Current State

### Performance Measurements
| Operation | SHM | Unix Socket | TCP Loopback |
|-----------|-----|-------------|--------------|
| One-way (64B) | 126 ns | 2,187 ns | 7,663 ns |
| Roundtrip | ~23 µs | ~9.5 µs | ~20 µs |

### Root Cause
In ping-pong patterns, every request-response cycle requires:
1. Writer commits data → calls futex wake (syscall)
2. Reader wakes up, processes, writes response
3. Repeat

The futex syscall overhead (~10µs per wake) dominates roundtrip latency.

**Contrast with streaming**: Our `dataWaiters` optimization eliminates unnecessary wakes when readers aren't actually waiting, making streaming very fast.

## Research Findings

### 1. Facebook Folly - Adaptive Spin Strategy

Folly's concurrent queues use a sophisticated **three-phase waiting strategy**:

```cpp
// Phase 1: Spin with pause instructions (2 microseconds default)
spin_pause_until(deadline, opt, condition);

// Phase 2: Yield-based spinning (give up CPU slice)
spin_yield_until(deadline, condition);

// Phase 3: Block on futex
futexWait(&state, expected, deadline);
```

**Key parameters** from `WaitOptions.h`:
- Default spin max: **2 microseconds**
- Reasoning: "On circa-2013 devbox hardware, it costs about 7 usec to FUTEX_WAIT and then be awoken"

**Adaptive adjustment**: Folly's `TurnSequencer` dynamically adjusts spin cutoff based on success rate:
- If spinning succeeds: increase cutoff
- If spinning times out: decrease cutoff
- Uses exponential moving average (7/8 weight)

### 2. PAUSE Instruction Benefits

From Folly's `asm_volatile_pause()`:
- Reduces power consumption during spinning
- Gives capabilities to hyperthread sibling
- Takes ~7ns on modern x86 (0.5ns is the actual pause, rest is loop overhead)
- Critical for not starving other threads

### 3. SPSCQueue (rigtorp) - Simple but Fast

SPSCQueue achieves **133ns RTT** (roundtrip time) using:
- Lock-free ring buffer with cached indices
- Pure spinning with no blocking (for lowest latency)
- No futex at all - relies on `front()` polling

**Tradeoff**: Burns CPU cycles, not suitable for all workloads.

### 4. Hybrid Approaches (Folly's UnboundedQueue)

Folly provides two modes:
- `spin only`: Pure spinning, lowest latency (~5ns per op)
- `may block`: Spin then futex, slightly higher latency (~35ns) but CPU-friendly

**Performance comparison** (from UnboundedQueue benchmarks):
```
SPSC try   spin only        5 ns      5 ns      5 ns
SPSC wait  spin only        6 ns      6 ns      5 ns
SPSC try   may block       38 ns     37 ns     35 ns
SPSC wait  may block       34 ns     34 ns     33 ns
```

## Recommended Strategy for SHM Transport

### Proposal: Adaptive Spin-Wait Before Futex

Implement a **spin-then-futex** strategy for the reader:

```go
const (
    // Spin budget before falling back to futex
    // 2µs ≈ 4000 iterations at ~0.5ns/pause on modern CPUs
    SpinIterations = 4000
    
    // Adaptive parameters
    MinSpinLimit = 200
    MaxSpinLimit = 20000
)

func (r *ShmRing) waitForDataWithSpin(ctx context.Context) error {
    // Phase 1: Fast spin with pause
    for i := 0; i < SpinIterations; i++ {
        if r.hasData() {
            return nil
        }
        runtime_procyield(1) // Go's PAUSE equivalent
    }
    
    // Phase 2: Fall back to futex
    return r.waitForDataFutex(ctx)
}
```

### Implementation Details

#### 1. Add `runtime_procyield` linkname

```go
//go:linkname runtime_procyield runtime.procyield
func runtime_procyield(cycles uint32)
```

This calls x86 PAUSE or ARM YIELD instruction.

#### 2. Adaptive Spin Cutoff

Track spin success/failure and adjust cutoff:

```go
type SpinState struct {
    cutoff atomic.Uint32 // Current spin iteration limit
}

func (s *SpinState) recordSuccess(iterations int) {
    // Increase cutoff if we succeeded quickly
    prev := s.cutoff.Load()
    target := min(MaxSpinLimit, uint32(iterations*2))
    // Exponential moving average: (7*prev + target) / 8
    s.cutoff.Store((7*prev + target) / 8)
}

func (s *SpinState) recordTimeout() {
    // Decrease cutoff on timeout
    prev := s.cutoff.Load()
    s.cutoff.Store(max(MinSpinLimit, prev/2))
}
```

#### 3. Conditional Spinning Based on Load

Only spin when it makes sense:

```go
func (r *ShmRing) shouldSpin() bool {
    // Check if there are dataWaiters > 0 (meaning reader is blocking)
    // If yes, writer will wake us anyway, spinning may waste CPU
    // If no, we're in a tight ping-pong and should spin
    return atomic.LoadUint32(r.header().dataWaiters) == 0
}
```

### Expected Impact

Based on Folly benchmarks:
- **Best case** (spin succeeds): Reduce roundtrip from ~23µs to ~5-10µs
- **Typical case**: Match or beat Unix socket (~9.5µs)
- **Worst case** (spin fails): Add ~2µs overhead before futex

### Alternative: Busy-Poll Mode

For latency-critical applications, offer an opt-in busy-poll mode:

```go
type ShmDialOption struct {
    // BusyPoll enables pure spinning without futex for minimum latency
    // Warning: This will consume 100% CPU on idle connections
    BusyPoll bool
    
    // SpinMicroseconds controls spin duration before futex (default: 2)
    SpinMicroseconds int
}
```

## Cross-Platform Considerations

| Platform | Spin Instruction | Notes |
|----------|------------------|-------|
| x86/x64 | PAUSE | ~7ns, benefits hyperthread |
| ARM64 | YIELD | Similar semantics |
| Other | runtime.Gosched() | Less efficient but works |

## Implementation Priority

1. **Phase 1**: Add basic spin-wait loop with fixed iterations (low risk)
2. **Phase 2**: Implement adaptive spin cutoff (medium complexity)
3. **Phase 3**: Add busy-poll mode as dial option (optional)

## References

1. [Folly WaitOptions](https://github.com/facebook/folly/blob/main/folly/synchronization/WaitOptions.h)
2. [Folly TurnSequencer](https://github.com/facebook/folly/blob/main/folly/detail/TurnSequencer.h)
3. [Folly SaturatingSemaphore](https://github.com/facebook/folly/blob/main/folly/synchronization/SaturatingSemaphore.h)
4. [SPSCQueue](https://github.com/rigtorp/SPSCQueue) - 133ns RTT benchmark
5. [Folly UnboundedQueue](https://github.com/facebook/folly/blob/main/folly/concurrency/UnboundedQueue.h)

## Conclusion

The key insight is that **spinning for a short period (2µs) before falling back to futex** can dramatically reduce roundtrip latency when data arrives quickly, which is exactly the ping-pong pattern. This is a well-proven technique used by Facebook Folly and other high-performance libraries.

Our `dataWaiters` optimization already handles the streaming case well. Adding spin-wait will complete the optimization story for unary RPC patterns.
