# gRPC Read/Write Data Flow: Unix Sockets vs Shared Memory

This document provides a detailed code walkthrough comparing the read and write paths
for HTTP/2 (Unix sockets) vs shared memory transport in grpc-go, with **explicit
highlighting of where memory copies occur**.

---

## Copy Analysis Summary

> **Note:** This analysis counts only **payload data copies** (the actual message bytes).
> Small fixed-size copies (frame headers, metadata) are excluded as they're negligible.

### Write Path Payload Copies

| Stage | Location | Layer | Unix/HTTP2 | Shmem |
|-------|----------|-------|------------|-------|
| **W-C1** | `proto.Marshal()` | gRPC **(COMMON)** | ✓ 1 copy | ✓ 1 copy |
| **W-C2** | `compress()` | gRPC **(COMMON)** | ✓ 1 copy (if enabled) | ✓ 1 copy (if enabled) |
| **W-T1** | `framer.writeData()` → internal buf | HTTP/2 framing | ✓ 1 copy | — |
| **W-T2** | `bufWriter.Flush()` → kernel | Syscall/kernel | ✓ 1 copy | — |
| **W-T3** | Kernel socket buf → receiver socket | Kernel IPC | ✓ 1 copy | — |
| **W-S1** | `writeFrameBuffers()` → ring | Shmem transport | — | ✓ 1 copy |

**Transport-layer result:** Unix = **3 payload copies**, Shmem = **1 payload copy**

### Read Path Payload Copies

| Stage | Location | Layer | Unix/HTTP2 | Shmem |
|-------|----------|-------|------------|-------|
| **R-T1** | Kernel socket buf → `bufReader` | Syscall/kernel | ✓ 1 copy | — |
| **R-T2** | `readFrame()` → frame payload buf | HTTP/2 parsing | ✓ 1 copy | — |
| **R-S1** | `readFrameView()` from ring | Shmem transport | — | ✓ 0 copies (contiguous) |
| **R-S2** | (wrap-around fallback) | Shmem transport | — | ✓ 1 copy (rare) |
| **R-C1** | `decompress()` | gRPC **(COMMON)** | ✓ 1 copy (if enabled) | ✓ 1 copy (if enabled) |
| **R-C2** | `proto.Unmarshal()` | gRPC **(COMMON)** | ✓ 1 copy | ✓ 1 copy |

**Transport-layer result:** Unix = **2 payload copies**, Shmem = **0-1 payload copies**

---

## Overview Diagrams

### Write Path with Copy Locations

```
┌──────────────────────────────────────────────────────────────────────────────────────┐
│                         WRITE PATH - COPY ANALYSIS                                    │
├──────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                       │
│  gRPC Application Layer (COMMON - both paths do these copies)                         │
│  ┌──────────────────────────────────────────────────────────────────────────────┐    │
│  │  clientStream.SendMsg(m)                                                      │    │
│  │       │                                                                       │    │
│  │       ▼                                                                       │    │
│  │  csAttempt.sendMsg(m)                                                        │    │
│  │       │                                                                       │    │
│  │       ├── encode(codec, m)  ════════════════════ 📋 W-C1 (COMMON)           │    │
│  │       │   └─ proto.Marshal() allocates new []byte        (struct → bytes)    │    │
│  │       │                                                                       │    │
│  │       ├── compress(data) if enabled ════════════════ 📋 W-C2 (COMMON)        │    │
│  │       │   └─ Compressor writes to new buffer             (bytes → compressed)│    │
│  │       │                                                                       │    │
│  │       ├── prepareMsg(hdr, data)  (no copy - just slice references)           │    │
│  │       │                                                                       │    │
│  │       ▼                                                                       │    │
│  │  transportStream.Write(hdr, payld, opts)  ← PATHS DIVERGE HERE               │    │
│  └──────────────────────────────────────────────────────────────────────────────┘    │
│                          │                                                            │
│         ┌────────────────┴────────────────┐                                          │
│         ▼                                 ▼                                          │
│  ┌─────────────────────────────┐  ┌─────────────────────────────────────────────┐   │
│  │   UNIX/HTTP2 PATH           │  │          SHARED MEMORY PATH                  │   │
│  │   (3 additional copies)     │  │          (1 additional copy)                 │   │
│  ├─────────────────────────────┤  ├─────────────────────────────────────────────┤   │
│  │                             │  │                                              │   │
│  │ http2Client.write()         │  │ ShmClientTransport.write()                  │   │
│  │      │                      │  │      │                                       │   │
│  │      ├─ s.wq.get(quota)     │  │      ├─ acquireSendQuota()                  │   │
│  │      │                      │  │      │                                       │   │
│  │      ├─ controlBuf.put(df)  │  │      ├─ Build FrameHeader (16 bytes)        │   │
│  │      │  (enqueue, no copy)  │  │      │                                       │   │
│  │      │                      │  │      └─ writeFrameBuffersChunked()          │   │
│  │      ▼                      │  │              │                               │   │
│  │ [loopyWriter goroutine]     │  │              ├─ ReserveWrite(n)             │   │
│  │      │                      │  │              │  (get mmap slices)           │   │
│  │      ├─ handle(dataFrame)   │  │              │                               │   │
│  │      │                      │  │              ├─ copy(ring, data) ═══════════ │   │
│  │      ├─ processData()       │  │              │  📋 W-S1 (only transport      │   │
│  │      │                      │  │              │   copy! direct to mmap)       │   │
│  │      │                      │  │              │                               │   │
│  │ HTTP/2 FRAMING LAYER:       │  │              └─ Commit()                     │   │
│  │ ─────────────────────────── │  │                  └─ futex_wake (0-1 syscall)│   │
│  │      │                      │  │                                              │   │
│  │      ├─ framer.writeData()  │  │         ┌────────────────────────────────┐  │   │
│  │      │      │               │  │         │  Data now in shared memory!    │  │   │
│  │      │      └─ copy to      │  │         │  Reader can access directly.   │  │   │
│  │      │         internal buf │  │         │  NO kernel involvement.        │  │   │
│  │      │     ═══ 📋 W-T1      │  │         └────────────────────────────────┘  │   │
│  │      │         (HTTP/2      │  │                                              │   │
│  │      │          framing)    │  └─────────────────────────────────────────────┘   │
│  │      │                      │                                                     │
│  │      ├─ bufWriter.Write()   │                                                     │
│  │      │      │               │                                                     │
│  │      │      └─ append to    │                                                     │
│  │      │         flush buffer │                                                     │
│  │      │                      │                                                     │
│  │      └─ Flush()             │                                                     │
│  │           │                 │                                                     │
│  │           ▼                 │                                                     │
│  │  KERNEL TRANSITION:         │                                                     │
│  │  ────────────────────────── │                                                     │
│  │      │                      │                                                     │
│  │      ├─ net.Conn.Write()    │                                                     │
│  │      │      │               │                                                     │
│  │      │      └─ SYSCALL      │                                                     │
│  │      │         sendto()     │                                                     │
│  │      │     ═══ 📋 W-T2      │                                                     │
│  │      │         (user→kernel │                                                     │
│  │      │          buffer)     │                                                     │
│  │      │                      │                                                     │
│  │      ▼                      │                                                     │
│  │  KERNEL IPC:                │                                                     │
│  │  ────────────────────────── │                                                     │
│  │      │                      │                                                     │
│  │      └─ Socket buffer       │                                                     │
│  │         → receiver socket   │                                                     │
│  │     ═══ 📋 W-T3             │                                                     │
│  │         (kernel copies to   │                                                     │
│  │          receiver's socket  │                                                     │
│  │          buffer)            │                                                     │
│  │                             │                                                     │
│  └─────────────────────────────┘                                                     │
│                                                                                       │
│  ┌────────────────────────────────────────────────────────────────────────────────┐  │
│  │  TRANSPORT-LAYER COPY SUMMARY (payload only, excl. common encode/compress):     │  │
│  │  ──────────────────────────────────────────────────────────────────────────────│  │
│  │                                                                                 │  │
│  │  Unix/HTTP2: 3 payload copies (W-T1 + W-T2 + W-T3)                             │  │
│  │              W-T1: framer buf, W-T2: user→kernel, W-T3: kernel IPC             │  │
│  │              + 1-2 syscalls (sendto) + goroutine hop (loopyWriter)             │  │
│  │                                                                                 │  │
│  │  Shmem:      1 payload copy (W-S1: directly to mmap ring)                      │  │
│  │              + 0-1 futex wake (only if reader waiting)                         │  │
│  │              + no goroutine hop (synchronous write)                            │  │
│  │                                                                                 │  │
│  └────────────────────────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────────────────────────┘
```

### Read Path with Copy Locations

```
┌──────────────────────────────────────────────────────────────────────────────────────┐
│                         READ PATH - COPY ANALYSIS                                     │
├──────────────────────────────────────────────────────────────────────────────────────┤
│                                                                                       │
│  ┌─────────────────────────────────────┬─────────────────────────────────────────┐   │
│  │   UNIX/HTTP2 PATH                   │       SHARED MEMORY PATH                │   │
│  │   (3 copies before gRPC layer)      │       (0-1 copies before gRPC layer)   │   │
│  ├─────────────────────────────────────┼─────────────────────────────────────────┤   │
│  │                                     │                                          │   │
│  │  KERNEL IPC (sender side):          │  [processIncomingData() goroutine]      │   │
│  │  ───────────────────────────────    │       │                                  │   │
│  │       │                             │       │                                  │   │
│  │       └─ Sender's kernel copies     │       ▼                                  │   │
│  │          data to socket buffer      │  readFrameView()                        │   │
│  │          (already counted in        │       │                                  │   │
│  │           write path)               │       ├─ ReadSlices(16) for header     │   │
│  │                                     │       │  [futex wait if ring empty]     │   │
│  │  KERNEL → USER TRANSITION:          │       │                                  │   │
│  │  ───────────────────────────────    │       ├─ copy(hb[:], first)            │   │
│  │       │                             │       │  (16 bytes only - trivial)      │   │
│  │       ├─ SYSCALL: recvfrom()        │       │                                  │   │
│  │       │                             │       ├─ decodeFrameHeader()            │   │
│  │       └─ Kernel socket buffer       │       │                                  │   │
│  │          → bufReader buffer         │       ├─ ReadSlices(payloadLen)         │   │
│  │      ═══ 📋 R-T1 (payload copy)     │       │                                  │   │
│  │          (kernel → user space)      │       │                                  │   │
│  │                                     │       ▼                                  │   │
│  │  HTTP/2 PARSING:                    │  ┌───────────────────────────────────┐  │   │
│  │  ───────────────────────────────    │  │ CONTIGUOUS (common case):         │  │   │
│  │       │                             │  │                                   │  │   │
│  │ [http2Client.reader() goroutine]    │  │  if len(pSecond) == 0 {          │  │   │
│  │       │                             │  │      contig := pFirst[:len]       │  │   │
│  │       ▼                             │  │      // NO COPY! ═══════ ✓ ZERO  │  │   │
│  │  framer.readFrame()                 │  │      // Return slice directly    │  │   │
│  │       │                             │  │      // into mmap'd ring memory  │  │   │
│  │       ├─ http2.ReadFrame()          │  │      buf = mem.NewBuffer(&contig) │  │   │
│  │       │      │                      │  │  }                                │  │   │
│  │       │      └─ Parse 9-byte header │  │                                   │  │   │
│  │       │         Read payload bytes  │  └───────────────────────────────────┘  │   │
│  │       │         into new []byte     │                                          │   │
│  │       │     ═══ 📋 R-T2 (payload)   │       ┌───────────────────────────────┐  │   │
│  │       │         (bufReader → frame  │       │ WRAP-AROUND (rare):           │  │   │
│  │       │          payload buffer)    │       │                               │  │   │
│  │       │                             │       │  } else {                     │  │   │
│  │       │                             │       │      contig := make([]byte)   │  │   │
│  │       │                             │       │      copy(contig, pFirst)     │  │   │
│  │       │                             │       │      copy(contig, pSecond)    │  │   │
│  │       │                             │       │  ═══ 📋 R-S2 (1 copy, rare)  │  │   │
│  │       │                             │       │      (only when data wraps   │  │   │
│  │       │                             │       │       around ring boundary)  │  │   │
│  │       │                             │       │  }                            │  │   │
│  │       │                             │       └───────────────────────────────┘  │   │
│  │       │                             │                                          │   │
│  │       ▼                             │       ▼                                  │   │
│  │                                     │                                          │   │
│  │  gRPC FRAME HANDLING:               │  gRPC FRAME HANDLING:                   │   │
│  │  ───────────────────────────────    │  ─────────────────────────────────────  │   │
│  │       │                             │       │                                  │   │
│  │  switch frame.(type):               │  switch fh.Type:                        │   │
│  │       │                             │       │                                  │   │
│  │  case *parsedDataFrame:             │  case FrameTypeMESSAGE:                 │   │
│  │       │                             │       │                                  │   │
│  │       ▼                             │       ▼                                  │   │
│  │  handleData(f)                      │  (inline handling)                      │   │
│  │       │                             │       │                                  │   │
│  │       ├─ s.fc.onData()              │       ├─ Stream flow control            │   │
│  │       │                             │       │                                  │   │
│  │       ├─ f.data.Ref()               │       └─ Transfer payloadBuf ownership │   │
│  │       │  (ref count, no copy)       │          to stream (no copy!)           │   │
│  │       │                             │                                          │   │
│  │       └─ s.write(recvMsg{buffer})   │       stream.write(recvMsg{buffer})     │   │
│  │              │                      │              │                           │   │
│  │              │                      │              │                           │   │
│  │              ▼                      │              ▼                           │   │
│  │       recvBuffer.put()              │       recvBuffer.put()                  │   │
│  │       (channel send, no copy)       │       (channel send, no copy)           │   │
│  │                                     │                                          │   │
│  └─────────────────────────────────────┴─────────────────────────────────────────┘   │
│                          │                                                            │
│                          ▼                                                            │
│  ┌──────────────────────────────────────────────────────────────────────────────┐    │
│  │  gRPC Application Layer (COMMON - both paths do these operations)             │    │
│  │  ────────────────────────────────────────────────────────────────────────────│    │
│  │                                                                               │    │
│  │  clientStream.RecvMsg(m)                                                      │    │
│  │       │                                                                       │    │
│  │       ▼                                                                       │    │
│  │  csAttempt.recvMsg(m)                                                        │    │
│  │       │                                                                       │    │
│  │       ▼                                                                       │    │
│  │  recv(&parser, codec, ...)                                                   │    │
│  │       │                                                                       │    │
│  │       ├── parser.recvMsg()                                                   │    │
│  │       │      │                                                               │    │
│  │       │      ├── r.ReadMessageHeader(5 bytes)  (from recvBuffer, no copy)    │    │
│  │       │      └── r.Read(length)                (from recvBuffer, no copy)    │    │
│  │       │                                                                       │    │
│  │       ├── decompress() if enabled ═══════════════ 📋 R-C1 (COMMON)          │    │
│  │       │   └─ Decompressor writes to new buffer                               │    │
│  │       │                                                                       │    │
│  │       └── codec.Unmarshal(data, m) ══════════════ 📋 R-C2 (COMMON)           │    │
│  │           └─ proto.Unmarshal() populates struct fields                       │    │
│  │                                                                               │    │
│  └──────────────────────────────────────────────────────────────────────────────┘    │
│                                                                                       │
│  ┌────────────────────────────────────────────────────────────────────────────────┐  │
│  │  COPY COUNT SUMMARY (transport layer only, excluding decompress/unmarshal):    │  │
│  │  ──────────────────────────────────────────────────────────────────────────────│  │
│  │                                                                                 │  │
│  │  Unix/HTTP2: 2 copies (kernel→bufReader, bufReader→frame payload)              │  │
│  │              + 1 syscall (recvfrom)                                             │  │
│  │              Note: Data already in kernel buffer from sender's write            │  │
│  │                                                                                 │  │
│  │  Shmem:      0 copies (contiguous case - direct mmap access)                   │  │
│  │              1 copy (wrap-around case - rare with large ring)                  │  │
│  │              + 0-1 futex wait (only if ring was empty)                         │  │
│  │                                                                                 │  │
│  └────────────────────────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────────────────────────┘
```

---

## Detailed Code Walkthrough

### WRITE PATH

#### Step 1: Application calls SendMsg (no copy here)
**File:** [stream.go#L937](stream.go#L937)

```go
func (cs *clientStream) SendMsg(m any) (err error) {
    defer func() {
        if err != nil && err != io.EOF {
            cs.finish(err)
        }
    }()
    if cs.sentLast {
        return status.Errorf(codes.Internal, "SendMsg called after CloseSend")
    }
    // ... retry logic, binlog, stats ...
    err = cs.withRetry(func(a *csAttempt) error {
        return a.sendMsg(m, nil)  // ← dispatches to attempt
    }, cs.commitAttemptLocked)
    return err
}
```

#### Step 2: csAttempt.sendMsg prepares the message — 📋 W-C1 and W-C2 (COMMON to both paths)
**File:** [stream.go#L1095](stream.go#L1095)

```go
func (a *csAttempt) sendMsg(m any, payInfo *payloadInfo) (err error) {
    // 📋 W-C1 (COMMON): Encode the protobuf message
    // proto.Marshal() allocates new []byte and serializes struct fields
    data, err := encode(cs.codec, m)  // ← COPY: struct → []byte
    if err != nil { return err }
    
    // 📋 W-C2 (COMMON, if compression enabled): Compress data
    // Compressor writes to new buffer
    compData, pf, err := compress(data, cs.compressorV0, cs.compressorV1, ...)  // ← COPY
    if err != nil { return err }
    
    // NO COPY: Prepend gRPC header (5 bytes: compression flag + length)
    // Just creates slice references
    hdr, payld := prepareMsg(data, codec, compData, pf, ...)
    
    // 4. Write to transport  ← THIS IS WHERE PATHS DIVERGE
    if err := a.transportStream.Write(hdr, payld, &transport.WriteOptions{...}); err != nil {
        return err
    }
    return nil
}
```

---

#### UNIX/HTTP2 Path — 📋 W-T1, W-T2, W-T3 (transport-layer copies)

##### Step 3a: http2Client.write (no copy, just enqueue reference)
**File:** [http2_client.go#L1109](http2_client.go#L1109)

```go
// Write formats the data into HTTP2 data frame(s) and sends it out.
// NOTE: No copy here - just creates a descriptor and enqueues it
func (t *http2Client) write(s *ClientStream, hdr []byte, data mem.BufferSlice, opts *WriteOptions) error {
    // 1. Check stream state
    if opts.Last {
        if !s.compareAndSwapState(streamActive, streamWriteDone) {
            return errStreamDone
        }
    }
    
    // 2. Create dataFrame descriptor (holds references, no copy)
    df := &dataFrame{
        streamID:  s.id,
        endStream: opts.Last,
        h:         hdr,      // ← Reference to existing slice
        data:      data,     // ← Reference to existing BufferSlice
    }
    
    // 3. Wait for flow control quota
    dataLen := data.Len()
    if hdr != nil || dataLen != 0 {
        if err := s.wq.get(int32(len(hdr) + dataLen)); err != nil {
            return err
        }
    }
    
    // 4. Enqueue to controlBuf (picked up by loopyWriter)
    // NO COPY: Just adds reference to queue
    data.Ref()
    if err := t.controlBuf.put(df); err != nil {
        data.Free()
        return err
    }
    return nil
}
```

##### Step 4a: controlBuf.put enqueues the frame (no copy)
**File:** [controlbuf.go#L348](controlbuf.go#L348)

```go
// NO COPY: Just adds item to linked list
func (c *controlBuffer) put(it cbItem) error {
    _, err := c.executeAndPut(nil, it)
    return err
}

func (c *controlBuffer) executeAndPut(f func() bool, it cbItem) (bool, error) {
    c.mu.Lock()
    defer c.mu.Unlock()
    
    if c.closed { return false, ErrConnClosing }
    
    // Track if consumer (loopyWriter) is waiting
    var wakeUp bool
    if c.consumerWaiting {
        wakeUp = true
        c.consumerWaiting = false
    }
    
    c.list.enqueue(it)  // ← Pointer added to queue, no data copy
    
    // Wake up loopyWriter if it was blocked
    if wakeUp {
        select {
        case c.wakeupCh <- struct{}{}:
        default:
        }
    }
    return true, nil
}
```

##### Step 5a: loopyWriter processes frames (orchestrates W-T1 + W-T2)
**File:** [controlbuf.go#L586](controlbuf.go#L586)

```go
func (l *loopyWriter) run() (err error) {
    defer func() {
        if !isIOError(err) {
            l.framer.writer.Flush()  // Final flush on exit
        }
        l.cbuf.finish()
    }()
    
    for {
        // 1. Block until frame available
        it, err := l.cbuf.get(true)
        if err != nil { return err }
        
        // 2. Handle frame (dataFrame, windowUpdate, etc.)
        // For dataFrame: l.handle() calls processData() which calls
        // framer.writeData() → triggers W-T1 copy
        if err = l.handle(it); err != nil { return err }
        
        // 3. Process data frames for active streams
        if _, err = l.processData(); err != nil { return err }
        
        // 4. Batching loop - drain queue before flushing
        for {
            it, err := l.cbuf.get(false)  // Non-blocking
            if it != nil {
                l.handle(it)
                l.processData()
                continue
            }
            // 5. Flush to wire when queue empty → triggers W-T2 copy
            l.framer.writer.Flush()  // ← SYSCALL + 📋 W-T2
            break
        }
    }
}
```

##### Step 6a: framer.writeData — 📋 W-T1 (HTTP/2 framing)
**File:** [http_util.go#L441](http_util.go#L441)

```go
// 📋 W-T1: HTTP/2 framing layer copies data to internal buffer
func (f *framer) writeData(forceFlush bool, s *Stream, endStream bool, d []byte) error {
    // f.fr.WriteData internally does:
    //   1. Write 9-byte HTTP/2 frame header
    //   2. Copy payload bytes 'd' to internal buffer
    //   ═════════════════════════════════════════════════════ 📋 W-T1
    //   (gRPC payload → framer's internal []byte buffer)
    if err := f.fr.WriteData(s.id, endStream, d); err != nil {
        return connectionErrorf(...)
    }
    if forceFlush {
        return f.writer.Flush()
    }
    return nil
}
```

##### Step 7a: bufWriter.Flush — 📋 W-T2 (Kernel transition)
**File:** [http_util.go#L319](http_util.go#L319)

```go
// 📋 W-T2: Kernel transition - user space to kernel socket buffer
func (w *bufWriter) Flush() error {
    if w.offset == 0 {
        return nil
    }
    // net.Conn.Write() triggers SYSCALL: sendto()
    // The kernel COPIES data from user-space buffer (w.buf) 
    // into the kernel's socket send buffer
    // ═══════════════════════════════════════════════════════════ 📋 W-T2
    // (bufWriter.buf → kernel socket buffer)
    _, err := w.conn.Write(w.buf[:w.offset])
    w.offset = 0
    return err
}
```

##### Step 8a: Kernel IPC — 📋 W-T3 (Kernel internal)

```
📋 W-T3: Kernel copies data between socket buffers
───────────────────────────────────────────────────────────────────────────
The kernel transfers data from sender's socket send buffer 
to receiver's socket receive buffer.

For Unix domain sockets, this is an in-kernel copy (no network hardware).
For TCP sockets, additional copies occur for network stack processing.

This copy happens entirely within the kernel - invisible to user code
but still has CPU and memory bandwidth cost.
═══════════════════════════════════════════════════════════ 📋 W-T3
(sender socket buffer → receiver socket buffer)
```

**Total HTTP/2 overhead:** 
- **3 copies in transport layer** (framer→bufWriter→kernel→receiver)
- 1-2 syscalls per batch
- Goroutine hop (loopyWriter)

---

#### SHARED MEMORY Path — 📋 W-S1 only (final copy)

##### Step 3b: ShmClientTransport.write (no copy yet)
**File:** [shm_client_transport.go#L947](shm_client_transport.go#L947)

```go
// NOTE: No intermediate copies - data goes directly to ring buffer
func (t *ShmClientTransport) write(s *ClientStream, hdr []byte, data mem.BufferSlice, opts *WriteOptions) error {
    // 1. Check transport closed
    if t.closed.Load() {
        return ErrConnClosing
    }
    
    // 2. Check stream state
    if opts.Last {
        if !s.compareAndSwapState(streamActive, streamWriteDone) {
            return errStreamDone
        }
    }
    
    payloadLen := len(hdr) + data.Len()
    
    // 3. Wait for flow control (our own implementation)
    if err := t.acquireSendQuota(s.ctx, s.id, payloadLen); err != nil {
        return err
    }
    
    // 4. Build frame header (16 bytes, no allocation for large data)
    fh := FrameHeader{
        StreamID: s.id,
        Type:     FrameTypeMESSAGE,
        Flags:    0,
    }
    if opts != nil && !opts.Last {
        fh.Flags = MessageFlagMORE
    }
    
    // 5. Write directly to ring buffer — THIS IS WHERE THE ONLY COPY HAPPENS
    if err := writeFrameBuffersChunked(s.ctx, t.clientToServer, fh, hdr, data, 0); err != nil {
        return err
    }
    
    return nil  // Done! No goroutine hop, no syscall (usually)
}
```

##### Step 4b: writeFrameBuffers — 📋 W-S1 (single copy to mmap)
**File:** [frame.go#L435](frame.go#L435)

```go
// 📋 W-S1: THE ONLY TRANSPORT-LAYER COPY
// Data is copied directly into mmap'd shared memory ring buffer
func writeFrameBuffers(ctx context.Context, tx *ShmRing, fh FrameHeader, hdr []byte, payload mem.BufferSlice) error {
    dataLen := payload.Len()
    payloadLen := len(hdr) + dataLen
    fh.Length = uint32(payloadLen)
    
    total := frameHeaderSize + payloadLen
    
    // 1. Reserve space in ring buffer
    // Returns slices pointing directly into mmap'd memory!
    // May block via futex if ring is full (0-1 syscall)
    res, err := tx.ReserveWrite(ctx, total)
    if err != nil { return err }
    
    // 2. Encode frame header (16 bytes)
    var fhBytes [frameHeaderSize]byte
    encodeFrameHeaderTo(&fhBytes, fh)
    
    // 3. Copy everything directly into ring memory (mmap'd)
    // ═══════════════════════════════════════════════════════════ 📋 W-S1
    // This is the ONLY copy in the shmem transport layer!
    // Data goes directly from gRPC buffers → shared memory
    // The receiver can read it directly - no kernel involvement
    written := 0
    writeSeq := func(src []byte) error {
        for len(src) > 0 {
            if written < len(res.First) {
                n := copy(res.First[written:], src)  // ← DIRECT TO MMAP
                written += n
                src = src[n:]
            }
            // Handle wrap-around into res.Second if needed
        }
        return nil
    }
    
    writeSeq(fhBytes[:])    // Frame header (16 bytes)
    writeSeq(hdr)           // gRPC header (5 bytes)
    for _, buf := range payload {
        writeSeq(buf.ReadOnlyData())  // Message data
    }
    
    // 4. Publish the write
    return res.Commit(total)
}
```

##### Step 5b: ReserveWrite gets ring slices
**File:** [ring.go#L1117](ring.go#L1117)

```go
func (r *ShmRing) ReserveWrite(ctx context.Context, n int) (WriteReservation, error) {
    if uint64(n) > r.capacity {
        return WriteReservation{}, errors.New("reservation larger than ring capacity")
    }
    
    hdr := r.header()
    
    for {
        select {
        case <-ctx.Done():
            return WriteReservation{}, ctx.Err()
        default:
        }
        
        // Check available space (lock-free)
        writeIdx := hdr.WriteIndex()
        readIdx := hdr.ReadIndex()
        available := r.capacity - (writeIdx - readIdx)
        
        if uint64(n) <= available {
            // Space available - return slices into mmap'd memory
            writePos := writeIdx & r.capMask
            
            if writePos+uint64(n) <= r.capacity {
                // No wrap: single contiguous slice
                first = unsafe.Slice((*byte)(r.dataPtr() + writePos), n)
            } else {
                // Wrap: two slices (end + beginning)
                firstLen := r.capacity - writePos
                first = unsafe.Slice((*byte)(r.dataPtr() + writePos), firstLen)
                second = unsafe.Slice((*byte)(r.dataPtr()), n - firstLen)
            }
            
            return WriteReservation{
                First:    first,    // ← Direct pointer to mmap memory!
                Second:   second,
                ring:     r,
                writeIdx: writeIdx,
            }, nil
        }
        
        // No space - wait via futex
        r.waitForSpace(ctx, hdr)  // ← Blocks until reader advances
    }
}
```

##### Step 6b: Commit publishes and wakes reader (no copy)
**File:** [ring.go#L1092](ring.go#L1092)

```go
// NO COPY: Just updates index and optionally wakes reader
func (wr *WriteReservation) Commit(written int) error {
    hdr := wr.ring.header()
    
    // Publish new write index (atomic store with release semantics)
    // The data is already in shared memory from the copy in writeFrameBuffers
    hdr.SetWriteIndex(wr.writeIdx + uint64(written))
    
    if written > 0 {
        hdr.IncrementDataSequence()
        
        // Only wake reader if it's waiting - avoids unnecessary syscalls
        // This is 0 syscalls if reader is actively polling
        // or 1 futex_wake syscall if reader was blocked
        if hdr.DataWaiters() > 0 {
            wr.ring.signalData(&hdr.dataSeq)  // ← futex_wake() - only syscall!
        }
    }
    
    return nil
}
```

**Total shmem write overhead:**
- **1 copy in transport layer** (direct to mmap'd ring)
- 0-1 futex calls (only if reader waiting)
- No goroutine coordination

---

### READ PATH

#### UNIX/HTTP2 Path — 📋 R-T1, R-T2 (transport-layer copies)

##### Step 1a: http2Client.reader goroutine — 📋 R-T1 (kernel → user)
**File:** [http2_client.go#L1649](http2_client.go#L1649)

```go
func (t *http2Client) reader(errCh chan<- error) {
    defer func() {
        close(t.readerDone)
        if errClose != nil {
            t.Close(errClose)
        }
    }()
    
    // Read server preface (settings frame)
    if err := t.readServerPreface(); err != nil {
        errCh <- err
        return
    }
    close(errCh)
    
    // Main read loop
    for {
        t.controlBuf.throttle()  // Flow control backpressure
        
        // 📋 R-T1 happens inside framer.readFrame():
        //   - SYSCALL: recvfrom() reads from kernel socket buffer
        //   - Kernel copies data into bufReader's user-space buffer
        //   ═══════════════════════════════════════════════════════ 📋 R-T1
        //   (kernel socket buffer → bufReader.buf)
        frame, err := t.framer.readFrame()
        if err != nil {
            errClose = connectionErrorf(...)
            return
        }
        
        // Dispatch by frame type
        switch frame := frame.(type) {
        case *http2.MetaHeadersFrame:
            t.operateHeaders(frame)
        case *parsedDataFrame:
            t.handleData(frame)
            frame.data.Free()
        case *http2.WindowUpdateFrame:
            t.handleWindowUpdate(frame)
        }
    }
}
```

##### Step 1a (continued): framer.readFrame — 📋 R-T2 (parse)
**File:** [http_util.go](http_util.go)

```go
// Inside framer.readFrame():
// 📋 R-T2: HTTP/2 frame parsing allocates new buffer for payload
//
// The http2.Framer.ReadFrame() internally:
//   1. Reads 9-byte HTTP/2 frame header from bufReader
//   2. Allocates new []byte for payload
//   3. Copies payload from bufReader into new buffer
//   ═══════════════════════════════════════════════════════ 📋 R-T2
//   (bufReader.buf → newly allocated frame payload []byte)
```

##### Step 2a: handleData processes DATA frames (no copy, ref count)
**File:** [http2_client.go#L1188](http2_client.go#L1188)

```go
// NO COPY: Just takes reference and passes to stream
func (t *http2Client) handleData(f *parsedDataFrame) {
    size := f.Header().Length
    
    // Connection-level flow control
    if w := t.fc.onData(size); w > 0 {
        t.controlBuf.put(&outgoingWindowUpdate{streamID: 0, increment: w})
    }
    
    // Find the stream
    s := t.getStream(f)
    if s == nil { return }
    
    // Stream-level flow control
    if size > 0 {
        if err := s.fc.onData(size); err != nil {
            t.closeStream(s, io.EOF, true, http2.ErrCodeFlowControl, ...)
            return
        }
        
        dataLen := f.data.Len()
        if dataLen > 0 {
            f.data.Ref()  // Increment ref count, no copy
            s.write(recvMsg{buffer: f.data})  // Pass reference to stream
        }
    }
    
    // Handle end-of-stream
    if f.StreamEnded() {
        t.closeStream(s, io.EOF, false, ...)
    }
}
```

**Total HTTP/2 read overhead:**
- **2 copies in transport layer** (kernel→bufReader, bufReader→frame)
- 1 syscall (recvfrom)
- Data already in kernel from sender's write (that was W-T3 on write side)

---

#### SHARED MEMORY Path — 📋 ZERO COPIES (usually)

##### Step 1b: processIncomingData goroutine (no copy)
**File:** [shm_client_transport.go#L291](shm_client_transport.go#L291)

```go
// NO COPY HERE: Just receives zero-copy buffer from readFrameView
func (t *ShmClientTransport) processIncomingData(ctx context.Context) {
    defer func() {
        if !t.closed.Load() {
            go t.Close(errors.New("incoming data processing ended"))
        }
    }()
    
    for {
        if t.closed.Load() { return }
        
        // readFrameView returns ZERO-COPY buffer when data is contiguous!
        // The payloadBuf points directly into mmap'd ring memory
        fh, payloadBuf, err := readFrameView(ctx, t.serverToClient)
        if err != nil {
            if errors.Is(err, io.EOF) || errors.Is(err, ErrRingClosed) {
                return
            }
            continue
        }
        
        // Update keepalive timestamp
        atomic.StoreInt64(&t.lastRead, time.Now().UnixNano())
        
        // Get payload bytes - this is a DIRECT VIEW into shared memory!
        var payload []byte
        if payloadBuf != nil {
            payload = payloadBuf.ReadOnlyData()  // No copy, just pointer
        }
        
        // Handle transport-level frames
        switch fh.Type {
        case FrameTypeGOAWAY:
            // Handle graceful shutdown...
        case FrameTypeWindowUpdate:
            delta := binary.LittleEndian.Uint32(payload[:4])
            t.addSendQuota(fh.StreamID, delta)
            payloadBuf.Free()  // Release ring reservation
            continue
        }
        
        // Find stream
        t.mu.RLock()
        stream, ok := t.streams[fh.StreamID]
        t.mu.RUnlock()
        if !ok {
            payloadBuf.Free()
            continue
        }
        
        // Handle stream-level frames
        switch fh.Type {
        case FrameTypeMESSAGE:
            // Transfer buffer ownership to stream - NO COPY!
            // The stream receives a view directly into shared memory
            stream.write(recvMsg{buffer: mem.BufferSlice{payloadBuf}})
            // payloadBuf NOT freed here - stream owns it now
            
        case FrameTypeHEADERS:
            h, _ := decodeHeaders(payload)
            // ... populate metadata ...
            payloadBuf.Free()
        }
    }
}
```

##### Step 2b: readFrameView provides zero-copy access — ✓ ZERO COPY or 📋 1 COPY (rare)
**File:** [frame.go#L615](frame.go#L615)

```go
// ZERO-COPY when data is contiguous in ring (common case)
// 1 COPY only when data wraps around ring boundary (rare)
func readFrameView(ctx context.Context, rx *ShmRing) (FrameHeader, mem.Buffer, error) {
    for {
        // 1. Read frame header (16 bytes) - always copy header, it's tiny
        first, second, commitHeader, err := rx.ReadSlices(ctx, frameHeaderSize)
        if err != nil { return FrameHeader{}, nil, err }
        
        // Copy header bytes (16 bytes - trivial cost)
        var hb [frameHeaderSize]byte
        copy(hb[:], first)
        if len(first) < frameHeaderSize {
            copy(hb[len(first):], second)
        }
        commitHeader.Commit(frameHeaderSize)
        
        fh, _ := decodeFrameHeader(hb[:])
        
        // Skip PAD frames
        if fh.Type == FrameTypePAD {
            if fh.Length > 0 {
                rx.ReadExact(ctx, int(fh.Length), nil)
            }
            continue
        }
        
        if fh.Length == 0 {
            return fh, nil, nil
        }
        
        // 2. Read payload
        payloadLen := int(fh.Length)
        pFirst, pSecond, commitPayload, err := rx.ReadSlices(ctx, payloadLen)
        if err != nil { return FrameHeader{}, nil, err }
        
        // FAST PATH: Contiguous payload (no wrap-around)
        if len(pSecond) == 0 {
            contig := pFirst[:payloadLen]
            
            // Small payloads: copy and commit immediately
            if mem.IsBelowBufferPoolingThreshold(payloadLen) {
                commitPayload.Commit(payloadLen)
                result := make([]byte, payloadLen)
                copy(result, contig)
                return fh, mem.SliceBuffer(result), nil
            }
            
            // Large payloads: ZERO-COPY!
            // Return slice directly into mmap'd memory
            pool := &ringCommitPool{commit: *commitPayload}
            buf := mem.NewBuffer(&contig, pool)  // ← Zero-copy view
            return fh, buf, nil
        }
        
        // SLOW PATH: Wrap-around, must copy
        contig := make([]byte, payloadLen)
        copy(contig, pFirst)
        copy(contig[len(pFirst):], pSecond)
        commitPayload.Commit(payloadLen)
        return fh, mem.SliceBuffer(contig), nil
    }
}
```

---

#### Common Path (both transports) — 📋 COPIES for decompress/unmarshal

##### Step 3: Stream receives message (no copy)
**File:** [transport.go#L376](transport.go#L376)

```go
// NO COPY: Just passes reference through channel
func (s *Stream) write(m recvMsg) {
    s.buf.put(m)  // ← recvBuffer (channel-based queue), no data copy
}
```

##### Step 4: Application calls RecvMsg (no copy)
**File:** [stream.go#L1006](stream.go#L1006)

```go
func (cs *clientStream) RecvMsg(m any) error {
    // ... binlog, header logging ...
    
    err := cs.withRetry(func(a *csAttempt) error {
        return a.recvMsg(m, recvInfo)  // ← Dispatch to attempt
    }, cs.commitAttemptLocked)
    
    if err != nil || !cs.desc.ServerStreams {
        cs.finish(err)
    }
    return err
}
```

##### Step 5: csAttempt.recvMsg parses the message — 📋 COPIES for decompress/unmarshal
**File:** [stream.go#L1146](stream.go#L1146)

```go
func (a *csAttempt) recvMsg(m any, payInfo *payloadInfo) (err error) {
    // Setup decompressor based on response encoding
    if !a.decompressorSet {
        if ct := a.transportStream.RecvCompress(); ct != "" {
            a.decompressorV1 = encoding.GetCompressor(ct)
        }
        a.decompressorSet = true
    }
    
    // Parse and decode the message
    // This internally calls decompress() and codec.Unmarshal()
    // which may involve additional copies (common to both transports)
    if err := recv(&a.parser, cs.codec, a.transportStream, 
                   a.decompressorV0, m, *cs.callInfo.maxReceiveMessageSize,
                   payInfo, a.decompressorV1, false); err != nil {
        if err == io.EOF {
            return a.transportStream.Status().Err()
        }
        return toRPCErr(err)
    }
    return nil
}
```

##### Step 6: recv() decodes the message — 📋 COPIES (common to both)
**File:** [rpc_util.go#L1014](rpc_util.go#L1014)

```go
func recv(p *parser, c baseCodec, s recvCompressor, dc Decompressor, 
          m any, maxReceiveMessageSize int, payInfo *payloadInfo,
          compressor encoding.Compressor, isServer bool) error {
    
    // 1. Read and decompress
    // 📋 COPY (if compression enabled): decompress() allocates new buffer
    data, err := recvAndDecompress(p, s, dc, maxReceiveMessageSize, payInfo, compressor, isServer)
    if err != nil { return err }
    
    defer data.Free()
    
    // 📋 COPY: Unmarshal protobuf into struct fields
    // proto.Unmarshal() populates the message struct from bytes
    if err := c.Unmarshal(data, m); err != nil {
        return status.Errorf(codes.Internal, "failed to unmarshal: %v", err)
    }
    
    return nil
}
```

##### Step 7: parser.recvMsg reads from stream (no copy, reads from buffer)
**File:** [rpc_util.go#L771](rpc_util.go#L771)

```go
// NO COPY: Reads directly from recvBuffer which holds transport data
func (p *parser) recvMsg(maxReceiveMessageSize int) (payloadFormat, mem.BufferSlice, error) {
    // 1. Read gRPC header (5 bytes: 1 byte flags + 4 bytes length)
    err := p.r.ReadMessageHeader(p.header[:])  // ← Reads from recvBuffer
    if err != nil { return 0, nil, err }
    
    pf := payloadFormat(p.header[0])
    length := binary.BigEndian.Uint32(p.header[1:])
    
    // 2. Size checks
    if int(length) > maxReceiveMessageSize {
        return 0, nil, status.Errorf(codes.ResourceExhausted, "message too large")
    }
    
    // 3. Read message payload - returns reference, no copy
    data, err := p.r.Read(int(length))  // ← Reads from recvBuffer
    if err != nil { return 0, nil, err }
    
    return pf, data, nil
}
```

---

## Summary: Copy Locations by Layer

### Transport Layer Copies (where shmem wins)

| Layer | Operation | Unix/HTTP2 | Shmem |
|-------|-----------|------------|-------|
| **Write: HTTP/2 framing** | framer.writeData() | 📋 W-T1 | ✗ skipped |
| **Write: Kernel transition** | bufWriter.Flush() | 📋 W-T2 | ✗ skipped |
| **Write: Kernel IPC** | socket → socket | 📋 W-T3 | ✗ skipped |
| **Write: Ring buffer** | writeFrameBuffers() | ✗ N/A | 📋 W-S1 (only 1) |
| **Read: Kernel transition** | recvfrom() | 📋 R-T1 | ✗ skipped |
| **Read: HTTP/2 parsing** | readFrame() | 📋 R-T2 | ✗ skipped |
| **Read: Ring buffer** | readFrameView() | ✗ N/A | ✓ R-S1=0 (usually) |

### gRPC Layer Copies (COMMON - same for both transports)

| Layer | Operation | Both Transports |
|-------|-----------|-----------------|
| **Write: Encode** | proto.Marshal() | 📋 W-C1 (struct → bytes) |
| **Write: Compress** | compress() | 📋 W-C2 (if enabled) |
| **Read: Decompress** | decompress() | 📋 R-C1 (if enabled) |
| **Read: Decode** | proto.Unmarshal() | 📋 R-C2 (bytes → struct) |

### Total Payload Copies Per Direction

| Path | Unix/HTTP2 | Shmem | Savings |
|------|------------|-------|---------|
| **Write (transport)** | 3 copies (W-T1+T2+T3) | 1 copy (W-S1) | **67% fewer** |
| **Read (transport)** | 2 copies (R-T1+T2) | 0-1 copies (R-S1/S2) | **50-100% fewer** |
| **Round-trip (transport)** | 5 copies | 1-2 copies | **60-80% fewer** |

---

## Summary Comparison

| Aspect | Unix/HTTP2 | Shared Memory |
|--------|------------|---------------|
| **Write syscalls** | 1-2 per batch | 0-1 futex |
| **Write copies (transport)** | 3 (framer→bufWriter→kernel→socket) | 1 (→ring) |
| **Write latency** | ~3-5µs | ~0.5-0.7µs |
| **Goroutine hop** | Yes (loopyWriter) | No (direct) |
| **Read syscalls** | 1 per batch | 0-1 futex |
| **Read copies (transport)** | 2 (socket→kernel→bufReader→frame) | 0-1 (zero-copy) |
| **Read latency** | ~2-4µs | ~0.3-0.5µs |
| **Flow control** | HTTP/2 WINDOW_UPDATE | Custom shmem flow control |
| **Batching** | loopyWriter batches before flush | N/A (direct writes) |

---

## Key Files Reference

### HTTP/2 Transport (where copies happen)
- [http2_client.go#L1109](http2_client.go#L1109) - `http2Client.write()` (enqueue, no copy)
- [controlbuf.go#L586](controlbuf.go#L586) - `loopyWriter.run()` (processes queue)
- [http_util.go#L441](http_util.go#L441) - `framer.writeData()` — **📋 W-T1**
- [http_util.go#L319](http_util.go#L319) - `bufWriter.Flush()` — **📋 W-T2** (+ syscall)
- [http2_client.go#L1649](http2_client.go#L1649) - `http2Client.reader()` — **📋 R-T1** (syscall)
- [http_util.go](http_util.go) - `framer.readFrame()` — **📋 R-T2**
- [http2_client.go#L1188](http2_client.go#L1188) - `handleData()` (ref count, no copy)

### Shared Memory Transport (minimal copies)
- [shm_client_transport.go#L947](shm_client_transport.go#L947) - `ShmClientTransport.write()` (no copy, orchestrates)
- [frame.go#L435](frame.go#L435) - `writeFrameBuffers()` — **📋 W-S1 (only transport copy!)**
- [ring.go#L1117](ring.go#L1117) - `ReserveWrite()` (returns mmap slices)
- [ring.go#L1092](ring.go#L1092) - `Commit()` (publishes, 0-1 futex)
- [shm_client_transport.go#L291](shm_client_transport.go#L291) - `processIncomingData()` (no copy)
- [frame.go#L615](frame.go#L615) - `readFrameView()` — **✓ R-S1=0** (zero-copy, usually)

### Common gRPC Path (COMMON - copies in both transports)
- [stream.go#L937](stream.go#L937) - `clientStream.SendMsg()` (dispatches)
- [stream.go#L1095](stream.go#L1095) - `csAttempt.sendMsg()` — **📋 W-C1 + W-C2**
- [stream.go#L1006](stream.go#L1006) - `clientStream.RecvMsg()` (dispatches)
- [rpc_util.go#L771](rpc_util.go#L771) - `parser.recvMsg()` (reads from buffer)
- [rpc_util.go#L1014](rpc_util.go#L1014) - `recv()` — **📋 R-C1 + R-C2**
