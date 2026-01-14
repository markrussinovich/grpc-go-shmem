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

// Binary server demonstrates interceptors with shared memory transport.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"time"

	"google.golang.org/grpc"
	pb "google.golang.org/grpc/examples/features/proto/echo"
	"google.golang.org/grpc/internal/transport"
)

var shmName = flag.String("shm_name", "interceptor_shm", "Shared memory segment name")

type server struct {
	pb.UnimplementedEchoServer
}

func (s *server) UnaryEcho(_ context.Context, in *pb.EchoRequest) (*pb.EchoResponse, error) {
	fmt.Printf("UnaryEcho: received %q\n", in.Message)
	return &pb.EchoResponse{Message: in.Message}, nil
}

func (s *server) BidirectionalStreamingEcho(stream pb.Echo_BidirectionalStreamingEchoServer) error {
	fmt.Println("BidirectionalStreamingEcho: started")
	for {
		in, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Printf("BidirectionalStreamingEcho: received %q\n", in.Message)
		if err := stream.Send(&pb.EchoResponse{Message: in.Message}); err != nil {
			return err
		}
	}
}

func unaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	fmt.Printf("[Server Interceptor] Unary call: %s\n", info.FullMethod)

	resp, err := handler(ctx, req)

	duration := time.Since(start)
	fmt.Printf("[Server Interceptor] Unary call completed in %v, err=%v\n", duration, err)

	return resp, err
}

type wrappedStream struct {
	grpc.ServerStream
}

func (w *wrappedStream) RecvMsg(m any) error {
	err := w.ServerStream.RecvMsg(m)
	if err == nil {
		fmt.Printf("[Server Stream Interceptor] RecvMsg: %T\n", m)
	}
	return err
}

func (w *wrappedStream) SendMsg(m any) error {
	fmt.Printf("[Server Stream Interceptor] SendMsg: %T\n", m)
	return w.ServerStream.SendMsg(m)
}

func streamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	fmt.Printf("[Server Interceptor] Stream call: %s (client=%v, server=%v)\n",
		info.FullMethod, info.IsClientStream, info.IsServerStream)

	start := time.Now()
	err := handler(srv, &wrappedStream{ss})
	duration := time.Since(start)

	fmt.Printf("[Server Interceptor] Stream call completed in %v, err=%v\n", duration, err)
	return err
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

	s := grpc.NewServer(
		grpc.UnaryInterceptor(unaryInterceptor),
		grpc.StreamInterceptor(streamInterceptor),
	)
	pb.RegisterEchoServer(s, &server{})

	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
