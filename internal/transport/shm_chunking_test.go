//go:build linux

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

package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/mem"
)

// TestChunkedWriteSmallRing tests that large messages are properly chunked
// when the ring buffer is smaller than the message size.
func TestChunkedWriteSmallRing(t *testing.T) {
	// Use a small 64KB ring to force chunking for larger messages.
	const smallRingSize = 64 * 1024

	segmentName := fmt.Sprintf("grpc_shm_chunk_test_%d", time.Now().UnixNano())
	seg, err := CreateSegment(segmentName, smallRingSize, smallRingSize)
	if err != nil {
		t.Fatalf("CreateSegment failed: %v", err)
	}
	defer func() {
		_ = seg.Close()
		_ = RemoveSegment(segmentName)
	}()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	testCases := []struct {
		name    string
		msgSize int
	}{
		{"1KB_fits_single_frame", 1024},
		{"8KB_fits_single_frame", 8 * 1024},
		{"16KB_fits_single_frame", 16 * 1024},
		{"32KB_needs_chunking", 32 * 1024},   // Half capacity, should trigger chunking
		{"48KB_needs_chunking", 48 * 1024},   // 3/4 capacity
		{"64KB_needs_chunking", 64 * 1024},   // Full capacity, definitely needs chunks
		{"128KB_needs_chunking", 128 * 1024}, // 2x capacity
		{"256KB_needs_chunking", 256 * 1024}, // 4x capacity
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Generate random payload
			payload := make([]byte, tc.msgSize)
			if _, err := rand.Read(payload); err != nil {
				t.Fatalf("Failed to generate random payload: %v", err)
			}

			// Write using chunked function
			fh := FrameHeader{
				StreamID: 1,
				Type:     FrameTypeMESSAGE,
				Flags:    0,
			}

			// Start reader in goroutine
			readDone := make(chan struct{})
			var readErr error
			var received []byte

			go func() {
				defer close(readDone)
				received, readErr = readChunkedMessage(ctx, rx)
			}()

			// Write the message (using nil header and BufferSlice for test)
			data := mem.SliceBuffer(payload)
			defer data.Free()

			err := writeFrameBuffersChunked(ctx, tx, fh, nil, mem.BufferSlice{data}, 0)
			if err != nil {
				t.Fatalf("writeFrameBuffersChunked failed: %v", err)
			}

			// Wait for reader
			select {
			case <-readDone:
				if readErr != nil {
					t.Fatalf("Reader error: %v", readErr)
				}
			case <-ctx.Done():
				t.Fatal("Timeout waiting for reader")
			}

			// Verify payload
			if !bytes.Equal(received, payload) {
				t.Errorf("Payload mismatch: got %d bytes, want %d bytes", len(received), len(payload))
				if len(received) > 0 && len(payload) > 0 {
					// Show first difference
					for i := 0; i < len(received) && i < len(payload); i++ {
						if received[i] != payload[i] {
							t.Errorf("First difference at byte %d: got %x, want %x", i, received[i], payload[i])
							break
						}
					}
				}
			}
		})
	}
}

// readChunkedMessage reads a potentially chunked MESSAGE and reassembles it.
func readChunkedMessage(ctx context.Context, rx *ShmRing) ([]byte, error) {
	var result []byte

	for {
		fh, payload, err := readFrame(ctx, rx)
		if err != nil {
			return nil, fmt.Errorf("readFrame failed: %w", err)
		}

		if fh.Type != FrameTypeMESSAGE {
			return nil, fmt.Errorf("unexpected frame type: %v", fh.Type)
		}

		result = append(result, payload...)

		// Check if this is the last chunk
		if fh.Flags&MessageFlagMORE == 0 {
			break
		}
	}

	return result, nil
}

// TestChunkedWriteWithHeader tests chunking with non-empty header prefix.
func TestChunkedWriteWithHeader(t *testing.T) {
	const smallRingSize = 64 * 1024

	segmentName := fmt.Sprintf("grpc_shm_chunk_hdr_test_%d", time.Now().UnixNano())
	seg, err := CreateSegment(segmentName, smallRingSize, smallRingSize)
	if err != nil {
		t.Fatalf("CreateSegment failed: %v", err)
	}
	defer func() {
		_ = seg.Close()
		_ = RemoveSegment(segmentName)
	}()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 5-byte gRPC header + 100KB payload = needs chunking with 64KB ring
	hdr := []byte{0, 0, 1, 0x86, 0xA0} // compressed=false, length=100000
	payloadSize := 100 * 1024
	payload := make([]byte, payloadSize)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("Failed to generate random payload: %v", err)
	}

	fh := FrameHeader{
		StreamID: 1,
		Type:     FrameTypeMESSAGE,
		Flags:    0,
	}

	// Start reader
	readDone := make(chan struct{})
	var readErr error
	var received []byte

	go func() {
		defer close(readDone)
		received, readErr = readChunkedMessage(ctx, rx)
	}()

	// Write with header
	data := mem.SliceBuffer(payload)
	defer data.Free()

	err = writeFrameBuffersChunked(ctx, tx, fh, hdr, mem.BufferSlice{data}, 0)
	if err != nil {
		t.Fatalf("writeFrameBuffersChunked failed: %v", err)
	}

	// Wait for reader
	select {
	case <-readDone:
		if readErr != nil {
			t.Fatalf("Reader error: %v", readErr)
		}
	case <-ctx.Done():
		t.Fatal("Timeout waiting for reader")
	}

	// Verify combined hdr + payload
	expected := append(hdr, payload...)
	if !bytes.Equal(received, expected) {
		t.Errorf("Payload mismatch: got %d bytes, want %d bytes", len(received), len(expected))
	}
}

// TestChunkedWriteFastPath tests that small messages bypass chunking.
func TestChunkedWriteFastPath(t *testing.T) {
	const smallRingSize = 64 * 1024

	segmentName := fmt.Sprintf("grpc_shm_chunk_fast_test_%d", time.Now().UnixNano())
	seg, err := CreateSegment(segmentName, smallRingSize, smallRingSize)
	if err != nil {
		t.Fatalf("CreateSegment failed: %v", err)
	}
	defer func() {
		_ = seg.Close()
		_ = RemoveSegment(segmentName)
	}()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Small message that fits in single frame
	payload := make([]byte, 1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("Failed to generate random payload: %v", err)
	}

	fh := FrameHeader{
		StreamID: 1,
		Type:     FrameTypeMESSAGE,
		Flags:    0,
	}

	// Start reader
	readDone := make(chan struct{})
	var received []byte
	var readErr error

	go func() {
		defer close(readDone)
		fhRead, payload, err := readFrame(ctx, rx)
		if err != nil {
			readErr = err
			return
		}
		// Fast path should NOT set MORE flag
		if fhRead.Flags&MessageFlagMORE != 0 {
			readErr = fmt.Errorf("unexpected MORE flag on small message")
			return
		}
		received = payload
	}()

	// Write
	data := mem.SliceBuffer(payload)
	defer data.Free()

	err = writeFrameBuffersChunked(ctx, tx, fh, nil, mem.BufferSlice{data}, 0)
	if err != nil {
		t.Fatalf("writeFrameBuffersChunked failed: %v", err)
	}

	// Wait for reader
	select {
	case <-readDone:
		if readErr != nil {
			t.Fatalf("Reader error: %v", readErr)
		}
	case <-ctx.Done():
		t.Fatal("Timeout waiting for reader")
	}

	if !bytes.Equal(received, payload) {
		t.Errorf("Payload mismatch: got %d bytes, want %d bytes", len(received), len(payload))
	}
}

// TestChunkedWriteExplicitChunkSize tests chunking with explicit chunk size.
func TestChunkedWriteExplicitChunkSize(t *testing.T) {
	const ringSize = 256 * 1024

	segmentName := fmt.Sprintf("grpc_shm_chunk_explicit_test_%d", time.Now().UnixNano())
	seg, err := CreateSegment(segmentName, ringSize, ringSize)
	if err != nil {
		t.Fatalf("CreateSegment failed: %v", err)
	}
	defer func() {
		_ = seg.Close()
		_ = RemoveSegment(segmentName)
	}()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 100KB payload with explicit 10KB chunk size = 10 chunks
	payloadSize := 100 * 1024
	chunkSize := 10 * 1024

	payload := make([]byte, payloadSize)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("Failed to generate random payload: %v", err)
	}

	fh := FrameHeader{
		StreamID: 1,
		Type:     FrameTypeMESSAGE,
		Flags:    0,
	}

	// Count frames received
	readDone := make(chan struct{})
	var readErr error
	var received []byte
	var frameCount int

	go func() {
		defer close(readDone)
		for {
			fhRead, p, err := readFrame(ctx, rx)
			if err != nil {
				readErr = err
				return
			}
			frameCount++
			received = append(received, p...)
			if fhRead.Flags&MessageFlagMORE == 0 {
				break
			}
		}
	}()

	// Write with explicit chunk size
	data := mem.SliceBuffer(payload)
	defer data.Free()

	err = writeFrameBuffersChunked(ctx, tx, fh, nil, mem.BufferSlice{data}, chunkSize)
	if err != nil {
		t.Fatalf("writeFrameBuffersChunked failed: %v", err)
	}

	// Wait for reader
	select {
	case <-readDone:
		if readErr != nil {
			t.Fatalf("Reader error: %v", readErr)
		}
	case <-ctx.Done():
		t.Fatal("Timeout waiting for reader")
	}

	// Verify
	if !bytes.Equal(received, payload) {
		t.Errorf("Payload mismatch: got %d bytes, want %d bytes", len(received), len(payload))
	}

	expectedFrames := (payloadSize + chunkSize - 1) / chunkSize // ceiling division
	if frameCount != expectedFrames {
		t.Errorf("Frame count mismatch: got %d, want %d", frameCount, expectedFrames)
	}
}

// BenchmarkChunkedWriteSmallRing benchmarks chunked writes with small ring.
func BenchmarkChunkedWriteSmallRing(b *testing.B) {
	const smallRingSize = 64 * 1024

	segmentName := fmt.Sprintf("grpc_shm_chunk_bench_%d", time.Now().UnixNano())
	seg, err := CreateSegment(segmentName, smallRingSize, smallRingSize)
	if err != nil {
		b.Fatalf("CreateSegment failed: %v", err)
	}
	defer func() {
		_ = seg.Close()
		_ = RemoveSegment(segmentName)
	}()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	payloadSizes := []int{1024, 8 * 1024, 32 * 1024, 64 * 1024, 128 * 1024}

	for _, size := range payloadSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			payload := make([]byte, size)
			fh := FrameHeader{StreamID: 1, Type: FrameTypeMESSAGE}

			// Start reader goroutine
			done := make(chan struct{})
			go func() {
				defer close(done)
				for i := 0; i < b.N; i++ {
					_, _ = readChunkedMessage(ctx, rx)
				}
			}()

			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				data := mem.SliceBuffer(payload)
				_ = writeFrameBuffersChunked(ctx, tx, fh, nil, mem.BufferSlice{data}, 0)
				data.Free()
			}

			b.StopTimer()
			<-done
		})
	}
}
