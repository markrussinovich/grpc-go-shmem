# Shared Memory Transport - Integration Status Demo

## Current State

This directory contains placeholder examples that demonstrate the **current integration status** of the shared memory transport.

### ✅ What Works (Transport Layer)

The shared memory transport is **fully functional** at the transport layer:
- Futex-based cross-process synchronization
- Zero-copy ring buffer communication  
- HTTP/2-style frame protocol
- Bidirectional streaming without deadlocks
- All transport-level tests pass

### ❌ What Doesn't Work (gRPC Integration)

Standard gRPC examples (like `helloworld`) **do not work** because:
- Transport is not integrated with `grpc.NewClient()`/`grpc.NewServer()`
- No custom resolver for `shm://` scheme
- No bridge between transport frames and gRPC's Stream interface

## Working Examples

To see the transport in action, run the **existing tests**:

```bash
# Unary RPC tests
go test -v ./internal/transport/shm -run TestUnary

# Cancellation test
go test -v ./internal/transport/shm -run TestCancel

# Bidirectional streaming tests (shows deadlock prevention)
go test -v ./internal/transport/shm -run TestBidirectional
```

## What's Required for Full Integration

To make standard gRPC examples work:

1. **High Priority** (~1-2 weeks):
   - Complete `transport.ClientTransport` interface
   - Complete `transport.ServerTransport` interface
   - Bridge frame-based streams to gRPC Stream interface
   - Integrate with `grpc.NewServer().Serve(listener)`

2. **Medium Priority** (~1 week):
   - Custom resolver for `shm://` URLs
   - Proper metadata/compression handling
   - Error status propagation

## Architecture

**Current (Working):**
```
Test Code → ShmUnaryClient → Ring Buffers → Shared Memory
```

**Required (For Examples):**
```
Application → grpc.NewClient() → Transport Selection → Shm Transport → Ring Buffers
```

## Summary

The **transport implementation is complete and correct**. What remains is the **integration layer** to make it work with standard gRPC APIs. The core functionality (futex synchronization, bidirectional streaming, deadlock prevention) is fully implemented and tested.
