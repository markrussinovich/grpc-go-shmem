/*
 *
 * Copyright 2019 gRPC authors.
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

// Binary server demonstrates keepalive configuration with shared memory transport.
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
	"google.golang.org/grpc/keepalive"
)

var shmName = flag.String("shm_name", "keepalive_shm", "Shared memory segment name")

var kaep = keepalive.EnforcementPolicy{
	MinTime:             5 * time.Second,
	PermitWithoutStream: true,
}

var kasp = keepalive.ServerParameters{
	MaxConnectionIdle:     15 * time.Second,
	MaxConnectionAge:      30 * time.Second,
	MaxConnectionAgeGrace: 5 * time.Second,
	Time:                  5 * time.Second,
	Timeout:               1 * time.Second,
}

type server struct {
	pb.UnimplementedEchoServer
}

func (s *server) UnaryEcho(_ context.Context, in *pb.EchoRequest) (*pb.EchoResponse, error) {
	fmt.Printf("Received: %v\n", in.Message)
	return &pb.EchoResponse{Message: in.Message}, nil
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
	fmt.Printf("Keepalive settings:\n")
	fmt.Printf("  MaxConnectionIdle: %v\n", kasp.MaxConnectionIdle)
	fmt.Printf("  MaxConnectionAge: %v\n", kasp.MaxConnectionAge)
	fmt.Printf("  Time: %v\n", kasp.Time)
	fmt.Printf("  Timeout: %v\n", kasp.Timeout)

	s := grpc.NewServer(
		grpc.KeepaliveEnforcementPolicy(kaep),
		grpc.KeepaliveParams(kasp),
	)
	pb.RegisterEchoServer(s, &server{})

	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
