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

// Package main implements an echo server using shared memory transport.
// It demonstrates all four RPC types: unary, server streaming, client streaming,
// and bidirectional streaming.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"

	"google.golang.org/grpc"
	pb "google.golang.org/grpc/examples/features/proto/echo"
	"google.golang.org/grpc/internal/transport"
)

var (
	shmName     = flag.String("shm_name", "echo_shm", "Shared memory segment name")
	segmentSize = flag.Uint64("seg_size", 4*1024*1024, "Segment size in bytes (default 4MB)")
	ringASize   = flag.Uint64("ring_a", 1024*1024, "Ring A size in bytes (default 1MB)")
	ringBSize   = flag.Uint64("ring_b", 1024*1024, "Ring B size in bytes (default 1MB)")
)

type echoServer struct {
	pb.UnimplementedEchoServer
}

// UnaryEcho implements the unary echo RPC.
func (s *echoServer) UnaryEcho(_ context.Context, in *pb.EchoRequest) (*pb.EchoResponse, error) {
	fmt.Printf("UnaryEcho: received %q\n", in.Message)
	return &pb.EchoResponse{Message: in.Message}, nil
}

// ServerStreamingEcho implements server-side streaming echo.
func (s *echoServer) ServerStreamingEcho(in *pb.EchoRequest, stream pb.Echo_ServerStreamingEchoServer) error {
	fmt.Printf("ServerStreamingEcho: received %q, sending 5 responses\n", in.Message)
	for i := 0; i < 5; i++ {
		msg := fmt.Sprintf("%s (response %d)", in.Message, i+1)
		if err := stream.Send(&pb.EchoResponse{Message: msg}); err != nil {
			return err
		}
	}
	return nil
}

// ClientStreamingEcho implements client-side streaming echo.
func (s *echoServer) ClientStreamingEcho(stream pb.Echo_ClientStreamingEchoServer) error {
	fmt.Println("ClientStreamingEcho: receiving messages...")
	var messages []string
	for {
		in, err := stream.Recv()
		if err == io.EOF {
			// Client finished sending, respond with concatenation of all messages
			response := fmt.Sprintf("received %d messages", len(messages))
			fmt.Printf("ClientStreamingEcho: %s\n", response)
			return stream.SendAndClose(&pb.EchoResponse{Message: response})
		}
		if err != nil {
			return err
		}
		fmt.Printf("ClientStreamingEcho: received %q\n", in.Message)
		messages = append(messages, in.Message)
	}
}

// BidirectionalStreamingEcho implements bidirectional streaming echo.
func (s *echoServer) BidirectionalStreamingEcho(stream pb.Echo_BidirectionalStreamingEchoServer) error {
	fmt.Println("BidirectionalStreamingEcho: started")
	for {
		in, err := stream.Recv()
		if err == io.EOF {
			fmt.Println("BidirectionalStreamingEcho: client closed stream")
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Printf("BidirectionalStreamingEcho: echoing %q\n", in.Message)
		if err := stream.Send(&pb.EchoResponse{Message: in.Message}); err != nil {
			return err
		}
	}
}

func main() {
	flag.Parse()

	// Create shared memory listener
	addr := &transport.ShmAddr{Name: *shmName}
	lis, err := transport.NewShmListener(addr, *segmentSize, *ringASize, *ringBSize)
	if err != nil {
		log.Fatalf("failed to create shm listener: %v", err)
	}
	defer lis.Close()

	// Create and register gRPC server
	s := grpc.NewServer()
	pb.RegisterEchoServer(s, &echoServer{})

	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║       Shared Memory Echo Server - All RPC Types         ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Printf("Listening on shm://%s\n", *shmName)
	fmt.Printf("  Segment size: %d bytes\n", *segmentSize)
	fmt.Printf("  Ring A size:  %d bytes\n", *ringASize)
	fmt.Printf("  Ring B size:  %d bytes\n", *ringBSize)
	fmt.Println()
	fmt.Println("Supported RPCs:")
	fmt.Println("  - UnaryEcho: echo single message")
	fmt.Println("  - ServerStreamingEcho: echo message 5 times")
	fmt.Println("  - ClientStreamingEcho: collect messages, return count")
	fmt.Println("  - BidirectionalStreamingEcho: echo each message immediately")
	fmt.Println()

	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
