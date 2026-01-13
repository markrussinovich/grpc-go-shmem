# Cancellation (Shared Memory Transport)

This example demonstrates context cancellation over shared memory transport.

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

- Client-initiated cancellation using context
- Server detecting context cancellation
- Proper cleanup on cancellation
- All works over shared memory transport
