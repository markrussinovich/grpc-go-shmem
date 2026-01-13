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

// Binary server demonstrates deadline handling with shared memory transport.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc"
	pb "google.golang.org/grpc/examples/features/proto/echo"
	"google.golang.org/grpc/internal/transport"
)

var shmName = flag.String("shm_name", "deadline_shm", "Shared memory segment name")

type server struct {
	pb.UnimplementedEchoServer
}

func (s *server) UnaryEcho(ctx context.Context, in *pb.EchoRequest) (*pb.EchoResponse, error) {
	fmt.Printf("Received: %v\n", in.Message)

	deadline, ok := ctx.Deadline()
	if ok {
		fmt.Printf("Deadline received: %v (in %v)\n", deadline, time.Until(deadline))
	} else {
		fmt.Printf("No deadline received\n")
	}

	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()

	select {
	case <-timer.C:
		fmt.Printf("Finished processing\n")
	case <-ctx.Done():
		fmt.Printf("Context cancelled: %v\n", ctx.Err())
		return nil, ctx.Err()
	}

	return &pb.EchoResponse{Message: in.Message}, nil
}

func (s *server) BidirectionalStreamingEcho(stream pb.Echo_BidirectionalStreamingEchoServer) error {
	for {
		in, err := stream.Recv()
		if err != nil {
			return err
		}
		fmt.Printf("Received: %v\n", in.Message)

		ctx := stream.Context()
		deadline, ok := ctx.Deadline()
		if ok {
			fmt.Printf("Deadline: %v (in %v)\n", deadline, time.Until(deadline))
		}

		if err := stream.Send(&pb.EchoResponse{Message: in.Message}); err != nil {
			return err
		}
	}
}

func main() {
	flag.Parse()

	addr := &transport.ShmAddr{Name: *shmName}
	lis, err := transport.NewShmListener(addr, 4*1024*1024, 1024*1024, 1024*1024)
	if err != nil {
		log.Fatalf("failed to create shm listener: %v", err)
	}
	defer lis.Close()

	fmt.Printf("Server listening on shm://%s\n", *shmName)
	s := grpc.NewServer()
	pb.RegisterEchoServer(s, &server{})
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
