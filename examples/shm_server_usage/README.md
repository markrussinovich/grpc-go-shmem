# Shared Memory Server Usage Example

This example demonstrates how to use `grpc.NewServer()` with the shared memory transport.
It pairs with `shm_client_usage` to form a complete working client/server example.

## Key Code

```go
// Create a shared memory listener
addr := &transport.ShmAddr{Name: "usage_demo"}
lis, err := transport.NewShmListener(addr, segmentSize, ringASize, ringBSize)
if err != nil {
    log.Fatalf("failed to create shm listener: %v", err)
}
defer lis.Close()

// Create gRPC server (exactly like TCP)
s := grpc.NewServer()
pb.RegisterGreeterServer(s, &server{})

// Serve using the shared memory listener
if err := s.Serve(lis); err != nil {
    log.Fatalf("failed to serve: %v", err)
}
```

## Running the Example

**Terminal 1 - Start the server:**
```bash
cd examples/shm_server_usage
go run .
```

**Terminal 2 - Run the client:**
```bash
cd examples/shm_client_usage
go run .
```

## Expected Output

**Server:**
```
╔══════════════════════════════════════════════════════════╗
║        Shared Memory Server Usage Example                ║
╚══════════════════════════════════════════════════════════╝

✓ Created shared memory listener
  Segment name: usage_demo
  Segment size: 2097152 bytes (2.0 MB)
  Ring A size:  524288 bytes (512.0 KB)
  Ring B size:  524288 bytes (512.0 KB)

✓ Registered Greeter service
✓ Listening on shm://usage_demo

To connect, run the client:
  go run ../shm_client_usage -shm_name=usage_demo

Key takeaways:
  1. Create listener with transport.NewShmListener()
  2. Pass listener to grpc.Server.Serve() - same as TCP
  3. Everything else works exactly like TCP gRPC

2025/01/28 12:00:00 Received: World #1
2025/01/28 12:00:00 Received: World #2
2025/01/28 12:00:00 Received: World #3
```

## Configuration

- `-shm_name`: Shared memory segment name (default: "usage_demo")
- `-seg_size`: Total segment size in bytes (default: 2MB)
- `-ring_a`: Ring A buffer size in bytes (default: 512KB)
- `-ring_b`: Ring B buffer size in bytes (default: 512KB)

## See Also

- [shm_client_usage](../shm_client_usage/) - The corresponding client example
- [helloworld_shm](../helloworld_shm/) - Full helloworld example over shared memory
