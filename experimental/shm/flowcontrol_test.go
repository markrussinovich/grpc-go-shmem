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

// TestConfigureFlowControlForBench drives a full gRPC round-trip over
// the shared-memory transport after switching it into the fair-
// comparison profile (HTTP/2 spec defaults: 65535-byte window, 16384-
// byte frame). The response payload (256 KiB) far exceeds both the
// fair window and frame size, so a correct implementation must split it
// across many DATA frames and drive WINDOW_UPDATE round-trips without
// deadlocking. A regression in the exported knobs would surface here as
// a hang (caught by the context deadline) or a transport error.
func TestConfigureFlowControlForBench(t *testing.T) {
	shm.ConfigureFlowControlForBench(65535, 16384)
	defer shm.ResetFlowControlForBench()

	name := fmt.Sprintf("test_fc_bench_%d", time.Now().UnixNano())
	lis, err := shm.NewListener(name, nil)
	if err != nil {
		t.Fatalf("shm.NewListener: %v", err)
	}
	defer lis.Close()

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

	const respSize = 256 * 1024 // spans many 16 KiB frames and exceeds the 64 KiB window
	req := &testpb.SimpleRequest{
		ResponseType: testpb.PayloadType_COMPRESSABLE,
		ResponseSize: respSize,
		Payload:      benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, respSize),
	}
	resp, err := client.UnaryCall(ctx, req)
	if err != nil {
		t.Fatalf("UnaryCall under fair flow-control profile: %v", err)
	}
	if got := len(resp.GetPayload().GetBody()); got != respSize {
		t.Fatalf("response payload size = %d, want %d", got, respSize)
	}
}
