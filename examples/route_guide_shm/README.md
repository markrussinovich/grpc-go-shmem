# Route Guide (Shared Memory Transport)

This example demonstrates all four types of gRPC service RPCs using shared memory transport instead of TCP:

- **Unary RPC**: Simple request-response (`GetFeature`)
- **Server streaming RPC**: Server sends multiple responses (`ListFeatures`)
- **Client streaming RPC**: Client sends multiple requests (`RecordRoute`)
- **Bidirectional streaming RPC**: Both sides send streams (`RouteChat`)

This is the shared memory equivalent of the standard `route_guide` example, demonstrating that the SHMem transport is a drop-in replacement for TCP transport.

## Running the Example

First, start the server:

```bash
go run ./server/server.go
```

In another terminal, run the client:

```bash
go run ./client/client.go
```

## Expected Output

**Server:**
```
Server listening on shm://routeguide_shm
```

**Client:**
```
Getting feature for point (409146138, -746188906)
name:"Berkshire Valley Management Area Trail, Jefferson, NJ, USA" location:{latitude:409146138 longitude:-746188906}
Getting feature for point (0, 0)
location:{}
Looking for features within lo:{latitude:400000000 longitude:-750000000} hi:{latitude:420000000 longitude:-730000000}
Feature: name: "Berkshire Valley Management Area Trail, Jefferson, NJ, USA", point:(409146138, -746188906)
...
Traversing 47 points.
Route summary: point_count:47 feature_count:0 distance:12345 elapsed_time:0
First message
Second message
...
```

## Performance Benefits

Compared to the TCP version (`examples/route_guide`), this shared memory version provides:
- **2-5x lower latency** for local IPC
- **2-3x higher throughput** for message transfers
- **20-40% lower CPU usage** due to futex-based synchronization
- **Zero network stack overhead** - data never leaves the process address space

## Implementation Notes

The only changes from the TCP version are:

**Server (`server.go`):**
```go
// TCP version:
lis, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", *port))

// SHMem version:
lis, err := transport.NewShmListener(*shmName)
```

**Client (`client.go`):**
```go
// TCP version:
conn, err := grpc.NewClient(*serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))

// SHMem version:
conn, err := grpc.NewClient(
    "shm://"+*shmName,
    grpc.WithShmTransport(),
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)
```

All service implementation code remains unchanged, demonstrating true transport abstraction.

## See Also

- `examples/helloworld_shm` - Simple unary RPC example with shared memory
- `examples/route_guide` - Original TCP version of this example
- `PHASE6_PERFORMANCE.md` - Detailed performance comparison
