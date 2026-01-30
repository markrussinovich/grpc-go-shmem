# Shared Memory Transport for gRPC-Go

## Overview

This document describes the design, implementation, and usage of the shared memory transport for gRPC-Go. This transport provides high-performance inter-process communication (IPC) for gRPC services running on the same machine, achieving 2-5x lower latency and 2-3x higher throughput compared to TCP loopback.

**Key Characteristics:**
- **Zero-copy data transfer** via memory-mapped regions
- **Futex-based synchronization** for efficient cross-process blocking
- **HTTP/2-style framing** with custom frame types
- **Full gRPC compatibility** - works with all RPC patterns (unary, streaming, bidirectional)
- **Linux-optimized** with fallback stubs for other platforms

---

## Table of Contents

1. [Architecture Overview](#architecture-overview)
2. [Memory Layout](#memory-layout)
3. [Ring Buffer Design](#ring-buffer-design)
4. [Futex-Based Synchronization](#futex-based-synchronization)
5. [Frame Protocol](#frame-protocol)
6. [Transport Layer](#transport-layer)
7. [gRPC Integration](#grpc-integration)
8. [Connection Lifecycle](#connection-lifecycle)
9. [Flow Control](#flow-control)
10. [Test Coverage](#test-coverage)
11. [Examples](#examples)
12. [Benchmark Results](#benchmark-results)
13. [API Reference](#api-reference)

---

## Architecture Overview

### High-Level Design

```
Ã¢â€Å’Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Â
Ã¢â€â€š                         Shared Memory Segment                       Ã¢â€â€š
Ã¢â€â€š  Ã¢â€Å’Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Â  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€â€š  Segment Header (128 bytes)                                   Ã¢â€â€š  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€â€š  - Magic, Version, Flags, PID tracking, Ready flags          Ã¢â€â€š  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€â€Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Ëœ  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€Å’Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Â  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€â€š  Ring A: Client Ã¢â€ â€™ Server (64 MiB default)                    Ã¢â€â€š  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€â€š  Ã¢â€Å’Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Â  Ã¢â€â€š  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€â€š  Ã¢â€â€š Ring Header (64 bytes)                                 Ã¢â€â€š  Ã¢â€â€š  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€â€š  Ã¢â€â€š - Capacity, Write/Read indices, Futex sequences       Ã¢â€â€š  Ã¢â€â€š  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€â€š  Ã¢â€â€Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Ëœ  Ã¢â€â€š  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€â€š  Ã¢â€Å’Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Â  Ã¢â€â€š  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€â€š  Ã¢â€â€š Data Area (power-of-2 capacity)                        Ã¢â€â€š  Ã¢â€â€š  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€â€š  Ã¢â€â€Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Ëœ  Ã¢â€â€š  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€â€Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Ëœ  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€Å’Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Â  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€â€š  Ring B: Server Ã¢â€ â€™ Client (64 MiB default)                    Ã¢â€â€š  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€â€š  [Same structure as Ring A]                                   Ã¢â€â€š  Ã¢â€â€š
Ã¢â€â€š  Ã¢â€â€Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Ëœ  Ã¢â€â€š
Ã¢â€â€Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Ëœ
         Ã¢â€ â€˜                                        Ã¢â€ â€˜
         Ã¢â€â€š                                        Ã¢â€â€š
    Ã¢â€Å’Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Â´Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Â                              Ã¢â€Å’Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Â´Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Â
    Ã¢â€â€š  Client Ã¢â€â€š                              Ã¢â€â€š  Server Ã¢â€â€š
    Ã¢â€â€š Process Ã¢â€â€š                              Ã¢â€â€š Process Ã¢â€â€š
    Ã¢â€â€Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Ëœ                              Ã¢â€â€Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Ëœ
```

### Components

| Component | File | Description |
|-----------|------|-------------|
| Segment | `shm_segment.go` | Shared memory segment management |
| Ring Buffer | `ring.go` | SPSC circular buffer with futex sync |
| Frame Protocol | `frame.go` | HTTP/2-style frame encoding/decoding |
| Client Transport | `shm_client_transport.go` | gRPC ClientTransport implementation |
| Server Transport | `shm_server_transport.go` | gRPC ServerTransport implementation |
| Listener | `shm_listener.go` | Accepts new shared memory connections |
| Dialer | `shm_dialer.go` | Establishes shared memory connections |
| Futex | `shm_futex_linux.go` | Linux futex syscall wrappers |

---

## Memory Layout

### Segment Header (128 bytes)

```go
type SegmentHeader struct {
    magic       [8]byte  // 0x00: "GRPCSHM\0"
    version     uint32   // 0x08: protocol version (currently 1)
    flags       uint32   // 0x0C: reserved flags
    totalSize   uint64   // 0x10: total segment size
    ringAOff    uint64   // 0x18: offset to ring A header
    ringACap    uint64   // 0x20: ring A capacity (power of 2)
    ringBOff    uint64   // 0x28: offset to ring B header
    ringBCap    uint64   // 0x30: ring B capacity (power of 2)
    serverPID   uint32   // 0x38: server process ID
    clientPID   uint32   // 0x3C: client process ID
    serverReady uint32   // 0x40: server ready flag (0Ã¢â€ â€™1)
    clientReady uint32   // 0x44: client mapped flag (0Ã¢â€ â€™1)
    closed      uint32   // 0x48: closed flag
    pad         uint32   // 0x4C: padding
    maxStreams  uint32   // 0x50: max concurrent streams
    reserved    [44]byte // 0x54-0x7F: reserved to 128B
}
```

### Ring Header (64 bytes)

```go
type RingHeader struct {
    capacity      uint64  // 0x00: power-of-two capacity
    widx          uint64  // 0x08: monotonic write index
    ridx          uint64  // 0x10: monotonic read index
    dataSeq       uint32  // 0x18: data sequence (futex)
    spaceSeq      uint32  // 0x1C: space sequence (futex)
    closed        uint32  // 0x20: closed flag
    pad           uint32  // 0x24: padding
    contigSeq     uint32  // 0x28: contiguity sequence
    spaceWaiters  uint32  // 0x2C: writers waiting for space
    contigWaiters uint32  // 0x30: writers waiting for contiguity
    dataWaiters   uint32  // 0x34: readers waiting for data
    reserved      [8]byte // 0x38-0x3F: reserved to 64B
}
```

### Default Sizes

```go
const (
    DefaultSegmentSize = 136 * 1024 * 1024  // 136 MiB total
    DefaultRingASize   = 64 * 1024 * 1024   // 64 MiB clientÃ¢â€ â€™server
    DefaultRingBSize   = 64 * 1024 * 1024   // 64 MiB serverÃ¢â€ â€™client
    MinRingCapacity    = 4096               // 4 KiB minimum
)
```

---

## Ring Buffer Design

### Single-Producer Single-Consumer (SPSC) Model

The ring buffer implements a lock-free SPSC queue using monotonically increasing indices:

```go
// Write position in physical buffer
writePos := writeIdx & capacityMask  // capacityMask = capacity - 1

// Available space calculation
used := writeIdx - readIdx
available := capacity - used
```

### Key Properties

1. **Power-of-2 capacity** - enables fast modulo via bitwise AND
2. **Monotonic indices** - never wrap, simplifies overflow handling
3. **Atomic access** - all index updates are atomic
4. **Zero-copy** - data is written directly to mmap'd memory

### Write Algorithm (Pseudocode)

```go
func (r *ShmRing) WriteBlocking(data []byte) error {
    for {
        // Check closure
        if r.header.Closed() {
            return ErrRingClosed
        }

        // Calculate available space
        writeIdx := r.header.WriteIndex()
        readIdx := r.header.ReadIndex()
        available := r.capacity - (writeIdx - readIdx)

        if len(data) <= available {
            // Perform write (handle wrap-around)
            writePos := writeIdx & r.capMask
            if writePos + len(data) <= r.capacity {
                // Simple case: contiguous write
                copy(r.dataArea[writePos:], data)
            } else {
                // Wrap case: split write
                firstChunk := r.capacity - writePos
                copy(r.dataArea[writePos:], data[:firstChunk])
                copy(r.dataArea[0:], data[firstChunk:])
            }

            // Publish write atomically
            r.header.SetWriteIndex(writeIdx + len(data))

            // Signal waiting readers
            r.header.IncrementDataSequence()
            if r.header.DataWaiters() > 0 {
                futexWake(&r.header.dataSeq, 1)
            }
            return nil
        }

        // Wait for space using futex
        r.header.IncSpaceWaiters()
        spaceSeq := r.header.SpaceSequence()
        futexWait(&r.header.spaceSeq, spaceSeq)
        r.header.DecSpaceWaiters()
    }
}
```

### Read Algorithm (Pseudocode)

```go
func (r *ShmRing) ReadBlocking(buf []byte) (int, error) {
    for {
        // Calculate available data
        writeIdx := r.header.WriteIndex()
        readIdx := r.header.ReadIndex()
        available := writeIdx - readIdx

        if available > 0 {
            // Read data (handle wrap-around)
            toRead := min(len(buf), available)
            readPos := readIdx & r.capMask

            if readPos + toRead <= r.capacity {
                copy(buf, r.dataArea[readPos:readPos+toRead])
            } else {
                firstChunk := r.capacity - readPos
                copy(buf, r.dataArea[readPos:])
                copy(buf[firstChunk:], r.dataArea[:toRead-firstChunk])
            }

            // Publish read atomically
            r.header.SetReadIndex(readIdx + toRead)

            // Signal waiting writers
            r.header.IncrementSpaceSequence()
            if r.header.SpaceWaiters() > 0 {
                futexWake(&r.header.spaceSeq, 1)
            }
            return toRead, nil
        }

        // Check for closed ring
        if r.header.Closed() {
            return 0, io.EOF
        }

        // Wait for data using futex
        r.header.IncDataWaiters()
        dataSeq := r.header.DataSequence()
        futexWait(&r.header.dataSeq, dataSeq)
        r.header.DecDataWaiters()
    }
}
```

---

## Futex-Based Synchronization

### Overview

Linux futexes (fast userspace mutexes) provide efficient cross-process synchronization:

- **Zero syscalls in fast path**: When data/space is available, no kernel calls needed
- **Efficient blocking**: Threads sleep in kernel until woken by `FUTEX_WAKE`
- **Cross-process safe**: Works across separate process address spaces

### Futex Operations

```go
// Wait until *addr != val (or spurious wake)
func futexWait(addr *uint32, val uint32) error {
    // Atomically check value before syscall (prevents lost-wake race)
    if atomic.LoadUint32(addr) != val {
        return nil  // Value already changed
    }

    syscall.Syscall6(
        syscall.SYS_FUTEX,
        uintptr(unsafe.Pointer(addr)),
        futexOpWait,  // FUTEX_WAIT (shared, for cross-process)
        uintptr(val),
        0,  // timeout (infinite)
        0, 0,
    )
}

// Wake up to n threads waiting on addr
func futexWake(addr *uint32, n int) (int, error) {
    r1, _, errno := syscall.Syscall6(
        syscall.SYS_FUTEX,
        uintptr(unsafe.Pointer(addr)),
        futexOpWake,  // FUTEX_WAKE (shared, for cross-process)
        uintptr(n),
        0, 0, 0,
    )
    return int(r1), errno
}
```

### Sequence Numbers

Three sequence numbers coordinate different conditions:

| Sequence | Incremented By | Waiters | Purpose |
|----------|----------------|---------|---------|
| `dataSeq` | Writer | Readers | Signals "data available" |
| `spaceSeq` | Reader | Writers | Signals "fullÃ¢â€ â€™not-full" transition |
| `contigSeq` | Reader | Writers | Signals "any read completed" |

### Adaptive Spin-Wait

Before falling back to futex (which has ~7-10Ã‚Âµs wake overhead), the transport performs a brief spin-wait:

```go
const (
    spinIterationsDefault = 300  // ~2Ã‚Âµs at 7ns/PAUSE
    spinIterationsMin     = 50
    spinIterationsMax     = 2000
)

// Spin-wait with PAUSE instruction
for spin := 0; spin < spinLimit; spin++ {
    if dataAvailable() {
        return  // Fast path - no syscall needed
    }
    runtime_procyield(1)  // PAUSE instruction
}
// Fall back to futex
futexWait(...)
```

---

## Frame Protocol

### Frame Header (16 bytes)

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                         Length (32)                           |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                        Stream ID (32)                         |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|    Type (8)   |   Flags (8)   |        Reserved (16)          |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                        Reserved2 (32)                         |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

### Frame Types

| Type | Value | Description |
|------|-------|-------------|
| `PAD` | 0x00 | Padding (unused) |
| `HEADERS` | 0x01 | Initial headers (method, authority, metadata) |
| `MESSAGE` | 0x02 | gRPC message payload |
| `TRAILERS` | 0x03 | Final status and trailing metadata |
| `CANCEL` | 0x04 | Stream cancellation |
| `GOAWAY` | 0x05 | Connection shutdown |
| `PING` | 0x06 | Keepalive ping |
| `PONG` | 0x07 | Keepalive response |
| `HALFCLOSE` | 0x08 | Client finished sending |
| `WindowUpdate` | 0x09 | Flow control window update |

### Control Frame Types (Connection Setup)

| Type | Value | Description |
|------|-------|-------------|
| `CONNECT` | 0x10 | Client connect request |
| `ACCEPT` | 0x11 | Server accepts connection |
| `REJECT` | 0x12 | Server rejects connection |

### Headers Payload (Version 1)

```go
type HeadersV1 struct {
    Version          uint8   // must be 1
    HdrType          uint8   // 0=client-initial, 1=server-initial
    Method           string  // e.g., "/package.Service/Method"
    Authority        string  // target authority
    DeadlineUnixNano uint64  // RPC deadline (0 if none)
    Metadata         []KV    // key-value pairs
}
```

### Trailers Payload (Version 1)

```go
type TrailersV1 struct {
    Version        uint8   // must be 1
    GRPCStatusCode uint32  // codes.Code value
    GRPCStatusMsg  string  // status message
    Metadata       []KV    // trailing metadata
}
```

### Writing a Frame

```go
func writeFrame(ctx context.Context, tx *ShmRing, fh FrameHeader, payload []byte) error {
    fh.Length = uint32(len(payload))

    // Atomically reserve space for header + payload
    total := frameHeaderSize + len(payload)
    res, err := tx.ReserveWrite(ctx, total)
    if err != nil {
        return err
    }

    // Encode header
    var hdr [16]byte
    encodeFrameHeaderTo(&hdr, fh)

    // Write header and payload
    res.Write(hdr[:])
    res.Write(payload)
    res.Commit()

    return nil
}
```

---

## Transport Layer

### Client Transport

`ShmClientTransport` implements gRPC's `ClientTransport` interface:

```go
type ShmClientTransport struct {
    segment        *Segment
    clientToServer *ShmRing    // Ring A
    serverToClient *ShmRing    // Ring B

    streams        map[uint32]*ClientStream
    streamID       uint32      // Next stream ID (odd for client)

    // Flow control
    connSendQuota  int64
    streamSendQuota map[uint32]int64

    // Lifecycle
    ctx            context.Context
    cancel         context.CancelFunc
    closed         atomic.Bool
    draining       atomic.Bool
}
```

**Key Methods:**

```go
// Create a new RPC stream
func (t *ShmClientTransport) NewStream(ctx context.Context, callHdr *CallHdr) (*ClientStream, error)

// Write data to a stream
func (t *ShmClientTransport) Write(s *ClientStream, hdr []byte, data mem.BufferSlice, opts *WriteOptions) error

// Close the transport
func (t *ShmClientTransport) Close(err error)

// Graceful shutdown
func (t *ShmClientTransport) GracefulClose()
```

### Server Transport

`ShmServerTransport` implements gRPC's `ServerTransport` interface:

```go
type ShmServerTransport struct {
    segment        *Segment
    serverToClient *ShmRing    // Ring B
    clientToServer *ShmRing    // Ring A

    streams        map[uint32]*ServerStream
    handleFunc     func(*ServerStream)

    // Flow control
    connSendQuota  int64
    streamSendQuota map[uint32]int64

    // Lifecycle
    ctx            context.Context
    cancel         context.CancelFunc
    closed         atomic.Bool
    draining       atomic.Bool
}
```

**Key Methods:**

```go
// Handle incoming streams (blocking)
func (t *ShmServerTransport) HandleStreams(ctx context.Context, handle func(*ServerStream))

// Write response data
func (t *ShmServerTransport) Write(s *ServerStream, hdr []byte, data mem.BufferSlice, opts *WriteOptions) error

// Write trailers (end of RPC)
func (t *ShmServerTransport) WriteStatus(s *ServerStream, st *status.Status) error
```

### Background Reader

Both transports run a background goroutine that reads frames and dispatches them:

```go
func (t *ShmClientTransport) processIncomingData(ctx context.Context) {
    for {
        if t.closed.Load() {
            return
        }

        // Block on next frame (futex-based)
        fh, payload, err := readFrameView(ctx, t.serverToClient)
        if err != nil {
            return
        }

        // Update keepalive timestamp
        atomic.StoreInt64(&t.lastRead, time.Now().UnixNano())

        // Dispatch by frame type
        switch fh.Type {
        case FrameTypeHEADERS:
            t.handleHeaders(fh.StreamID, payload)
        case FrameTypeMESSAGE:
            t.handleMessage(fh.StreamID, payload)
        case FrameTypeTRAILERS:
            t.handleTrailers(fh.StreamID, payload)
        case FrameTypePING:
            writeFrame(ctx, t.clientToServer, FrameHeader{Type: FrameTypePONG}, payload)
        case FrameTypeGOAWAY:
            t.handleGoAway(fh.Flags, payload)
        // ...
        }
    }
}
```

---

## gRPC Integration

### Client-Side Integration

Use `grpc.WithShmTransport()` to enable shared memory:

```go
import (
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
)

conn, err := grpc.NewClient(
    "shm://my_service",  // shm:// scheme triggers SHM dialer
    grpc.WithShmTransport(),
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)
```

### Server-Side Integration

Create a `ShmListener` and pass it to gRPC:

```go
import (
    "google.golang.org/grpc"
    "google.golang.org/grpc/internal/transport"
)

// Create shared memory listener
lis, err := transport.NewShmListener(
    &transport.ShmAddr{Name: "my_service"},
    2*1024*1024,   // 2MB segment
    512*1024,      // 512KB ring A
    512*1024,      // 512KB ring B
)

// Standard gRPC server
server := grpc.NewServer()
pb.RegisterMyServiceServer(server, &myServer{})
server.Serve(lis)
```

### Resolver Registration

The `shm://` URL scheme is handled by a custom resolver:

```go
// Registered in internal/transport/resolver.go
func init() {
    resolver.Register(&shmResolverBuilder{})
}

type shmResolverBuilder struct{}

func (b *shmResolverBuilder) Scheme() string { return "shm" }

func (b *shmResolverBuilder) Build(target resolver.Target, ...) (resolver.Resolver, error) {
    // Parse "shm://segment_name" Ã¢â€ â€™ address "shm:segment_name"
    addr := resolver.Address{Addr: "shm:" + target.URL.Host}
    cc.UpdateState(resolver.State{Addresses: []resolver.Address{addr}})
    return &shmResolver{}, nil
}
```

---

## Connection Lifecycle

### Connection Establishment

```
Ã¢â€Å’Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Â                              Ã¢â€Å’Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Â
Ã¢â€â€š   Client   Ã¢â€â€š                              Ã¢â€â€š   Server   Ã¢â€â€š
Ã¢â€â€Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Â¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Ëœ                              Ã¢â€â€Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Â¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€Ëœ
      Ã¢â€â€š                                           Ã¢â€â€š
      Ã¢â€â€š  1. Open control segment                  Ã¢â€â€š
      Ã¢â€â€š  Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â€š
      Ã¢â€â€š                                           Ã¢â€â€š
      Ã¢â€â€š  2. Send CONNECT frame                    Ã¢â€â€š
      Ã¢â€â€š  Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€“Âº  Ã¢â€â€š
      Ã¢â€â€š                                           Ã¢â€â€š
      Ã¢â€â€š  3. Server creates data segment           Ã¢â€â€š
      Ã¢â€â€š                                           Ã¢â€â€š
      Ã¢â€â€š  4. Send ACCEPT frame (segment name)      Ã¢â€â€š
      Ã¢â€â€š  Ã¢â€”â€žÃ¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬  Ã¢â€â€š
      Ã¢â€â€š                                           Ã¢â€â€š
      Ã¢â€â€š  5. Open data segment                     Ã¢â€â€š
      Ã¢â€â€š  Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â€š
      Ã¢â€â€š                                           Ã¢â€â€š
      Ã¢â€â€š  6. Set clientReady flag                  Ã¢â€â€š
      Ã¢â€â€š  Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â€š
      Ã¢â€â€š                                           Ã¢â€â€š
      Ã¢â€â€š  7. Begin RPC communication               Ã¢â€â€š
      Ã¢â€â€š  Ã¢â€”â€žÃ¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€“Âº  Ã¢â€â€š
```

### Connection Shutdown

**Graceful Close:**
1. Send `GOAWAY` frame with `DRAINING` flag
2. Wait for active streams to complete
3. Close rings and unmap segment

**Immediate Close:**
1. Send `GOAWAY` frame with `IMMEDIATE` flag
2. Cancel all streams
3. Close rings and unmap segment

---

## Flow Control

### HTTP/2-Style Window Management

The shared memory transport implements HTTP/2-style flow control:

```go
const maxWindowSize = (1 << 31) - 1  // 2GB - 1

// Connection-level and stream-level windows
type ShmClientTransport struct {
    connSendQuota    int64
    streamSendQuota  map[uint32]int64
    connInFlow       trInFlow
    streamInFlow     map[uint32]*inFlow
}
```

### Acquiring Send Quota

Before sending data, the transport acquires quota from both connection and stream windows:

```go
func (t *ShmClientTransport) acquireSendQuota(ctx context.Context, streamID uint32, n int) error {
    for {
        t.sendQuotaMu.Lock()

        connOK := t.connSendQuota >= int64(n)
        streamOK := t.streamSendQuota[streamID] >= int64(n)

        if connOK && streamOK {
            t.connSendQuota -= int64(n)
            t.streamSendQuota[streamID] -= int64(n)
            t.sendQuotaMu.Unlock()
            return nil
        }

        ch := t.quotaSignal
        t.sendQuotaMu.Unlock()

        // Wait for quota update
        select {
        case <-ch:
            continue
        case <-ctx.Done():
            return ctx.Err()
        }
    }
}
```

### Window Updates

Receivers send `WindowUpdate` frames to replenish sender quotas:

```go
func (t *ShmServerTransport) sendWindowUpdate(streamID uint32, delta uint32) {
    buf := make([]byte, 4)
    binary.LittleEndian.PutUint32(buf, delta)
    writeFrame(context.Background(), t.serverToClient,
        FrameHeader{Type: FrameTypeWindowUpdate, StreamID: streamID}, buf)
}
```

---

## Test Coverage

### Unit Tests

| Test File | Coverage |
|-----------|----------|
| `shm_test.go` | Segment/ring header layout, atomic operations, utilities |
| `ring_test.go` | Ring buffer read/write, wrap-around, capacity |
| `ringbuf_test.go` | In-memory ring buffer (no shared memory) |
| `frame_test.go` | Frame encoding/decoding |

### Integration Tests

| Test | Description |
|------|-------------|
| `TestCreateAndOpenSegment` | Segment creation/opening between processes |
| `TestFutexBasic` | Futex wait/wake synchronization |
| `TestFutexWithSharedMemory` | Cross-process futex via mmap |
| `TestCrossProcessEcho` | Full echo server in separate process |
| `TestCrossProcessBackpressure` | Backpressure under load |

### Transport Tests

| Test | Description |
|------|-------------|
| `TestClientTransport_NewStream_Integration` | Stream creation and write |
| `TestFullRPC_Integration` | Complete unary RPC flow |
| `TestShmDeadlinePropagation` | Deadline across transport |
| `TestShmMetadataPropagation` | Metadata in headers/trailers |
| `TestShmConcurrentStreams` | Multiple simultaneous streams |
| `TestShmGracefulClose` | Graceful shutdown with GOAWAY |
| `TestShmMaxStreams` | Max concurrent stream enforcement |

### Advanced Tests

| Test | Description |
|------|-------------|
| `TestShmPingPongSizes` | Various payload sizes |
| `TestShmStreamError` | Error handling and propagation |
| `TestShmFlowControlBlocksUntilWindowUpdate` | Flow control behavior |
| `TestShmKeepaliveClientConfiguration` | Keepalive parameter handling |
| `TestChunkedWriteSmallRing` | Large messages with chunking |

### Running Tests

```bash
# Run all shared memory tests
go test -v ./internal/transport -run "Shm|Ring|Segment|Futex"

# Run with race detector
go test -race ./internal/transport -run "Shm"

# Run cross-process tests
go test -v ./internal/transport -run "CrossProcess"

# Run benchmarks
go test -bench=. -benchmem ./internal/transport
```

---

## Examples

### Hello World (Unary RPC)

**Server:**
```go
package main

import (
    "google.golang.org/grpc"
    "google.golang.org/grpc/internal/transport"
    pb "your/proto/package"
)

func main() {
    // Create shared memory listener
    lis, _ := transport.NewShmListener(
        &transport.ShmAddr{Name: "helloworld_shm"},
        2*1024*1024, 512*1024, 512*1024,
    )

    server := grpc.NewServer()
    pb.RegisterGreeterServer(server, &greeterServer{})
    server.Serve(lis)
}
```

**Client:**
```go
package main

import (
    "context"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
    pb "your/proto/package"
)

func main() {
    conn, _ := grpc.NewClient(
        "shm://helloworld_shm",
        grpc.WithShmTransport(),
        grpc.WithTransportCredentials(insecure.NewCredentials()),
    )
    defer conn.Close()

    client := pb.NewGreeterClient(conn)
    resp, _ := client.SayHello(context.Background(), &pb.HelloRequest{Name: "World"})
    fmt.Println("Response:", resp.Message)
}
```

### Route Guide (All RPC Types)

The `examples/route_guide_shm/` directory demonstrates all four RPC patterns:

- **Unary**: `GetFeature` - single request, single response
- **Server Streaming**: `ListFeatures` - single request, stream of responses
- **Client Streaming**: `RecordRoute` - stream of requests, single response
- **Bidirectional**: `RouteChat` - bidirectional streams

---

## Benchmark Results

### Ring Buffer Performance

```
BenchmarkShmRingWriteRead/size=64      21M ops    57 ns/op    1,104 MB/s
BenchmarkShmRingWriteRead/size=1024    11M ops   103 ns/op    9,862 MB/s
BenchmarkShmRingWriteRead/size=64KB   256K ops  4.2 Ã‚Âµs/op   15,520 MB/s
BenchmarkShmRingWriteRead/size=1MB     25K ops   47 Ã‚Âµs/op   22,142 MB/s
```

### Roundtrip Latency

```
BenchmarkShmRingRoundtrip/size=64      6M ops    200 ns/op     640 MB/s
BenchmarkShmRingRoundtrip/size=1KB     4M ops    305 ns/op   6,737 MB/s
BenchmarkShmRingRoundtrip/size=4KB   2.4M ops    504 ns/op  16,244 MB/s
```

### Large Payload Performance

```
BenchmarkShmRingLargePayloads/1MB      24K ops    48 Ã‚Âµs/op   21,653 MB/s
BenchmarkShmRingLargePayloads/4MB       5K ops   204 Ã‚Âµs/op   20,512 MB/s
BenchmarkShmRingLargePayloads/16MB    1.5K ops   742 Ã‚Âµs/op   22,606 MB/s
BenchmarkShmRingLargePayloads/64MB     332 ops  3.3 ms/op   20,416 MB/s
BenchmarkShmRingLargePayloads/128MB    176 ops  6.9 ms/op   19,466 MB/s
BenchmarkShmRingLargePayloads/256MB     73 ops  16 ms/op    16,670 MB/s
```

### Comparison with TCP/Unix Sockets

| Transport | Roundtrip (1KB) | One-Way (1KB) | Throughput |
|-----------|-----------------|---------------|------------|
| **Shared Memory** | ~0.7 Ã‚Âµs | ~147 ns | 7+ GB/s |
| Unix Sockets | ~9.6 Ã‚Âµs | ~2.6 Ã‚Âµs | 390 MB/s |
| TCP Loopback | ~18 Ã‚Âµs | ~6.4 Ã‚Âµs | 160 MB/s |

**Speedup Summary:**
- **SHM vs TCP**: 10-30x lower latency, 15-40x higher throughput
- **SHM vs Unix**: 5-13x lower latency, 10-20x higher throughput

---

## API Reference

### Creating a Segment

```go
// Create a new shared memory segment
seg, err := transport.CreateSegment(name string, ringASize, ringBSize uint64) (*Segment, error)

// Open an existing segment
seg, err := transport.OpenSegment(name string) (*Segment, error)

// Close and cleanup
seg.Close()
transport.RemoveSegment(name)
```

### Creating a Listener

```go
lis, err := transport.NewShmListener(
    addr *ShmAddr,       // e.g., &ShmAddr{Name: "myservice"}
    segmentSize uint64,  // Total segment size
    ringASize uint64,    // ClientÃ¢â€ â€™Server ring size
    ringBSize uint64,    // ServerÃ¢â€ â€™Client ring size
) (*ShmListener, error)

// Accept connections
conn, err := lis.Accept()

// Set max concurrent streams
lis.SetMaxStreams(100)
```

### Dialing

```go
// Using the dialer directly
transport, err := transport.DialShm(ctx, segmentName, opts *DialOptions) (ClientTransport, error)

// Using gRPC client
conn, err := grpc.NewClient("shm://segment_name", grpc.WithShmTransport(), ...)
```

### Dial Options

```go
type DialOptions struct {
    SegmentSize    uint64        // Total segment size
    RingASize      uint64        // ClientÃ¢â€ â€™Server ring
    RingBSize      uint64        // ServerÃ¢â€ â€™Client ring
    ConnectTimeout time.Duration // Connection timeout
    KeepaliveParams keepalive.ClientParameters
}
```

---

## Limitations

1. **Linux-only**: Futex syscalls are Linux-specific (stubs exist for other platforms)
2. **Local IPC only**: Both processes must run on the same machine
3. **Single connection per segment**: Each segment supports one client-server pair
4. **Fixed buffer sizes**: Ring buffers are pre-allocated, not dynamic

---

## Debugging

### Environment Variables

```bash
# Enable shared memory debug logging
export GRPC_SHM_DEBUG=1

# Enable futex debug logging
export GRPC_SHM_FUTEX_DEBUG=1
```

### Inspecting Segments

```bash
# List shared memory files
ls -la /dev/shm/

# View segment contents (hex)
xxd /dev/shm/your_segment_name | head -100
```

### Common Issues

| Issue | Cause | Solution |
|-------|-------|----------|
| "segment already exists" | Stale segment from crash | `rm /dev/shm/segment_name` |
| "data larger than ring capacity" | Message exceeds ring size | Increase ring size or chunk data |
| "timeout waiting for server" | Server not ready | Check server is running and ready |
| Deadlock in bidirectional streaming | Both rings full | Use concurrent read/write goroutines |

---

## Future Work

- Multi-client support (multiple connections per segment)
- Automatic segment sizing based on workload
- Windows/macOS support via platform-specific primitives
- Integration with gRPC stats/tracing hooks
- Automatic `shm://` scheme detection without explicit option

---

*Last updated: January 2026*
*gRPC-Go Shared Memory Transport v1.0*
