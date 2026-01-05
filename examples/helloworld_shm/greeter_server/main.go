/*
 *
 * Copyright 2015 gRPC authors.
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

// Package main implements a server for Greeter service using shared memory transport.
package main

import (
	"context"
	"flag"
	"log"

	"google.golang.org/grpc"
	pb "google.golang.org/grpc/examples/helloworld/helloworld"
	"google.golang.org/grpc/internal/transport"
)

var (
	segmentName = flag.String("segment", "helloworld_shm", "Shared memory segment name")
	segmentSize = flag.Uint64("seg_size", 2*1024*1024, "Segment size in bytes (default 2MB)")
	ringASize   = flag.Uint64("ring_a", 512*1024, "Ring A size in bytes (default 512KB)")
	ringBSize   = flag.Uint64("ring_b", 512*1024, "Ring B size in bytes (default 512KB)")
)

// server is used to implement helloworld.GreeterServer.
type server struct {
	pb.UnimplementedGreeterServer
}

// SayHello implements helloworld.GreeterServer
func (s *server) SayHello(_ context.Context, in *pb.HelloRequest) (*pb.HelloReply, error) {
	log.Printf("Received: %v", in.GetName())
	return &pb.HelloReply{Message: "Hello " + in.GetName()}, nil
}

func main() {
	flag.Parse()
	
	// Create shared memory listener
	addr := &transport.ShmAddr{Name: *segmentName}
	lis, err := transport.NewShmListener(addr, *segmentSize, *ringASize, *ringBSize)
	if err != nil {
		log.Fatalf("failed to create shm listener: %v", err)
	}
	defer lis.Close()
	
	s := grpc.NewServer()
	pb.RegisterGreeterServer(s, &server{})
	log.Printf("server listening on shared memory segment: %s", *segmentName)
	log.Printf("  Segment size: %d bytes", *segmentSize)
	log.Printf("  Ring A size: %d bytes", *ringASize)
	log.Printf("  Ring B size: %d bytes", *ringBSize)
	log.Println("Waiting for client connections...")
	
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
