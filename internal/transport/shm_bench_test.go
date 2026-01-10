//go:build linux

/*
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
 */

package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// BenchmarkShmRingWriteRead measures raw ring buffer throughput
func BenchmarkShmRingWriteRead(b *testing.B) {
	sizes := []int{64, 256, 1024, 4096, 16384, 65536}
	const ringSize = 1024 * 1024 // 1MB ring for benchmarks

	for _, size := range sizes {
		size := size // capture
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			segName := fmt.Sprintf("bench-ring-%d-%d", size, time.Now().UnixNano())
			seg, err := CreateSegment(segName, ringSize, ringSize)
			if err != nil {
				b.Fatalf("CreateSegment failed: %v", err)
			}
			b.Cleanup(func() {
				seg.Close()
				RemoveSegment(segName)
			})

			ring := NewShmRingFromSegment(seg.A, seg.Mem)
			ctx := context.Background()
			data := make([]byte, size)

			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				// Write
				res, err := ring.ReserveWrite(size, ctx)
				if err != nil {
					b.Fatalf("ReserveWrite failed at iter %d: %v", i, err)
				}
				n := copy(res.First, data)
				if len(res.Second) > 0 && n < size {
					copy(res.Second, data[n:])
				}
				if err := res.Commit(size); err != nil {
					b.Fatalf("Commit failed: %v", err)
				}

				// Read
				first, second, commit, err := ring.ReadSlices(size, ctx)
				if err != nil {
					b.Fatalf("ReadSlices failed at iter %d: %v", i, err)
				}
				_ = first
				_ = second
				commit(size)
			}
		})
	}
}

// BenchmarkShmRingThroughput measures sustained streaming throughput
func BenchmarkShmRingThroughput(b *testing.B) {
	sizes := []int{1024, 4096, 16384, 65536}
	const ringSize = 1024 * 1024 // 1MB ring for benchmarks

	for _, size := range sizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			segName := fmt.Sprintf("bench-throughput-%d-%d", size, time.Now().UnixNano())
			seg, err := CreateSegment(segName, ringSize, ringSize)
			if err != nil {
				b.Fatalf("CreateSegment failed: %v", err)
			}
			defer func() {
				seg.Close()
				RemoveSegment(segName)
			}()

			ring := NewShmRingFromSegment(seg.A, seg.Mem)
			ctx := context.Background()
			data := make([]byte, size)

			// Writer and reader in separate goroutines
			var wg sync.WaitGroup
			errCh := make(chan error, 2)

			b.SetBytes(int64(size))
			b.ResetTimer()

			// Writer goroutine
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < b.N; i++ {
					res, err := ring.ReserveWrite(size, ctx)
					if err != nil {
						errCh <- err
						return
					}
					copy(res.First, data)
					if len(res.Second) > 0 {
						copy(res.Second, data[len(res.First):])
					}
					res.Commit(size)
				}
			}()

			// Reader goroutine
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < b.N; i++ {
					first, second, commit, err := ring.ReadSlices(size, ctx)
					if err != nil {
						errCh <- err
						return
					}
					_ = first
					_ = second
					commit(size)
				}
			}()

			wg.Wait()
			select {
			case err := <-errCh:
				b.Fatalf("Error: %v", err)
			default:
			}
		})
	}
}

// BenchmarkTCPLoopback measures TCP loopback performance for comparison
func BenchmarkTCPLoopback(b *testing.B) {
	sizes := []int{64, 256, 1024, 4096, 16384, 65536}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			// Create listener
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatalf("Listen failed: %v", err)
			}
			defer listener.Close()

			addr := listener.Addr().String()
			data := make([]byte, size)
			recvBuf := make([]byte, size)

			var wg sync.WaitGroup
			errCh := make(chan error, 2)

			// Server goroutine
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := listener.Accept()
				if err != nil {
					errCh <- err
					return
				}
				defer conn.Close()

				for i := 0; i < b.N; i++ {
					_, err := io.ReadFull(conn, recvBuf)
					if err != nil {
						errCh <- err
						return
					}
				}
			}()

			// Give server time to start
			time.Sleep(10 * time.Millisecond)

			// Client
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				b.Fatalf("Dial failed: %v", err)
			}
			defer conn.Close()

			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_, err := conn.Write(data)
				if err != nil {
					b.Fatalf("Write failed: %v", err)
				}
			}

			wg.Wait()
			select {
			case err := <-errCh:
				b.Fatalf("Server error: %v", err)
			default:
			}
		})
	}
}

// BenchmarkTCPLoopbackRoundtrip measures TCP loopback round-trip time
func BenchmarkTCPLoopbackRoundtrip(b *testing.B) {
	sizes := []int{64, 256, 1024, 4096}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatalf("Listen failed: %v", err)
			}
			defer listener.Close()

			addr := listener.Addr().String()
			data := make([]byte, size)
			recvBuf := make([]byte, size)

			var wg sync.WaitGroup
			errCh := make(chan error, 2)
			started := make(chan struct{})

			// Echo server
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := listener.Accept()
				if err != nil {
					errCh <- err
					return
				}
				defer conn.Close()
				close(started)

				buf := make([]byte, size)
				for i := 0; i < b.N; i++ {
					_, err := io.ReadFull(conn, buf)
					if err != nil {
						errCh <- err
						return
					}
					_, err = conn.Write(buf)
					if err != nil {
						errCh <- err
						return
					}
				}
			}()

			conn, err := net.Dial("tcp", addr)
			if err != nil {
				b.Fatalf("Dial failed: %v", err)
			}
			defer conn.Close()

			<-started

			b.SetBytes(int64(size * 2)) // round trip
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_, err := conn.Write(data)
				if err != nil {
					b.Fatalf("Write failed: %v", err)
				}
				_, err = io.ReadFull(conn, recvBuf)
				if err != nil {
					b.Fatalf("Read failed: %v", err)
				}
			}

			wg.Wait()
			select {
			case err := <-errCh:
				b.Fatalf("Server error: %v", err)
			default:
			}
		})
	}
}

// BenchmarkShmRingRoundtrip measures SHM round-trip time
func BenchmarkShmRingRoundtrip(b *testing.B) {
	sizes := []int{64, 256, 1024, 4096}
	const ringSize = 1024 * 1024 // 1MB ring for benchmarks

	for _, size := range sizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			segName := fmt.Sprintf("bench-rt-%d-%d", size, time.Now().UnixNano())
			seg, err := CreateSegment(segName, ringSize, ringSize)
			if err != nil {
				b.Fatalf("CreateSegment failed: %v", err)
			}
			defer func() {
				seg.Close()
				RemoveSegment(segName)
			}()

			// Ring A: client -> server, Ring B: server -> client
			clientToServer := NewShmRingFromSegment(seg.A, seg.Mem)
			serverToClient := NewShmRingFromSegment(seg.B, seg.Mem)

			ctx := context.Background()
			data := make([]byte, size)

			var wg sync.WaitGroup
			errCh := make(chan error, 2)
			started := make(chan struct{})

			// Echo server
			wg.Add(1)
			go func() {
				defer wg.Done()
				close(started)

				for i := 0; i < b.N; i++ {
					// Read from client
					first, second, commit, err := clientToServer.ReadSlices(size, ctx)
					if err != nil {
						errCh <- err
						return
					}

					// Write to client (echo)
					res, err := serverToClient.ReserveWrite(size, ctx)
					if err != nil {
						errCh <- err
						return
					}
					copy(res.First, first)
					if len(second) > 0 && len(res.Second) > 0 {
						copy(res.Second, second)
					}
					res.Commit(size)
					commit(size)
				}
			}()

			<-started

			b.SetBytes(int64(size * 2))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				// Write to server
				res, err := clientToServer.ReserveWrite(size, ctx)
				if err != nil {
					b.Fatalf("ReserveWrite failed: %v", err)
				}
				copy(res.First, data)
				res.Commit(size)

				// Read from server
				first, _, commit, err := serverToClient.ReadSlices(size, ctx)
				if err != nil {
					b.Fatalf("ReadSlices failed: %v", err)
				}
				_ = first
				commit(size)
			}

			wg.Wait()
			select {
			case err := <-errCh:
				b.Fatalf("Server error: %v", err)
			default:
			}
		})
	}
}

// BenchmarkUnixSocketLoopback for comparison
func BenchmarkUnixSocketLoopback(b *testing.B) {
	sizes := []int{64, 256, 1024, 4096, 16384, 65536}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			sockPath := fmt.Sprintf("/tmp/bench-unix-%d.sock", time.Now().UnixNano())

			listener, err := net.Listen("unix", sockPath)
			if err != nil {
				b.Fatalf("Listen failed: %v", err)
			}
			defer listener.Close()

			data := make([]byte, size)
			recvBuf := make([]byte, size)

			var wg sync.WaitGroup
			errCh := make(chan error, 2)

			// Server
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := listener.Accept()
				if err != nil {
					errCh <- err
					return
				}
				defer conn.Close()

				for i := 0; i < b.N; i++ {
					_, err := io.ReadFull(conn, recvBuf)
					if err != nil {
						errCh <- err
						return
					}
				}
			}()

			time.Sleep(10 * time.Millisecond)

			conn, err := net.Dial("unix", sockPath)
			if err != nil {
				b.Fatalf("Dial failed: %v", err)
			}
			defer conn.Close()

			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_, err := conn.Write(data)
				if err != nil {
					b.Fatalf("Write failed: %v", err)
				}
			}

			wg.Wait()
			select {
			case err := <-errCh:
				b.Fatalf("Server error: %v", err)
			default:
			}
		})
	}
}
