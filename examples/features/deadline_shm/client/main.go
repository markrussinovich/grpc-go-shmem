/*
 *
 * Copyright 2018 gRPC authors.
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

// Binary client demonstrates deadline handling with shared memory transport.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	pb "google.golang.org/grpc/examples/features/proto/echo"
	"google.golang.org/grpc/status"
)

var shmName = flag.String("shm_name", "deadline_shm", "Shared memory segment name")

func unaryCall(c pb.EchoClient, timeout time.Duration) {
	fmt.Printf("--- calling UnaryEcho with timeout %v ---\n", timeout)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	r, err := c.UnaryEcho(ctx, &pb.EchoRequest{Message: "Hello, shm!"})
	if err != nil {
		s := status.Convert(err)
		if s.Code() == codes.DeadlineExceeded {
			fmt.Printf("DeadlineExceeded: %v\n", s.Message())
		} else {
			fmt.Printf("Error: %v\n", err)
		}
		return
	}
	fmt.Printf("Response: %s\n", r.Message)
}

func streamingCall(c pb.EchoClient, timeout time.Duration) {
	fmt.Printf("--- calling BidirectionalStreamingEcho with timeout %v ---\n", timeout)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	stream, err := c.BidirectionalStreamingEcho(ctx)
	if err != nil {
		log.Printf("failed to call BidirectionalStreamingEcho: %v", err)
		return
	}

	for i := 0; i < 5; i++ {
		if err := stream.Send(&pb.EchoRequest{Message: fmt.Sprintf("message %d", i)}); err != nil {
			s := status.Convert(err)
			if s.Code() == codes.DeadlineExceeded {
				fmt.Printf("DeadlineExceeded on send: %v\n", s.Message())
			} else {
				fmt.Printf("Send error: %v\n", err)
			}
			return
		}

		_, err := stream.Recv()
		if err != nil {
			s := status.Convert(err)
			if s.Code() == codes.DeadlineExceeded {
				fmt.Printf("DeadlineExceeded on recv: %v\n", s.Message())
			} else {
				fmt.Printf("Recv error: %v\n", err)
			}
			return
		}
		fmt.Printf("Received message %d\n", i)
		time.Sleep(50 * time.Millisecond)
	}
	stream.CloseSend()
	fmt.Printf("Streaming completed successfully\n")
}

func main() {
	flag.Parse()

	conn, err := grpc.NewClient(
		"shm://"+*shmName,
		grpc.WithShmTransport(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	defer conn.Close()

	c := pb.NewEchoClient(conn)

	unaryCall(c, 1*time.Second)
	unaryCall(c, 100*time.Millisecond)
	streamingCall(c, 2*time.Second)
	streamingCall(c, 150*time.Millisecond)
}
