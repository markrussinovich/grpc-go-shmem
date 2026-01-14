# Keepalive (Shared Memory Transport)

This example demonstrates keepalive configuration over shared memory transport.

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

- Server keepalive parameters (MaxConnectionIdle, MaxConnectionAge, etc.)
- Client keepalive parameters (Time, Timeout, PermitWithoutStream)
- Server enforcement policy
- Connection health monitoring over shared memory

Note: Keepalive with shared memory works the same as TCP, maintaining
connection health through periodic pings.
