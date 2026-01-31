//go:build linux || windows

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

// Binary server demonstrates how to handle canceled contexts when a client
// cancels an in-flight RPC.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"strings"

	"google.golang.org/grpc"

	pb "google.golang.org/grpc/examples/features/proto/echo"
	"google.golang.org/grpc/internal/transport"
)

var addr = flag.String("addr", "shm://cancel_shm", "the address to serve on")

type server struct {
	pb.UnimplementedEchoServer
}

func (s *server) BidirectionalStreamingEcho(stream pb.Echo_BidirectionalStreamingEchoServer) error {
	for {
		in, err := stream.Recv()
		if err != nil {
			fmt.Printf("server: error receiving from stream: %v\n", err)
			if err == io.EOF {
				return nil
			}
			return err
		}
		fmt.Printf("echoing message %q\n", in.Message)
		stream.Send(&pb.EchoResponse{Message: in.Message})
	}
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
	s := grpc.NewServer()
	pb.RegisterEchoServer(s, &server{})
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
