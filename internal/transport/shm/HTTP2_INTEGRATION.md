# HTTP/2 Integration for Shared Memory Transport

## Overview

This document describes the HTTP/2 integration for the gRPC shared memory transport, with a focus on cross-process synchronization using futex and preventing deadlocks in bidirectional streaming.

## Architecture

### Futex-Based Synchronization

The shared memory transport uses Linux futexes for efficient cross-process synchronization:

- **Data Sequence (`dataSeq`)**: Incremented when data is written; readers wait on this
- **Space Sequence (`spaceSeq`)**: Incremented when data is read; writers wait on this  
- **Contiguity Sequence (`contigSeq`)**: Incremented on reads to wake writers waiting for any space

This futex-based design provides:
- **Zero syscalls in the fast path**: When data/space is available, no kernel calls needed
- **Efficient blocking**: When waiting, threads sleep in kernel until woken by futex_wake
- **Cross-process safety**: Works across separate process address spaces

### Ring Buffer Design

Two ring buffers provide full-duplex communication:
- **Ring A**: Client → Server
- **Ring B**: Server → Client

Each ring is a power-of-2 circular buffer with:
- Atomic write/read indices (monotonically increasing)
- Capacity-based masking for wrap-around
- Header at offset 0, data area starting at offset 64

### Frame Protocol

HTTP/2-style framing provides message delineation:

```
Frame Header (16 bytes):
- uint32 length    // payload size
- uint32 streamID  // stream identifier
- uint8  type      // HEADERS, MESSAGE, TRAILERS, CANCEL, GOAWAY, PING, PONG
- uint8  flags     // frame-specific flags
- uint16 reserved
- uint32 reserved
```

Frame types mirror HTTP/2 semantics:
- **HEADERS**: Initial headers (method, authority, metadata)
- **MESSAGE**: gRPC message payload
- **TRAILERS**: Final status and metadata
- **CANCEL**: Stream cancellation
- **GOAWAY**: Connection shutdown
- **PING/PONG**: Keepalive

## Bidirectional Streaming Architecture

### The Deadlock Problem

In bidirectional streaming, a naive implementation can deadlock:

```
Scenario:
1. Client fills Server→Client buffer while trying to send more
2. Server fills Client→Server buffer while trying to send more
3. Both sides block on write()
4. Neither can read to drain buffers
5. DEADLOCK!
```

### Solution: Concurrent Read/Write

The key principle: **Never block both read and write simultaneously.**

Each side has independent goroutines:

```
┌─────────────┐                      ┌─────────────┐
│   Client    │                      │   Server    │
├─────────────┤                      ├─────────────┤
│ Reader      │◄──── Ring B ────────┤ Sender      │
│ Goroutine   │      (S→C)          │ Goroutine   │
│             │                      │             │
│ Sender      │────── Ring A ───────►│ Reader      │
│ Goroutine   │      (C→S)          │ Goroutine   │
└─────────────┘                      └─────────────┘
```

**Why this prevents deadlock:**
- Even if Ring A fills, Server's Reader can still drain it
- Even if Ring B fills, Client's Reader can still drain it
- No circular dependency between reads and writes

### Implementation Details

#### ShmStreamingClient

```go
type ShmStreamingClient struct {
    tx *ShmRing  // client → server
    rx *ShmRing  // server → client
    
    // Single reader goroutine
    // Dispatches frames to per-stream channels
    
    // Per-stream sender goroutines
    // Each stream has its own send queue
}
```

**Reader goroutine:**
- Blocks on `readFrame(rx, ctx)`
- Dispatches to stream-specific channels
- Never blocks on send (uses buffered channels)

**Per-stream sender goroutines:**
- Pulls from buffered `sendQueue`
- Writes frames to ring buffer
- Can block on ring space, but doesn't prevent reading

#### ShmStreamingServer

```go
type ShmStreamingServer struct {
    tx *ShmRing  // server → client
    rx *ShmRing  // client → server
    
    // Single reader goroutine
    // Dispatches to stream handlers
    
    // Per-stream sender goroutines
    // Each stream has its own send queue
}
```

**Same architecture as client:**
- Independent reader and sender goroutines
- Buffered queues for backpressure
- Stream isolation

### Stream State Machine

```
Client Stream:
    CREATE → HEADERS → [MESSAGE]* → CLOSE_SEND → [RECV]* → TRAILERS → DONE
              ↓                                      ↑
              └──────── concurrent ─────────────────┘

Server Stream:
    NEW → HEADERS → RECV_LOOP ──┐
                       ↓         ↓
                    SEND_LOOP    │
                       ↓         ↓
                    TRAILERS → DONE
```

Both send and receive can happen concurrently within a stream.

## Testing

### Test Coverage

1. **TestBidirectionalStreamingNoDeadlock**
   - Client and server exchange 100 messages concurrently
   - Verifies no timeout/deadlock

2. **TestBidirectionalStreamingFullBuffers**
   - Uses small ring buffers (32KB)
   - Sends large messages (8KB) to force buffer full
   - Both sides send/receive concurrently
   - Verifies no deadlock when buffers fill

3. **TestConcurrentStreams**
   - Multiple streams (5) running simultaneously
   - Verifies stream isolation and concurrency

### Expected Behavior

With proper concurrent read/write:
- ✅ No deadlocks even when both buffers are full
- ✅ Backpressure handled gracefully via buffered queues
- ✅ Stream isolation (one blocked stream doesn't affect others)
- ✅ Clean cancellation and shutdown

## Performance Characteristics

### Advantages of Futex-Based Shared Memory

**vs TCP Loopback:**
- No kernel network stack overhead
- No socket syscalls on fast path
- Zero-copy message transfer
- Futex wait/wake is faster than socket select/epoll

**Estimated latency improvement:** 2-5x reduction

**Estimated throughput improvement:** 2-3x increase

### Memory Efficiency

- Fixed-size ring buffers (configurable, default 1MB each)
- No per-message allocation for transport
- Efficient wrap-around using power-of-2 masking

## Future Work

### Integration with gRPC Transport Interface

Currently, the streaming implementations are standalone. Full integration would require:

1. Implement `transport.ClientTransport` interface methods
2. Implement `transport.ServerTransport` interface methods
3. Wire into gRPC's internal transport selection
4. Handle metadata, compression, flow control per gRPC spec

### Additional Features

- Stream-level flow control (window updates)
- Connection-level settings frames
- Graceful GOAWAY handling
- Statistics and observability

## Conclusion

The shared memory transport provides:
- ✅ **Futex-based cross-process synchronization**
- ✅ **HTTP/2-style frame protocol**
- ✅ **Bidirectional streaming without deadlocks**
- ✅ **Concurrent read/write architecture**
- ✅ **High performance and low latency**

The key innovation is the **separation of read and write paths** via independent goroutines and buffered queues, ensuring that even when one ring buffer fills, the other direction can still make progress, preventing circular dependencies and deadlocks.
