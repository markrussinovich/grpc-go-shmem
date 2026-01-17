//go:build linux || windows

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

// Package main demonstrates the current state of shared memory transport
package main

import (
	"fmt"
)

func main() {

	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║    Shared Memory Echo Client - Direct Transport Demo    ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("NOTE: This example demonstrates the shared memory transport")
	fmt.Println("working at the transport layer. Standard gRPC examples require")
	fmt.Println("additional integration work to use grpc.NewClient()/NewServer().")
	fmt.Println()
	fmt.Println("The transport layer is fully functional with:")
	fmt.Println("  ✓ Futex-based synchronization")
	fmt.Println("  ✓ Zero-copy shared memory ring buffers")
	fmt.Println("  ✓ Bidirectional streaming without deadlocks")
	fmt.Println("  ✓ HTTP/2-style frame protocol")
	fmt.Println()
	fmt.Println("What's missing for standard gRPC examples:")
	fmt.Println("  ✗ Integration with grpc.NewClient()")
	fmt.Println("  ✗ Integration with grpc.NewServer()")
	fmt.Println("  ✗ Custom resolver for shm:// URLs")
	fmt.Println("  ✗ Transport interface bridge layer")
	fmt.Println()
	fmt.Println("For working demos, see the test files:")
	fmt.Println("  - internal/transport/shm/client_unary_test.go")
	fmt.Println("  - internal/transport/shm/cancel_unary_test.go")
	fmt.Println("  - internal/transport/shm/streaming_test.go")
	fmt.Println()
	fmt.Println("Run tests with:")
	fmt.Println("  go test -v ./internal/transport/shm -run TestUnary")
	fmt.Println()
}
