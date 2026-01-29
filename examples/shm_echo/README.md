# Shared Memory Echo Example

This example demonstrates all four gRPC RPC types using the shared memory transport:

1. **UnaryEcho** - Single request/response
2. **ServerStreamingEcho** - Client sends one request, server streams multiple responses
3. **ClientStreamingEcho** - Client streams multiple requests, server sends one response
4. **BidirectionalStreamingEcho** - Both client and server stream messages

## Running the Example

Open two terminals and run:

**Terminal 1 - Start the server:**
```bash
cd examples/shm_echo/server
go run .
```

**Terminal 2 - Run the client:**
```bash
cd examples/shm_echo/client
go run .
```

## Expected Output

**Server:**
```
╔══════════════════════════════════════════════════════════╗
║       Shared Memory Echo Server - All RPC Types         ║
╚══════════════════════════════════════════════════════════╝
Listening on shm://echo_shm
...
UnaryEcho: received "Hello, shared memory!"
ServerStreamingEcho: received "Stream me!", sending 5 responses
ClientStreamingEcho: receiving messages...
ClientStreamingEcho: received "message 1"
ClientStreamingEcho: received "message 2"
ClientStreamingEcho: received "message 3"
ClientStreamingEcho: received 3 messages
BidirectionalStreamingEcho: started
BidirectionalStreamingEcho: echoing "ping 1"
BidirectionalStreamingEcho: echoing "ping 2"
BidirectionalStreamingEcho: echoing "ping 3"
BidirectionalStreamingEcho: client closed stream
```

**Client:**
```
╔══════════════════════════════════════════════════════════╗
║       Shared Memory Echo Client - All RPC Types         ║
╚══════════════════════════════════════════════════════════╝
Connecting to shm://echo_shm

--- UnaryEcho ---
Response: "Hello, shared memory!"

--- ServerStreamingEcho ---
Response: "Stream me! (response 1)"
Response: "Stream me! (response 2)"
Response: "Stream me! (response 3)"
Response: "Stream me! (response 4)"
Response: "Stream me! (response 5)"

--- ClientStreamingEcho ---
Sending: "message 1"
Sending: "message 2"
Sending: "message 3"
Response: "received 3 messages"

--- BidirectionalStreamingEcho ---
Sending: "ping 1"
Received: "ping 1"
Sending: "ping 2"
Received: "ping 2"
Sending: "ping 3"
Received: "ping 3"

All RPC types completed successfully!
```

## Configuration

Both server and client accept command-line flags:

**Server flags:**
- `-shm_name`: Shared memory segment name (default: "echo_shm")
- `-seg_size`: Total segment size in bytes (default: 4MB)
- `-ring_a`: Ring A buffer size (default: 1MB)
- `-ring_b`: Ring B buffer size (default: 1MB)

**Client flags:**
- `-shm_name`: Shared memory segment name (default: "echo_shm")

## Proto Definition

This example uses the Echo service from `examples/features/proto/echo/echo.proto`:

```protobuf
service Echo {
  rpc UnaryEcho(EchoRequest) returns (EchoResponse) {}
  rpc ServerStreamingEcho(EchoRequest) returns (stream EchoResponse) {}
  rpc ClientStreamingEcho(stream EchoRequest) returns (EchoResponse) {}
  rpc BidirectionalStreamingEcho(stream EchoRequest) returns (stream EchoResponse) {}
}
```

