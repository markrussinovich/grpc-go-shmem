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

// Binary client demonstrates keepalive configuration with shared memory transport.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pb "google.golang.org/grpc/examples/features/proto/echo"
	"google.golang.org/grpc/keepalive"
)

var shmName = flag.String("shm_name", "keepalive_shm", "Shared memory segment name")

var kacp = keepalive.ClientParameters{
	Time:                10 * time.Second,
	Timeout:             time.Second,
	PermitWithoutStream: true,
}

func main() {
	flag.Parse()

	fmt.Printf("Connecting to shm://%s\n", *shmName)
	fmt.Printf("Keepalive settings:\n")
	fmt.Printf("  Time: %v\n", kacp.Time)
	fmt.Printf("  Timeout: %v\n", kacp.Timeout)
	fmt.Printf("  PermitWithoutStream: %v\n", kacp.PermitWithoutStream)

	conn, err := grpc.NewClient(
		"shm://"+*shmName,
		grpc.WithShmTransport(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(kacp),
	)
	if err != nil {
		log.Fatalf("did not connect: %v", err)
	}
	defer conn.Close()

	c := pb.NewEchoClient(conn)

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		r, err := c.UnaryEcho(ctx, &pb.EchoRequest{Message: fmt.Sprintf("message %d", i)})
		cancel()
		if err != nil {
			log.Printf("UnaryEcho failed: %v", err)
		} else {
			fmt.Printf("Response: %s\n", r.Message)
		}

		fmt.Printf("Waiting 3 seconds...\n")
		time.Sleep(3 * time.Second)
	}

	fmt.Println("Keeping connection open for 20 seconds to observe keepalive...")
	time.Sleep(20 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	r, err := c.UnaryEcho(ctx, &pb.EchoRequest{Message: "final message"})
	cancel()
	if err != nil {
		log.Printf("Final UnaryEcho failed: %v", err)
	} else {
		fmt.Printf("Final Response: %s\n", r.Message)
	}

	fmt.Println("Done")
}
