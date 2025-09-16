package shm

import (
	"bytes"
	"fmt"
	"io"
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
