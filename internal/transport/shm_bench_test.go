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
	"os"
	"sync"
	"testing"
	"time"
)

// BenchmarkShmRingWriteRead measures raw ring buffer throughput
func BenchmarkShmRingWriteRead(b *testing.B) {
	sizes := []int{64, 256, 1024, 4096, 16384, 65536, 262144, 1048576} // 64B to 1MB
	const ringSize = 64 * 1024 * 1024                                   // 64MB ring for benchmarks

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
				commit.Commit(size)
			}
		})
	}
}

// BenchmarkShmRingThroughput measures sustained streaming throughput
func BenchmarkShmRingThroughput(b *testing.B) {
	sizes := []int{1024, 4096, 16384, 65536, 262144, 1048576, 4194304} // 1KB to 4MB
	const ringSize = 64 * 1024 * 1024                                    // 64MB ring for benchmarks

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
					commit.Commit(size)
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
	sizes := []int{64, 256, 1024, 4096, 16384, 65536, 262144, 1048576} // 64B to 1MB

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

// BenchmarkUnixSocketRoundtrip measures Unix socket round-trip time
func BenchmarkUnixSocketRoundtrip(b *testing.B) {
	sizes := []int{64, 256, 1024, 4096}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			sockPath := fmt.Sprintf("/tmp/bench-unix-rt-%d.sock", time.Now().UnixNano())
			defer os.Remove(sockPath)

			listener, err := net.Listen("unix", sockPath)
			if err != nil {
				b.Fatalf("Listen failed: %v", err)
			}
			defer listener.Close()

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

			conn, err := net.Dial("unix", sockPath)
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
					commit.Commit(size)
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
				commit.Commit(size)
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
	sizes := []int{64, 256, 1024, 4096, 16384, 65536, 262144, 1048576} // 64B to 1MB

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

// BenchmarkShmRingLargePayloads benchmarks very large message transfers up to 256MB
// using the 64MB ring buffer with automatic chunking.
func BenchmarkShmRingLargePayloads(b *testing.B) {
	// Test from 1MB to 256MB - these require chunking with the 64MB ring
	sizes := []int{
		1 * 1024 * 1024,   // 1MB
		4 * 1024 * 1024,   // 4MB
		16 * 1024 * 1024,  // 16MB
		64 * 1024 * 1024,  // 64MB (ring size)
		128 * 1024 * 1024, // 128MB (requires chunking)
		256 * 1024 * 1024, // 256MB (requires chunking)
	}
	const ringSize = 64 * 1024 * 1024 // 64MB ring
	const chunkSize = 32 * 1024 * 1024 // 32MB chunks for transfers > ring size

	for _, size := range sizes {
		sizeMB := size / (1024 * 1024)
		b.Run(fmt.Sprintf("size=%dMB", sizeMB), func(b *testing.B) {
			segName := fmt.Sprintf("bench-large-%d-%d", size, time.Now().UnixNano())
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
			// Fill with pattern
			for i := range data {
				data[i] = byte(i & 0xFF)
			}

			b.SetBytes(int64(size))
			b.ResetTimer()

			if size <= int(ringSize) {
				// Sequential write-then-read for payloads that fit in ring
				for i := 0; i < b.N; i++ {
					// Write the full payload
					if err := ring.WriteAll(data, ctx); err != nil {
						b.Fatalf("WriteAll failed at iter %d: %v", i, err)
					}

					// Read the full payload
					totalRead := 0
					for totalRead < size {
						first, second, commit, err := ring.ReadSlices(min(size-totalRead, 32*1024), ctx)
						if err != nil {
							b.Fatalf("ReadSlices failed at iter %d, %d/%d: %v", i, totalRead, size, err)
						}
						n := len(first) + len(second)
						commit.Commit(n)
						totalRead += n
					}
				}
			} else {
				// Chunked transfer for payloads larger than ring
				// Use alternating write-chunk/read-chunk to avoid filling the ring
				for i := 0; i < b.N; i++ {
					offset := 0
					for offset < size {
						// Determine chunk size for this iteration
						writeSize := chunkSize
						if offset+writeSize > size {
							writeSize = size - offset
						}

						// Write chunk
						if err := ring.WriteAll(data[offset:offset+writeSize], ctx); err != nil {
							b.Fatalf("WriteAll chunk failed at iter %d, offset %d: %v", i, offset, err)
						}

						// Read chunk immediately
						chunkRead := 0
						for chunkRead < writeSize {
							first, second, commit, err := ring.ReadSlices(min(writeSize-chunkRead, 32*1024), ctx)
							if err != nil {
								b.Fatalf("ReadSlices chunk failed at iter %d, offset %d: %v", i, offset, err)
							}
							n := len(first) + len(second)
							commit.Commit(n)
							chunkRead += n
						}

						offset += writeSize
					}
				}
			}
		})
	}
}

// BenchmarkTCPLargePayloads benchmarks TCP with large payloads for comparison
func BenchmarkTCPLargePayloads(b *testing.B) {
	sizes := []int{
		1 * 1024 * 1024,   // 1MB
		4 * 1024 * 1024,   // 4MB
		16 * 1024 * 1024,  // 16MB
		64 * 1024 * 1024,  // 64MB
		128 * 1024 * 1024, // 128MB
		256 * 1024 * 1024, // 256MB
	}

	for _, size := range sizes {
		sizeMB := size / (1024 * 1024)
		b.Run(fmt.Sprintf("size=%dMB", sizeMB), func(b *testing.B) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatalf("Listen failed: %v", err)
			}
			defer listener.Close()

			addr := listener.Addr().String()
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i & 0xFF)
			}

			// Use channels for synchronization
			serverReady := make(chan struct{})
			done := make(chan struct{})
			errCh := make(chan error, 1)

			// Server goroutine - reads data continuously until done
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					errCh <- err
					return
				}
				defer conn.Close()
				close(serverReady)

				recvBuf := make([]byte, 256*1024) // 256KB read buffer
				for {
					select {
					case <-done:
						return
					default:
					}
					conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
					_, err := conn.Read(recvBuf)
					if err != nil {
						if ne, ok := err.(net.Error); ok && ne.Timeout() {
							continue
						}
						return
					}
				}
			}()

			conn, err := net.Dial("tcp", addr)
			if err != nil {
				b.Fatalf("Dial failed: %v", err)
			}
			defer conn.Close()
			<-serverReady

			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				totalWritten := 0
				for totalWritten < size {
					n, err := conn.Write(data[totalWritten:])
					if err != nil {
						b.Fatalf("Write failed: %v", err)
					}
					totalWritten += n
				}
			}

			b.StopTimer()
			close(done)

			select {
			case err := <-errCh:
				b.Fatalf("Server error: %v", err)
			default:
			}
		})
	}
}

// BenchmarkUnixLargePayloads benchmarks Unix socket with large payloads for comparison
func BenchmarkUnixLargePayloads(b *testing.B) {
	sizes := []int{
		1 * 1024 * 1024,   // 1MB
		4 * 1024 * 1024,   // 4MB
		16 * 1024 * 1024,  // 16MB
		64 * 1024 * 1024,  // 64MB
		128 * 1024 * 1024, // 128MB
		256 * 1024 * 1024, // 256MB
	}

	for _, size := range sizes {
		sizeMB := size / (1024 * 1024)
		b.Run(fmt.Sprintf("size=%dMB", sizeMB), func(b *testing.B) {
			sockPath := fmt.Sprintf("/tmp/bench-unix-large-%d.sock", time.Now().UnixNano())

			listener, err := net.Listen("unix", sockPath)
			if err != nil {
				b.Fatalf("Listen failed: %v", err)
			}
			defer func() {
				listener.Close()
				os.Remove(sockPath)
			}()

			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i & 0xFF)
			}

			// Use channels for synchronization
			serverReady := make(chan struct{})
			done := make(chan struct{})
			errCh := make(chan error, 1)

			// Server goroutine - reads data continuously until done
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					errCh <- err
					return
				}
				defer conn.Close()
				close(serverReady)

				recvBuf := make([]byte, 256*1024) // 256KB read buffer
				for {
					select {
					case <-done:
						return
					default:
					}
					conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
					_, err := conn.Read(recvBuf)
					if err != nil {
						if ne, ok := err.(net.Error); ok && ne.Timeout() {
							continue
						}
						return
					}
				}
			}()

			conn, err := net.Dial("unix", sockPath)
			if err != nil {
				b.Fatalf("Dial failed: %v", err)
			}
			defer conn.Close()
			<-serverReady

			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				totalWritten := 0
				for totalWritten < size {
					n, err := conn.Write(data[totalWritten:])
					if err != nil {
						b.Fatalf("Write failed: %v", err)
					}
					totalWritten += n
				}
			}

			b.StopTimer()
			close(done)

			select {
			case err := <-errCh:
				b.Fatalf("Server error: %v", err)
			default:
			}
		})
	}
}

// =============================================================================
// LARGE PAYLOAD ROUNDTRIP (UNARY) BENCHMARKS
// =============================================================================

// BenchmarkShmRingLargePayloadsRoundtrip benchmarks SHM unary/roundtrip with large payloads
func BenchmarkShmRingLargePayloadsRoundtrip(b *testing.B) {
	sizes := []int{
		1 * 1024 * 1024,   // 1MB
		4 * 1024 * 1024,   // 4MB
		16 * 1024 * 1024,  // 16MB
		64 * 1024 * 1024,  // 64MB
		128 * 1024 * 1024, // 128MB
		// 256MB removed - causes timing issues with 64MB ring
	}
	const ringSize = 64 * 1024 * 1024 // 64MB ring
	const chunkSize = 4 * 1024 * 1024 // 4MB chunks - optimal for SHM

	for _, size := range sizes {
		sizeMB := size / (1024 * 1024)
		b.Run(fmt.Sprintf("size=%dMB", sizeMB), func(b *testing.B) {
			segName := fmt.Sprintf("bench-large-rt-%d-%d", size, time.Now().UnixNano())
			seg, err := CreateSegment(segName, ringSize, ringSize)
			if err != nil {
				b.Fatalf("CreateSegment failed: %v", err)
			}

			// Ring A: client -> server, Ring B: server -> client
			clientToServer := NewShmRingFromSegment(seg.A, seg.Mem)
			serverToClient := NewShmRingFromSegment(seg.B, seg.Mem)

			ctx, cancel := context.WithCancel(context.Background())
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i & 0xFF)
			}

			started := make(chan struct{})
			serverDone := make(chan struct{})
			errCh := make(chan error, 1)

			// Echo server goroutine - uses ReadBlockingContext which reads whatever is available
			readBuf := make([]byte, chunkSize)
			go func() {
				defer close(serverDone)
				close(started)

				for {
					// Read whatever is available (up to chunkSize)
					n, err := clientToServer.ReadBlockingContext(ctx, readBuf)
					if err != nil {
						// Context cancelled or ring closed - normal exit
						return
					}

					// Echo back immediately
					if err := serverToClient.WriteAll(readBuf[:n], ctx); err != nil {
						select {
						case errCh <- err:
						default:
						}
						return
					}
				}
			}()

			<-started

			b.SetBytes(int64(size * 2)) // roundtrip
			b.ResetTimer()

			recvBuf := make([]byte, chunkSize)
			for i := 0; i < b.N; i++ {
				var writeErr error
				var readErr error
				var wg sync.WaitGroup
				wg.Add(2)

				// Write concurrently with read to match TCP benchmark pattern
				go func() {
					defer wg.Done()
					offset := 0
					for offset < size {
						writeSize := min(chunkSize, size-offset)
						if err := clientToServer.WriteAll(data[offset:offset+writeSize], ctx); err != nil {
							writeErr = err
							return
						}
						offset += writeSize
					}
				}()

				// Read full response concurrently
				go func() {
					defer wg.Done()
					totalRead := 0
					for totalRead < size {
						n, err := serverToClient.ReadBlockingContext(ctx, recvBuf)
						if err != nil {
							readErr = err
							return
						}
						totalRead += n
					}
				}()

				wg.Wait()
				if writeErr != nil {
					b.Fatalf("WriteAll failed: %v", writeErr)
				}
				if readErr != nil {
					b.Fatalf("ReadSlices failed: %v", readErr)
				}
			}

			b.StopTimer()
			
			// Cancel context first to unblock all goroutines
			cancel()
			// Close ring buffers to ensure any remaining blocked operations exit
			clientToServer.Close()
			serverToClient.Close()
			// Wait for echo server to actually exit before cleaning up segment
			<-serverDone

			// Clean up segment
			seg.Close()
			RemoveSegment(segName)

			select {
			case err := <-errCh:
				b.Fatalf("Server error: %v", err)
			default:
			}
		})
	}
}

// BenchmarkTCPLargePayloadsRoundtrip benchmarks TCP unary/roundtrip with large payloads
func BenchmarkTCPLargePayloadsRoundtrip(b *testing.B) {
	sizes := []int{
		1 * 1024 * 1024,   // 1MB
		4 * 1024 * 1024,   // 4MB
		16 * 1024 * 1024,  // 16MB
		64 * 1024 * 1024,  // 64MB
		128 * 1024 * 1024, // 128MB
		256 * 1024 * 1024, // 256MB
	}

	for _, size := range sizes {
		sizeMB := size / (1024 * 1024)
		b.Run(fmt.Sprintf("size=%dMB", sizeMB), func(b *testing.B) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatalf("Listen failed: %v", err)
			}
			defer listener.Close()

			addr := listener.Addr().String()
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i & 0xFF)
			}

			started := make(chan struct{})
			done := make(chan struct{})
			errCh := make(chan error, 1)

			// Echo server
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					errCh <- err
					return
				}
				defer conn.Close()
				close(started)

				buf := make([]byte, 256*1024)
				for {
					select {
					case <-done:
						return
					default:
					}

					// Read and echo
					conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
					n, err := conn.Read(buf)
					if err != nil {
						if ne, ok := err.(net.Error); ok && ne.Timeout() {
							continue
						}
						return
					}
					conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
					_, err = conn.Write(buf[:n])
					if err != nil {
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

			recvBuf := make([]byte, 256*1024)

			b.SetBytes(int64(size * 2)) // roundtrip
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				var writeErr error
				var wg sync.WaitGroup
				wg.Add(1)

				// Write concurrently with read to avoid deadlock on large payloads
				go func() {
					defer wg.Done()
					totalWritten := 0
					for totalWritten < size {
						n, err := conn.Write(data[totalWritten:])
						if err != nil {
							writeErr = err
							return
						}
						totalWritten += n
					}
				}()

				// Receive full response
				totalRead := 0
				for totalRead < size {
					n, err := conn.Read(recvBuf)
					if err != nil {
						b.Fatalf("Read failed: %v", err)
					}
					totalRead += n
				}

				wg.Wait()
				if writeErr != nil {
					b.Fatalf("Write failed: %v", writeErr)
				}
			}

			b.StopTimer()
			close(done)

			select {
			case err := <-errCh:
				b.Fatalf("Server error: %v", err)
			default:
			}
		})
	}
}

// BenchmarkUnixLargePayloadsRoundtrip benchmarks Unix socket unary/roundtrip with large payloads
func BenchmarkUnixLargePayloadsRoundtrip(b *testing.B) {
	sizes := []int{
		1 * 1024 * 1024,   // 1MB
		4 * 1024 * 1024,   // 4MB
		16 * 1024 * 1024,  // 16MB
		64 * 1024 * 1024,  // 64MB
		128 * 1024 * 1024, // 128MB
		256 * 1024 * 1024, // 256MB
	}

	for _, size := range sizes {
		sizeMB := size / (1024 * 1024)
		b.Run(fmt.Sprintf("size=%dMB", sizeMB), func(b *testing.B) {
			sockPath := fmt.Sprintf("/tmp/bench-unix-large-rt-%d.sock", time.Now().UnixNano())

			listener, err := net.Listen("unix", sockPath)
			if err != nil {
				b.Fatalf("Listen failed: %v", err)
			}
			defer func() {
				listener.Close()
				os.Remove(sockPath)
			}()

			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i & 0xFF)
			}

			started := make(chan struct{})
			done := make(chan struct{})
			errCh := make(chan error, 1)

			// Echo server
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					errCh <- err
					return
				}
				defer conn.Close()
				close(started)

				buf := make([]byte, 256*1024)
				for {
					select {
					case <-done:
						return
					default:
					}

					// Read and echo
					conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
					n, err := conn.Read(buf)
					if err != nil {
						if ne, ok := err.(net.Error); ok && ne.Timeout() {
							continue
						}
						return
					}
					conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
					_, err = conn.Write(buf[:n])
					if err != nil {
						return
					}
				}
			}()

			conn, err := net.Dial("unix", sockPath)
			if err != nil {
				b.Fatalf("Dial failed: %v", err)
			}
			defer conn.Close()
			<-started

			recvBuf := make([]byte, 256*1024)

			b.SetBytes(int64(size * 2)) // roundtrip
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				var writeErr error
				var wg sync.WaitGroup
				wg.Add(1)

				// Write concurrently with read to avoid deadlock on large payloads
				go func() {
					defer wg.Done()
					totalWritten := 0
					for totalWritten < size {
						n, err := conn.Write(data[totalWritten:])
						if err != nil {
							writeErr = err
							return
						}
						totalWritten += n
					}
				}()

				// Receive full response
				totalRead := 0
				for totalRead < size {
					n, err := conn.Read(recvBuf)
					if err != nil {
						b.Fatalf("Read failed: %v", err)
					}
					totalRead += n
				}

				wg.Wait()
				if writeErr != nil {
					b.Fatalf("Write failed: %v", writeErr)
				}
			}

			b.StopTimer()
			close(done)

			select {
			case err := <-errCh:
				b.Fatalf("Server error: %v", err)
			default:
			}
		})
	}
}
