# Deadline (Shared Memory Transport)

This example demonstrates deadline and timeout handling over shared memory transport.

## Running

Start the server:
```bash
go run ./server/main.go
```

In another terminal, run the client:
```bash
go run ./client/main.go
```

## What This Demonstrates

- Setting deadlines on RPC calls
- How deadlines propagate from client to server context
- Handling DeadlineExceeded status
- All works over shared memory transport
