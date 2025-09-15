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
// The core component is a lock-free ring buffer that enables efficient
// bidirectional data transfer between client and server processes. The
// implementation prioritizes correctness and portability while maintaining
// low-overhead data paths.
package shm
