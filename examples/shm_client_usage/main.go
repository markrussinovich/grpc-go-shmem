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

// Package main demonstrates how to use grpc.NewClient with shared memory transport.
// This is a complete working example that connects to shm_server_usage.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pb "google.golang.org/grpc/examples/helloworld/helloworld"
)

var (
	shmName = flag.String("shm_name", "usage_demo", "Shared memory segment name")
	name    = flag.String("name", "World", "Name to greet")
)

func main() {
	flag.Parse()

	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║        Shared Memory Client Usage Example                ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Printf("Connecting to shm://%s\n\n", *shmName)

	// Create a client connection using shared memory transport.
	// Key options:
	//   - grpc.WithShmTransport(): enables the shared memory transport
	//   - Target format: "shm://<segment_name>"
	conn, err := grpc.NewClient(
		"shm://"+*shmName,
		grpc.WithShmTransport(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	fmt.Println("✓ Connected to server via shared memory")

	// Create the greeter client
	client := pb.NewGreeterClient(conn)

	// Make multiple RPC calls to demonstrate the connection works
	for i := 1; i <= 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		greeting := fmt.Sprintf("%s #%d", *name, i)
		resp, err := client.SayHello(ctx, &pb.HelloRequest{Name: greeting})
		cancel()

		if err != nil {
			log.Fatalf("SayHello failed: %v", err)
		}
		fmt.Printf("Call %d: %s\n", i, resp.GetMessage())
	}

	fmt.Println()
	fmt.Println("✓ All RPC calls completed successfully!")
	fmt.Println()
	fmt.Println("Key takeaways:")
	fmt.Println("  1. Use grpc.WithShmTransport() to enable shared memory")
	fmt.Println("  2. Target format is shm://<segment_name>")
	fmt.Println("  3. Everything else works exactly like TCP gRPC")
}
