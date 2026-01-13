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

// Binary client demonstrates context cancellation with shared memory transport.
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

var shmName = flag.String("shm_name", "cancel_shm", "Shared memory segment name")

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Println("--- Starting BidirectionalStreamingEcho ---")
	stream, err := c.BidirectionalStreamingEcho(ctx)
	if err != nil {
		log.Fatalf("failed to call BidirectionalStreamingEcho: %v", err)
	}

	for i := 0; i < 3; i++ {
		msg := fmt.Sprintf("message %d", i)
		fmt.Printf("Client: sending %q\n", msg)
		if err := stream.Send(&pb.EchoRequest{Message: msg}); err != nil {
			log.Fatalf("failed to send: %v", err)
		}

		r, err := stream.Recv()
		if err != nil {
			log.Fatalf("failed to receive: %v", err)
		}
		fmt.Printf("Client: received %q\n", r.Message)
		time.Sleep(100 * time.Millisecond)
	}

	fmt.Println("Client: cancelling context...")
	cancel()

	time.Sleep(100 * time.Millisecond)

	fmt.Println("Client: attempting send after cancellation...")
	err = stream.Send(&pb.EchoRequest{Message: "after cancel"})
	if err != nil {
		s := status.Convert(err)
		if s.Code() == codes.Canceled {
			fmt.Printf("Client: send correctly failed with Canceled: %v\n", s.Message())
		} else {
			fmt.Printf("Client: send failed with %v: %v\n", s.Code(), s.Message())
		}
	}

	fmt.Println("Client: attempting recv after cancellation...")
	_, err = stream.Recv()
	if err != nil {
		s := status.Convert(err)
		if s.Code() == codes.Canceled {
			fmt.Printf("Client: recv correctly failed with Canceled: %v\n", s.Message())
		} else {
			fmt.Printf("Client: recv failed with %v: %v\n", s.Code(), s.Message())
		}
	}

	fmt.Println("--- Done ---")
}
