//go:build linux || windows

/*
 *
 * Copyright 2026 gRPC authors.
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

package shm_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/benchmark"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/experimental/shm"
	testpb "google.golang.org/grpc/interop/grpc_testing"
)

// TestFullGRPCWithSHM exercises the complete gRPC client/server path
// over the shared-memory transport against the experimental/shm public
// API. This is the integration smoke test for the public surface.
func TestFullGRPCWithSHM(t *testing.T) {
	name := fmt.Sprintf("test_fullgrpc_%d", time.Now().UnixNano())
	lis, err := shm.NewListener(name, nil)
	if err != nil {
		t.Fatalf("shm.NewListener: %v", err)
	}
	defer lis.Close()

	t.Logf("SHM listener created: %s", lis.Addr().String())

	stopSrv := benchmark.StartServer(benchmark.ServerInfo{Type: "protobuf", Listener: lis})
	defer stopSrv()

	// Give the server a moment to start accepting connections.
	time.Sleep(100 * time.Millisecond)

	target := fmt.Sprintf("shm://%s", name)
	conn, err := grpc.NewClient(target,
		shm.WithTransport(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	client := testpb.NewBenchmarkServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	payload := benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, 0)
	req := &testpb.SimpleRequest{
		ResponseType: testpb.PayloadType_COMPRESSABLE,
		ResponseSize: 0,
		Payload:      payload,
	}
	if _, err := client.UnaryCall(ctx, req); err != nil {
		t.Fatalf("UnaryCall: %v", err)
	}
}
