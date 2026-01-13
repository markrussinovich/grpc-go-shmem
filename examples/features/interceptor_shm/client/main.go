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

// Binary client demonstrates interceptors with shared memory transport.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pb "google.golang.org/grpc/examples/features/proto/echo"
)

var shmName = flag.String("shm_name", "interceptor_shm", "Shared memory segment name")

func unaryInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	start := time.Now()
	fmt.Printf("[Client Interceptor] Unary call: %s\n", method)

	err := invoker(ctx, method, req, reply, cc, opts...)

	duration := time.Since(start)
	fmt.Printf("[Client Interceptor] Unary call completed in %v, err=%v\n", duration, err)

	return err
}

type wrappedStream struct {
	grpc.ClientStream
}

func (w *wrappedStream) RecvMsg(m any) error {
	err := w.ClientStream.RecvMsg(m)
	if err == nil {
		fmt.Printf("[Client Stream Interceptor] RecvMsg: %T\n", m)
	}
	return err
}

func (w *wrappedStream) SendMsg(m any) error {
	fmt.Printf("[Client Stream Interceptor] SendMsg: %T\n", m)
	return w.ClientStream.SendMsg(m)
}

func streamInterceptor(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	fmt.Printf("[Client Interceptor] Stream call: %s (client=%v, server=%v)\n",
		method, desc.ClientStreams, desc.ServerStreams)

	start := time.Now()
	clientStream, err := streamer(ctx, desc, cc, method, opts...)
	if err != nil {
		return nil, err
	}

	fmt.Printf("[Client Interceptor] Stream established in %v\n", time.Since(start))
	return &wrappedStream{clientStream}, nil
}

func callUnaryEcho(c pb.EchoClient, message string) {
	fmt.Printf("--- calling UnaryEcho ---\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r, err := c.UnaryEcho(ctx, &pb.EchoRequest{Message: message})
	if err != nil {
		log.Printf("UnaryEcho failed: %v", err)
		return
	}
	fmt.Printf("Response: %s\n", r.Message)
}

func callBidirectionalEcho(c pb.EchoClient) {
	fmt.Printf("--- calling BidirectionalStreamingEcho ---\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := c.BidirectionalStreamingEcho(ctx)
	if err != nil {
		log.Printf("BidirectionalStreamingEcho failed: %v", err)
		return
	}

	for i := 0; i < 3; i++ {
		msg := fmt.Sprintf("message %d", i)
		if err := stream.Send(&pb.EchoRequest{Message: msg}); err != nil {
			log.Printf("failed to send: %v", err)
			return
		}
	}
	stream.CloseSend()

	for {
		r, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("failed to receive: %v", err)
			return
		}
		fmt.Printf("Response: %s\n", r.Message)
	}
}

func main() {
	flag.Parse()

	conn, err := grpc.NewClient(
		"shm://"+*shmName,
		grpc.WithShmTransport(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(unaryInterceptor),
		grpc.WithStreamInterceptor(streamInterceptor),
	)
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	defer conn.Close()

	c := pb.NewEchoClient(conn)

	callUnaryEcho(c, "hello from shm interceptor example")
	time.Sleep(100 * time.Millisecond)

	callBidirectionalEcho(c)
}
