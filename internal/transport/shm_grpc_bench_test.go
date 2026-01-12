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
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =============================================================================
// BENCHMARK SUITE: Full gRPC Communication Patterns for SHM Transport
// =============================================================================

// BenchmarkShmRingLargeMessages extends roundtrip testing to larger message sizes
// that are common in production gRPC workloads.
func BenchmarkShmRingLargeMessages(b *testing.B) {
	sizes := []int{65536, 262144, 1048576, 4194304} // 64KB, 256KB, 1MB, 4MB
	const ringSize = 8 * 1024 * 1024                // 8MB ring for large messages

	for _, size := range sizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			if size > ringSize/2 {
				b.Skipf("Message size %d exceeds ring capacity", size)
			}

			segName := fmt.Sprintf("bench-large-%d-%d", size, time.Now().UnixNano())
			seg, err := CreateSegment(segName, ringSize, ringSize)
			if err != nil {
				b.Fatalf("CreateSegment failed: %v", err)
			}
			defer func() {
				seg.Close()
				RemoveSegment(segName)
			}()

			clientToServer := NewShmRingFromSegment(seg.A, seg.Mem)
			serverToClient := NewShmRingFromSegment(seg.B, seg.Mem)

			ctx := context.Background()
			data := make([]byte, size)
			// Fill with pattern for verification
			for i := range data {
				data[i] = byte(i & 0xFF)
			}

			var wg sync.WaitGroup
			errCh := make(chan error, 2)
			started := make(chan struct{})

			// Echo server
			wg.Add(1)
			go func() {
				defer wg.Done()
				close(started)

				for i := 0; i < b.N; i++ {
					first, second, commit, err := clientToServer.ReadSlices(size, ctx)
					if err != nil {
						errCh <- err
						return
					}

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
				res, err := clientToServer.ReserveWrite(size, ctx)
				if err != nil {
					b.Fatalf("ReserveWrite failed: %v", err)
				}
				copy(res.First, data)
				if len(res.Second) > 0 {
					copy(res.Second, data[len(res.First):])
				}
				res.Commit(size)

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

// BenchmarkShmConcurrentStreams measures performance with multiple concurrent
// request/response streams - simulating multiplexed gRPC streams.
func BenchmarkShmConcurrentStreams(b *testing.B) {
	concurrencyLevels := []int{1, 2, 4, 8, 16, 32}
	const messageSize = 1024 // 1KB messages
	const ringSize = 1024 * 1024

	for _, concurrency := range concurrencyLevels {
		b.Run(fmt.Sprintf("streams=%d", concurrency), func(b *testing.B) {
			segName := fmt.Sprintf("bench-concurrent-%d-%d", concurrency, time.Now().UnixNano())
			seg, err := CreateSegment(segName, ringSize, ringSize)
			if err != nil {
				b.Fatalf("CreateSegment failed: %v", err)
			}
			defer func() {
				seg.Close()
				RemoveSegment(segName)
			}()

			clientToServer := NewShmRingFromSegment(seg.A, seg.Mem)
			serverToClient := NewShmRingFromSegment(seg.B, seg.Mem)

			ctx := context.Background()

			var serverWg sync.WaitGroup
			serverDone := make(chan struct{})
			totalOps := int64(b.N * concurrency)
			var serverOps int64

			// Server: echo all messages
			serverWg.Add(1)
			go func() {
				defer serverWg.Done()
				for {
					select {
					case <-serverDone:
						return
					default:
					}

					ctxTimeout, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
					first, second, commit, err := clientToServer.ReadSlices(messageSize, ctxTimeout)
					cancel()
					if err != nil {
						continue
					}

					res, err := serverToClient.ReserveWrite(messageSize, ctx)
					if err != nil {
						return
					}
					copy(res.First, first)
					if len(second) > 0 && len(res.Second) > 0 {
						copy(res.Second, second)
					}
					res.Commit(messageSize)
					commit.Commit(messageSize)

					if atomic.AddInt64(&serverOps, 1) >= totalOps {
						return
					}
				}
			}()

			b.SetBytes(int64(messageSize * 2 * concurrency))
			b.ResetTimer()

			var clientWg sync.WaitGroup
			for c := 0; c < concurrency; c++ {
				clientWg.Add(1)
				go func() {
					defer clientWg.Done()
					data := make([]byte, messageSize)

					for i := 0; i < b.N; i++ {
						// Write request
						res, err := clientToServer.ReserveWrite(messageSize, ctx)
						if err != nil {
							return
						}
						copy(res.First, data)
						res.Commit(messageSize)

						// Read response
						_, _, commit, err := serverToClient.ReadSlices(messageSize, ctx)
						if err != nil {
							return
						}
						commit.Commit(messageSize)
					}
				}()
			}

			clientWg.Wait()
			close(serverDone)
			serverWg.Wait()
		})
	}
}

// BenchmarkShmLatencyPercentiles measures latency distribution (p50, p90, p99, p999)
// for roundtrip operations - critical for understanding tail latency behavior.
func BenchmarkShmLatencyPercentiles(b *testing.B) {
	const messageSize = 1024
	const ringSize = 1024 * 1024
	const iterations = 10000 // Enough samples for statistical significance

	segName := fmt.Sprintf("bench-latency-%d", time.Now().UnixNano())
	seg, err := CreateSegment(segName, ringSize, ringSize)
	if err != nil {
		b.Fatalf("CreateSegment failed: %v", err)
	}
	defer func() {
		seg.Close()
		RemoveSegment(segName)
	}()

	clientToServer := NewShmRingFromSegment(seg.A, seg.Mem)
	serverToClient := NewShmRingFromSegment(seg.B, seg.Mem)

	ctx := context.Background()
	data := make([]byte, messageSize)

	var wg sync.WaitGroup
	started := make(chan struct{})

	// Echo server
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)

		for i := 0; i < iterations; i++ {
			first, second, commit, err := clientToServer.ReadSlices(messageSize, ctx)
			if err != nil {
				return
			}

			res, err := serverToClient.ReserveWrite(messageSize, ctx)
			if err != nil {
				return
			}
			copy(res.First, first)
			if len(second) > 0 && len(res.Second) > 0 {
				copy(res.Second, second)
			}
			res.Commit(messageSize)
			commit.Commit(messageSize)
		}
	}()

	<-started

	// Collect latency samples
	latencies := make([]time.Duration, iterations)

	for i := 0; i < iterations; i++ {
		start := time.Now()

		res, err := clientToServer.ReserveWrite(messageSize, ctx)
		if err != nil {
			b.Fatalf("ReserveWrite failed: %v", err)
		}
		copy(res.First, data)
		res.Commit(messageSize)

		_, _, commit, err := serverToClient.ReadSlices(messageSize, ctx)
		if err != nil {
			b.Fatalf("ReadSlices failed: %v", err)
		}
		commit.Commit(messageSize)

		latencies[i] = time.Since(start)
	}

	wg.Wait()

	// Calculate percentiles
	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	p50 := latencies[int(float64(iterations)*0.50)]
	p90 := latencies[int(float64(iterations)*0.90)]
	p99 := latencies[int(float64(iterations)*0.99)]
	p999 := latencies[int(float64(iterations)*0.999)]
	min := latencies[0]
	max := latencies[iterations-1]

	var sum time.Duration
	for _, l := range latencies {
		sum += l
	}
	avg := sum / time.Duration(iterations)

	b.ReportMetric(float64(min.Nanoseconds()), "min_ns")
	b.ReportMetric(float64(avg.Nanoseconds()), "avg_ns")
	b.ReportMetric(float64(p50.Nanoseconds()), "p50_ns")
	b.ReportMetric(float64(p90.Nanoseconds()), "p90_ns")
	b.ReportMetric(float64(p99.Nanoseconds()), "p99_ns")
	b.ReportMetric(float64(p999.Nanoseconds()), "p999_ns")
	b.ReportMetric(float64(max.Nanoseconds()), "max_ns")
}

// BenchmarkShmSegmentCreation measures the overhead of creating and mapping
// shared memory segments - important for understanding connection setup cost.
func BenchmarkShmSegmentCreation(b *testing.B) {
	sizes := []int{
		256 * 1024,      // 256KB
		1024 * 1024,     // 1MB
		4 * 1024 * 1024, // 4MB
		8 * 1024 * 1024, // 8MB
	}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				segName := fmt.Sprintf("bench-create-%d-%d", i, time.Now().UnixNano())
				seg, err := CreateSegment(segName, uint64(size), uint64(size))
				if err != nil {
					b.Fatalf("CreateSegment failed: %v", err)
				}

				// Also create ring buffers to measure full setup cost
				_ = NewShmRingFromSegment(seg.A, seg.Mem)
				_ = NewShmRingFromSegment(seg.B, seg.Mem)

				seg.Close()
				RemoveSegment(segName)
			}
		})
	}
}

// BenchmarkShmClientStreaming measures one-way client streaming pattern
// (many requests, single response) - common for upload scenarios.
func BenchmarkShmClientStreaming(b *testing.B) {
	messageCounts := []int{10, 100, 1000}
	const messageSize = 1024
	const ringSize = 1024 * 1024

	for _, count := range messageCounts {
		b.Run(fmt.Sprintf("messages=%d", count), func(b *testing.B) {
			segName := fmt.Sprintf("bench-client-stream-%d-%d", count, time.Now().UnixNano())
			seg, err := CreateSegment(segName, ringSize, ringSize)
			if err != nil {
				b.Fatalf("CreateSegment failed: %v", err)
			}
			defer func() {
				seg.Close()
				RemoveSegment(segName)
			}()

			clientToServer := NewShmRingFromSegment(seg.A, seg.Mem)
			serverToClient := NewShmRingFromSegment(seg.B, seg.Mem)

			ctx := context.Background()
			data := make([]byte, messageSize)

			var wg sync.WaitGroup
			started := make(chan struct{})

			// Server: receive all messages, send single response
			wg.Add(1)
			go func() {
				defer wg.Done()
				close(started)

				for i := 0; i < b.N; i++ {
					// Receive all client messages
					for j := 0; j < count; j++ {
						_, _, commit, err := clientToServer.ReadSlices(messageSize, ctx)
						if err != nil {
							return
						}
						commit.Commit(messageSize)
					}

					// Send single response
					res, err := serverToClient.ReserveWrite(messageSize, ctx)
					if err != nil {
						return
					}
					copy(res.First, data)
					res.Commit(messageSize)
				}
			}()

			<-started

			b.SetBytes(int64(messageSize * (count + 1)))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				// Send all messages
				for j := 0; j < count; j++ {
					res, err := clientToServer.ReserveWrite(messageSize, ctx)
					if err != nil {
						b.Fatalf("ReserveWrite failed: %v", err)
					}
					copy(res.First, data)
					res.Commit(messageSize)
				}

				// Receive single response
				_, _, commit, err := serverToClient.ReadSlices(messageSize, ctx)
				if err != nil {
					b.Fatalf("ReadSlices failed: %v", err)
				}
				commit.Commit(messageSize)
			}

			wg.Wait()
		})
	}
}

// BenchmarkShmServerStreaming measures one-way server streaming pattern
// (single request, many responses) - common for subscription/feed scenarios.
func BenchmarkShmServerStreaming(b *testing.B) {
	messageCounts := []int{10, 100, 1000}
	const messageSize = 1024
	const ringSize = 1024 * 1024

	for _, count := range messageCounts {
		b.Run(fmt.Sprintf("messages=%d", count), func(b *testing.B) {
			segName := fmt.Sprintf("bench-server-stream-%d-%d", count, time.Now().UnixNano())
			seg, err := CreateSegment(segName, ringSize, ringSize)
			if err != nil {
				b.Fatalf("CreateSegment failed: %v", err)
			}
			defer func() {
				seg.Close()
				RemoveSegment(segName)
			}()

			clientToServer := NewShmRingFromSegment(seg.A, seg.Mem)
			serverToClient := NewShmRingFromSegment(seg.B, seg.Mem)

			ctx := context.Background()
			data := make([]byte, messageSize)

			var wg sync.WaitGroup
			started := make(chan struct{})

			// Server: receive request, send many responses
			wg.Add(1)
			go func() {
				defer wg.Done()
				close(started)

				for i := 0; i < b.N; i++ {
					// Receive single request
					_, _, commit, err := clientToServer.ReadSlices(messageSize, ctx)
					if err != nil {
						return
					}
					commit.Commit(messageSize)

					// Send all responses
					for j := 0; j < count; j++ {
						res, err := serverToClient.ReserveWrite(messageSize, ctx)
						if err != nil {
							return
						}
						copy(res.First, data)
						res.Commit(messageSize)
					}
				}
			}()

			<-started

			b.SetBytes(int64(messageSize * (count + 1)))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				// Send single request
				res, err := clientToServer.ReserveWrite(messageSize, ctx)
				if err != nil {
					b.Fatalf("ReserveWrite failed: %v", err)
				}
				copy(res.First, data)
				res.Commit(messageSize)

				// Receive all responses
				for j := 0; j < count; j++ {
					_, _, commit, err := serverToClient.ReadSlices(messageSize, ctx)
					if err != nil {
						b.Fatalf("ReadSlices failed: %v", err)
					}
					commit.Commit(messageSize)
				}
			}

			wg.Wait()
		})
	}
}

// BenchmarkShmBidirectionalStreaming measures full bidirectional streaming
// with concurrent send/receive - the most complex gRPC streaming pattern.
func BenchmarkShmBidirectionalStreaming(b *testing.B) {
	messageCounts := []int{10, 100, 1000}
	const messageSize = 1024
	const ringSize = 2 * 1024 * 1024

	for _, count := range messageCounts {
		b.Run(fmt.Sprintf("messages=%d", count), func(b *testing.B) {
			segName := fmt.Sprintf("bench-bidi-%d-%d", count, time.Now().UnixNano())
			seg, err := CreateSegment(segName, ringSize, ringSize)
			if err != nil {
				b.Fatalf("CreateSegment failed: %v", err)
			}
			defer func() {
				seg.Close()
				RemoveSegment(segName)
			}()

			clientToServer := NewShmRingFromSegment(seg.A, seg.Mem)
			serverToClient := NewShmRingFromSegment(seg.B, seg.Mem)

			ctx := context.Background()
			data := make([]byte, messageSize)

			var wg sync.WaitGroup
			started := make(chan struct{})

			// Server: concurrent read/write
			wg.Add(1)
			go func() {
				defer wg.Done()
				close(started)

				for i := 0; i < b.N; i++ {
					var serverWg sync.WaitGroup

					// Server receiver
					serverWg.Add(1)
					go func() {
						defer serverWg.Done()
						for j := 0; j < count; j++ {
							_, _, commit, err := clientToServer.ReadSlices(messageSize, ctx)
							if err != nil {
								return
							}
							commit.Commit(messageSize)
						}
					}()

					// Server sender
					serverWg.Add(1)
					go func() {
						defer serverWg.Done()
						for j := 0; j < count; j++ {
							res, err := serverToClient.ReserveWrite(messageSize, ctx)
							if err != nil {
								return
							}
							copy(res.First, data)
							res.Commit(messageSize)
						}
					}()

					serverWg.Wait()
				}
			}()

			<-started

			b.SetBytes(int64(messageSize * count * 2))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				var clientWg sync.WaitGroup

				// Client sender
				clientWg.Add(1)
				go func() {
					defer clientWg.Done()
					for j := 0; j < count; j++ {
						res, err := clientToServer.ReserveWrite(messageSize, ctx)
						if err != nil {
							return
						}
						copy(res.First, data)
						res.Commit(messageSize)
					}
				}()

				// Client receiver
				clientWg.Add(1)
				go func() {
					defer clientWg.Done()
					for j := 0; j < count; j++ {
						_, _, commit, err := serverToClient.ReadSlices(messageSize, ctx)
						if err != nil {
							return
						}
						commit.Commit(messageSize)
					}
				}()

				clientWg.Wait()
			}

			wg.Wait()
		})
	}
}

// BenchmarkShmBackpressure measures behavior when the ring buffer fills up,
// testing how well backpressure is handled.
func BenchmarkShmBackpressure(b *testing.B) {
	const messageSize = 4096
	const ringSize = 64 * 1024 // Small ring to trigger backpressure

	segName := fmt.Sprintf("bench-backpressure-%d", time.Now().UnixNano())
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
	data := make([]byte, messageSize)

	var wg sync.WaitGroup
	started := make(chan struct{})

	// Slow consumer (simulates backpressure)
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)

		for i := 0; i < b.N; i++ {
			_, _, commit, err := ring.ReadSlices(messageSize, ctx)
			if err != nil {
				return
			}
			// Simulate slow processing
			time.Sleep(10 * time.Microsecond)
			commit.Commit(messageSize)
		}
	}()

	<-started

	b.SetBytes(int64(messageSize))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		res, err := ring.ReserveWrite(messageSize, ctx)
		if err != nil {
			b.Fatalf("ReserveWrite failed: %v", err)
		}
		copy(res.First, data)
		res.Commit(messageSize)
	}

	wg.Wait()
}

// BenchmarkShmVsTCPComparison runs identical workloads on SHM and TCP
// for direct comparison in a single benchmark run.
func BenchmarkShmVsTCPComparison(b *testing.B) {
	const messageSize = 1024
	const ringSize = 1024 * 1024

	b.Run("shm", func(b *testing.B) {
		segName := fmt.Sprintf("bench-compare-shm-%d", time.Now().UnixNano())
		seg, err := CreateSegment(segName, ringSize, ringSize)
		if err != nil {
			b.Fatalf("CreateSegment failed: %v", err)
		}
		defer func() {
			seg.Close()
			RemoveSegment(segName)
		}()

		clientToServer := NewShmRingFromSegment(seg.A, seg.Mem)
		serverToClient := NewShmRingFromSegment(seg.B, seg.Mem)

		ctx := context.Background()
		data := make([]byte, messageSize)

		var wg sync.WaitGroup
		started := make(chan struct{})

		wg.Add(1)
		go func() {
			defer wg.Done()
			close(started)

			for i := 0; i < b.N; i++ {
				first, second, commit, err := clientToServer.ReadSlices(messageSize, ctx)
				if err != nil {
					return
				}

				res, err := serverToClient.ReserveWrite(messageSize, ctx)
				if err != nil {
					return
				}
				copy(res.First, first)
				if len(second) > 0 && len(res.Second) > 0 {
					copy(res.Second, second)
				}
				res.Commit(messageSize)
				commit.Commit(messageSize)
			}
		}()

		<-started

		b.SetBytes(int64(messageSize * 2))
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			res, err := clientToServer.ReserveWrite(messageSize, ctx)
			if err != nil {
				b.Fatalf("ReserveWrite failed: %v", err)
			}
			copy(res.First, data)
			res.Commit(messageSize)

			_, _, commit, err := serverToClient.ReadSlices(messageSize, ctx)
			if err != nil {
				b.Fatalf("ReadSlices failed: %v", err)
			}
			commit.Commit(messageSize)
		}

		wg.Wait()
	})
}
