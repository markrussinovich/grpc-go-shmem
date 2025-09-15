/*
 * Copyright 2025 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package shm provides a shared memory transport implementation for gRPC.
//
// This package implements a high-performance shared memory transport that allows
// gRPC communication between processes on the same machine without going through
// the kernel network stack. The transport uses memory-mapped files and efficient
// synchronization primitives to minimize latency and maximize throughput for
// local inter-process communication.
//
// # Core Components
//
// The foundation is a lock-free ring buffer (Ring) that provides:
//
//   - Single-Producer/Single-Consumer (SPSC) semantics for optimal performance
//   - Non-blocking operations that return immediately with progress made
//   - Power-of-two capacity requirement for efficient modulo operations
//   - Zero-copy reservation API for high-performance in-place I/O
//   - Cache-line padding to prevent false sharing between producer and consumer
//
// # SPSC Design
//
// The ring buffer is designed for exactly one writer goroutine and one reader
// goroutine operating concurrently. This constraint enables lock-free operation
// using only atomic operations for synchronization. Each side owns its respective
// index (writer owns write index, reader owns read index) and only reads the
// other side's index, eliminating contention.
//
// # Non-Blocking Semantics
//
// All I/O operations (Read, Write, ReserveWrite, PeekRead) are non-blocking and
// return immediately. If insufficient space or data is available, operations
// return partial progress (which may be zero) rather than blocking. This design
// enables building event-driven systems on top of the ring buffer.
//
// # Power-of-Two Capacity
//
// Ring buffer capacity is always rounded up to the next power of two (minimum 16 bytes).
// This requirement enables efficient modulo operations using bitwise AND with a mask,
// avoiding expensive division operations in the critical path. The capacity constraint
// also aligns with typical CPU cache line sizes for optimal memory access patterns.
//
// # Thread Safety
//
// The ring buffer is safe for concurrent use by one writer and one reader goroutine.
// All synchronization is handled through atomic operations on the read and write
// indices. The implementation has been thoroughly tested with Go's race detector
// and stress-tested with high-throughput concurrent workloads.
//
// The implementation prioritizes correctness and portability while maintaining
// low-overhead data paths suitable for high-frequency gRPC communication.
package shm
