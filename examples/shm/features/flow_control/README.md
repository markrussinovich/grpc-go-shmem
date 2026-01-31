# Flow Control Example (SHM Transport)

This example demonstrates flow control behavior for the shared memory transport.

## Note

The SHM transport is significantly faster than TCP, which means:
1. Messages are processed much faster than they can be sent
2. The ring buffer rarely fills up completely
3. Backpressure situations are less common

This example may behave differently than the TCP version because:
- The server can read messages faster than the client can send them
- The ring buffer provides sufficient capacity for bursts
- The 1-second "blocking detection" timeout may not trigger

To see flow control in action with SHM, you may need to:
- Use much larger messages
- Artificially slow down the receiver
- Use a smaller ring buffer

## Running

```bash
# Start server
go run ./server/*.go

# In another terminal, start client
go run ./client/*.go
```

The client will send messages until backpressure is detected (no message sent in 1 second),
then the server will read all messages. Due to SHM's speed, this may not show
"Sending is blocked" as quickly as TCP would.
