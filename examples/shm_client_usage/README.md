# Shared Memory Client Usage Example

This example demonstrates how to use `grpc.NewClient()` with the shared memory transport.
It pairs with `shm_server_usage` to form a complete working client/server example.

## Key Code

```go
// Connect using shared memory transport
conn, err := grpc.NewClient(
    "shm://usage_demo",                  // Target format: shm://<segment_name>
    grpc.WithShmTransport(),             // Enable shared memory transport
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)
```

## Running the Example

**Terminal 1 - Start the server first:**
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

```
╔══════════════════════════════════════════════════════════╗
║        Shared Memory Client Usage Example                ║
╚══════════════════════════════════════════════════════════╝
Connecting to shm://usage_demo

✓ Connected to server via shared memory
Call 1: Hello World #1
Call 2: Hello World #2
Call 3: Hello World #3

✓ All RPC calls completed successfully!

Key takeaways:
  1. Use grpc.WithShmTransport() to enable shared memory
  2. Target format is shm://<segment_name>
  3. Everything else works exactly like TCP gRPC
```

## Configuration

- `-shm_name`: Shared memory segment name (default: "usage_demo")
- `-name`: Name to use in greeting (default: "World")

## See Also

- [shm_server_usage](../shm_server_usage/) - The corresponding server example
- [helloworld_shm](../helloworld_shm/) - Full helloworld example over shared memory
