# Design v3.4 (FINAL, clean): SHM-only, no WU, multi-lane loopyWriter

**Status**: Final draft ready for implementation
**Last revised**: 2026-05-18
**Owner**: shm transport team
**Supersedes**: design-loopywriter-v3.md (which is the accumulated v1→v3.3
iteration history; this file is the clean rewrite per GPT-5.5 r4 feedback)

## 0. Strategy: Go-first validation

1. Implement v3.4 in Go (this repo, `grpc-go-shmem`)
2. Validate via Go bench matrix at WSL2 EPYC 7763
3. Update gRFC (`A-shared-memory-transport.md`) AFTER Go validation
4. .NET adapts after gRFC update; cross-process interop tests in CI gate the merge
5. During the validation window, Go↔.NET interop is broken (different FC models); acceptable

## 1. Constants and invariants

- TARGET: single-stream latency does NOT regress (Unary N=1 across sizes); multi-stream improves
- N=1000/64B target: ≤ 2.5 ms (current 6 ms, UDS 2.65 ms)
- ≤ 2 wake-FDs per connection (Doug's hard constraint)
- HTTP/2 frames byte-for-byte standard for DATA/HEADERS/RST/PING/SETTINGS/GOAWAY
- ONLY WindowUpdate frames are omitted between SHM peers (documented in gRFC post-validation)

## 2. Architecture

```
┌────────── CLIENT PROCESS ────────────────────────┐
│                                                  │
│  app G1 ─┐ enqueue to lane's controlBuffer       │
│  app G2 ─┤ (items: DATA, HEADERS, RST, PING,     │
│  app GN ─┘  SETTINGS, GOAWAY; NO WU)             │
│                                                  │
│  Lane writer goroutine (1 per lane, 8 default)   │
│    ├ drains lane's controlBuffer in a loop       │
│    ├ owns: active stream list IN THIS LANE       │
│    ├ owns: round-robin scheduler IN THIS LANE    │
│    ├ NO credit tracking (no WU)                  │
│    ├ blocks on lane ring space ONLY              │
│    └ parks on: per-lane wake (via shared bitmap) │
│                                                  │
│  Reader goroutine (single per direction)         │
│    ├ wakes on eventfd → reads lane status bitmap │
│    ├ ALWAYS drains lane rings (never pauses)     │
│    ├ pushes parsed frames to per-stream recvQueue│
│    └ enforces recvQueue HARD cap → RST_STREAM    │
└──────────────────────────────────────────────────┘
              │ (mmap)               ▲ (eventfd × 1/direction)
              ▼                      │
┌──────── SHARED MEMORY ────────────────────────────┐
│  Segment header (versioned; reserved bytes)      │
│  Lane status bitmap (atomic uint64 per direction)│
│  Lane[0..N-1] rings (each = existing ring layout)│
│  NO slot table. NO shared FC counters.           │
│  NO WU frames between SHM peers.                 │
└───────────────────────────────────────────────────┘
```

**Key invariants**:
- Each lane is independent SPSC. Lane writer owns its lane's scheduler state
- Sender never bypasses lane writer in P1 (no inline fast path)
- Reader ALWAYS drains lane rings — no "pause" semantics that would HOL the ring
- Flow control is implicit: ring capacity is the sender-side limit; per-stream recvQueue hard cap with immediate RST_STREAM is the receiver-side limit

## 3. Lane assignment

Both peers MUST use identical formula:

```go
// streamID is the HTTP/2 stream identifier (31-bit; high bit reserved)
func laneOf(streamID uint32, numLanes uint32) uint32 {
    return (streamID & 0x7FFFFFFF) % numLanes
}
```

Per-stream lane is fixed at stream open. Both client and server compute
independently with the same formula.

`numLanes` is in the segment header (§7), agreed at handshake. Default 8.

## 4. Lane writer

### 4.1 Data structures

```go
type frameItem struct {
    kind        frameKind  // DATA, HEADERS, TRAILERS, RST_STREAM, PING, SETTINGS, GOAWAY
    streamID    uint32     // 0 for connection-level frames
    payload     []byte
    payloadOff  int        // for chunked DATA
    flags       uint8
    ackCh       chan error // buffered cap=1
    next        *frameItem
}

type streamState struct {
    id            uint32
    laneIdx       uint32             // immutable after open
    pendingHead   *frameItem
    pendingTail   *frameItem
    pendingBytes  uint64

    // Writer-only (writer for this lane is single goroutine; no race)
    sched         schedState         // idle / active
    next, prev    *streamState       // intrusive linked-list node

    // Sender-facing
    mu     sync.Mutex
    cond   *sync.Cond
    closed bool
}

type schedState uint8
const (
    schedIdle schedState = iota
    schedActive
)

type laneWriter struct {
    laneIdx       uint32
    ring          *ShmRing                 // this lane's SPSC ring (existing)
    ctlBuf        *controlBuffer           // upstream controlbuf.go pattern (per-lane)
    streams       map[uint32]*streamState  // streams in this lane
    active        streamList               // round-robin queue
    spaceBlocked  bool                     // ring write returned WouldBlock
    closeCh       chan struct{}
}

type writerSet struct {
    lanes       [N_LANES]*laneWriter
    laneStatus  *uint64                    // pointer into shared memory; atomic
    spaceWaker  *shmDataSegWaker           // 1 eventfd per direction (existing)
}
```

### 4.2 Sender path

```go
func (s *streamState) sendDATA(data []byte, ackCh chan error) error {
    n := uint64(len(data))
    s.mu.Lock()
    for !s.closed && s.pendingBytes+n > maxSendQueueBytes {
        s.cond.Wait()
    }
    if s.closed {
        s.mu.Unlock()
        return ErrStreamClosed
    }
    // CRITICAL: reserve pendingBytes under lock to prevent racy over-cap
    // (GPT-5.5 R4 NEW-BLOCKER fix). Writer decrements after writing.
    s.pendingBytes += n
    s.mu.Unlock()

    lw := s.transport.writerSet.lanes[s.laneIdx]
    return lw.ctlBuf.put(&frameItem{
        kind:     frameDATA,
        streamID: s.id,
        payload:  data,
        ackCh:    ackCh,
    })
    // On error, sender must release pendingBytes; see lane writer's drain
    // for connection-close path.
}
```

**Connection-level frames (streamID=0)**: PING / SETTINGS / GOAWAY are
NOT associated with any stream. They are enqueued directly into lane 0’s
controlBuffer using a separate path that bypasses `streamState`:

```go
func (t *transport) sendConnLevelFrame(item *frameItem) error {
    // streamID MUST be 0
    lw := t.writerSet.lanes[0]
    return lw.ctlBuf.put(item)  // lane 0 is the designated control lane
}
```

Lane writer recognises `streamID==0` items in `handleControl` and routes
them to the control writer path (writes directly to ring; does NOT look
up a `streamState`).

### 4.3 Lane writer main loop

```go
func (lw *laneWriter) run() {
    for {
        // Drain all pending control items (cheap in-process)
        for item := lw.ctlBuf.tryGet(); item != nil; item = lw.ctlBuf.tryGet() {
            lw.handleControl(item)
        }
        if lw.processItem() {
            continue
        }
        if lw.spaceBlocked {
            select {
            case <-lw.ctlBuf.notifyCh:
            case <-lw.spaceWakerCh():
            case <-lw.closeCh:
                lw.drainAllWithError(ErrConnClosing)
                return
            }
        } else if lw.active.empty() {
            select {
            case <-lw.ctlBuf.notifyCh:
            case <-lw.closeCh:
                lw.drainAllWithError(ErrConnClosing)
                return
            }
        }
    }
}
```

### 4.4 processItem (no credit tracking)

```go
func (lw *laneWriter) processItem() (madeProgress bool) {
    if lw.active.empty() { return false }
    s := lw.active.popFront()
    if s.pendingHead == nil {
        s.sched = schedIdle
        return true
    }
    item := s.pendingHead

    var grant int
    if item.kind == frameDATA {
        remaining := len(item.payload) - item.payloadOff
        grant = min(remaining, maxFrameSize)
        // NO credit gates — ring capacity is the only limit
    } else {
        grant = len(item.payload)  // control frames written whole
    }

    ok, err := lw.tryReserveAndWrite(s, item, grant)
    if err != nil { lw.drainAllWithError(err); return false }
    if !ok {
        lw.spaceBlocked = true
        lw.active.pushFront(s)
        return false
    }

    if item.kind == frameDATA {
        item.payloadOff += grant
        if item.payloadOff < len(item.payload) {
            lw.active.pushBack(s)
            return true
        }
    }

    // Item complete; pop, ack
    s.mu.Lock()
    s.pendingHead = item.next
    if s.pendingHead == nil { s.pendingTail = nil }
    s.pendingBytes -= uint64(len(item.payload))
    s.cond.Broadcast()
    s.mu.Unlock()
    ackSender(item, nil)
    if s.pendingHead != nil {
        lw.active.pushBack(s)
    } else {
        s.sched = schedIdle
    }
    return true
}
```

### 4.5 Control event handling

```go
func (lw *laneWriter) handleControl(ev interface{}) {
    switch e := ev.(type) {
    case *frameItem:
        if e.streamID == 0 {
            // Connection-level frame (PING/SETTINGS/GOAWAY). Lane 0 only.
            // Write directly to ring (no streamState lookup).
            if lw.laneIdx != 0 {
                // Misrouted; drop with log
                ackSender(e, ErrInternal)
                return
            }
            ok, err := lw.writeControlFrameDirect(e)
            if err != nil { lw.drainAllWithError(err); return }
            if !ok {
                // Lane 0 reserved control space (§6) should prevent this
                lw.spaceBlocked = true
                // requeue: push to front of controlBuffer (priority)
                lw.ctlBuf.priorityPut(e)
                return
            }
            ackSender(e, nil)
            return
        }

        // Stream-scoped frame
        s := lw.streams[e.streamID]
        if s == nil { ackSender(e, ErrStreamClosed); return }
        wasIdle := (s.sched == schedIdle)
        s.mu.Lock()
        s.appendItem(e)
        s.mu.Unlock()
        if wasIdle {
            lw.active.pushBack(s)
            s.sched = schedActive
        }
    case *rstEvent:
        s := lw.streams[e.streamID]
        if s == nil { return }
        lw.closeStreamLocked(s, statusFromCode(e.code))
    case *closeStreamEvent:
        lw.closeStreamLocked(lw.streams[e.streamID], e.status)
    case *spaceWakeEvent:
        if lw.spaceBlocked { lw.spaceBlocked = false }
    }
}
```

**Note**: writer also decrements `s.pendingBytes` after popping items in
`processItem` (§4.4) to release the reservation made by `sendDATA`.
This is the consumer side of the lock-protected reserve/release.

### 4.6 Stream close

```go
func (lw *laneWriter) closeStreamLocked(s *streamState, st *status.Status) {
    if s == nil || (s.sched == schedIdle && s.closed) { return }
    s.mu.Lock()
    s.closed = true
    head := s.pendingHead
    s.pendingHead, s.pendingTail = nil, nil
    s.pendingBytes = 0
    s.cond.Broadcast()
    s.mu.Unlock()
    for it := head; it != nil; it = it.next {
        ackSender(it, status.ErrFromStatus(st))
    }
    if s.sched == schedActive { lw.active.remove(s) }
    s.sched = schedIdle
    delete(lw.streams, s.id)
}
```

## 5. Reader (no pause, deterministic FC)

### 5.1 Drain loop

Reader's job is to drain lane rings as fast as it can. It never "pauses"
a stream — pausing in a sequential ring is impossible without consuming
bytes.

**Lane bitmap clear race fix** (GPT-5.5 R4 BLOCKER): clear the bit
BEFORE draining the lane, not after. This way, if the writer sets the
bit during our drain, the next iteration sees it.

```go
func (r *Reader) loop() {
    for {
        status := atomic.LoadUint64(r.laneStatus)
        for status == 0 {
            select {
            case <-r.dataWaker.notifyCh():
            case <-r.closeCh:
                return
            }
            status = atomic.LoadUint64(r.laneStatus)
        }
        for laneIdx := uint32(0); laneIdx < r.numLanes; laneIdx++ {
            if status & (uint64(1) << laneIdx) == 0 { continue }
            // Clear bit FIRST. If writer sets it again during processing,
            // we'll observe the new state on the next iteration.
            atomic.AndUint64(r.laneStatus, ^(uint64(1) << laneIdx))
            r.processLane(laneIdx)
        }
    }
}

func (r *Reader) processLane(laneIdx uint32) {
    ring := r.lanes[laneIdx]
    for ring.Available() > 0 {
        frame, err := r.readFrame(ring)
        if err != nil { break }
        r.dispatchFrame(frame)
    }
}
```

**Memory ordering**: Writer does `release-store(writeIdx)` then
`atomic.OrUint64(&laneStatus, bit)`. Reader's `atomic.AndUint64` (with
full memory barrier) ensures that after clearing the bit, any subsequent
Load of `writeIdx` sees the latest writer-published value. The race
scenario "writer wrote F2 but reader already cleared bit" is benign:
reader's drain loop continues until `Available() == 0`, picking up F2
even if the bit was already cleared. The bit re-set by writer triggers
the NEXT loop iteration to recheck (spurious drain on empty ring, no
harm).

### 5.2 Frame dispatch + stream-level FC

```go
func (r *Reader) dispatchFrame(frame *Frame) {
    if frame.streamID == 0 {
        // Connection-level frame (PING/SETTINGS/GOAWAY)
        r.handleConnFrame(frame)
        return
    }
    if frame.kind == frameWINDOW_UPDATE {
        // SHM peers MUST NOT emit; if received from buggy peer, NOP
        shmStats.WUFramesIgnored.Add(1)
        return
    }

    s := r.streams[frame.streamID]
    if s == nil {
        // Stream unknown or closed — drop frame (already RSTed)
        return
    }

    // Stream-level FC: hard cap with immediate RST. NO pause, NO grace.
    s.rqMu.Lock()
    if s.recvQueueBytes + uint64(len(frame.payload)) > recvHardCap {
        s.rqMu.Unlock()
        r.sendRSTAsync(frame.streamID, ErrCodeFlowControlError)
        // Drop this frame; future frames also dropped after RST
        return
    }
    s.appendRecvItem(frame)
    s.recvQueueBytes += uint64(len(frame.payload))
    s.rqCond.Broadcast()  // wake app reader
    s.rqMu.Unlock()
}
```

**Why no pause / no soft cap / no grace**:

GPT-5.5 review identified that "pausing" a stream's frames in a single
sequential ring is impossible. Skipping requires consuming, which moves
readIdx and frees ring space — exactly the opposite of pausing.

v3.4 drops the pause idea entirely. Per-stream FC is a single hard cap.
Apps must process frames promptly; if not, the stream is killed. This
is simpler, race-free, and bounded.

**Hard cap default**: 256 KiB per stream.

At 1000 slow streams worst case: 1000 × 256 KiB = 256 MiB queued. Big
but bounded.

If even tighter memory needed in future, add a connection-level aggregate
cap that triggers conn-wide RST or GOAWAY when exceeded.

### 5.3 RST rate-limiting

If 1000 streams hit hard cap simultaneously, reader rate-limits RST
emission: max 100 RST/sec per lane. Frames for not-yet-RSTed streams
that already exceeded cap are silently dropped (sender will see no ack
and eventually time out, or hit its sendQueue cap and block).

```go
type lanRSTLimiter struct {
    pending []uint32  // streamIDs queued for RST
    lastEmit time.Time
}
```

This prevents wire-flood under adversarial slow-consumer scenarios.

## 6. Flow control parameters

| Parameter | Default | Notes |
|---|---:|---|
| `N_LANES` | 8 | Both peers must agree (segment header) |
| `total_ring_capacity` | 64 MiB | Existing default; split equally across lanes |
| `per_lane_capacity` | 8 MiB | Derived: 64 MiB / 8 lanes |
| `maxFrameSize` | 16 KiB | HTTP/2 default; matches Doug's fair-default |
| `maxSendQueueBytes` | 256 KiB | Per stream, sender-side pending cap |
| `recvHardCap` | 256 KiB | Per stream, receiver-side queue cap; overflow = RST_STREAM |
| `lane0_control_prio` | reserved 4 KiB | Lane 0 reserves bytes for SETTINGS/PING/GOAWAY |
| `RST_rate_per_lane` | 100/sec | RST emission rate limit |

**Lane 0 control priority**: Lane 0 reserves a fixed 4 KiB of its ring
capacity exclusively for control frames (PING/SETTINGS/GOAWAY). DATA
frames cannot consume the reserved space. This guarantees control plane
liveness even under data saturation.

**Memory at scale**: 1000 streams × 256 KiB recvHardCap = 256 MiB
worst-case queued data per direction. With 64 MiB ring + 256 MiB
queue = ~320 MiB per connection. Acceptable for connections that
sustain 1000 active streams; pathological if a server has 100,000 such
connections.

## 7. Segment header (versioned, with reserved bytes)

```c
struct SegmentHeader {  // Total: 256 bytes (cache-line aligned)
    /* Existing fields (unchanged offsets to maintain compat with v3.3) */
    uint32 magic;                  // offset 0  : 'SHMC'
    uint32 version;                // offset 4  : 2 for v3.4
    uint32 client_pid;             // offset 8
    uint32 server_pid;             // offset 12
    uint64 ring_capacity;          // offset 16 : per-lane capacity
    /* ... existing v3.3 fields ... */

    /* NEW v3.4 fields */
    uint32 lane_count;             // both peers must agree
    uint32 reserved_pad_1;
    uint64 c2s_lane_status;        // atomic bitmap; bit i = "lane i c→s has data"
    uint64 s2c_lane_status;        // atomic bitmap; bit i = "lane i s→c has data"

    /* Reserved space for future protocol extensions */
    uint8 reserved[256 - /* sum of above */];
};
```

**Reserved bytes**: documented in gRFC (post-validation) as:
- v3.4 peers MUST zero on init
- v3.4 peers MUST NOT read or interpret
- Future versions MAY use these bytes; version field MUST be bumped
- Total header size is 256 bytes; reserved fills to that boundary

**Version handling**:
- Version 1: pre-v3.4 (existing); refuses v3.4 peers
- Version 2: v3.4 (this design); both peers must be version 2
- Mismatch at handshake = connection refused

**Wire vs ABI**: HTTP/2 frame wire format is unchanged. The segment
header is an SHM ABI / handshake structure, not HTTP/2 wire. Bumping
version=2 affects shared-memory layout, not on-wire bytes.

## 8. N=1 fast path policy

v3.4 P1 implements NO inline fast path. All paths route through lane
writer.

Estimated overhead per RPC: 0.5-1 µs (channel send + writer goroutine
wake + dequeue + ring write).

If P1 benchmark shows Unary N=1 regresses > 1% (≈ 1.1 µs), Phase P1.5
adds the fast path:
- Sender holds lane writer's `inlineMu.TryLock()`
- If acquired AND lane is idle, write directly to ring
- Otherwise enqueue via controlBuffer

DO NOT preemptively implement. Only add if benchmark requires it.

## 9. Wire compatibility

ONE change from RFC 7540: WU frames are NOT emitted between SHM peers.
Receiver IGNORES any received WU (logs as warning, treats as NOP).

All other HTTP/2 frames byte-for-byte standard. No new HEADERS metadata.
No new SETTINGS extension. No new capability negotiation.

Documented in gRFC (post-validation) as "SHM transport's flow-control
model: ring-backed, no WU".

## 10. Test matrix

| Test | Type | Goal |
|---|---|---|
| `TestSHMv34_DATAChunking` | unit | 100 KiB message → multiple maxFrameSize frames |
| `TestSHMv34_LaneAssignment` | unit | Same streamID → same lane on client and server |
| `TestSHMv34_LaneFullBackpressure` | concurrency | Lane ring full → sender blocks → reader drains → resumes |
| `TestSHMv34_NoWUEmitted` | unit | No WU frame on wire (assert via packet capture) |
| `TestSHMv34_WUReceivedIsNop` | unit | Receive synthetic WU; reader logs and continues |
| `TestSHMv34_RecvHardCapRST` | concurrency | Stream A above hardCap → RST_STREAM emitted; A removed |
| `TestSHMv34_RSTRateLimit` | concurrency | 1000 streams above cap → RST rate-limited to 100/sec/lane |
| `TestSHMv34_LaneZeroControlPrio` | concurrency | Lane 0 saturated with DATA; PING still goes through |
| `TestSHMv34_BackpressureClose` | concurrency | Sender blocked on cond; stream closes; sender returns ErrStreamClosed |
| `TestSHMv34_RSTMidWrite` | concurrency | RST while writing → item dropped cleanly |
| `TestSHMv34_SegmentVersionMismatch` | unit | v3.3 peer connects to v3.4 → refused at handshake |
| `BenchmarkConcurrent/N=1` | **regression** | within 1% of 113 µs (1.1 µs budget) |
| `BenchmarkConcurrent/N=10` | regression | ≤ 75 µs ±5% |
| `BenchmarkConcurrent/N=100/64KB` | regression | ≤ 6.1 ms ±5% |
| `BenchmarkConcurrent/N=1000/64B` | **target** | ≤ 2.5 ms |
| `BenchmarkConcurrent/N=1000/64KB` | regression-critical | ≤ 132 ms |

## 11. Phased delivery

| Phase | Scope | Outcome target |
|---|---|---|
| P1 | controlBuffer port + multi-lane (8) + no-WU + receiver hard cap | All v3.4 features land together |
| P1 measure | Full Go bench matrix | Decision point on P1.5 |
| P1.5 (conditional) | Add N=1 inline fast path IF benchmark shows N=1 regress > 1% | Restore N=1 latency |
| P2 (conditional) | Tune lane count, queue caps, ring capacity if N=1000/64KB regresses | Restore throughput |
| gRFC update | After Go validation passes all targets | Document v3.4 model |
| .NET adapt | Mirror Go behavior; cross-process CI tests gate merge | Restore Go↔.NET interop |

## 12. Implementation risks (top 3)

1. **Single-writer per lane vs parallel memcpy**: At N=1000/64KB, all 125 streams in a lane serialize through one writer goroutine. Memcpy is single-threaded. Risk: regression from current parallel-sender model. Mitigation: measure in P1; if regress, P2 adds parallel ring write within lane (separate reserveIdx).

2. **Receiver hard-cap = aggressive kill**: 256 KiB hard cap with immediate RST_STREAM may kill streams under transient slowness. Risk: app developers see "spurious" RSTs. Mitigation: cap is conservative for typical workloads; tune in P2 if needed; document behavior loudly in gRFC.

3. **Lane 0 control reserve**: Reserving 4 KiB of lane 0 for control frames means data on lane 0 has slightly less capacity. Risk: lane 0 streams unfairly disadvantaged. Mitigation: 4 KiB is < 0.05% of lane capacity; effectively invisible.

## 13. References

- `design-loopywriter-v3.md` (this file's predecessor, accumulated v1→v3.3 iteration; archived as historical record)
- Adversarial reviews: 4 rounds with GPT-5.5, 68 unique flaws found
- Upstream reference: `internal/transport/controlbuf.go`
- .NET counterpart: `c:\src\grpc-dotnet-shm`
