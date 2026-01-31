//go:build linux || windows

/*
 *
 * Copyright 2023 gRPC authors.
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

// Binary server demonstrates how gRPC flow control blocks sending when the
// receiver is not ready over shared memory transport.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"google.golang.org/grpc"

	pb "google.golang.org/grpc/examples/features/proto/echo"
	"google.golang.org/grpc/internal/transport"
)

var addr = flag.String("addr", "shm://flow_control_shm", "the address to serve on")

var payload = string(make([]byte, 8*1024)) // 8KB

// server is used to implement EchoServer.
type server struct {
	pb.UnimplementedEchoServer
}

func (s *server) BidirectionalStreamingEcho(stream pb.Echo_BidirectionalStreamingEchoServer) error {
	log.Printf("New stream began.")

	// Phase 1: Delay reading to demonstrate client-side backpressure.
	// The client will fill the ring buffer and block on Send().
	log.Printf("Server: Delaying read for 1 second to demonstrate client-side backpressure...")
	time.Sleep(1 * time.Second)

	// Read all messages from the client
	recvCount := 0
	for {
		if _, err := stream.Recv(); err != nil {
			if err == io.EOF {
				log.Printf("Server: Read %d messages from client.", recvCount)
				break
			}
			log.Printf("Error receiving data: %v", err)
			return err
		}
		recvCount++
	}

	// Phase 2: Send messages back to the client.
	// The client will delay reading, causing the server's ring buffer to fill.
	const numMessagesToSend = 50000 // Need many messages to fill 64MB ring buffer
	const blockingTimeout = 200 * time.Millisecond

	blocked := false
	sentCount := 0
	sendDone := make(chan struct{})

	go func() {
		defer close(sendDone)
		for i := 0; i < numMessagesToSend; i++ {
			if err := stream.Send(&pb.EchoResponse{Message: payload}); err != nil {
				log.Printf("Error sending data after %d messages: %v", sentCount, err)
				return
			}
			sentCount++
		}
	}()

	// Wait for sending to complete or timeout (indicating blocking)
	select {
	case <-sendDone:
		log.Printf("Server: Sent all %d messages without blocking.", sentCount)
	case <-time.After(blockingTimeout):
		blocked = true
		log.Printf("Server: Sending is blocked after ~%d messages (ring buffer full).", sentCount)
	}

	// Wait for sender to finish
	<-sendDone
	log.Printf("Server: Finished sending %d messages total.", sentCount)

	if blocked {
		log.Printf("✓ Flow control demonstrated: server sending was blocked by backpressure.")
	}

	log.Printf("Stream ended successfully.")
	return nil
}

func main() {
	flag.Parse()

	name := strings.TrimPrefix(strings.TrimPrefix(*addr, "shm://"), "shm:")
	lis, err := transport.NewShmListener(
		&transport.ShmAddr{Name: name},
		transport.DefaultSegmentSize,
		transport.DefaultRingASize,
		transport.DefaultRingBSize,
	)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	fmt.Printf("server listening at shm://%s\n", name)

	grpcServer := grpc.NewServer()
	pb.RegisterEchoServer(grpcServer, &server{})

	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
