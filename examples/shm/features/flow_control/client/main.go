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

// Binary client demonstrates how the gRPC flow control blocks sending when the
// receiver is not ready over shared memory transport.
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pb "google.golang.org/grpc/examples/features/proto/echo"
)

var addr = flag.String("addr", "shm://flow_control_shm", "the address to connect to")

var payload = string(make([]byte, 8*1024)) // 8KB

func main() {
	flag.Parse()
	// SHM transport is much faster than TCP, so we need a longer timeout
	// to allow the flow control test to complete.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	defer conn.Close()

	c := pb.NewEchoClient(conn)

	stream, err := c.BidirectionalStreamingEcho(ctx)
	if err != nil {
		log.Fatalf("Error creating stream: %v", err)
	}
	log.Printf("New stream began.")

	// For SHM transport: The ring buffer is finite (~2MB default), so when the
	// server isn't reading, the buffer fills up and Send() blocks.
	//
	// This example demonstrates flow control in two phases:
	// Phase 1: Client sends until blocked (server is delaying reads)
	// Phase 2: Server sends until blocked (client is delaying reads)
	//
	// We use a shorter blocking detection timeout for SHM since the buffer fills faster.
	const blockingTimeout = 200 * time.Millisecond
	const numMessagesToSend = 50000 // Need many messages to fill 64MB ring buffer

	// Phase 1: Send messages until we detect blocking or reach the limit
	blocked := false
	sentCount := 0
	sendDone := make(chan struct{})

	go func() {
		defer close(sendDone)
		for i := 0; i < numMessagesToSend; i++ {
			if err := stream.Send(&pb.EchoRequest{Message: payload}); err != nil {
				log.Printf("Error sending data after %d messages: %v", sentCount, err)
				return
			}
			sentCount++
		}
	}()

	// Wait for sending to complete or timeout (indicating blocking)
	timer := time.NewTimer(blockingTimeout)
	select {
	case <-sendDone:
		timer.Stop()
		log.Printf("Sent all %d messages without blocking.", sentCount)
	case <-timer.C:
		blocked = true
		log.Printf("Sending is blocked after ~%d messages (ring buffer full).", sentCount)
	}

	// Wait for sender to finish (it will complete once server starts reading)
	<-sendDone
	log.Printf("Finished sending %d messages total.", sentCount)
	stream.CloseSend()

	if blocked {
		log.Printf("✓ Flow control demonstrated: client sending was blocked by backpressure.")
	}

	// Phase 2: Delay before reading to let server experience backpressure
	log.Printf("Client: Delaying read for 2 seconds to demonstrate server-side backpressure...")
	time.Sleep(2 * time.Second)

	// Read all the data sent by the server
	recvCount := 0
	for {
		if _, err := stream.Recv(); err != nil {
			if err == io.EOF {
				log.Printf("Read %d messages from server.", recvCount)
				log.Printf("Stream ended successfully.")
				return
			}
			log.Printf("Error receiving data after %d messages: %v", recvCount, err)
			return
		}
		recvCount++
	}
}
