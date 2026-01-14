# Interceptor (Shared Memory Transport)

This example demonstrates unary and stream interceptors over shared memory transport.

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

- Unary interceptors (client and server)
- Stream interceptors (client and server)
- Request/response logging
- Timing measurement
- All works over shared memory transport

Note: This is a simplified version without TLS/authentication since shared memory
transport is inherently local and doesn't require encryption.
