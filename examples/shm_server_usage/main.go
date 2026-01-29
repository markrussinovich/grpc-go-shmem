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

// Package main demonstrates how to use grpc.NewServer with shared memory transport.
// This is a complete working example that pairs with shm_client_usage.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"google.golang.org/grpc"
	pb "google.golang.org/grpc/examples/helloworld/helloworld"
	"google.golang.org/grpc/internal/transport"
)

var (
	shmName     = flag.String("shm_name", "usage_demo", "Shared memory segment name")
	segmentSize = flag.Uint64("seg_size", 2*1024*1024, "Segment size in bytes (default 2MB)")
	ringASize   = flag.Uint64("ring_a", 512*1024, "Ring A size in bytes (default 512KB)")
	ringBSize   = flag.Uint64("ring_b", 512*1024, "Ring B size in bytes (default 512KB)")
)

// server implements the Greeter service
type server struct {
	pb.UnimplementedGreeterServer
}

// SayHello implements helloworld.GreeterServer
func (s *server) SayHello(_ context.Context, in *pb.HelloRequest) (*pb.HelloReply, error) {
	log.Printf("Received: %v", in.GetName())
	return &pb.HelloReply{Message: "Hello " + in.GetName()}, nil
}

func main() {
	flag.Parse()

	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║        Shared Memory Server Usage Example                ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Create a shared memory listener.
	// Key steps:
	//   1. Create an ShmAddr with the segment name
	//   2. Call NewShmListener with size parameters
	//   3. Pass the listener to grpc.Server.Serve()
	addr := &transport.ShmAddr{Name: *shmName}
	lis, err := transport.NewShmListener(addr, *segmentSize, *ringASize, *ringBSize)
	if err != nil {
		log.Fatalf("failed to create shm listener: %v", err)
	}
	defer lis.Close()

	fmt.Printf("✓ Created shared memory listener\n")
	fmt.Printf("  Segment name: %s\n", *shmName)
	fmt.Printf("  Segment size: %d bytes (%.1f MB)\n", *segmentSize, float64(*segmentSize)/(1024*1024))
	fmt.Printf("  Ring A size:  %d bytes (%.1f KB)\n", *ringASize, float64(*ringASize)/1024)
	fmt.Printf("  Ring B size:  %d bytes (%.1f KB)\n", *ringBSize, float64(*ringBSize)/1024)
	fmt.Println()

	// Create gRPC server (exactly like TCP)
	s := grpc.NewServer()
	pb.RegisterGreeterServer(s, &server{})

	fmt.Println("✓ Registered Greeter service")
	fmt.Printf("✓ Listening on shm://%s\n", *shmName)
	fmt.Println()
	fmt.Println("To connect, run the client:")
	fmt.Printf("  go run ../shm_client_usage -shm_name=%s\n", *shmName)
	fmt.Println()
	fmt.Println("Key takeaways:")
	fmt.Println("  1. Create listener with transport.NewShmListener()")
	fmt.Println("  2. Pass listener to grpc.Server.Serve() - same as TCP")
	fmt.Println("  3. Everything else works exactly like TCP gRPC")
	fmt.Println()

	// Serve blocks forever, handling incoming connections
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
