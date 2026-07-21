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
	testpb "google.golang.org/grpc/interop/grpc_testing"
	shm "google.golang.org/grpc/plugin/shm"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
)

// TestRegistryPathUnary exercises the full client/server path over the SHM
// transport selected entirely through the exported pluggable-transport
// registries — the client via resolver.Address.TransportType and the server via
// the tagged listener — rather than the experimental WithContextDialer seam.
func TestRegistryPathUnary(t *testing.T) {
	name := fmt.Sprintf("test_registry_%d", time.Now().UnixNano())

	// Server: the plugin listener tags accepted conns as TransportType "shm",
	// so server.go selects the registered server builder.
	lis, err := shm.NewListener(name)
	if err != nil {
		t.Fatalf("shm.NewListener: %v", err)
	}
	defer lis.Close()

	stopSrv := benchmark.StartServer(benchmark.ServerInfo{Type: "protobuf", Listener: lis})
	defer stopSrv()
	time.Sleep(100 * time.Millisecond)

	// Client: a manual resolver tags the address with TransportType "shm", so
	// createTransport selects the registered client builder.
	r := manual.NewBuilderWithScheme("shmreg")
	r.InitialState(resolver.State{
		Addresses: []resolver.Address{{Addr: name, TransportType: shm.Name}},
	})

	conn, err := grpc.NewClient("shmreg:///"+name,
		grpc.WithResolvers(r),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	client := testpb.NewBenchmarkServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := &testpb.SimpleRequest{
		ResponseType: testpb.PayloadType_COMPRESSABLE,
		ResponseSize: 64,
		Payload:      benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, 64),
	}
	resp, err := client.UnaryCall(ctx, req)
	if err != nil {
		t.Fatalf("UnaryCall over registry path: %v", err)
	}
	if got := len(resp.GetPayload().GetBody()); got != 64 {
		t.Fatalf("response payload = %d bytes, want 64", got)
	}
}

// TestRegistryPathStreaming exercises bidirectional streaming over the SHM
// transport selected through the registries. Streaming drives the wrapped
// stream's repeated Write/Read and the server-side SendMsg fast-path fallback
// (which, on the plugin path, falls back to the portable Write because the
// wrapped stream does not expose WriteProto).
func TestRegistryPathStreaming(t *testing.T) {
	name := fmt.Sprintf("test_registry_stream_%d", time.Now().UnixNano())

	lis, err := shm.NewListener(name)
	if err != nil {
		t.Fatalf("shm.NewListener: %v", err)
	}
	defer lis.Close()

	stopSrv := benchmark.StartServer(benchmark.ServerInfo{Type: "protobuf", Listener: lis})
	defer stopSrv()
	time.Sleep(100 * time.Millisecond)

	r := manual.NewBuilderWithScheme("shmregstream")
	r.InitialState(resolver.State{
		Addresses: []resolver.Address{{Addr: name, TransportType: shm.Name}},
	})

	conn, err := grpc.NewClient("shmregstream:///"+name,
		grpc.WithResolvers(r),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()

	client := testpb.NewBenchmarkServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	stream, err := client.StreamingCall(ctx)
	if err != nil {
		t.Fatalf("StreamingCall over registry path: %v", err)
	}

	const n = 5
	const sz = 256
	for i := 0; i < n; i++ {
		req := &testpb.SimpleRequest{
			ResponseType: testpb.PayloadType_COMPRESSABLE,
			ResponseSize: sz,
			Payload:      benchmark.NewPayload(testpb.PayloadType_COMPRESSABLE, sz),
		}
		if err := stream.Send(req); err != nil {
			t.Fatalf("stream.Send #%d: %v", i, err)
		}
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("stream.Recv #%d: %v", i, err)
		}
		if got := len(resp.GetPayload().GetBody()); got != sz {
			t.Fatalf("ping-pong #%d payload = %d bytes, want %d", i, got, sz)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
}
