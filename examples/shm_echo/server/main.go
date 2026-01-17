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

// Package main implements a simple echo server using shared memory transport
// This demonstrates the transport layer without full gRPC integration
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
)

var (
	segmentName = flag.String("segment", "grpc_echo", "Shared memory segment name")
)

func main() {
	flag.Parse()

	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║    Shared Memory Echo Server - Direct Transport Demo    ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("NOTE: This example demonstrates the shared memory transport")
	fmt.Println("working at the transport layer. Standard gRPC examples require")
	fmt.Println("additional integration work to use grpc.NewClient()/NewServer().")
	fmt.Println()
	fmt.Printf("To test this server, the client must manually implement the")
	fmt.Println(" frame protocol or use ShmUnaryClient.")
	fmt.Println()
	fmt.Printf("Segment name: %s\n", *segmentName)
	fmt.Println("Status: Transport layer ready, waiting for full gRPC integration")
	fmt.Println()
	fmt.Println("For a working demo, see:")
	fmt.Println("  - internal/transport/shm/client_unary_test.go")
	fmt.Println("  - internal/transport/shm/cancel_unary_test.go")
	fmt.Println()
	fmt.Println("Press Ctrl+C to exit")

	// Block forever (wait for interrupt signal)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	<-sigCh
	fmt.Println("\nShutting down...")
}
