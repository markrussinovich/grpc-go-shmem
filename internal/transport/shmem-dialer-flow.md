# Shared Memory Transport: Dialer Flow

This document describes how the shared memory (shmem) transport plugs into gRPC's dial flow, comparing it with Unix sockets to illustrate the integration points.

## Architecture Diagram

See [shmem_vs_unix_architecture.png](shmem_vs_unix_architecture.png) for a visual representation.

---

## Dial Setup Flow

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           grpc.NewClient(target)                            │
└─────────────────────────────────────┬───────────────────────────────────────┘
                                      │
                                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                    Resolver Selection (by scheme)                            │
├─────────────────────────────────┬───────────────────────────────────────────┤
│     unix:///var/run/sock        │           shm://segment_name              │
│              │                  │                    │                      │
│              ▼                  │                    ▼                      │
│   unix.resolverBuilder          │         shmResolverBuilder                │
│   networktype.Set("unix")       │         returns "shm:segment"             │
└─────────────────────────────────┴───────────────────────────────────────────┘
                                      │
                                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                     addrConn.createTransport()                               │
│                              │                                               │
│                              ▼                                               │
│                   transport.NewHTTP2Client()                                 │
└─────────────────────────────────┬───────────────────────────────────────────┘
                                  │
                                  ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           dial(ctx, opts)                                    │
│                                                                              │
│  opts.Dialer is called (either default or custom from WithContextDialer)    │
└─────────────────────────────────┬───────────────────────────────────────────┘
                                  │
            ┌─────────────────────┴─────────────────────┐
            │                                           │
            ▼                                           ▼
┌───────────────────────────┐             ┌─────────────────────────────────┐
│      UNIX SOCKET          │             │        SHARED MEMORY            │
├───────────────────────────┤             ├─────────────────────────────────┤
│                           │             │                                 │
│  net.Dialer.DialContext   │             │  transport.DialShm()            │
│  ("unix", "/var/run/...")  │             │       │                         │
│           │               │             │       ▼                         │
│           ▼               │             │  CreateSegment/OpenSegment      │
│      net.Conn             │             │       │                         │
│   (Unix domain socket)    │             │       ▼                         │
│                           │             │  ShmClientTransport             │
│                           │             │       │                         │
│                           │             │       ▼                         │
│                           │             │  shmClientConn (wrapper)        │
│                           │             │  implements:                    │
│                           │             │   - net.Conn (stub)             │
│                           │             │   - ClientTransportProvider     │
└─────────────┬─────────────┘             └───────────────┬─────────────────┘
              │                                           │
              ▼                                           ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                                                                              │
│   ══════════════════════ DIVERGENCE POINT ══════════════════════            │
│                                                                              │
│   http2_client.go:237-240:                                                   │
│   ┌────────────────────────────────────────────────────────────────────┐    │
│   │ if provider, ok := conn.(ClientTransportProvider); ok {            │    │
│   │     return provider.GetClientTransport(), nil  // ← SHMEM EXITS    │    │
│   │ }                                                                  │    │
│   │ // Continue building http2Client...             // ← UNIX CONTINUES│    │
│   └────────────────────────────────────────────────────────────────────┘    │
│                                                                              │
└─────────────────────────────────┬───────────────────────────────────────────┘
                                  │
            ┌─────────────────────┴─────────────────────┐
            │                                           │
            ▼                                           ▼
┌───────────────────────────┐             ┌─────────────────────────────────┐
│      UNIX SOCKET          │             │        SHARED MEMORY            │
├───────────────────────────┤             ├─────────────────────────────────┤
│                           │             │                                 │
│  Create http2Client:      │             │  Return directly:               │
│   - Wrap net.Conn         │             │   - ShmClientTransport          │
│   - Create framer         │             │   - Already initialized         │
│   - Create controlBuf     │             │   - Has ring buffers ready      │
│   - Start loopyWriter     │             │                                 │
│   - Start reader loop     │             │                                 │
│                           │             │                                 │
└───────────────────────────┘             └─────────────────────────────────┘
              │                                           │
              ▼                                           ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                         ClientTransport (interface)                          │
│                                                                              │
│  Both http2Client and ShmClientTransport implement this interface            │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Unary RPC Write Flow

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        grpc.Invoke(ctx, method, req, reply)                  │
└─────────────────────────────────────┬───────────────────────────────────────┘
                                      │
                                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           newClientStream()                                  │
│                                  │                                           │
│                                  ▼                                           │
│                      ClientTransport.NewStream()                             │
└─────────────────────────────────────┬───────────────────────────────────────┘
                                      │
            ┌─────────────────────────┴─────────────────────┐
            │                                               │
            ▼                                               ▼
┌───────────────────────────┐                 ┌─────────────────────────────┐
│      UNIX (http2Client)   │                 │  SHMEM (ShmClientTransport) │
├───────────────────────────┤                 ├─────────────────────────────┤
│                           │                 │                             │
│ controlBuf.executeAndPut  │                 │ writeFrame(HEADERS, ...)    │
│   (headerFrame)           │                 │       │                     │
│       │                   │                 │       ▼                     │
│       ▼                   │                 │ ShmRing.ReserveWrite()      │
│ loopyWriter wakes up      │                 │       │                     │
│       │                   │                 │ encode + copy to ring       │
│       ▼                   │                 │       │                     │
│ framer.writeHeaders()     │                 │ Commit()                    │
│       │                   │                 │                             │
│       ▼                   │                 │                             │
│ net.Conn.Write()          │                 │                             │
│ (syscall: sendto)         │                 │                             │
│                           │                 │                             │
└───────────────────────────┘                 └─────────────────────────────┘
              │                                               │
              ▼                                               ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Stream created                                  │
└─────────────────────────────────────┬───────────────────────────────────────┘
                                      │
                                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                    cs.SendMsg(req) - marshal + compress                      │
│                                  │                                           │
│                                  ▼                                           │
│                       ClientStream.Write(hdr, data)                          │
│                                  │                                           │
│                                  ▼                                           │
│                        s.ct.write(s, hdr, data, opts)                        │
│                                                                              │
│                    s.ct is clientTransport interface                         │
└─────────────────────────────────────┬───────────────────────────────────────┘
                                      │
            ┌─────────────────────────┴─────────────────────┐
            │                                               │
            ▼                                               ▼
┌───────────────────────────────────────┐   ┌─────────────────────────────────┐
│         UNIX (http2Client.write)      │   │ SHMEM (ShmClientTransport.write)│
├───────────────────────────────────────┤   ├─────────────────────────────────┤
│                                       │   │                                 │
│ 1. s.wq.get(sz) - wait for quota      │   │ 1. acquireSendQuota(streamID)   │
│         │                             │   │         │                       │
│         ▼                             │   │         ▼                       │
│ 2. Create dataFrame{                  │   │ 2. Build FrameHeader{           │
│      streamID, h: hdr, data: data     │   │      StreamID, Type: MESSAGE,   │
│    }                                  │   │      Length: len(payload)       │
│         │                             │   │    }                            │
│         ▼                             │   │         │                       │
│ 3. t.controlBuf.put(df)               │   │         ▼                       │
│    (enqueue for async write)          │   │ 3. writeFrameBuffersChunked()   │
│         │                             │   │    (direct sync write)          │
│         ▼                             │   │         │                       │
│ 4. loopyWriter.run() loop:            │   │         ▼                       │
│    ┌──────────────────────────┐       │   │ 4. ShmRing.ReserveWrite(size)   │
│    │ Wait on controlBuf.get() │       │   │    ┌─────────────────────────┐  │
│    │         │                │       │   │    │ Atomic check space      │  │
│    │         ▼                │       │   │    │ If full: futex_wait     │  │
│    │ handle(dataFrame)        │       │   │    │ Return WriteReservation │  │
│    │         │                │       │   │    └─────────────────────────┘  │
│    │         ▼                │       │   │         │                       │
│    │ preprocessData()         │       │   │         ▼                       │
│    │ (flow control check)     │       │   │ 5. Copy header + data to ring   │
│    │         │                │       │   │    ┌─────────────────────────┐  │
│    │         ▼                │       │   │    │ encodeFrameHeaderTo()   │  │
│    │ processData()            │       │   │    │ copy(res.First, hdr)    │  │
│    │         │                │       │   │    │ copy(res.First/Second,  │  │
│    │         ▼                │       │   │    │       payload)          │  │
│    │ l.framer.writeData()     │       │   │    └─────────────────────────┘  │
│    └──────────────────────────┘       │   │         │                       │
│         │                             │   │         ▼                       │
│         ▼                             │   │ 6. res.Commit(totalBytes)       │
│ 5. framer.writeData():                │   │    ┌─────────────────────────┐  │
│    ┌──────────────────────────┐       │   │    │ Atomic update writeIdx  │  │
│    │ Build 9-byte HTTP2 header│       │   │    │ If reader waiting:      │  │
│    │ Write header to bufWriter│       │   │    │   futex_wake            │  │
│    │ Write data chunks        │       │   │    └─────────────────────────┘  │
│    │ Flush bufWriter          │       │   │                                 │
│    └──────────────────────────┘       │   │                                 │
│         │                             │   │                                 │
│         ▼                             │   │                                 │
│ 6. bufWriter → net.Conn.Write()       │   │                                 │
│    ┌──────────────────────────┐       │   │                                 │
│    │ syscall: sendto()        │       │   │                                 │
│    │ Kernel copies to socket  │       │   │                                 │
│    │ buffer, schedules TX     │       │   │                                 │
│    └──────────────────────────┘       │   │                                 │
│                                       │   │                                 │
└───────────────────────────────────────┘   └─────────────────────────────────┘
```

---

## Data Path Comparison

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                            UNIX SOCKET PATH                                  │
│                                                                              │
│  User Space                              │  Kernel Space                     │
│  ────────────────────────────────────────┼───────────────────────────────────│
│                                          │                                   │
│  ┌──────────┐   copy    ┌──────────┐     │    ┌──────────┐    ┌──────────┐  │
│  │ gRPC msg │ ───────▶  │ bufWriter│  sendto  │ Socket   │    │ Receiver │  │
│  │ (heap)   │           │ (heap)   │ ───────▶ │ Buffer   │───▶│ Buffer   │  │
│  └──────────┘           └──────────┘     │    └──────────┘    └──────────┘  │
│                                          │          │               │       │
│       1 copy                1 copy       │     1 copy          1 copy       │
│                              + syscall   │                                   │
│                                          │                                   │
│  Total: 4 copies + 2 syscalls per direction                                 │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────┐
│                         SHARED MEMORY PATH                                   │
│                                                                              │
│  Process A (Writer)                      │  Process B (Reader)               │
│  ────────────────────────────────────────┼───────────────────────────────────│
│                                          │                                   │
│  ┌──────────┐   copy    ┌────────────────┴───────────────┐                  │
│  │ gRPC msg │ ───────▶  │     Shared Memory Ring Buffer  │  ◀─── read      │
│  │ (heap)   │           │     (mmap'd in both processes) │       (0 copy)  │
│  └──────────┘           └────────────────────────────────┘                  │
│                                          │                                   │
│       1 copy                             │         0 copies                  │
│   (+ futex_wake if reader waiting)       │    (+ futex_wait if empty)        │
│                                          │                                   │
│  Total: 1 copy + 0-1 syscalls per direction                                 │
│                                                                              │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Code Walkthrough

### Step 1: User calls `grpc.NewClient` with shmem options

```go
// Example usage
conn, err := grpc.NewClient("shm://my_segment", 
    grpc.WithShmTransport(),
    grpc.WithTransportCredentials(insecure.NewCredentials()))
```

---

### Step 2: `WithShmTransport()` sets up the custom dialer

📁 [shm_grpc_helpers.go](../../shm_grpc_helpers.go#L37-L80)

```go
// WithShmTransport returns a DialOption that configures the client to use
// shared memory transport. This should be used with addresses of the form
// "shm://segment_name".
func WithShmTransport() DialOption {
	return WithShmTransportAndOptions(nil)
}

func WithShmTransportAndOptions(opts *transport.DialOptions) DialOption {
	if opts == nil {
		opts = transport.DefaultDialOptions()
	}

	// Create a context dialer that understands shm:// addresses
	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		// Check if this is a shm:// address
		if strings.HasPrefix(addr, "shm:") {
			// Extract segment name from "shm:segment_name"
			segmentName := strings.TrimPrefix(addr, "shm:")

			// Use the shared memory dialer
			clientTransport, err := transport.DialShm(ctx, segmentName, opts)
			if err != nil {
				return nil, fmt.Errorf("failed to dial shared memory segment %q: %v", segmentName, err)
			}

			// Wrap the transport in a net.Conn-compatible interface
			shmTransport := clientTransport.(*transport.ShmClientTransport)
			localAddr := &transport.ShmAddr{Name: segmentName + "_client"}
			return &shmClientConn{
				transport:  shmTransport,
				localAddr:  localAddr,
				remoteAddr: shmTransport.RemoteAddr(),
			}, nil
		}
		return nil, fmt.Errorf("WithShmTransport can only dial shm:// addresses, got: %s", addr)
	}

	return WithContextDialer(dialer)  // <-- Plugs into standard gRPC dial options
}
```

---

### Step 3: `WithContextDialer` stores the dialer in options

📁 [dialoptions.go](../../dialoptions.go#L484-L488)

```go
func WithContextDialer(f func(context.Context, string) (net.Conn, error)) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.copts.Dialer = f   // <-- Stored in ConnectOptions.Dialer
	})
}
```

---

### Step 4: Resolver registers for `shm://` scheme

📁 [resolver.go](resolver.go#L27-L95)

```go
const scheme = "shm"

type shmResolverBuilder struct{}

func (*shmResolverBuilder) Build(target resolver.Target, cc resolver.ClientConn, _ resolver.BuildOptions) (resolver.Resolver, error) {
	segmentName := target.Endpoint()
	if segmentName == "" {
		segmentName = target.URL.Host
	}
	
	r := &shmResolver{
		target:      target,
		cc:          cc,
		segmentName: segmentName,
	}
	r.start()
	return r, nil
}

func (*shmResolverBuilder) Scheme() string {
	return scheme  // "shm"
}

func (r *shmResolver) start() {
	// Resolve to "shm:segment_name" address format
	addr := resolver.Address{
		Addr:       fmt.Sprintf("shm:%s", r.segmentName),  // <-- This is what the dialer sees
		ServerName: r.segmentName,
	}
	r.cc.UpdateState(resolver.State{
		Addresses: []resolver.Address{addr},
	})
}

// Registers on package init
func init() {
	resolver.Register(&shmResolverBuilder{})
}
```

---

### Step 5: `dial()` calls the custom dialer

📁 [http2_client.go](http2_client.go#L158-L187)

```go
func dial(ctx context.Context, fn func(context.Context, string) (net.Conn, error), addr resolver.Address, grpcUA string) (net.Conn, error) {
	address := addr.Addr   // "shm:my_segment"
	networkType, ok := networktype.Get(addr)
	
	if fn != nil {   // <-- fn is the custom dialer from WithShmTransport
		// ... unix special handling ...
		return fn(ctx, address)   // <-- Calls our shmem dialer with "shm:my_segment"
	}
	// Default path for TCP/Unix without custom dialer
	return internal.NetDialerWithTCPKeepalive().DialContext(ctx, networkType, address)
}
```

---

### Step 6: `DialShm` creates the transport

📁 [shm_dialer.go](shm_dialer.go#L62-L150)

```go
func DialShm(ctx context.Context, addr string, opts *DialOptions) (ClientTransport, error) {
	if opts == nil {
		opts = DefaultDialOptions()
	}

	// 1. Open control segment for handshake
	ctlName := addr + shmControlSuffix
	ctlSeg, err := OpenSegment(ctlName)
	// ...

	// 2. Wait for server to be ready
	if err := ctlSeg.WaitForServer(ctx); err != nil { ... }

	// 3. Create control rings and do handshake
	ctlTx := NewShmRingFromSegment(ctlSeg.A, ctlSeg.Mem)
	ctlRx := NewShmRingFromSegment(ctlSeg.B, ctlSeg.Mem)

	// 4. Send CONNECT request
	if err := writeFrame(ctx, ctlTx, FrameHeader{Type: FrameTypeCONNECT}, ...); err != nil { ... }

	// 5. Read response
	respFH, respPayload, err := readFrame(ctx, ctlRx)
	
	switch respFH.Type {
	case FrameTypeACCEPT:
		resp, _ := decodeConnectResponse(respPayload)
		segName := resp.segmentName

		// 6. Open the data segment
		segment, err := OpenSegment(segName)

		// 7. Wait for server and signal client ready
		segment.WaitForServer(ctx)
		segment.SetClientReadyAndSignal(true)

		// 8. Create the ShmClientTransport
		clientTransport, err := NewShmClientTransport(segment, localAddr, remoteAddr)
		return clientTransport, nil
	}
}
```

---

### Step 7: **DIVERGENCE POINT** - `NewHTTP2Client` checks for `ClientTransportProvider`

📁 [http2_client.go](http2_client.go#L204-L240)

```go
// ClientTransportProvider is an interface for connections that provide their own ClientTransport.
// This allows custom transports (like shared memory) to be used with gRPC's standard APIs.
type ClientTransportProvider interface {
	GetClientTransport() ClientTransport
}

func NewHTTP2Client(connectCtx, ctx context.Context, addr resolver.Address, opts ConnectOptions, onClose func(GoAwayReason)) (_ ClientTransport, err error) {
	// ...
	
	conn, err := dial(connectCtx, opts.Dialer, addr, opts.UserAgent)
	if err != nil { ... }

	// ═══════════════════════════════════════════════════════════════════
	// THIS IS THE KEY DIVERGENCE POINT
	// ═══════════════════════════════════════════════════════════════════
	
	// Check if the connection provides its own transport (e.g., shared memory transport)
	if provider, ok := conn.(ClientTransportProvider); ok {
		// Use the custom transport directly instead of wrapping in HTTP2
		return provider.GetClientTransport(), nil   // <-- SHMEM PATH EXITS HERE
	}

	// ═══════════════════════════════════════════════════════════════════
	// HTTP2/TCP/UNIX PATH CONTINUES BELOW
	// ═══════════════════════════════════════════════════════════════════
	
	defer func(conn net.Conn) {
		if err != nil {
			conn.Close()
		}
	}(conn)
	// ... creates http2Client wrapping the net.Conn ...
}
```

---

### Step 8: `shmClientConn` implements `ClientTransportProvider`

📁 [shm_grpc_helpers.go](../../shm_grpc_helpers.go#L145-L148)

```go
// GetClientTransport returns the underlying shared memory client transport.
// This is used internally by gRPC to access the transport after dialing.
func (c *shmClientConn) GetClientTransport() transport.ClientTransport {
	return c.transport   // <-- Returns ShmClientTransport directly
}
```

---

## Summary: Modified Core gRPC Files

| File | Modification |
|------|--------------|
| [http2_client.go](http2_client.go#L204-L240) | Added `ClientTransportProvider` interface check |
| [shm_grpc_helpers.go](../../shm_grpc_helpers.go) | New file: `WithShmTransport()` dial option |
| [resolver.go](resolver.go) | New file: `shm://` resolver |
| [shm_dialer.go](shm_dialer.go) | New file: `DialShm()` and handshake logic |
| [dialoptions.go](../../dialoptions.go#L484) | Unchanged - used existing `WithContextDialer` |

---

## Key Integration Points

| Component | Unix Socket | Shared Memory |
|-----------|-------------|---------------|
| **Resolver** | `internal/resolver/unix/` | `internal/transport/resolver.go` |
| **Dialer hook** | Default `net.Dialer` | `WithContextDialer` returns `shmClientConn` |
| **Divergence** | Returns `net.Conn` | Returns `ClientTransportProvider` |
| **Transport type** | `*http2Client` | `*ShmClientTransport` |
| **Write queue** | `controlBuf` + `loopyWriter` | Direct to ring buffer |
| **Framing** | `http2.Framer` (9-byte header) | Custom (16-byte header) |
| **Sync primitive** | Kernel socket buffer | Futex on ring indices |
| **Data copies** | 4 per direction | 1 per direction |

---

## Key Insight

**Only one modification to core gRPC code was required** - the `ClientTransportProvider` interface check in `http2_client.go`. Everything else is additive new files that plug into gRPC's existing extension points:

1. `resolver.Register()` - for custom URL schemes
2. `WithContextDialer()` - for custom connection establishment
3. `ClientTransport` interface - for custom transport implementations
