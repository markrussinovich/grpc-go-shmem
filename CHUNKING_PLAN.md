# Transport Layer Chunking Implementation Plan

## Problem Statement

Currently, the SHM transport layer will **fail** if a single frame (header + payload) exceeds
the ring buffer capacity. While the default capacity is 64 MiB (larger than gRPC's default
4 MiB message limit), this creates a hard dependency on buffer sizing that should be eliminated.

The `writeMessageChunked` function in `frame.go` exists but is **not used** by the transports.

## Current Architecture

### Write Paths

**Client Transport** (`shm_client_transport.go:877`):
```go
writeFrameBuffers(t.clientToServer, fh, hdr, data, s.ctx)
```

**Server Transport** (`shm_server_transport.go:865`):
```go
writeFrameBuffers(t.serverToClient, fh, hdr, data, context.Background())
```

### Existing Chunking Support

1. **ShmConn.WriteContext** (`conn.go:140-172`) - Already chunks raw writes to ring capacity
2. **writeMessageChunked** (`frame.go:624-646`) - Chunks MESSAGE frames with MORE flag
3. **MessageFlagMORE** - Frame flag indicating continuation data follows

### Ring Buffer Behavior

- `ReserveWrite` returns error if `n > capacity` (ring.go:943)
- `WriteBlocking` returns error if `len(data) > capacity` (ring.go:203)
- Default: 64 MiB capacity per ring

## Implementation Strategy

### Goal
Enable transport to handle messages of any size (up to gRPC limits) regardless of ring capacity,
**without regressing performance** for messages that fit in a single frame.

### Key Constraints

1. **Zero-copy for small messages**: Messages < ring capacity should still use single `writeFrameBuffers`
2. **Chunking for large messages**: Messages > (ring capacity - frame header) should auto-chunk
3. **Configurable chunk size**: Allow tuning for memory-constrained environments
4. **Backward compatible**: No changes to wire format (uses existing MORE flag)
5. **No polling**: Chunked writes must still block efficiently, not spin

### Phase 1: Add Configurable Chunk Size to Transports

Add a `maxFramePayload` field to both transports, defaulting to `0` (auto-detect from ring capacity).

```go
type ShmClientTransport struct {
    // ... existing fields ...
    maxFramePayload int // Maximum payload size per frame (0 = ring capacity - overhead)
}
```

### Phase 2: Implement Chunked Write Helper

Create a helper function that determines whether to use direct write or chunked write:

```go
// writeFrameBuffersChunked writes data, chunking if necessary for large payloads.
// If payloadSize <= maxFramePayload, uses single writeFrameBuffers (zero-copy path).
// Otherwise, chunks with MORE flag as per the protocol.
func writeFrameBuffersChunked(
    tx *ShmRing,
    fh FrameHeader,
    hdr []byte,
    data mem.BufferSlice,
    ctx context.Context,
    maxFramePayload int,
) error
```

### Phase 3: Integration Points

Update the `write` methods in both transports to use the chunked helper:

**Client Transport** (`ShmClientTransport.write`):
```go
if err := writeFrameBuffersChunked(t.clientToServer, fh, hdr, data, s.ctx, t.maxFramePayload); err != nil {
```

**Server Transport** (`ShmServerTransport.write`):
```go
if err := writeFrameBuffersChunked(t.serverToClient, fh, hdr, data, context.Background(), t.maxFramePayload); err != nil {
```

### Phase 4: Testing Strategy

1. **Small Ring Tests** - Create segments with 64KB rings, send 256KB messages
2. **Benchmark Comparison** - Compare small-ring chunked vs large-ring single-frame
3. **Existing Tests** - Ensure all 47 SHM tests still pass
4. **Large Message Tests** - Verify messages up to gRPC default max (4 MiB) work with small rings

### Test Cases to Add

```go
func TestShmLargeMessageWithSmallRing(t *testing.T)     // 256KB msg, 64KB ring
func TestShmChunkingPreservesData(t *testing.T)        // Data integrity across chunks
func TestShmChunkingWithFlowControl(t *testing.T)      // Flow control applies per-chunk
func BenchmarkShmChunking(b *testing.B)                // Performance with/without chunking
```

## Implementation Details

### Chunk Size Calculation

```go
const frameOverhead = frameHeaderSize // 16 bytes

func (t *ShmClientTransport) effectiveMaxPayload() int {
    if t.maxFramePayload > 0 {
        return t.maxFramePayload
    }
    // Use 90% of ring capacity to leave room for other frames
    ringCap := int(t.clientToServer.Capacity())
    maxPayload := ringCap - frameOverhead
    if maxPayload > 4*1024*1024 { // Cap at 4MB for reasonable chunking
        maxPayload = 4 * 1024 * 1024
    }
    return maxPayload
}
```

### Chunked Write Algorithm

```go
func writeFrameBuffersChunked(tx *ShmRing, fh FrameHeader, hdr []byte, data mem.BufferSlice, ctx context.Context, maxPayload int) error {
    totalPayload := len(hdr) + data.Len()

    // Fast path: fits in single frame
    if totalPayload <= maxPayload {
        return writeFrameBuffers(tx, fh, hdr, data, ctx)
    }

    // Slow path: chunk the payload
    // First frame includes hdr, subsequent frames are data-only
    remaining := data
    firstChunk := true

    for firstChunk || remaining.Len() > 0 {
        var chunkData mem.BufferSlice
        var chunkHdr []byte

        if firstChunk {
            chunkHdr = hdr
            // Calculate how much data fits with header
            dataInFirst := maxPayload - len(hdr)
            if dataInFirst > remaining.Len() {
                dataInFirst = remaining.Len()
            }
            chunkData, remaining = splitBufferSlice(remaining, dataInFirst)
            firstChunk = false
        } else {
            // Pure data chunks
            chunkSize := maxPayload
            if chunkSize > remaining.Len() {
                chunkSize = remaining.Len()
            }
            chunkData, remaining = splitBufferSlice(remaining, chunkSize)
        }

        chunkFh := fh
        if remaining.Len() > 0 {
            chunkFh.Flags |= MessageFlagMORE
        }

        if err := writeFrameBuffers(tx, chunkFh, chunkHdr, chunkData, ctx); err != nil {
            return err
        }
        chunkHdr = nil // Only first chunk has header
    }

    return nil
}
```

## Performance Considerations

1. **Hot Path Unchanged**: Single-frame writes use existing zero-copy path
2. **Chunking Overhead**: Only incurred for messages > maxPayload
3. **BufferSlice Splitting**: Need efficient split without allocation
4. **Flow Control**: Each chunk consumes send quota independently

## Benchmark Plan

Run before and after:
```bash
cd /workspaces/grpc-go-shmem/benchmark/Shmtcp && go test -bench=. -benchtime=3s
```

Expected results:
- Small messages: No regression (same code path)
- Large messages with large rings: No regression (same code path)
- Large messages with small rings: Works (previously would fail)

## Rollout

1. Implement in `frame.go` as `writeFrameBuffersChunked`
2. Add tests with small rings
3. Verify benchmarks show no regression
4. Update transports to use chunked writes
5. Run full test suite

## Files to Modify

1. `internal/transport/frame.go` - Add `writeFrameBuffersChunked` and helper
2. `internal/transport/shm_client_transport.go` - Use chunked writes
3. `internal/transport/shm_server_transport.go` - Use chunked writes
4. `internal/transport/shm_chunking_test.go` - New test file

## Success Criteria

- [ ] All 47 existing SHM tests pass
- [ ] 256KB message works with 64KB ring
- [ ] Benchmarks show â‰¤5% regression for small messages
- [ ] No new allocations on hot path for small messages
