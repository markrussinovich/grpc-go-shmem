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

// Binary client demonstrates how to send and receive metadata over shared memory transport.
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
	"google.golang.org/grpc/metadata"
)

var shmName = flag.String("shm_name", "metadata_shm", "Shared memory segment name")

const (
	timestampFormat = time.StampNano
	streamingCount  = 10
)

func unaryCallWithMetadata(c pb.EchoClient, message string) {
	fmt.Printf("--- unary ---\n")
	md := metadata.Pairs("timestamp", time.Now().Format(timestampFormat))
	ctx := metadata.NewOutgoingContext(context.Background(), md)

	var header, trailer metadata.MD
	r, err := c.UnaryEcho(ctx, &pb.EchoRequest{Message: message}, grpc.Header(&header), grpc.Trailer(&trailer))
	if err != nil {
		log.Fatalf("failed to call UnaryEcho: %v", err)
	}

	if t, ok := header["timestamp"]; ok {
		fmt.Printf("timestamp from header:\n")
		for i, e := range t {
			fmt.Printf(" %d. %s\n", i, e)
		}
	} else {
		log.Fatal("timestamp expected but doesn't exist in header")
	}
	if l, ok := header["location"]; ok {
		fmt.Printf("location from header:\n")
		for i, e := range l {
			fmt.Printf(" %d. %s\n", i, e)
		}
	} else {
		log.Fatal("location expected but doesn't exist in header")
	}
	fmt.Printf("response:\n")
	fmt.Printf(" - %s\n", r.Message)

	if t, ok := trailer["timestamp"]; ok {
		fmt.Printf("timestamp from trailer:\n")
		for i, e := range t {
			fmt.Printf(" %d. %s\n", i, e)
		}
	} else {
		log.Fatal("timestamp expected but doesn't exist in trailer")
	}
}

func serverStreamingWithMetadata(c pb.EchoClient, message string) {
	fmt.Printf("--- server streaming ---\n")
	md := metadata.Pairs("timestamp", time.Now().Format(timestampFormat))
	ctx := metadata.NewOutgoingContext(context.Background(), md)

	stream, err := c.ServerStreamingEcho(ctx, &pb.EchoRequest{Message: message})
	if err != nil {
		log.Fatalf("failed to call ServerStreamingEcho: %v", err)
	}

	header, err := stream.Header()
	if err != nil {
		log.Fatalf("failed to get header from stream: %v", err)
	}
	if t, ok := header["timestamp"]; ok {
		fmt.Printf("timestamp from header:\n")
		for i, e := range t {
			fmt.Printf(" %d. %s\n", i, e)
		}
	}
	if l, ok := header["location"]; ok {
		fmt.Printf("location from header:\n")
		for i, e := range l {
			fmt.Printf(" %d. %s\n", i, e)
		}
	}

	var rpcStatus error
	fmt.Printf("response:\n")
	for {
		r, err := stream.Recv()
		if err != nil {
			rpcStatus = err
			break
		}
		fmt.Printf(" - %s\n", r.Message)
	}
	if rpcStatus != io.EOF {
		log.Fatalf("failed to finish server streaming: %v", rpcStatus)
	}

	trailer := stream.Trailer()
	if t, ok := trailer["timestamp"]; ok {
		fmt.Printf("timestamp from trailer:\n")
		for i, e := range t {
			fmt.Printf(" %d. %s\n", i, e)
		}
	}
}

func clientStreamingWithMetadata(c pb.EchoClient, message string) {
	fmt.Printf("--- client streaming ---\n")
	md := metadata.Pairs("timestamp", time.Now().Format(timestampFormat))
	ctx := metadata.NewOutgoingContext(context.Background(), md)

	stream, err := c.ClientStreamingEcho(ctx)
	if err != nil {
		log.Fatalf("failed to call ClientStreamingEcho: %v", err)
	}

	header, err := stream.Header()
	if err != nil {
		log.Fatalf("failed to get header from stream: %v", err)
	}
	if t, ok := header["timestamp"]; ok {
		fmt.Printf("timestamp from header:\n")
		for i, e := range t {
			fmt.Printf(" %d. %s\n", i, e)
		}
	}
	if l, ok := header["location"]; ok {
		fmt.Printf("location from header:\n")
		for i, e := range l {
			fmt.Printf(" %d. %s\n", i, e)
		}
	}

	for i := 0; i < streamingCount; i++ {
		if err := stream.Send(&pb.EchoRequest{Message: message}); err != nil {
			log.Fatalf("failed to send streaming: %v", err)
		}
	}

	r, err := stream.CloseAndRecv()
	if err != nil {
		log.Fatalf("failed to CloseAndRecv: %v", err)
	}
	fmt.Printf("response:\n")
	fmt.Printf(" - %s\n", r.Message)

	trailer := stream.Trailer()
	if t, ok := trailer["timestamp"]; ok {
		fmt.Printf("timestamp from trailer:\n")
		for i, e := range t {
			fmt.Printf(" %d. %s\n", i, e)
		}
	}
}

func bidirectionalWithMetadata(c pb.EchoClient, message string) {
	fmt.Printf("--- bidirectional ---\n")
	md := metadata.Pairs("timestamp", time.Now().Format(timestampFormat))
	ctx := metadata.NewOutgoingContext(context.Background(), md)

	stream, err := c.BidirectionalStreamingEcho(ctx)
	if err != nil {
		log.Fatalf("failed to call BidirectionalStreamingEcho: %v", err)
	}

	header, err := stream.Header()
	if err != nil {
		log.Fatalf("failed to get header from stream: %v", err)
	}
	if t, ok := header["timestamp"]; ok {
		fmt.Printf("timestamp from header:\n")
		for i, e := range t {
			fmt.Printf(" %d. %s\n", i, e)
		}
	}
	if l, ok := header["location"]; ok {
		fmt.Printf("location from header:\n")
		for i, e := range l {
			fmt.Printf(" %d. %s\n", i, e)
		}
	}

	go func() {
		for i := 0; i < streamingCount; i++ {
			if err := stream.Send(&pb.EchoRequest{Message: message}); err != nil {
				log.Fatalf("failed to send streaming: %v", err)
			}
		}
		stream.CloseSend()
	}()

	var rpcStatus error
	fmt.Printf("response:\n")
	for {
		r, err := stream.Recv()
		if err != nil {
			rpcStatus = err
			break
		}
		fmt.Printf(" - %s\n", r.Message)
	}
	if rpcStatus != io.EOF {
		log.Fatalf("failed to finish bidirectional streaming: %v", rpcStatus)
	}

	trailer := stream.Trailer()
	if t, ok := trailer["timestamp"]; ok {
		fmt.Printf("timestamp from trailer:\n")
		for i, e := range t {
			fmt.Printf(" %d. %s\n", i, e)
		}
	}
}

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

	unaryCallWithMetadata(c, "this is examples/metadata_shm")
	time.Sleep(100 * time.Millisecond)

	serverStreamingWithMetadata(c, "this is examples/metadata_shm")
	time.Sleep(100 * time.Millisecond)

	clientStreamingWithMetadata(c, "this is examples/metadata_shm")
	time.Sleep(100 * time.Millisecond)

	bidirectionalWithMetadata(c, "this is examples/metadata_shm")
}
