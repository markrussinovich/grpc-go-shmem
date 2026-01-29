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

// Package main implements an echo client using shared memory transport.
// It demonstrates all four RPC types: unary, server streaming, client streaming,
// and bidirectional streaming.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pb "google.golang.org/grpc/examples/features/proto/echo"
)

var (
	shmName = flag.String("shm_name", "echo_shm", "Shared memory segment name")
)

func callUnaryEcho(client pb.EchoClient, message string) {
	fmt.Println("--- UnaryEcho ---")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.UnaryEcho(ctx, &pb.EchoRequest{Message: message})
	if err != nil {
		log.Fatalf("UnaryEcho failed: %v", err)
	}
	fmt.Printf("Response: %q\n\n", resp.Message)
}

func callServerStreamingEcho(client pb.EchoClient, message string) {
	fmt.Println("--- ServerStreamingEcho ---")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.ServerStreamingEcho(ctx, &pb.EchoRequest{Message: message})
	if err != nil {
		log.Fatalf("ServerStreamingEcho failed: %v", err)
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatalf("ServerStreamingEcho recv failed: %v", err)
		}
		fmt.Printf("Response: %q\n", resp.Message)
	}
	fmt.Println()
}

func callClientStreamingEcho(client pb.EchoClient, messages []string) {
	fmt.Println("--- ClientStreamingEcho ---")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.ClientStreamingEcho(ctx)
	if err != nil {
		log.Fatalf("ClientStreamingEcho failed: %v", err)
	}

	for _, msg := range messages {
		fmt.Printf("Sending: %q\n", msg)
		if err := stream.Send(&pb.EchoRequest{Message: msg}); err != nil {
			log.Fatalf("ClientStreamingEcho send failed: %v", err)
		}
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		log.Fatalf("ClientStreamingEcho close failed: %v", err)
	}
	fmt.Printf("Response: %q\n\n", resp.Message)
}

func callBidirectionalStreamingEcho(client pb.EchoClient, messages []string) {
	fmt.Println("--- BidirectionalStreamingEcho ---")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.BidirectionalStreamingEcho(ctx)
	if err != nil {
		log.Fatalf("BidirectionalStreamingEcho failed: %v", err)
	}

	// Send and receive in lockstep
	for _, msg := range messages {
		fmt.Printf("Sending: %q\n", msg)
		if err := stream.Send(&pb.EchoRequest{Message: msg}); err != nil {
			log.Fatalf("BidirectionalStreamingEcho send failed: %v", err)
		}

		resp, err := stream.Recv()
		if err != nil {
			log.Fatalf("BidirectionalStreamingEcho recv failed: %v", err)
		}
		fmt.Printf("Received: %q\n", resp.Message)
	}

	if err := stream.CloseSend(); err != nil {
		log.Fatalf("BidirectionalStreamingEcho close failed: %v", err)
	}
	fmt.Println()
}

func main() {
	flag.Parse()

	fmt.Println("╔══════════════════════════════════════════════════════════╗")
	fmt.Println("║       Shared Memory Echo Client - All RPC Types         ║")
	fmt.Println("╚══════════════════════════════════════════════════════════╝")
	fmt.Printf("Connecting to shm://%s\n\n", *shmName)

	// Connect to the server using shared memory transport
	conn, err := grpc.NewClient(
		"shm://"+*shmName,
		grpc.WithShmTransport(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	client := pb.NewEchoClient(conn)

	// Test all four RPC types
	callUnaryEcho(client, "Hello, shared memory!")

	callServerStreamingEcho(client, "Stream me!")

	callClientStreamingEcho(client, []string{"message 1", "message 2", "message 3"})

	callBidirectionalStreamingEcho(client, []string{"ping 1", "ping 2", "ping 3"})

	fmt.Println("All RPC types completed successfully!")
}
