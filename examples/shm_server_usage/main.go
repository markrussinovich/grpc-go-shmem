/*
 *
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
 *
 */

// Package main demonstrates using the shared memory transport with grpc.NewServer()
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/internal/transport"
)

// Example demonstrates server-side shared memory transport usage
func main() {
	fmt.Println("gRPC Shared Memory Transport - Server Example")
	fmt.Println("==============================================")
	fmt.Println()

	// Configuration
	segmentName := "my_segment"
	segmentSize := uint64(2 * 1024 * 1024) // 2MB
	ringASize := uint64(512 * 1024)        // 512KB
	ringBSize := uint64(512 * 1024)        // 512KB

	fmt.Printf("Configuration:\n")
	fmt.Printf("  Segment Name: %s\n", segmentName)
	fmt.Printf("  Segment Size: %d bytes (%.1f MB)\n", segmentSize, float64(segmentSize)/(1024*1024))
	fmt.Printf("  Ring A Size:  %d bytes (%.1f KB)\n", ringASize, float64(ringASize)/1024)
	fmt.Printf("  Ring B Size:  %d bytes (%.1f KB)\n", ringBSize, float64(ringBSize)/1024)
	fmt.Println()

	// Create shared memory listener
	fmt.Println("Creating shared memory listener...")
	addr := &transport.ShmAddr{Name: segmentName}
	listener, err := transport.NewShmListener(addr, segmentSize, ringASize, ringBSize)
	if err != nil {
		log.Fatalf("Failed to create listener: %v", err)
	}
	defer listener.Close()

	fmt.Printf("✓ Listener created and ready\n")
	fmt.Printf("  Address: %s\n", listener.Addr())
	fmt.Println()

	// Create gRPC server
	fmt.Println("Creating gRPC server...")
	_ = grpc.NewServer()

	// In a real application, you would register your service here:
	// pb.RegisterGreeterServer(s, &greeterServer{})

	fmt.Println("✓ gRPC server created")
	fmt.Println()

	fmt.Println("Server is now ready to accept connections")
	fmt.Println("Waiting for client to connect...")
	fmt.Println()
	fmt.Println("To connect, run a client with:")
	fmt.Printf("  grpc.NewClient(\"shm://%s\", grpc.WithShmTransport(), ...)\n", segmentName)
	fmt.Println()

	// In a real application, this would block serving:
	// if err := s.Serve(listener); err != nil {
	//     log.Fatalf("Failed to serve: %v", err)
	// }

	// For this example, we'll demonstrate the Accept flow
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fmt.Println("Calling Accept() (will timeout after 10 seconds if no client connects)...")

	// This will block until a client connects or context times out
	done := make(chan struct{})
	var conn any
	var acceptErr error

	go func() {
		conn, acceptErr = listener.Accept()
		close(done)
	}()

	select {
	case <-done:
		if acceptErr != nil {
			fmt.Printf("✗ Accept failed: %v\n", acceptErr)
			fmt.Println()
			fmt.Println("This is expected if no client connected.")
		} else {
			fmt.Printf("✓ Client connected!\n")
			fmt.Printf("  Connection: %v\n", conn)
			fmt.Println()
			fmt.Println("ServerTransport is now ready to handle RPCs")
		}
	case <-ctx.Done():
		fmt.Println("✗ Timeout waiting for client connection")
		fmt.Println()
		fmt.Println("This is expected - no client connected within 10 seconds.")
	}

	fmt.Println()
	fmt.Println("Server Example Complete")
	fmt.Println()
	fmt.Println("In a production server:")
	fmt.Println("1. Create listener with NewShmListener()")
	fmt.Println("2. Create gRPC server with grpc.NewServer()")
	fmt.Println("3. Register your service implementations")
	fmt.Println("4. Call server.Serve(listener) - blocks handling RPCs")
	fmt.Println("5. Accept() will be called internally for each client")
}
