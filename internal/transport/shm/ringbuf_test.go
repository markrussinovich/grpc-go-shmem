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

package shm_test

import (
	"bytes"
	"fmt"
	"testing"

	"google.golang.org/grpc/internal/transport/shm"
)

func TestRing_NewCapacityPowerOfTwo(t *testing.T) {
	testCases := []struct {
		minCap   int
		expected int
	}{
		{1, 16},      // Below minimum
		{8, 16},      // Below minimum
		{15, 16},     // Below minimum
		{16, 16},     // Exact minimum
		{17, 32},     // Round up
		{31, 32},     // Round up
		{32, 32},     // Exact power of two
		{33, 64},     // Round up
		{63, 64},     // Round up
		{64, 64},     // Exact power of two
		{65, 128},    // Round up
		{127, 128},   // Round up
		{128, 128},   // Exact power of two
		{129, 256},   // Round up
		{1000, 1024}, // Round up to next power of two
		{1024, 1024}, // Exact power of two
		{1025, 2048}, // Round up to next power of two
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("minCap_%d", tc.minCap), func(t *testing.T) {
			ring, err := shm.NewRing(tc.minCap)
			if err != nil {
				t.Fatalf("NewRing(%d) failed: %v", tc.minCap, err)
			}
			if ring == nil {
				t.Fatalf("NewRing(%d) returned nil ring", tc.minCap)
			}

			actual := ring.Capacity()
			if actual != tc.expected {
				t.Errorf("NewRing(%d).Capacity() = %d, want %d", tc.minCap, actual, tc.expected)
			}
		})
	}
}

func TestRing_CapacityReportsCorrectly(t *testing.T) {
	testCases := []int{16, 32, 64, 128, 256, 512, 1024, 2048, 4096}

	for _, expectedCap := range testCases {
		t.Run(fmt.Sprintf("capacity_%d", expectedCap), func(t *testing.T) {
			// Request exactly the capacity (which is a power of two)
			ring, err := shm.NewRing(expectedCap)
			if err != nil {
				t.Fatalf("NewRing(%d) failed: %v", expectedCap, err)
			}

			actual := ring.Capacity()
			if actual != expectedCap {
				t.Errorf("Ring.Capacity() = %d, want %d", actual, expectedCap)
			}
		})
	}
}

func TestRing_NewRingInvalidCapacity(t *testing.T) {
	testCases := []int{0, -1, -10, -100}

	for _, invalidCap := range testCases {
		t.Run(fmt.Sprintf("invalid_%d", invalidCap), func(t *testing.T) {
			ring, err := shm.NewRing(invalidCap)
			if err == nil {
				t.Errorf("NewRing(%d) should have returned an error", invalidCap)
			}
			if ring != nil {
				t.Errorf("NewRing(%d) should have returned nil ring on error", invalidCap)
			}
		})
	}
}

func TestRing_Availability(t *testing.T) {
	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	// After creating a new ring, no data has been written yet
	if availRead := ring.AvailableRead(); availRead != 0 {
		t.Errorf("AvailableRead() = %d, want 0 for new ring", availRead)
	}

	// All capacity should be available for writing
	expectedCap := ring.Capacity()
	if availWrite := ring.AvailableWrite(); availWrite != expectedCap {
		t.Errorf("AvailableWrite() = %d, want %d for new ring", availWrite, expectedCap)
	}

	// Test different ring sizes
	testCases := []int{16, 32, 128, 256, 1024}
	for _, capacity := range testCases {
		t.Run(fmt.Sprintf("capacity_%d", capacity), func(t *testing.T) {
			r, err := shm.NewRing(capacity)
			if err != nil {
				t.Fatalf("NewRing(%d) failed: %v", capacity, err)
			}

			if availRead := r.AvailableRead(); availRead != 0 {
				t.Errorf("AvailableRead() = %d, want 0 for new ring", availRead)
			}

			actualCap := r.Capacity()
			if availWrite := r.AvailableWrite(); availWrite != actualCap {
				t.Errorf("AvailableWrite() = %d, want %d for new ring", availWrite, actualCap)
			}
		})
	}
}

func TestRing_WriteRead_Simple(t *testing.T) {
	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	// Initially, should have no data available to read
	if availRead := ring.AvailableRead(); availRead != 0 {
		t.Errorf("AvailableRead() = %d, want 0 before write", availRead)
	}

	// Write "hello"
	data := []byte("hello")
	n, err := ring.Write(data)
	if err != nil {
		t.Fatalf("Write(%q) failed: %v", data, err)
	}
	if n != len(data) {
		t.Errorf("Write(%q) = %d, want %d bytes written", data, n, len(data))
	}

	// After write, should have data available to read
	expectedAvail := len(data)
	if availRead := ring.AvailableRead(); availRead != expectedAvail {
		t.Errorf("AvailableRead() = %d, want %d after write", availRead, expectedAvail)
	}

	// Available write space should be reduced
	expectedWrite := ring.Capacity() - len(data)
	if availWrite := ring.AvailableWrite(); availWrite != expectedWrite {
		t.Errorf("AvailableWrite() = %d, want %d after write", availWrite, expectedWrite)
	}

	// Now read the data back
	readBuf := make([]byte, 5)
	nRead, err := ring.Read(readBuf)
	if err != nil {
		t.Fatalf("Read() failed: %v", err)
	}
	if nRead != len(data) {
		t.Errorf("Read() = %d bytes, want %d", nRead, len(data))
	}
	if !bytes.Equal(readBuf, data) {
		t.Errorf("Read() = %q, want %q", readBuf, data)
	}

	// After reading, should have no data left
	if availRead := ring.AvailableRead(); availRead != 0 {
		t.Errorf("AvailableRead() = %d, want 0 after reading all data", availRead)
	}

	// Available write space should be back to full capacity
	if availWrite := ring.AvailableWrite(); availWrite != ring.Capacity() {
		t.Errorf("AvailableWrite() = %d, want %d after reading all data", availWrite, ring.Capacity())
	}
}

func TestRing_Write_Wrap(t *testing.T) {
	// Use a small ring to easily test wrap-around
	ring, err := shm.NewRing(16)
	if err != nil {
		t.Fatalf("NewRing(16) failed: %v", err)
	}

	// Write 10 bytes - should fit without wrapping
	data1 := []byte("1234567890") // 10 bytes
	n1, err := ring.Write(data1)
	if err != nil {
		t.Fatalf("First Write(%q) failed: %v", data1, err)
	}
	if n1 != len(data1) {
		t.Errorf("First Write(%q) = %d, want %d bytes written", data1, n1, len(data1))
	}

	// Available read should be 10
	if availRead := ring.AvailableRead(); availRead != 10 {
		t.Errorf("AvailableRead() = %d, want 10 after first write", availRead)
	}

	// Available write should be 6 (16 - 10)
	if availWrite := ring.AvailableWrite(); availWrite != 6 {
		t.Errorf("AvailableWrite() = %d, want 6 after first write", availWrite)
	}

	// NOTE: This test will be completed when Read is implemented
	// TODO: Read 5 bytes, then write 10 bytes to test wrap-around
	// For now, test writing up to the limit
	
	// Try to write 6 more bytes - should fit exactly
	data2 := []byte("abcdef") // 6 bytes
	n2, err := ring.Write(data2)
	if err != nil {
		t.Fatalf("Second Write(%q) failed: %v", data2, err)
	}
	if n2 != len(data2) {
		t.Errorf("Second Write(%q) = %d, want %d bytes written", data2, n2, len(data2))
	}

	// Ring should now be full
	if availRead := ring.AvailableRead(); availRead != 16 {
		t.Errorf("AvailableRead() = %d, want 16 after second write (full)", availRead)
	}
	if availWrite := ring.AvailableWrite(); availWrite != 0 {
		t.Errorf("AvailableWrite() = %d, want 0 after second write (full)", availWrite)
	}

	// Try to write when full - should return 0 bytes written
	data3 := []byte("x")
	n3, err := ring.Write(data3)
	if err != nil {
		t.Fatalf("Write to full ring failed: %v", err)
	}
	if n3 != 0 {
		t.Errorf("Write to full ring = %d, want 0 bytes written", n3)
	}
}

func TestRing_Write_Closed(t *testing.T) {
	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	// Write should work before closing
	data := []byte("test")
	n, err := ring.Write(data)
	if err != nil {
		t.Fatalf("Write before close failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("Write before close = %d, want %d bytes written", n, len(data))
	}

	// Close the ring
	ring.Close()

	// Write should return ErrClosed after closing
	data2 := []byte("should fail")
	n2, err2 := ring.Write(data2)
	if err2 != shm.ErrClosed {
		t.Errorf("Write after close error = %v, want ErrClosed", err2)
	}
	if n2 != 0 {
		t.Errorf("Write after close = %d, want 0 bytes written", n2)
	}
}

func TestRing_Read_Empty(t *testing.T) {
	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	// Read from empty ring should return (0, nil)
	buf := make([]byte, 10)
	n, err := ring.Read(buf)
	if err != nil {
		t.Errorf("Read from empty ring failed: %v", err)
	}
	if n != 0 {
		t.Errorf("Read from empty ring = %d, want 0 bytes", n)
	}

	// Test reading from closed empty ring
	ring.Close()
	n2, err2 := ring.Read(buf)
	if err2 != shm.ErrClosed {
		t.Errorf("Read from closed empty ring error = %v, want ErrClosed", err2)
	}
	if n2 != 0 {
		t.Errorf("Read from closed empty ring = %d, want 0 bytes", n2)
	}
}

func TestRing_WriteRead_Wrap(t *testing.T) {
	// Use a small ring to test wrap-around during read
	ring, err := shm.NewRing(16)
	if err != nil {
		t.Fatalf("NewRing(16) failed: %v", err)
	}

	// Write data that will wrap around when we read it
	// First, fill most of the buffer
	data1 := []byte("123456789012") // 12 bytes
	n1, err := ring.Write(data1)
	if err != nil {
		t.Fatalf("First write failed: %v", err)
	}
	if n1 != len(data1) {
		t.Errorf("First write = %d, want %d bytes", n1, len(data1))
	}

	// Read part of it to advance the read pointer
	readBuf1 := make([]byte, 8)
	nRead1, err := ring.Read(readBuf1)
	if err != nil {
		t.Fatalf("First read failed: %v", err)
	}
	if nRead1 != 8 {
		t.Errorf("First read = %d, want 8 bytes", nRead1)
	}
	if !bytes.Equal(readBuf1, []byte("12345678")) {
		t.Errorf("First read = %q, want %q", readBuf1, "12345678")
	}

	// Now write more data that will wrap around in the buffer
	data2 := []byte("abcdefghijk") // 11 bytes
	n2, err := ring.Write(data2)
	if err != nil {
		t.Fatalf("Second write failed: %v", err)
	}
	if n2 != len(data2) {
		t.Errorf("Second write = %d, want %d bytes", n2, len(data2))
	}

	// Now read all remaining data - this should exercise wrap-around in read
	remainingData := []byte("9012") // 4 bytes remaining from first write
	remainingData = append(remainingData, data2...) // plus 11 bytes from second write
	
	readBuf2 := make([]byte, len(remainingData))
	nRead2, err := ring.Read(readBuf2)
	if err != nil {
		t.Fatalf("Second read failed: %v", err)
	}
	if nRead2 != len(remainingData) {
		t.Errorf("Second read = %d, want %d bytes", nRead2, len(remainingData))
	}
	if !bytes.Equal(readBuf2, remainingData) {
		t.Errorf("Second read = %q, want %q", readBuf2, remainingData)
	}

	// Ring should now be empty
	if availRead := ring.AvailableRead(); availRead != 0 {
		t.Errorf("AvailableRead() = %d, want 0 after reading all data", availRead)
	}
	if availWrite := ring.AvailableWrite(); availWrite != ring.Capacity() {
		t.Errorf("AvailableWrite() = %d, want %d after reading all data", availWrite, ring.Capacity())
	}
}

func TestRing_Read_PartialRead(t *testing.T) {
	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	// Write some data
	data := []byte("hello")
	n, err := ring.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("Write = %d, want %d bytes", n, len(data))
	}

	// Try to read more than available - should only read what's available
	largeBuf := make([]byte, 20)
	nRead, err := ring.Read(largeBuf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if nRead != len(data) {
		t.Errorf("Read = %d, want %d bytes (partial read)", nRead, len(data))
	}
	if !bytes.Equal(largeBuf[:nRead], data) {
		t.Errorf("Read data = %q, want %q", largeBuf[:nRead], data)
	}

	// Try to read with a smaller buffer
	ring.Write([]byte("world123"))
	smallBuf := make([]byte, 3)
	nRead2, err := ring.Read(smallBuf)
	if err != nil {
		t.Fatalf("Read with small buffer failed: %v", err)
	}
	if nRead2 != 3 {
		t.Errorf("Read with small buffer = %d, want 3 bytes", nRead2)
	}
	if !bytes.Equal(smallBuf, []byte("wor")) {
		t.Errorf("Read with small buffer = %q, want %q", smallBuf, "wor")
	}
}

func TestRing_Close_WriteFails(t *testing.T) {
	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	// Ring should not be closed initially
	if ring.Closed() {
		t.Error("New ring should not be closed")
	}

	// Close the ring first
	ring.Close()

	// Ring should now be closed
	if !ring.Closed() {
		t.Error("Ring should be closed after Close()")
	}

	// Write should return ErrClosed after closing
	data := []byte("should fail")
	n, err := ring.Write(data)
	if err != shm.ErrClosed {
		t.Errorf("Write after close error = %v, want ErrClosed", err)
	}
	if n != 0 {
		t.Errorf("Write after close = %d, want 0 bytes written", n)
	}
}

func TestRing_Close_ReadDrainsThenErrClosed(t *testing.T) {
	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	// Write some data first
	testData := []byte("hello world")
	n, err := ring.Write(testData)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(testData) {
		t.Fatalf("Write = %d, want %d bytes written", n, len(testData))
	}

	// Close the ring
	ring.Close()

	// First read should drain the existing data successfully
	buf := make([]byte, len(testData))
	n, err = ring.Read(buf)
	if err != nil {
		t.Errorf("Read from closed ring with data failed: %v", err)
	}
	if n != len(testData) {
		t.Errorf("Read from closed ring = %d, want %d bytes", n, len(testData))
	}
	if !bytes.Equal(buf, testData) {
		t.Errorf("Read from closed ring = %q, want %q", buf, testData)
	}

	// Second read should return ErrClosed since ring is now empty and closed
	buf2 := make([]byte, 10)
	n2, err2 := ring.Read(buf2)
	if err2 != shm.ErrClosed {
		t.Errorf("Read from empty closed ring error = %v, want ErrClosed", err2)
	}
	if n2 != 0 {
		t.Errorf("Read from empty closed ring = %d, want 0 bytes", n2)
	}
}
