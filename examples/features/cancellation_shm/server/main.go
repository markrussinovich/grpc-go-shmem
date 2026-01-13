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

// Binary server demonstrates context cancellation with shared memory transport.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"

	"google.golang.org/grpc"
	pb "google.golang.org/grpc/examples/features/proto/echo"
	"google.golang.org/grpc/internal/transport"
)

var shmName = flag.String("shm_name", "cancel_shm", "Shared memory segment name")

type server struct {
	pb.UnimplementedEchoServer
}

func (s *server) BidirectionalStreamingEcho(stream pb.Echo_BidirectionalStreamingEchoServer) error {
	fmt.Println("--- BidirectionalStreamingEcho ---")
	ctx := stream.Context()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("Server: context cancelled, stopping stream")
			return ctx.Err()
		default:
		}

		in, err := stream.Recv()
		if err == io.EOF {
			fmt.Println("Server: client closed the stream")
			return nil
		}
		if err != nil {
			fmt.Printf("Server: error receiving: %v\n", err)
			return err
		}

		fmt.Printf("Server: received: %v\n", in.Message)
		if err := stream.Send(&pb.EchoResponse{Message: in.Message}); err != nil {
			fmt.Printf("Server: error sending: %v\n", err)
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
