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

// Binary server demonstrates how to install and support compressors for
// incoming RPCs over shared memory transport.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"

	"google.golang.org/grpc"
	// Installing the gzip encoding registers it as an available compressor.
	// gRPC will automatically negotiate and use gzip if the client supports it.
	_ "google.golang.org/grpc/encoding/gzip"

	pb "google.golang.org/grpc/examples/features/proto/echo"
	"google.golang.org/grpc/internal/transport"
)

var addr = flag.String("addr", "shm://compression_shm", "the address to serve on")

type server struct {
	pb.UnimplementedEchoServer
}

func (s *server) UnaryEcho(_ context.Context, in *pb.EchoRequest) (*pb.EchoResponse, error) {
	fmt.Printf("UnaryEcho called with message %q\n", in.GetMessage())
	return &pb.EchoResponse{Message: in.Message}, nil
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
	s.Serve(lis)
}
