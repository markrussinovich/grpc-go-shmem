// Copyright 2024 gRPC authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package transport

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/internal/grpctest"
)

// Benchmark configuration
var (
	messageSizes = []int{
		0,      // Empty message
		64,     // 64 bytes
		1024,   // 1 KB
		4096,   // 4 KB
		16384,  // 16 KB
		65536,  // 64 KB
		262144, // 256 KB
		1048576, // 1 MB
		4194304, // 4 MB
	}
)

type benchmarkTestService struct {
	grpctest.UnimplementedEchoTestServiceServer
}

func (s *benchmarkTestService) UnaryCall(ctx context.Context, req *grpctest.SimpleRequest) (*grpctest.SimpleResponse, error) {
	return &grpctest.SimpleResponse{
		Payload: req.Payload,
	}, nil
}

func (s *benchmarkTestService) StreamingCall(stream grpctest.EchoTestService_StreamingCallServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			return err
		}
		if err := stream.Send(&grpctest.SimpleResponse{
			Payload: req.Payload,
		}); err != nil {
			return err
		}
	}
}

// setupShmServer creates a shared memory transport server
func setupShmServer(b *testing.B, segmentName string) (*grpc.Server, func()) {
	addr := &ShmAddr{Name: segmentName}
	lis, err := NewShmListener(addr, 16*1024*1024, 4*1024*1024, 4*1024*1024)
	if err != nil {
		b.Fatalf("Failed to create SHM listener: %v", err)
	}

	server := grpc.NewServer()
	grpctest.RegisterEchoTestServiceServer(server, &benchmarkTestService{})

	go server.Serve(lis)

	cleanup := func() {
		server.Stop()
		lis.Close()
	}

	// Give server time to start
	time.Sleep(50 * time.Millisecond)

	return server, cleanup
}

// setupTCPServer creates a TCP transport server
func setupTCPServer(b *testing.B) (*grpc.Server, string, func()) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("Failed to create TCP listener: %v", err)
	}

	server := grpc.NewServer()
	grpctest.RegisterEchoTestServiceServer(server, &benchmarkTestService{})

	go server.Serve(lis)

	cleanup := func() {
		server.Stop()
		lis.Close()
	}

	return server, lis.Addr().String(), cleanup
}

// setupUnixServer creates a Unix domain socket server
func setupUnixServer(b *testing.B) (*grpc.Server, string, func()) {
	tmpDir := b.TempDir()
	sockPath := filepath.Join(tmpDir, "test.sock")

	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		b.Fatalf("Failed to create Unix listener: %v", err)
	}

	server := grpc.NewServer()
	grpctest.RegisterEchoTestServiceServer(server, &benchmarkTestService{})

	go server.Serve(lis)

	cleanup := func() {
		server.Stop()
		lis.Close()
		os.Remove(sockPath)
	}

	return server, sockPath, cleanup
}

// Benchmark unary RPC with different transports and message sizes
func BenchmarkUnaryRPC(b *testing.B) {
	for _, size := range messageSizes {
		b.Run(fmt.Sprintf("size=%dB", size), func(b *testing.B) {
			// Benchmark shared memory transport
			b.Run("shm", func(b *testing.B) {
				segmentName := fmt.Sprintf("bench_unary_%d", size)
				_, cleanup := setupShmServer(b, segmentName)
				defer cleanup()

				conn, err := grpc.NewClient(
					fmt.Sprintf("shm://%s", segmentName),
					WithShmTransport(),
					grpc.WithTransportCredentials(insecure.NewCredentials()),
				)
				if err != nil {
					b.Fatalf("Failed to dial: %v", err)
				}
				defer conn.Close()

				client := grpctest.NewEchoTestServiceClient(conn)
				payload := make([]byte, size)

				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					_, err := client.UnaryCall(context.Background(), &grpctest.SimpleRequest{
						Payload: payload,
					})
					if err != nil {
						b.Fatalf("RPC failed: %v", err)
					}
				}
				b.StopTimer()
			})

			// Benchmark TCP transport
			b.Run("tcp", func(b *testing.B) {
				_, addr, cleanup := setupTCPServer(b)
				defer cleanup()

				conn, err := grpc.NewClient(
					addr,
					grpc.WithTransportCredentials(insecure.NewCredentials()),
				)
				if err != nil {
					b.Fatalf("Failed to dial: %v", err)
				}
				defer conn.Close()

				client := grpctest.NewEchoTestServiceClient(conn)
				payload := make([]byte, size)

				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					_, err := client.UnaryCall(context.Background(), &grpctest.SimpleRequest{
						Payload: payload,
					})
					if err != nil {
						b.Fatalf("RPC failed: %v", err)
					}
				}
				b.StopTimer()
			})

			// Benchmark Unix domain socket transport
			b.Run("unix", func(b *testing.B) {
				_, sockPath, cleanup := setupUnixServer(b)
				defer cleanup()

				conn, err := grpc.NewClient(
					fmt.Sprintf("unix://%s", sockPath),
					grpc.WithTransportCredentials(insecure.NewCredentials()),
				)
				if err != nil {
					b.Fatalf("Failed to dial: %v", err)
				}
				defer conn.Close()

				client := grpctest.NewEchoTestServiceClient(conn)
				payload := make([]byte, size)

				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					_, err := client.UnaryCall(context.Background(), &grpctest.SimpleRequest{
						Payload: payload,
					})
					if err != nil {
						b.Fatalf("RPC failed: %v", err)
					}
				}
				b.StopTimer()
			})
		})
	}
}

// Benchmark bidirectional streaming with different transports
func BenchmarkStreamingRPC(b *testing.B) {
	for _, size := range messageSizes {
		b.Run(fmt.Sprintf("size=%dB", size), func(b *testing.B) {
			// Benchmark shared memory transport
			b.Run("shm", func(b *testing.B) {
				segmentName := fmt.Sprintf("bench_stream_%d", size)
				_, cleanup := setupShmServer(b, segmentName)
				defer cleanup()

				conn, err := grpc.NewClient(
					fmt.Sprintf("shm://%s", segmentName),
					WithShmTransport(),
					grpc.WithTransportCredentials(insecure.NewCredentials()),
				)
				if err != nil {
					b.Fatalf("Failed to dial: %v", err)
				}
				defer conn.Close()

				client := grpctest.NewEchoTestServiceClient(conn)
				payload := make([]byte, size)

				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					stream, err := client.StreamingCall(context.Background())
					if err != nil {
						b.Fatalf("Failed to create stream: %v", err)
					}

					// Send and receive 10 messages per iteration
					for j := 0; j < 10; j++ {
						if err := stream.Send(&grpctest.SimpleRequest{Payload: payload}); err != nil {
							b.Fatalf("Send failed: %v", err)
						}
						if _, err := stream.Recv(); err != nil {
							b.Fatalf("Recv failed: %v", err)
						}
					}

					stream.CloseSend()
				}
				b.StopTimer()
			})

			// Benchmark TCP transport
			b.Run("tcp", func(b *testing.B) {
				_, addr, cleanup := setupTCPServer(b)
				defer cleanup()

				conn, err := grpc.NewClient(
					addr,
					grpc.WithTransportCredentials(insecure.NewCredentials()),
				)
				if err != nil {
					b.Fatalf("Failed to dial: %v", err)
				}
				defer conn.Close()

				client := grpctest.NewEchoTestServiceClient(conn)
				payload := make([]byte, size)

				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					stream, err := client.StreamingCall(context.Background())
					if err != nil {
						b.Fatalf("Failed to create stream: %v", err)
					}

					// Send and receive 10 messages per iteration
					for j := 0; j < 10; j++ {
						if err := stream.Send(&grpctest.SimpleRequest{Payload: payload}); err != nil {
							b.Fatalf("Send failed: %v", err)
						}
						if _, err := stream.Recv(); err != nil {
							b.Fatalf("Recv failed: %v", err)
						}
					}

					stream.CloseSend()
				}
				b.StopTimer()
			})

			// Benchmark Unix domain socket transport
			b.Run("unix", func(b *testing.B) {
				_, sockPath, cleanup := setupUnixServer(b)
				defer cleanup()

				conn, err := grpc.NewClient(
					fmt.Sprintf("unix://%s", sockPath),
					grpc.WithTransportCredentials(insecure.NewCredentials()),
				)
				if err != nil {
					b.Fatalf("Failed to dial: %v", err)
				}
				defer conn.Close()

				client := grpctest.NewEchoTestServiceClient(conn)
				payload := make([]byte, size)

				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					stream, err := client.StreamingCall(context.Background())
					if err != nil {
						b.Fatalf("Failed to create stream: %v", err)
					}

					// Send and receive 10 messages per iteration
					for j := 0; j < 10; j++ {
						if err := stream.Send(&grpctest.SimpleRequest{Payload: payload}); err != nil {
							b.Fatalf("Send failed: %v", err)
						}
						if _, err := stream.Recv(); err != nil {
							b.Fatalf("Recv failed: %v", err)
						}
					}

					stream.CloseSend()
				}
				b.StopTimer()
			})
		})
	}
}

// Benchmark throughput with concurrent operations
func BenchmarkThroughput(b *testing.B) {
	messageSize := 1024 // 1KB messages
	concurrency := []int{1, 10, 50, 100}

	for _, conc := range concurrency {
		b.Run(fmt.Sprintf("concurrency=%d", conc), func(b *testing.B) {
			// Benchmark shared memory transport
			b.Run("shm", func(b *testing.B) {
				segmentName := fmt.Sprintf("bench_throughput_%d", conc)
				_, cleanup := setupShmServer(b, segmentName)
				defer cleanup()

				conn, err := grpc.NewClient(
					fmt.Sprintf("shm://%s", segmentName),
					WithShmTransport(),
					grpc.WithTransportCredentials(insecure.NewCredentials()),
				)
				if err != nil {
					b.Fatalf("Failed to dial: %v", err)
				}
				defer conn.Close()

				client := grpctest.NewEchoTestServiceClient(conn)
				payload := make([]byte, messageSize)

				b.ResetTimer()
				b.ReportAllocs()
				b.SetParallelism(conc)
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						_, err := client.UnaryCall(context.Background(), &grpctest.SimpleRequest{
							Payload: payload,
						})
						if err != nil {
							b.Fatalf("RPC failed: %v", err)
						}
					}
				})
				b.StopTimer()
			})

			// Benchmark TCP transport
			b.Run("tcp", func(b *testing.B) {
				_, addr, cleanup := setupTCPServer(b)
				defer cleanup()

				conn, err := grpc.NewClient(
					addr,
					grpc.WithTransportCredentials(insecure.NewCredentials()),
				)
				if err != nil {
					b.Fatalf("Failed to dial: %v", err)
				}
				defer conn.Close()

				client := grpctest.NewEchoTestServiceClient(conn)
				payload := make([]byte, messageSize)

				b.ResetTimer()
				b.ReportAllocs()
				b.SetParallelism(conc)
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						_, err := client.UnaryCall(context.Background(), &grpctest.SimpleRequest{
							Payload: payload,
						})
						if err != nil {
							b.Fatalf("RPC failed: %v", err)
						}
					}
				})
				b.StopTimer()
			})
		})
	}
}

// Benchmark latency measurement with detailed percentiles
func BenchmarkLatency(b *testing.B) {
	messageSize := 1024 // 1KB messages

	measureLatency := func(b *testing.B, client grpctest.EchoTestServiceClient, payload []byte) {
		latencies := make([]time.Duration, b.N)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			start := time.Now()
			_, err := client.UnaryCall(context.Background(), &grpctest.SimpleRequest{
				Payload: payload,
			})
			latencies[i] = time.Since(start)
			if err != nil {
				b.Fatalf("RPC failed: %v", err)
			}
		}
		b.StopTimer()

		// Calculate percentiles
		// Note: For production, use a proper percentile calculation library
		b.ReportMetric(float64(latencies[0].Microseconds()), "p0_us")
		if len(latencies) > 0 {
			p50 := latencies[len(latencies)/2]
			p99 := latencies[len(latencies)*99/100]
			b.ReportMetric(float64(p50.Microseconds()), "p50_us")
			b.ReportMetric(float64(p99.Microseconds()), "p99_us")
		}
	}

	// Benchmark shared memory transport
	b.Run("shm", func(b *testing.B) {
		segmentName := "bench_latency_shm"
		_, cleanup := setupShmServer(b, segmentName)
		defer cleanup()

		conn, err := grpc.NewClient(
			fmt.Sprintf("shm://%s", segmentName),
			WithShmTransport(),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			b.Fatalf("Failed to dial: %v", err)
		}
		defer conn.Close()

		client := grpctest.NewEchoTestServiceClient(conn)
		payload := make([]byte, messageSize)

		measureLatency(b, client, payload)
	})

	// Benchmark TCP transport
	b.Run("tcp", func(b *testing.B) {
		_, addr, cleanup := setupTCPServer(b)
		defer cleanup()

		conn, err := grpc.NewClient(
			addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			b.Fatalf("Failed to dial: %v", err)
		}
		defer conn.Close()

		client := grpctest.NewEchoTestServiceClient(conn)
		payload := make([]byte, messageSize)

		measureLatency(b, client, payload)
	})

	// Benchmark Unix domain socket transport
	b.Run("unix", func(b *testing.B) {
		_, sockPath, cleanup := setupUnixServer(b)
		defer cleanup()

		conn, err := grpc.NewClient(
			fmt.Sprintf("unix://%s", sockPath),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			b.Fatalf("Failed to dial: %v", err)
		}
		defer conn.Close()

		client := grpctest.NewEchoTestServiceClient(conn)
		payload := make([]byte, messageSize)

		measureLatency(b, client, payload)
	})
}
