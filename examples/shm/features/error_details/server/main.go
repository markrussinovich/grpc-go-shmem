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

// Binary server demonstrates how to provide error messages in gRPC responses
// over shared memory transport.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	epb "google.golang.org/genproto/googleapis/rpc/errdetails"
	pb "google.golang.org/grpc/examples/helloworld/helloworld"
	"google.golang.org/grpc/internal/transport"
)

var addr = flag.String("addr", "shm://error_details_shm", "the address to serve on")

// server is used to implement helloworld.GreeterServer.
type server struct {
	pb.UnimplementedGreeterServer
	mu    sync.Mutex
	count map[string]int
}

// SayHello implements helloworld.GreeterServer
func (s *server) SayHello(_ context.Context, in *pb.HelloRequest) (*pb.HelloReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Track the number of times the user has been greeted.
	s.count[in.Name]++
	if s.count[in.Name] > 1 {
		st := status.New(codes.ResourceExhausted, "Request limit exceeded.")
		ds, err := st.WithDetails(
			&epb.QuotaFailure{
				Violations: []*epb.QuotaFailure_Violation{{
					Subject:     fmt.Sprintf("name:%s", in.Name),
					Description: "Limit one greeting per person",
				}},
			},
		)
		if err != nil {
			return nil, st.Err()
		}
		return nil, ds.Err()
	}
	return &pb.HelloReply{Message: "Hello " + in.Name}, nil
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
	pb.RegisterGreeterServer(s, &server{count: make(map[string]int)})
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
