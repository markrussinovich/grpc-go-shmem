//go:build linux || windows

/*
 *
 * Copyright 2025 gRPC authors.
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

package grpc_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/benchmark"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/internal/transport"
	testpb "google.golang.org/grpc/interop/grpc_testing"
)

// TestFullGRPCWithSHM tests the complete gRPC path with shared memory transport.
// This mimics what the benchmark does to identify the failure point.
func (s) TestFullGRPCWithSHM(t *testing.T) {
	// Create a shared memory listener
	name := fmt.Sprintf("test_fullgrpc_%d", time.Now().UnixNano())
	lis, err := transport.NewShmListener(&transport.ShmAddr{Name: name}, 64*1024*1024, 64*1024*1024, 64*1024*1024)
	if err != nil {
		t.Fatalf("Failed to create SHM listener: %v", err)
	}
	defer lis.Close()
	defer transport.RemoveSegment(name)

	t.Logf("SHM Listener created: %s", lis.Addr().String())

	// Start the benchmark server (uses grpc.NewServer internally)
	stopSrv := benchmark.StartServer(benchmark.ServerInfo{Type: "protobuf", Listener: lis})
	defer stopSrv()

	t.Log("gRPC server started with SHM listener")

	// Give server a moment to start accepting connections
	time.Sleep(100 * time.Millisecond)

	// Dial using SHM transport
	target := fmt.Sprintf("shm://%s", name)
	dialOpts := []grpc.DialOption{
		grpc.WithShmTransport(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	t.Logf("Dialing target: %s", target)

	conn, err := grpc.NewClient(target, dialOpts...)
	if err != nil {
		t.Fatalf("grpc.NewClient failed: %v", err)
	}
	defer conn.Close()

	t.Log("gRPC client connected")

	client := testpb.NewBenchmarkServiceClient(conn)

	// Make a simple unary call
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	payload := benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, 0) // Empty payload like benchmark
	req := &testpb.SimpleRequest{
		ResponseType: payload.Type,
		ResponseSize: int32(0), // Empty response
		Payload:      payload,
	}

	t.Log("Making UnaryCall...")

	// Enable verbose tracing
	t.Log("About to call UnaryCall...")
	resp, err := client.UnaryCall(ctx, req)
	if err != nil {
		t.Logf("UnaryCall failed with error: %v", err)
		t.Logf("Error type: %T", err)
		t.Fatalf("UnaryCall failed: %v", err)
	}

	t.Logf("UnaryCall succeeded! Response payload size: %d", len(resp.Payload.Body))
}
