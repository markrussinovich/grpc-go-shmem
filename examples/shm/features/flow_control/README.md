# Flow Control Example (SHM Transport)

This example demonstrates flow control behavior for the shared memory transport.

## How It Works

The SHM transport uses a 64 MiB ring buffer. To demonstrate backpressure:

1. **Client sends many messages rapidly** - 50,000 messages of 8 KB each
2. **Server delays reading** - Waits 1 second before reading to fill the buffer
3. **Client detects blocking** - When `Send()` takes longer than 200ms, it indicates the ring buffer is full
4. **Server reads and responds** - After reading all client messages, server sends back 50,000 messages
5. **Client delays reading** - Waits 2 seconds to fill the buffer in the other direction
6. **Server detects blocking** - Server's `Send()` blocks when client isn't reading

## Expected Output

```
Server: Delaying read for 1 second to demonstrate client-side backpressure...
Sending is blocked after ~24034 messages (ring buffer full).
Finished sending 50000 messages total.
✓ Flow control demonstrated: client sending was blocked by backpressure.
Client: Delaying read for 2 seconds to demonstrate server-side backpressure...
Server: Read 50000 messages from client.
Server: Sending is blocked after ~6433 messages (ring buffer full).
Server: Finished sending 50000 messages total.
✓ Flow control demonstrated: server sending was blocked by backpressure.
```

## Running

```bash
# Start server
go run ./server/*.go

# In another terminal, start client
go run ./client/*.go
```

## Key Observations

- **~24,000 messages** to fill the ring buffer from client side (with 8 KB messages)
- **~6,000 messages** to fill the ring buffer from server side (depends on timing)
- The blocking detection uses a 200ms timeout on `Send()` operations
- Unlike TCP, SHM backpressure is purely in-process (no kernel network buffers involved)
