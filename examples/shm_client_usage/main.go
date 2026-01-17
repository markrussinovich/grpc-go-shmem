//go:build linux || windows

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

// Package main demonstrates how to use grpc.NewClient with shared memory transport
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/internal/transport"
)

func main() {
	// Example 1: Basic usage with default options
	fmt.Println("Example 1: Creating client with default shared memory transport options")

	_, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// This creates a client that will use shared memory transport when connecting to shm:// addresses
	conn, err := grpc.NewClient(
		"shm://my_service_segment",
		grpc.WithShmTransport(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Printf("Note: Connection will fail without a running server: %v", err)
	} else {
		defer conn.Close()
		fmt.Println("Client created successfully with shared memory transport")
	}

	// Example 2: Custom options
	fmt.Println("\nExample 2: Creating client with custom shared memory transport options")

	customOpts := &transport.DialOptions{
		SegmentSize:    2 * 1024 * 1024, // 2MB total
		RingASize:      512 * 1024,      // 512KB client->server
		RingBSize:      512 * 1024,      // 512KB server->client
		ConnectTimeout: 10 * time.Second,
	}

	conn2, err := grpc.NewClient(
		"shm://large_segment",
		grpc.WithShmTransportAndOptions(customOpts),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Printf("Note: Connection will fail without a running server: %v", err)
	} else {
		defer conn2.Close()
		fmt.Println("Client created successfully with custom options")
	}

	fmt.Println("\nTo use this client for actual RPC calls:")
	fmt.Println("1. Start a server using grpc.NewServer().Serve(shmListener)")
	fmt.Println("2. Generate protobuf stubs for your service")
	fmt.Println("3. Call methods like: client.MyMethod(ctx, request)")
	fmt.Println("\nSee examples/helloworld for a complete working example")
}
