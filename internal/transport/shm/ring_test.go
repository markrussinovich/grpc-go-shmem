package shm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestShmRingBasics(t *testing.T) {
	// Create a test segment for ring operations with unique name
	segName := fmt.Sprintf("test-ring-basics-%d", time.Now().UnixNano())
	segment, err := CreateSegment(segName, 4096, 4096)
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	defer segment.Close()

	// Get ring A for testing
	ringA := segment.A
	ring := NewShmRingFromSegment(ringA, segment.Mem)

	// Test basic write and read
	testData := []byte("hello world")

	if err := ring.WriteBlocking(testData); err != nil {
		t.Fatalf("WriteBlocking failed: %v", err)
	}

	readBuf := make([]byte, len(testData))
	n, err := ring.ReadBlocking(readBuf)
	if err != nil {
		t.Fatalf("ReadBlocking failed: %v", err)
	}

	if n != len(testData) {
		t.Fatalf("expected to read %d bytes, got %d", len(testData), n)
	}

	if !bytes.Equal(readBuf[:n], testData) {
		t.Fatalf("data mismatch: expected %q, got %q", testData, readBuf[:n])
	}
}

func TestShmRingEmpty(t *testing.T) {
	segName := fmt.Sprintf("test-ring-empty-%d", time.Now().UnixNano())
	segment, err := CreateSegment(segName, 4096, 4096)
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	defer segment.Close()

	ringA := segment.A
	ring := NewShmRingFromSegment(ringA, segment.Mem)

	// Test reading from empty ring with timeout
	readBuf := make([]byte, 100)

	done := make(chan struct{})
	var readErr error
	var readBytes int

	go func() {
		defer close(done)
		readBytes, readErr = ring.ReadBlocking(readBuf)
	}()

	// Close ring after short delay to unblock reader
	time.AfterFunc(100*time.Millisecond, func() {
		ring.Close()
	})

	select {
	case <-done:
		if readErr != io.EOF {
			t.Fatalf("expected EOF from closed empty ring, got: %v", readErr)
		}
		if readBytes != 0 {
			t.Fatalf("expected 0 bytes from empty ring, got %d", readBytes)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadBlocking should have returned after ring close")
	}
}

func TestShmRingWrapAround(t *testing.T) {
	// Create small ring to test wrap-around with unique name
	segName := fmt.Sprintf("test-ring-wrap-%d", time.Now().UnixNano())
	segment, err := CreateSegment(segName, 4096, 4096)
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	defer segment.Close()

	ringA := segment.A
	ring := NewShmRingFromSegment(ringA, segment.Mem)

	// Write data that wraps around the ring
	capacity := ring.Capacity()
	testData := make([]byte, capacity/2) // Half capacity
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	// Write twice to force wrap-around
	if err := ring.WriteBlocking(testData); err != nil {
		t.Fatalf("first WriteBlocking failed: %v", err)
	}

	// Read half to make space and advance read pointer
	readBuf := make([]byte, len(testData)/2)
	n, err := ring.ReadBlocking(readBuf)
	if err != nil {
		t.Fatalf("ReadBlocking failed: %v", err)
	}
	if n != len(readBuf) {
		t.Fatalf("expected %d bytes, got %d", len(readBuf), n)
	}

	// Write again - this should wrap around
	if err := ring.WriteBlocking(testData); err != nil {
		t.Fatalf("second WriteBlocking failed: %v", err)
	}

	// Read remaining data
	remainingBuf := make([]byte, capacity)
	totalRead := 0
	for totalRead < len(testData)+len(testData)/2 {
		n, err := ring.ReadBlocking(remainingBuf[totalRead:])
		if err != nil {
			t.Fatalf("ReadBlocking failed: %v", err)
		}
		totalRead += n
	}

	// Verify data integrity
	expected := append(testData[len(testData)/2:], testData...)
	if !bytes.Equal(remainingBuf[:totalRead], expected) {
		t.Fatalf("wrap-around data mismatch")
	}
}

func TestShmRingConcurrent(t *testing.T) {
	segName := fmt.Sprintf("test-ring-concurrent-%d", time.Now().UnixNano())
	segment, err := CreateSegment(segName, 4096, 4096)
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	defer segment.Close()

	ringA := segment.A
	ring := NewShmRingFromSegment(ringA, segment.Mem)

	const numMessages = 100
	const messageSize = 32

	var wg sync.WaitGroup
	wg.Add(2)

	// Producer goroutine
	go func() {
		defer wg.Done()
		for i := 0; i < numMessages; i++ {
			msg := make([]byte, messageSize)
			for j := range msg {
				msg[j] = byte(i % 256)
			}
			if err := ring.WriteBlocking(msg); err != nil {
				t.Errorf("WriteBlocking failed: %v", err)
				return
			}
		}
		ring.Close() // Signal end of data
	}()

	// Consumer goroutine
	received := make([][]byte, 0, numMessages)
	go func() {
		defer wg.Done()
		buf := make([]byte, messageSize)
		for {
			n, err := ring.ReadBlocking(buf)
			if err == io.EOF {
				break // Ring closed and empty
			}
			if err != nil {
				t.Errorf("ReadBlocking failed: %v", err)
				return
			}
			msg := make([]byte, n)
			copy(msg, buf[:n])
			received = append(received, msg)
		}
	}()

	// Wait with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Verify all messages received
		if len(received) != numMessages {
			t.Fatalf("expected %d messages, got %d", numMessages, len(received))
		}

		// Verify message content
		for i, msg := range received {
			expected := byte(i % 256)
			for _, b := range msg {
				if b != expected {
					t.Fatalf("message %d content mismatch: expected %d, got %d", i, expected, b)
				}
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent test timed out")
	}
}

func TestShmRingUtilities(t *testing.T) {
	segName := fmt.Sprintf("test-ring-utils-%d", time.Now().UnixNano())
	segment, err := CreateSegment(segName, 4096, 4096)
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	defer segment.Close()

	ringA := segment.A
	ring := NewShmRingFromSegment(ringA, segment.Mem)

	// Test initial state
	if !ring.IsEmpty() {
		t.Fatal("new ring should be empty")
	}
	if ring.IsFull() {
		t.Fatal("new ring should not be full")
	}
	if ring.IsClosed() {
		t.Fatal("new ring should not be closed")
	}
	if ring.Used() != 0 {
		t.Fatal("new ring should have 0 used bytes")
	}
	if ring.Available() != ring.Capacity() {
		t.Fatal("new ring should have full capacity available")
	}

	// Write some data
	testData := []byte("test data")
	if err := ring.WriteBlocking(testData); err != nil {
		t.Fatalf("WriteBlocking failed: %v", err)
	}

	// Test state after write
	if ring.IsEmpty() {
		t.Fatal("ring should not be empty after write")
	}
	if ring.Used() != uint64(len(testData)) {
		t.Fatalf("expected %d used bytes, got %d", len(testData), ring.Used())
	}
	if ring.Available() != ring.Capacity()-uint64(len(testData)) {
		t.Fatalf("available bytes mismatch")
	}

	// Test closure
	if err := ring.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if !ring.IsClosed() {
		t.Fatal("ring should be closed after Close()")
	}
}

func TestShmRingFullBuffer(t *testing.T) {
	// Create small ring to test full buffer handling with unique name
	segName := fmt.Sprintf("test-ring-full-%d", time.Now().UnixNano())
	segment, err := CreateSegment(segName, 4096, 4096)
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	defer segment.Close()

	ringA := segment.A
	ring := NewShmRingFromSegment(ringA, segment.Mem)

	// Fill the ring completely
	capacity := ring.Capacity()
	fillData := make([]byte, capacity)
	for i := range fillData {
		fillData[i] = byte(i % 256)
	}

	if err := ring.WriteBlocking(fillData); err != nil {
		t.Fatalf("WriteBlocking failed: %v", err)
	}

	if !ring.IsFull() {
		t.Fatal("ring should be full")
	}

	// Try to write more - should block
	done := make(chan struct{})
	var writeErr error

	go func() {
		defer close(done)
		writeErr = ring.WriteBlocking([]byte("overflow"))
	}()

	// Close ring after delay to unblock writer
	time.AfterFunc(100*time.Millisecond, func() {
		ring.Close()
	})

	select {
	case <-done:
		if writeErr != ErrRingClosed {
			t.Fatalf("expected ErrRingClosed, got: %v", writeErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WriteBlocking should have returned after ring close")
	}
}

// TestShmRingNoPolling verifies that blocking operations are event-driven, not polling
func TestShmRingNoPolling(t *testing.T) {
	segName := fmt.Sprintf("test-ring-no-polling-%d", time.Now().UnixNano())
	segment, err := CreateSegment(segName, 4096, 4096) // Use minimum required size
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	defer segment.Close()

	ringA := segment.A
	ring := NewShmRingFromSegment(ringA, segment.Mem)

	ctx := context.Background()

	// Fill the ring completely
	fillData := make([]byte, ring.Capacity())
	if err := ring.WriteAll(fillData, ctx); err != nil {
		t.Fatalf("WriteAll failed: %v", err)
	}

	// Track goroutine count to detect polling loops
	initialGoroutines := runtime.NumGoroutine()

	// Start a writer that should block (no polling)
	writerDone := make(chan error, 1)
	go func() {
		// This should block until reader frees space
		err := ring.WriteBlocking([]byte("test"))
		writerDone <- err
	}()

	// Let writer block for a short time
	time.Sleep(50 * time.Millisecond)

	// Check that goroutine count is stable (no polling creating new goroutines)
	currentGoroutines := runtime.NumGoroutine()
	if currentGoroutines > initialGoroutines+2 { // +1 for writer, +1 for tolerance
		t.Fatalf("suspected polling: goroutine count increased from %d to %d", initialGoroutines, currentGoroutines)
	}

	// Read some data to unblock writer
	buf := make([]byte, 10)
	n, err := ring.ReadBlocking(buf)
	if err != nil || n != 10 {
		t.Fatalf("ReadBlocking failed: %v, n=%d", err, n)
	}

	// Writer should now complete
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("writer failed: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("writer should have unblocked after read")
	}

	// Verify goroutine count returned to baseline
	finalGoroutines := runtime.NumGoroutine()
	if finalGoroutines > initialGoroutines+1 { // +1 for tolerance
		t.Fatalf("goroutine leak: count went from %d to %d", initialGoroutines, finalGoroutines)
	}
}

// TestShmRingStressSPSC runs stress test with variable-length writes/reads
func TestShmRingStressSPSC(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	segName := fmt.Sprintf("test-ring-stress-%d", time.Now().UnixNano())
	segment, err := CreateSegment(segName, 4096, 4096)
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}
	defer segment.Close()

	ringA := segment.A
	ring := NewShmRingFromSegment(ringA, segment.Mem)

	const numOperations = 1000 // Reduced for smaller ring buffer
	ctx := context.Background()

	var wg sync.WaitGroup
	var producerErr, consumerErr error

	// Producer goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()

		for i := 0; i < numOperations; i++ {
			// Variable-length messages (1-50 bytes to fit in smaller ring)
			size := 1 + (i % 50)
			data := make([]byte, size)

			// Fill with pattern and calculate checksum
			var csum uint32
			for j := range data {
				data[j] = byte((i + j) % 256)
				csum += uint32(data[j])
			}

			// Write message with length prefix
			msgBytes := make([]byte, 4+len(data)) // 4-byte length + data
			msgBytes[0] = byte(len(data))
			msgBytes[1] = byte(len(data) >> 8)
			msgBytes[2] = byte(csum)
			msgBytes[3] = byte(csum >> 8)
			copy(msgBytes[4:], data)

			if err := ring.WriteAll(msgBytes, ctx); err != nil {
				producerErr = fmt.Errorf("WriteAll failed at %d: %v", i, err)
				return
			}
		}

		// Close the ring to signal the consumer that no more data is coming
		ring.Close()
	}()

	// Consumer goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()

		for i := 0; i < numOperations; i++ {
			// Read 4-byte header first
			header, err := ring.ReadExact(4, nil, ctx)
			if err != nil {
				if err == io.EOF {
					// Producer closed the ring, but we expected more data
					consumerErr = fmt.Errorf("unexpected EOF at operation %d", i)
					return
				}
				consumerErr = fmt.Errorf("ReadExact header failed at %d: %v", i, err)
				return
			}

			// Parse length and checksum
			length := int(header[0]) | (int(header[1]) << 8)
			expectedCsum := uint32(header[2]) | (uint32(header[3]) << 8)

			// Read data
			data, err := ring.ReadExact(length, nil, ctx)
			if err != nil {
				if err == io.EOF {
					consumerErr = fmt.Errorf("unexpected EOF while reading data at operation %d", i)
					return
				}
				consumerErr = fmt.Errorf("ReadExact data failed at %d: %v", i, err)
				return
			}

			// Verify checksum
			var actualCsum uint32
			for _, b := range data {
				actualCsum += uint32(b)
			}
			if actualCsum != expectedCsum {
				consumerErr = fmt.Errorf("checksum mismatch at %d: expected %d, got %d", i, expectedCsum, actualCsum)
				return
			}

			// Verify data pattern
			for j, b := range data {
				expected := byte((i + j) % 256)
				if b != expected {
					consumerErr = fmt.Errorf("data mismatch at %d[%d]: expected %d, got %d", i, j, expected, b)
					return
				}
			}
		}
	}()

	// Wait for both goroutines with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Both goroutines completed
		if producerErr != nil {
			t.Fatalf("producer failed: %v", producerErr)
		}
		if consumerErr != nil {
			t.Fatalf("consumer failed: %v", consumerErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("test timed out - goroutines did not complete")
	}
}
