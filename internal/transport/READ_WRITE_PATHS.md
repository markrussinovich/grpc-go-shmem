# gRPC-Go Read/Write Paths: HTTP2 vs Shared Memory Transport

This document traces the complete read and write paths for both HTTP2 (Unix socket/TCP) and shared memory transport in grpc-go.

---

## Table of Contents

1. [Write Path Overview](#write-path-overview)
2. [HTTP2 Write Path](#http2-write-path-detailed)
3. [Shared Memory Write Path](#shared-memory-write-path-detailed)
4. [Read Path Overview](#read-path-overview)
5. [HTTP2 Read Path](#http2-read-path-detailed)
6. [Shared Memory Read Path](#shared-memory-read-path-detailed)
7. [Key Data Structures](#key-data-structures)
8. [Flow Control Comparison](#flow-control-comparison)

---

## Write Path Overview

### High-Level Flow (Both Transports)

```
Application                    gRPC Core                      Transport Layer
    │                              │                              │
    ▼                              │                              │
clientStream.SendMsg(m)            │                              │
    │                              │                              │
    └──► prepareMsg()              │                              │
         (marshal + compress)      │                              │
              │                    │                              │
              ▼                    │                              │
         csAttempt.sendMsg()       │                              │
              │                    │                              │
              ▼                    │                              │
         a.transportStream.Write(hdr, payload, opts)              │
              │                    │                              │
              ├──────────────────────► HTTP2: http2Client.write() │
              │                    │  or                          │
              └──────────────────────► Shmem: ShmClientTransport.write()
```

---

## HTTP2 Write Path (Detailed)

### Entry Point: `stream.go` lines 938-1006

```go
// stream.go:938
func (cs *clientStream) SendMsg(m any) (err error) {
    // 1. Prepare message: marshal + compress
    hdr, data, payload, pf, err := prepareMsg(m, cs.codec, cs.compressorV0, 
        cs.compressorV1, cs.cc.dopts.copts.BufferPool)
    
    // 2. Size check
    if payloadLen > *cs.callInfo.maxSendMessageSize {
        return status.Errorf(codes.ResourceExhausted, ...)
    }
    
    // 3. Call transport write via csAttempt
    op := func(a *csAttempt) error {
        return a.sendMsg(m, hdr, payload, dataLen, payloadLen)
    }
    err = cs.withRetry(op, ...)
}
```

### csAttempt.sendMsg: `stream.go` lines 1122-1144

```go
// stream.go:1122
func (a *csAttempt) sendMsg(m any, hdr []byte, payld mem.BufferSlice, 
                            dataLength, payloadLength int) error {
    // Direct call to transport
    if err := a.transportStream.Write(hdr, payld, 
        &transport.WriteOptions{Last: !cs.desc.ClientStreams}); err != nil {
        return io.EOF
    }
    return nil
}
```

### http2Client.write(): `http2_client.go` lines 1109-1137

```go
// http2_client.go:1109
func (t *http2Client) write(s *ClientStream, hdr []byte, data mem.BufferSlice, 
                            opts *WriteOptions) error {
    // 1. Check/update stream state
    if opts.Last {
        if !s.compareAndSwapState(streamActive, streamWriteDone) {
            return errStreamDone
        }
    }
    
    // 2. Create dataFrame control buffer item
    df := &dataFrame{
        streamID:  s.id,
        endStream: opts.Last,
        h:         hdr,
        data:      data,
    }
    
    // 3. Wait for flow control quota (BLOCKING)
    if err := s.wq.get(int32(len(hdr) + dataLen)); err != nil {
        return err
    }
    
    // 4. Enqueue to controlBuf (wakes loopyWriter)
    if err := t.controlBuf.put(df); err != nil {
        return err
    }
    return nil
}
```

### controlBuf.put(): `controlbuf.go` lines 348-395

```go
// controlbuf.go:348
func (c *controlBuffer) put(it cbItem) error {
    _, err := c.executeAndPut(nil, it)
    return err
}

// controlbuf.go:360
func (c *controlBuffer) executeAndPut(f func() bool, it cbItem) (bool, error) {
    c.mu.Lock()
    defer c.mu.Unlock()
    
    // Wake up loopyWriter if waiting
    var wakeUp bool
    if c.consumerWaiting {
        wakeUp = true
        c.consumerWaiting = false
    }
    
    // Enqueue item
    c.list.enqueue(it)
    
    if wakeUp {
        // Signal wakeupCh to unblock loopyWriter.get()
        close(c.wakeupCh)  // effectively signals waiting goroutine
    }
}
```

### loopyWriter.run(): `controlbuf.go` lines 583-640

```go
// controlbuf.go:583
func (l *loopyWriter) run() (err error) {
    for {
        // 1. Block until item available
        it, err := l.cbuf.get(true)  // blocking get
        if err != nil {
            return err
        }
        
        // 2. Handle control frame (headers, settings, etc.)
        if err = l.handle(it); err != nil {
            return err
        }
        
        // 3. Process any pending data frames
        if _, err = l.processData(); err != nil {
            return err
        }
        
        // 4. Batch processing loop for efficiency
        for {
            it, err := l.cbuf.get(false)  // non-blocking
            if it == nil {
                // Flush to wire
                l.framer.writer.Flush()
                break
            }
            l.handle(it)
            l.processData()
        }
    }
}
```

### loopyWriter.processData(): `controlbuf.go` lines 940-1040

```go
// controlbuf.go:940
func (l *loopyWriter) processData() (bool, error) {
    str := l.activeStreams.dequeue()
    if str == nil {
        return true, nil  // no active streams
    }
    
    dataItem := str.itl.peek().(*dataFrame)
    
    // 1. Calculate how much we can send (flow control)
    maxSize := http2MaxFrameLen  // 16KB
    if strQuota := int(l.oiws) - str.bytesOutStanding; strQuota <= 0 {
        str.state = waitingOnStreamQuota
        return false, nil
    } else if maxSize > strQuota {
        maxSize = strQuota
    }
    if maxSize > int(l.sendQuota) {  // connection-level
        maxSize = int(l.sendQuota)
    }
    
    // 2. Copy data to writeBuf
    hSize := min(maxSize, len(dataItem.h))
    dSize := min(maxSize-hSize, reader.Remaining())
    l.writeBuf = append(l.writeBuf, dataItem.h[:hSize])
    l.writeBuf, _ = reader.Peek(dSize, l.writeBuf)
    
    // 3. Write HTTP/2 DATA frame
    err := l.framer.writeData(dataItem.streamID, endStream, l.writeBuf)
    
    // 4. Update quotas
    str.bytesOutStanding += size
    l.sendQuota -= uint32(size)
}
```

### framer.writeData(): `http_util.go` lines 441-475

```go
// http_util.go:441
func (f *framer) writeData(streamID uint32, endStream bool, data [][]byte) error {
    var flags http2.Flags
    if endStream {
        flags = http2.FlagDataEndStream
    }
    
    // Calculate total length
    length := uint32(0)
    for _, d := range data {
        length += uint32(len(d))
    }
    
    // 1. Write 9-byte HTTP/2 frame header manually
    f.headerBuf = append(f.headerBuf[:0],
        byte(length>>16), byte(length>>8), byte(length),  // 3 bytes: length
        byte(http2.FrameData),                            // 1 byte: type
        byte(flags),                                       // 1 byte: flags
        byte(streamID>>24), byte(streamID>>16),           // 4 bytes: stream ID
        byte(streamID>>8), byte(streamID))
    
    // 2. Write header to bufWriter
    f.writer.Write(f.headerBuf)
    
    // 3. Write payload chunks to bufWriter
    for _, d := range data {
        f.writer.Write(d)
    }
    return nil
}
```

### bufWriter.Write() → net.Conn: `http_util.go` lines 298-355

```go
// http_util.go:298
type bufWriter struct {
    buf       []byte      // internal buffer (typically 32KB)
    offset    int
    batchSize int
    conn      io.Writer   // underlying net.Conn
}

// http_util.go:319
func (w *bufWriter) Write(b []byte) (int, error) {
    for len(b) > 0 {
        copied := copy(w.buf[w.offset:], b)
        w.offset += copied
        
        if w.offset >= w.batchSize {
            // Buffer full → flush to kernel
            w.conn.Write(w.buf[:w.offset])  // ← SYSCALL: sendto()
            w.offset = 0
        }
    }
    return len(b), nil
}
```

### HTTP2 Write Path Summary

| Step | Function | File:Line | Description |
|------|----------|-----------|-------------|
| 1 | `SendMsg()` | stream.go:938 | Entry point |
| 2 | `prepareMsg()` | rpc_util.go | Marshal + compress |
| 3 | `csAttempt.sendMsg()` | stream.go:1122 | Call transport |
| 4 | `http2Client.write()` | http2_client.go:1109 | Create dataFrame |
| 5 | `s.wq.get()` | controlbuf.go | Wait for flow control |
| 6 | `controlBuf.put()` | controlbuf.go:348 | Enqueue + wake loopy |
| 7 | `loopyWriter.run()` | controlbuf.go:583 | Main write loop |
| 8 | `processData()` | controlbuf.go:940 | Build write buffer |
| 9 | `framer.writeData()` | http_util.go:441 | Build HTTP/2 frame |
| 10 | `bufWriter.Write()` | http_util.go:319 | Buffer writes |
| 11 | `net.Conn.Write()` | stdlib | **SYSCALL** to kernel |

**Total: 4+ memory copies, 1-2 syscalls per frame**

---

## Shared Memory Write Path (Detailed)

### ShmClientTransport.write(): `shm_client_transport.go` lines 946-995

```go
// shm_client_transport.go:946
func (t *ShmClientTransport) write(s *ClientStream, hdr []byte, data mem.BufferSlice, 
                                   opts *WriteOptions) error {
    // 1. Check transport/stream state
    if t.closed.Load() {
        return ErrConnClosing
    }
    if opts.Last {
        if !s.compareAndSwapState(streamActive, streamWriteDone) {
            return errStreamDone
        }
    }
    
    payloadLen := len(hdr) + data.Len()
    
    // 2. Wait for flow control quota (custom implementation)
    if err := t.acquireSendQuota(s.ctx, s.id, payloadLen); err != nil {
        return err
    }
    
    // 3. Build frame header
    fh := FrameHeader{
        StreamID: s.id,
        Type:     FrameTypeMESSAGE,
        Flags:    0,
    }
    if opts != nil && !opts.Last {
        fh.Flags = MessageFlagMORE
    }
    
    // 4. Write directly to ring buffer (no intermediate buffers!)
    if err := writeFrameBuffersChunked(s.ctx, t.clientToServer, fh, hdr, data, 0); err != nil {
        return err
    }
    return nil
}
```

### writeFrameBuffersChunked(): `frame.go` lines 500-556

```go
// frame.go:500
func writeFrameBuffersChunked(ctx context.Context, tx *ShmRing, fh FrameHeader, 
                               hdr []byte, data mem.BufferSlice, maxFramePayload int) error {
    payloadLen := len(hdr) + data.Len()
    
    // Calculate chunk size (half ring capacity by default)
    if maxFramePayload <= 0 {
        cap := int(tx.Capacity())
        maxFramePayload = cap/2 - frameHeaderSize
    }
    
    // FAST PATH: payload fits in single frame
    if payloadLen <= maxFramePayload {
        return writeFrameBuffers(ctx, tx, fh, hdr, data)
    }
    
    // SLOW PATH: chunk large payloads
    combined := make([]byte, payloadLen)
    copy(combined, hdr)
    // ... copy data buffers
    
    for len(remaining) > 0 {
        chunkFH := fh
        if len(remaining) > chunkSize {
            chunkFH.Flags |= MessageFlagMORE
        }
        writeFrame(ctx, tx, chunkFH, chunk)
    }
}
```

### writeFrameBuffers(): `frame.go` lines 432-497

```go
// frame.go:432
func writeFrameBuffers(ctx context.Context, tx *ShmRing, fh FrameHeader, 
                       hdr []byte, payload mem.BufferSlice) error {
    dataLen := payload.Len()
    payloadLen := len(hdr) + dataLen
    fh.Length = uint32(payloadLen)
    
    total := frameHeaderSize + payloadLen
    
    // 1. Reserve space in ring buffer (may block via futex)
    res, err := tx.ReserveWrite(ctx, total)
    if err != nil {
        return err
    }
    
    // 2. Encode frame header directly into ring memory
    var fhBytes [frameHeaderSize]byte
    encodeFrameHeaderTo(&fhBytes, fh)
    
    // 3. Write header + payload sequentially into reservation
    written := 0
    writeSeq := func(src []byte) error {
        // Copy to res.First, then res.Second if wrapping
        if written < len(res.First) {
            n := copy(res.First[written:], src)
            written += n
            src = src[n:]
        }
        if len(src) > 0 {
            copy(res.Second[written-len(res.First):], src)
            written += len(src)
        }
        return nil
    }
    
    writeSeq(fhBytes[:])           // Frame header
    writeSeq(hdr)                   // gRPC header (5 bytes)
    for _, buf := range payload {
        writeSeq(buf.ReadOnlyData()) // Message payload
    }
    
    // 4. Commit and signal reader (futex_wake if needed)
    return res.Commit(total)
}
```

### ShmRing.ReserveWrite(): `ring.go` lines 1117-1192

```go
// ring.go:1117
func (r *ShmRing) ReserveWrite(ctx context.Context, n int) (WriteReservation, error) {
    hdr := r.header()
    
    for {
        // Check context cancellation
        select {
        case <-ctx.Done():
            return WriteReservation{}, ctx.Err()
        default:
        }
        
        // Load current indices atomically
        writeIdx := hdr.WriteIndex()
        readIdx := hdr.ReadIndex()
        
        // Calculate available space
        usedBefore := writeIdx - readIdx
        available := r.capacity - usedBefore
        
        if uint64(n) <= available {
            // FAST PATH: Space available
            writePos := writeIdx & r.capMask
            
            // Return slices pointing directly into mmap'd memory
            if writePos+uint64(n) <= r.capacity {
                // No wrap: single contiguous slice
                firstPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
                first := unsafe.Slice((*byte)(firstPtr), n)
                return WriteReservation{First: first, ...}, nil
            } else {
                // Wrap: two slices
                firstLen := r.capacity - writePos
                // ... create First and Second slices
                return WriteReservation{First: first, Second: second, ...}, nil
            }
        }
        
        // SLOW PATH: Need to wait for space
        hdr.IncSpaceWaiters()
        spaceSeq := hdr.SpaceSequence()
        
        // Futex wait on spaceSeq (blocks until reader frees space)
        r.waitSpace(ctx, &hdr.spaceSeq, spaceSeq, timeout)
        hdr.DecSpaceWaiters()
    }
}
```

### WriteReservation.Commit(): `ring.go` lines 1092-1115

```go
// ring.go:1092
func (wr *WriteReservation) Commit(written int) error {
    hdr := wr.ring.header()
    
    // 1. Publish new write index (atomic store with release semantics)
    hdr.SetWriteIndex(wr.writeIdx + uint64(written))
    
    // 2. Increment data sequence and wake reader if waiting
    if written > 0 {
        hdr.IncrementDataSequence()
        if hdr.DataWaiters() > 0 {
            wr.ring.signalData(&hdr.dataSeq)  // futex_wake
        }
    }
    return nil
}
```

### Shared Memory Write Path Summary

| Step | Function | File:Line | Description |
|------|----------|-----------|-------------|
| 1 | `SendMsg()` | stream.go:938 | Entry point |
| 2 | `prepareMsg()` | rpc_util.go | Marshal + compress |
| 3 | `csAttempt.sendMsg()` | stream.go:1122 | Call transport |
| 4 | `ShmClientTransport.write()` | shm_client_transport.go:946 | Direct to ring |
| 5 | `acquireSendQuota()` | shm_client_transport.go | Flow control wait |
| 6 | `writeFrameBuffersChunked()` | frame.go:500 | Chunk if needed |
| 7 | `writeFrameBuffers()` | frame.go:432 | Build frame in ring |
| 8 | `ReserveWrite()` | ring.go:1117 | Get ring space (may futex) |
| 9 | `Commit()` | ring.go:1092 | Publish + wake reader |

**Total: 1 memory copy (direct to ring), 0-1 futex calls**

---

## Read Path Overview

### High-Level Flow (Both Transports)

```
Wire/Ring                      Transport Layer                    gRPC Core
    │                              │                                   │
    ▼                              │                                   │
 [bytes arrive]                    │                                   │
    │                              │                                   │
    └──► reader goroutine          │                                   │
         (http2Client.reader       │                                   │
          or processIncomingData)  │                                   │
              │                    │                                   │
              ▼                    │                                   │
         frame parsing             │                                   │
              │                    │                                   │
              ▼                    │                                   │
         handleData/MESSAGE        │                                   │
              │                    │                                   │
              ▼                    │                                   │
         s.write(recvMsg{buffer})  │                                   │
              │                    │                                   │
              └──────────────────────► recvBuffer.put()                │
                                   │        │                          │
                                   │        ▼                          │
                                   │   recvBufferReader.Read()         │
                                   │        │                          │
                                   │        ▼                          │
                                   │   parser.recvMsg()                │
                                   │        │                          │
                                   │        ▼                          │
                                   │   recv() → Unmarshal              │
                                   │        │                          │
                                   │        └──► Application receives m
```

---

## HTTP2 Read Path (Detailed)

### http2Client.reader(): `http2_client.go` lines 1649-1720

```go
// http2_client.go:1649
func (t *http2Client) reader(errCh chan<- error) {
    defer func() {
        close(t.readerDone)
        if errClose != nil {
            t.Close(errClose)
        }
    }()
    
    // Read server preface first
    t.readServerPreface()
    
    // Main read loop
    for {
        // 1. Throttle if control buffer is backed up
        t.controlBuf.throttle()
        
        // 2. Read next HTTP/2 frame (SYSCALL: recv())
        frame, err := t.framer.readFrame()
        
        if t.keepaliveEnabled {
            atomic.StoreInt64(&t.lastRead, time.Now().UnixNano())
        }
        
        // 3. Dispatch frame by type
        switch frame := frame.(type) {
        case *http2.MetaHeadersFrame:
            t.operateHeaders(frame)
        case *parsedDataFrame:
            t.handleData(frame)
            frame.data.Free()
        case *http2.RSTStreamFrame:
            t.handleRSTStream(frame)
        case *http2.SettingsFrame:
            t.handleSettings(frame, false)
        case *http2.PingFrame:
            t.handlePing(frame)
        case *http2.GoAwayFrame:
            errClose = t.handleGoAway(frame)
        case *http2.WindowUpdateFrame:
            t.handleWindowUpdate(frame)
        }
    }
}
```

### http2Client.handleData(): `http2_client.go` lines 1188-1249

```go
// http2_client.go:1188
func (t *http2Client) handleData(f *parsedDataFrame) {
    size := f.Header().Length
    
    // 1. Update bandwidth estimator
    if t.bdpEst != nil {
        sendBDPPing = t.bdpEst.add(size)
    }
    
    // 2. Connection-level flow control
    if w := t.fc.onData(size); w > 0 {
        t.controlBuf.put(&outgoingWindowUpdate{
            streamID:  0,
            increment: w,
        })
    }
    
    // 3. Find target stream
    s := t.getStream(f)
    if s == nil {
        return
    }
    
    // 4. Stream-level flow control
    if size > 0 {
        if err := s.fc.onData(size); err != nil {
            t.closeStream(s, io.EOF, ...)
            return
        }
        
        dataLen := f.data.Len()
        if dataLen > 0 {
            f.data.Ref()  // Take reference to buffer
            
            // 5. Deliver to stream's receive buffer
            s.write(recvMsg{buffer: f.data})
        }
    }
    
    // 6. Handle end of stream
    if f.StreamEnded() {
        t.closeStream(s, io.EOF, ...)
    }
}
```

### Stream.write() → recvBuffer: `transport.go` lines 376-378, 78-100

```go
// transport.go:376
func (s *Stream) write(m recvMsg) {
    s.buf.put(m)
}

// transport.go:78
func (b *recvBuffer) put(r recvMsg) {
    b.mu.Lock()
    
    // Fast path: channel is empty, try direct send
    if len(b.backlog) == 0 {
        select {
        case b.c <- r:
            b.mu.Unlock()
            return
        default:
        }
    }
    
    // Slow path: buffer in backlog
    b.backlog = append(b.backlog, r)
    b.mu.Unlock()
}
```

### Application Recv Path: `stream.go` lines 1008-1036, `rpc_util.go`

```go
// stream.go:1008
func (cs *clientStream) RecvMsg(m any) error {
    err := cs.withRetry(func(a *csAttempt) error {
        return a.recvMsg(m, recvInfo)
    }, cs.commitAttemptLocked)
    return err
}

// stream.go:1146
func (a *csAttempt) recvMsg(m any, payInfo *payloadInfo) (err error) {
    // Call recv() which reads from transport and unmarshals
    if err := recv(&a.parser, cs.codec, a.transportStream, 
                   a.decompressorV0, m, *cs.callInfo.maxReceiveMessageSize, 
                   payInfo, a.decompressorV1, false); err != nil {
        return err
    }
    return nil
}
```

### recv() and parser.recvMsg(): `rpc_util.go` lines 771-793, 1013-1028

```go
// rpc_util.go:771
func (p *parser) recvMsg(maxReceiveMessageSize int) (payloadFormat, mem.BufferSlice, error) {
    // 1. Read 5-byte gRPC header
    err := p.r.ReadMessageHeader(p.header[:])
    if err != nil {
        return 0, nil, err
    }
    
    pf := payloadFormat(p.header[0])
    length := binary.BigEndian.Uint32(p.header[1:])
    
    // 2. Size check
    if int(length) > maxReceiveMessageSize {
        return 0, nil, status.Errorf(codes.ResourceExhausted, ...)
    }
    
    // 3. Read payload bytes
    data, err := p.r.Read(int(length))
    return pf, data, nil
}

// rpc_util.go:1013
func recv(p *parser, c baseCodec, s recvCompressor, dc Decompressor, 
          m any, maxReceiveMessageSize int, payInfo *payloadInfo, 
          compressor encoding.Compressor, isServer bool) error {
    // 1. Receive and decompress
    data, err := recvAndDecompress(p, s, dc, maxReceiveMessageSize, 
                                    payInfo, compressor, isServer)
    
    // 2. Unmarshal into message
    defer data.Free()
    if err := c.Unmarshal(data, m); err != nil {
        return status.Errorf(codes.Internal, "failed to unmarshal: %v", err)
    }
    return nil
}
```

### recvBufferReader.Read(): `transport.go` lines 250-275

```go
// transport.go:250
func (r *recvBufferReader) readClient(n int) (buf mem.Buffer, err error) {
    select {
    case <-r.ctxDone:
        // Context canceled
        r.clientStream.Close(ContextErr(r.ctx.Err()))
        m := <-r.recv.get()
        return r.readAdditional(m, n)
    case m := <-r.recv.get():
        // Message received from recvBuffer
        return r.readAdditional(m, n)
    }
}

// transport.go:291
func (r *recvBufferReader) readAdditional(m recvMsg, n int) (b mem.Buffer, err error) {
    r.recv.load()  // Load next message from backlog if any
    if m.err != nil {
        if m.buffer != nil {
            m.buffer.Free()
        }
        return nil, m.err
    }
    
    // Return the buffer (possibly split if larger than n)
    if m.buffer.Len() > n {
        m.buffer, r.last = mem.SplitUnsafe(m.buffer, n)
    }
    return m.buffer, nil
}
```

### HTTP2 Read Path Summary

| Step | Function | File:Line | Description |
|------|----------|-----------|-------------|
| 1 | `reader()` | http2_client.go:1649 | Reader goroutine loop |
| 2 | `framer.readFrame()` | net/http2 | **SYSCALL** recv from socket |
| 3 | `handleData()` | http2_client.go:1188 | Process DATA frame |
| 4 | `s.fc.onData()` | transport.go | Flow control accounting |
| 5 | `s.write(recvMsg)` | transport.go:376 | Deliver to stream buffer |
| 6 | `recvBuffer.put()` | transport.go:78 | Buffer message |
| 7 | `RecvMsg()` | stream.go:1008 | Application entry point |
| 8 | `recvBufferReader.Read()` | transport.go:250 | Block on channel |
| 9 | `parser.recvMsg()` | rpc_util.go:771 | Parse gRPC header |
| 10 | `recv()` | rpc_util.go:1013 | Decompress + unmarshal |

---

## Shared Memory Read Path (Detailed)

### processIncomingData() (Client): `shm_client_transport.go` lines 291-511

```go
// shm_client_transport.go:291
func (t *ShmClientTransport) processIncomingData(ctx context.Context) {
    defer func() {
        if !t.closed.Load() {
            go t.Close(errors.New("incoming data processing ended"))
        }
    }()
    
    for {
        if t.closed.Load() {
            return
        }
        
        // 1. Read frame from ring (blocks via futex if empty)
        fh, payloadBuf, err := readFrameView(ctx, t.serverToClient)
        if err != nil {
            // Handle EOF, context cancel, ring closed
            return
        }
        
        // 2. Update keepalive timestamp
        atomic.StoreInt64(&t.lastRead, time.Now().UnixNano())
        
        // 3. Dispatch by frame type
        switch fh.Type {
        case FrameTypeGOAWAY:
            // Handle graceful shutdown
            t.draining.Store(true)
            
        case FrameTypeWindowUpdate:
            delta := binary.LittleEndian.Uint32(payload[:4])
            t.addSendQuota(fh.StreamID, delta)
            
        case FrameTypeMESSAGE:
            // Find stream and deliver
            t.mu.RLock()
            stream, ok := t.streams[fh.StreamID]
            t.mu.RUnlock()
            
            if ok {
                // Apply flow control
                sz := uint32(len(payload))
                if wu := t.connInFlow.onData(sz); wu > 0 {
                    t.sendWindowUpdate(0, wu)
                }
                if err := stream.fc.onData(sz); err != nil {
                    // Flow control error
                    t.closeStream(stream, err, ...)
                    continue
                }
                
                // Zero-copy delivery: transfer buffer ownership
                if payloadBuf != nil {
                    payloadTransferred = true
                    stream.write(recvMsg{buffer: payloadBuf})
                    payloadBuf = nil
                }
            }
            
        case FrameTypeTRAILERS:
            // Handle trailers and close stream
            tr, _ := decodeTrailers(payload)
            t.closeStream(stream, err, false, 0, st, trailerMap, true)
            
        case FrameTypeCANCEL:
            stream.write(recvMsg{err: context.Canceled})
        }
    }
}
```

### readFrameView(): `frame.go` lines 611-690

```go
// frame.go:611
func readFrameView(ctx context.Context, rx *ShmRing) (FrameHeader, mem.Buffer, error) {
    for {
        // 1. Read frame header (16 bytes) from ring
        first, second, commitHeader, err := rx.ReadSlices(ctx, frameHeaderSize)
        if err != nil {
            return FrameHeader{}, nil, err
        }
        
        // Copy header bytes (may be split across wrap)
        var hb [frameHeaderSize]byte
        copy(hb[:], first)
        if len(first) < frameHeaderSize {
            copy(hb[len(first):], second)
        }
        commitHeader.Commit(frameHeaderSize)
        
        // 2. Decode frame header
        fh, err := decodeFrameHeader(hb[:])
        if err != nil {
            return FrameHeader{}, nil, err
        }
        
        // Skip padding frames
        if fh.Type == FrameTypePAD {
            if fh.Length > 0 {
                rx.ReadExact(ctx, int(fh.Length), nil)
            }
            continue
        }
        
        if fh.Length == 0 {
            return fh, nil, nil
        }
        
        // 3. Read payload (zero-copy when contiguous)
        pFirst, pSecond, commitPayload, err := rx.ReadSlices(ctx, int(fh.Length))
        if err != nil {
            return FrameHeader{}, nil, err
        }
        
        // FAST PATH: contiguous payload (no wrap)
        if len(pSecond) == 0 {
            contig := pFirst[:fh.Length]
            
            // Small payloads: copy and commit immediately
            if mem.IsBelowBufferPoolingThreshold(int(fh.Length)) {
                commitPayload.Commit(int(fh.Length))
                result := make([]byte, fh.Length)
                copy(result, contig)
                return fh, mem.SliceBuffer(result), nil
            }
            
            // Large payloads: wrap in buffer with deferred commit
            pool := &ringCommitPool{commit: *commitPayload}
            buf := mem.NewBuffer(&contig, pool)
            return fh, buf, nil
        }
        
        // SLOW PATH: wrapped payload (copy to contiguous)
        contig := make([]byte, fh.Length)
        copy(contig, pFirst)
        copy(contig[len(pFirst):], pSecond)
        commitPayload.Commit(int(fh.Length))
        return fh, mem.SliceBuffer(contig), nil
    }
}
```

### ShmRing.ReadSlices(): `ring.go` (conceptual, similar to ReadBlocking)

```go
// Returns slices directly into mmap'd ring memory
func (r *ShmRing) ReadSlices(ctx context.Context, n int) (first, second []byte, 
                                                          commit *ReadCommit, error) {
    hdr := r.header()
    
    for {
        // Check context
        select {
        case <-ctx.Done():
            return nil, nil, nil, ctx.Err()
        default:
        }
        
        writeIdx := hdr.WriteIndex()
        readIdx := hdr.ReadIndex()
        used := writeIdx - readIdx
        
        if used >= uint64(n) {
            // Data available - return slices into ring memory
            readPos := readIdx & r.capMask
            
            if readPos+uint64(n) <= r.capacity {
                // No wrap
                ptr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
                first = unsafe.Slice((*byte)(ptr), n)
                return first, nil, &ReadCommit{...}, nil
            } else {
                // Wrap case
                firstLen := r.capacity - readPos
                // ... create two slices
                return first, second, &ReadCommit{...}, nil
            }
        }
        
        // Wait for data via futex
        hdr.IncDataWaiters()
        dataSeq := hdr.DataSequence()
        r.waitData(ctx, &hdr.dataSeq, dataSeq, timeout)  // futex_wait
        hdr.DecDataWaiters()
    }
}
```

### Shared Memory Read Path Summary

| Step | Function | File:Line | Description |
|------|----------|-----------|-------------|
| 1 | `processIncomingData()` | shm_client_transport.go:291 | Reader goroutine |
| 2 | `readFrameView()` | frame.go:611 | Get frame from ring |
| 3 | `ReadSlices()` | ring.go | Get slices (may futex_wait) |
| 4 | `decodeFrameHeader()` | frame.go | Parse 16-byte header |
| 5 | Handle MESSAGE | shm_client_transport.go:420+ | Flow control + dispatch |
| 6 | `stream.write(recvMsg)` | transport.go:376 | Deliver to stream (zero-copy) |
| 7 | `recvBuffer.put()` | transport.go:78 | Buffer message |
| 8 | `RecvMsg()` | stream.go:1008 | Application entry point |
| 9 | `parser.recvMsg()` | rpc_util.go:771 | Parse gRPC header |
| 10 | `recv()` | rpc_util.go:1013 | Decompress + unmarshal |

**Key Difference: Zero-copy delivery when payload is contiguous in ring**

---

## Key Data Structures

### HTTP2 Transport

```go
// controlbuf.go:154 - Data frame for write queue
type dataFrame struct {
    streamID   uint32
    endStream  bool
    h          []byte         // gRPC message header
    data       mem.BufferSlice
    processing bool
    onEachWrite func()
}

// controlbuf.go:307 - Write queue
type controlBuffer struct {
    wakeupCh chan struct{}   // Wakes loopyWriter
    list     *itemList       // Queue of frames
    mu       sync.Mutex
}

// controlbuf.go:513 - Writer goroutine state
type loopyWriter struct {
    cbuf          *controlBuffer
    sendQuota     uint32         // Connection-level flow control
    estdStreams   map[uint32]*outStream
    activeStreams *outStreamList
    framer        *framer
    conn          net.Conn
}
```

### Shared Memory Transport

```go
// ring.go:1080 - Write reservation (zero-copy)
type WriteReservation struct {
    First    []byte   // Direct pointer into mmap'd memory
    Second   []byte   // For wrap-around
    ring     *ShmRing
    writeIdx uint64
    maxBytes int
}

// frame.go:23 - Frame header (16 bytes)
type FrameHeader struct {
    Type      FrameType  // MESSAGE, HEADERS, etc.
    Flags     uint8
    StreamID  uint32
    Length    uint32
    Reserved  uint16
    Reserved2 uint16
}

// shm_client_transport.go:44 - Transport state
type ShmClientTransport struct {
    segment        *Segment
    clientToServer *ShmRing  // Write ring
    serverToClient *ShmRing  // Read ring
    streams        map[uint32]*ClientStream
    connSendQuota  int64
    streamSendQuota map[uint32]int64
}
```

---

## Flow Control Comparison

### HTTP2 Flow Control (Write Path)

1. **Stream quota**: `s.wq.get(size)` blocks in `write()` before creating dataFrame
2. **Connection quota**: Checked in `loopyWriter.processData()` 
3. **Window updates**: Received via WINDOW_UPDATE frames, delivered to controlBuf

```go
// http2_client.go:1121
if err := s.wq.get(int32(len(hdr) + dataLen)); err != nil {
    return err  // Blocks until quota available
}
```

### Shared Memory Flow Control (Write Path)

1. **Unified quota check**: `acquireSendQuota()` checks both stream and connection
2. **Waits on channel**: Uses `quotaSignal` channel for notifications
3. **Window updates**: Received via FrameTypeWindowUpdate, updates quota atomically

```go
// shm_client_transport.go:969
if err := t.acquireSendQuota(s.ctx, s.id, payloadLen); err != nil {
    return err  // Blocks until quota available
}
```

### Key Difference

| Aspect | HTTP2 | Shared Memory |
|--------|-------|---------------|
| Backpressure mechanism | Channel + loopyWriter | Ring buffer full + futex |
| Batching | loopyWriter batches frames | Direct write, no batching |
| Syscalls per write | 1-2 (sendto) | 0-1 (futex_wake if reader waiting) |
| Memory copies | 4+ (app→heap→bufWriter→kernel→peer) | 1 (app→ring mmap) |

---

## Server-Side Write Path

### HTTP2 Server: `http2_server.go`

Server write path is nearly identical to client:
1. `ServerStream.SendMsg()` → `prepareMsg()` → `ss.t.write()`
2. `http2Server.write()` creates `dataFrame`, puts in `controlBuf`
3. `loopyWriter.run()` dequeues and writes to `framer`

### Shared Memory Server: `shm_server_transport.go`

```go
// shm_server_transport.go:956
func (t *ShmServerTransport) write(s *ServerStream, hdr []byte, data mem.BufferSlice, 
                                   _ *WriteOptions) error {
    // 1. Maybe write headers first
    if err := t.maybeWriteHeader(s); err != nil {
        return err
    }
    
    // 2. Flow control
    if err := t.acquireSendQuota(s.ctx, s.id, payloadLen); err != nil {
        return err
    }
    
    // 3. Direct ring write (holding writeMu for serialization)
    t.writeMu.Lock()
    defer t.writeMu.Unlock()
    return writeFrameBuffersChunked(context.Background(), t.serverToClient, fh, hdr, data, 0)
}
```

---

## Summary: Key Differences

| Aspect | HTTP2/Unix Socket | Shared Memory |
|--------|-------------------|---------------|
| **Write syscalls** | `sendto()` every flush | `futex_wake` only if reader waiting |
| **Read syscalls** | `recv()` every read | `futex_wait` only if ring empty |
| **Memory copies** | 4+ per message | 1 per message (direct to ring) |
| **Batching** | loopyWriter batches in bufWriter | No batching (direct commit) |
| **Backpressure** | TCP flow control + gRPC flow control | Ring full + futex wait |
| **Zero-copy read** | No (kernel → userspace copy) | Yes when contiguous in ring |
| **Frame format** | HTTP/2 (9-byte header) | Custom (16-byte header) |
