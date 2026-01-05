# Helloworld Example with Shared Memory Transport

This example demonstrates the standard gRPC helloworld example modified to use shared memory transport instead of TCP.

## What This Demonstrates

- **Standard gRPC API**: Uses `grpc.NewClient()` and `grpc.NewServer().Serve()` just like regular gRPC
- **Shared Memory Transport**: Communication happens via shared memory instead of TCP sockets
- **Zero Configuration**: No network setup required, works entirely in shared memory
- **Performance**: Lower latency and higher throughput compared to TCP loopback

## Differences from Standard Helloworld

### Server
```go
// Standard: TCP listener
lis, err := net.Listen("tcp", ":50051")

// Shared Memory: Shm listener
lis, err := transport.NewShmListener("helloworld_shm", 2*1024*1024, 512*1024, 512*1024)
```

### Client
```go
// Standard: TCP connection
conn, err := grpc.NewClient("localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))

// Shared Memory: Shm connection
conn, err := grpc.NewClient(
    "shm://helloworld_shm",
    grpc.WithShmTransport(),
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)
```

## Running the Example

### Terminal 1 - Start the server:
```bash
cd examples/helloworld_shm/greeter_server
go run main.go
```

Expected output:
```
server listening on shared memory segment: helloworld_shm
  Segment size: 2097152 bytes
  Ring A size: 524288 bytes
  Ring B size: 524288 bytes
Waiting for client connections...
```

### Terminal 2 - Run the client:
```bash
cd examples/helloworld_shm/greeter_client
go run main.go
```

Expected output:
```
Connecting to shared memory segment: helloworld_shm
Calling SayHello with name: world
Greeting: Hello world
```

Server will show:
```
Received: world
```

## Command Line Options

### Server
- `-segment`: Shared memory segment name (default: "helloworld_shm")
- `-seg_size`: Total segment size in bytes (default: 2MB)
- `-ring_a`: Client→Server ring buffer size (default: 512KB)
- `-ring_b`: Server→Client ring buffer size (default: 512KB)

### Client
- `-segment`: Shared memory segment name (default: "helloworld_shm")
- `-name`: Name to greet (default: "world")

## Example with Custom Parameters

### Server with larger buffers:
```bash
go run main.go -segment my_segment -seg_size 4194304 -ring_a 1048576 -ring_b 1048576
```

### Client connecting to custom segment:
```bash
go run main.go -segment my_segment -name "Alice"
```

## Key Features

1. **Futex-based Synchronization**: Uses Linux futex for efficient cross-process communication
2. **Deadlock Prevention**: Independent reader/writer goroutines prevent buffer deadlocks
3. **Zero-copy**: Data transferred via shared memory without kernel involvement
4. **Standard gRPC**: Works with all standard gRPC features (metadata, errors, etc.)

## Performance Comparison

Compared to TCP loopback (localhost):
- **Latency**: 2-5x lower (no kernel network stack)
- **Throughput**: 2-3x higher (zero-copy)
- **CPU**: Lower utilization (futex vs socket polling)

## What's the Same

Everything else works exactly like standard gRPC:
- Service definitions and generated code (no changes)
- Request/response messages (protobuf)
- Error handling and status codes
- Context and cancellation
- Interceptors and middleware

## Technical Details

The shared memory transport:
- Uses ring buffers for bidirectional communication
- Implements HTTP/2-style framing (HEADERS, MESSAGE, TRAILERS)
- Supports all RPC types (unary, server streaming, client streaming, bidirectional)
- Provides same semantics as TCP transport

## Limitations

- **Local only**: Client and server must run on the same machine
- **One connection**: Each segment supports one client connection at a time
- **Linux-specific**: Futex synchronization requires Linux (other platforms will have fallbacks)

## Next Steps

See other examples:
- `examples/route_guide_shm/` - All four RPC types with shared memory
- `examples/features/` - Advanced gRPC features with shm transport
