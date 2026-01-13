# Metadata (Shared Memory Transport)

This example demonstrates sending and receiving metadata over shared memory transport.

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

- Sending metadata from client to server via request headers
- Sending metadata from server to client via response headers and trailers
- Works identically to TCP version but over shared memory
