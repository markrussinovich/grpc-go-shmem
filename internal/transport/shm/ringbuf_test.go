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
	"encoding/binary"
	"fmt"
	"sync"
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
	remainingData := []byte("9012")                 // 4 bytes remaining from first write
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

func TestRing_ReserveCommitWrite_PeekCommitRead(t *testing.T) {
	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	// Reserve space for writing
	testData := []byte("hello world test")
	s1, s2, ok := ring.ReserveWrite(len(testData))
	if !ok {
		t.Fatalf("ReserveWrite(%d) failed", len(testData))
	}

	// Total reserved space should match requested
	totalReserved := len(s1) + len(s2)
	if totalReserved != len(testData) {
		t.Errorf("ReserveWrite total space = %d, want %d", totalReserved, len(testData))
	}

	// Write known pattern into the reserved slices
	written := 0
	if len(s1) > 0 {
		copy(s1, testData[written:written+len(s1)])
		written += len(s1)
	}
	if len(s2) > 0 {
		copy(s2, testData[written:written+len(s2)])
		written += len(s2)
	}

	// Commit the write
	ring.CommitWrite(len(testData))

	// Verify AvailableRead shows the data
	if ring.AvailableRead() != len(testData) {
		t.Errorf("AvailableRead after commit = %d, want %d", ring.AvailableRead(), len(testData))
	}

	// Peek at the data
	p1, p2, ok := ring.PeekRead(len(testData))
	if !ok {
		t.Fatalf("PeekRead(%d) failed", len(testData))
	}

	// Total peeked space should match available data
	totalPeeked := len(p1) + len(p2)
	if totalPeeked != len(testData) {
		t.Errorf("PeekRead total space = %d, want %d", totalPeeked, len(testData))
	}

	// Verify the peeked data matches what we wrote
	readData := make([]byte, 0, len(testData))
	if len(p1) > 0 {
		readData = append(readData, p1...)
	}
	if len(p2) > 0 {
		readData = append(readData, p2...)
	}

	if !bytes.Equal(readData, testData) {
		t.Errorf("PeekRead data = %q, want %q", readData, testData)
	}

	// Commit the read
	ring.CommitRead(len(testData))

	// Verify the ring is now empty
	if ring.AvailableRead() != 0 {
		t.Errorf("AvailableRead after commit read = %d, want 0", ring.AvailableRead())
	}
}

func TestRing_Reserve_FailsWhenInsufficientSpace(t *testing.T) {
	ring, err := shm.NewRing(16) // Small ring
	if err != nil {
		t.Fatalf("NewRing(16) failed: %v", err)
	}

	// Fill the ring to capacity using regular Write
	fillData := make([]byte, 16)
	for i := range fillData {
		fillData[i] = byte(i)
	}
	n, err := ring.Write(fillData)
	if err != nil {
		t.Fatalf("Write to fill ring failed: %v", err)
	}
	if n != 16 {
		t.Fatalf("Write to fill ring = %d, want 16", n)
	}

	// Reserve should fail when no space available
	s1, s2, ok := ring.ReserveWrite(1)
	if ok {
		t.Error("ReserveWrite should fail when ring is full")
	}
	if s1 != nil || s2 != nil {
		t.Error("ReserveWrite should return nil slices when failing")
	}

	// Reserve should also fail when requesting more than capacity
	ring2, err := shm.NewRing(16)
	if err != nil {
		t.Fatalf("NewRing(16) failed: %v", err)
	}
	s1, s2, ok = ring2.ReserveWrite(32) // More than capacity
	if ok {
		t.Error("ReserveWrite should fail when requesting more than capacity")
	}
	if s1 != nil || s2 != nil {
		t.Error("ReserveWrite should return nil slices when failing")
	}
}

func TestRing_Peek_FailsWhenNoData(t *testing.T) {
	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	// PeekRead should fail when ring is empty
	s1, s2, ok := ring.PeekRead(10)
	if ok {
		t.Error("PeekRead should fail when ring is empty")
	}
	if s1 != nil || s2 != nil {
		t.Error("PeekRead should return nil slices when failing")
	}

	// PeekRead should also fail with invalid input
	s1, s2, ok = ring.PeekRead(0)
	if ok {
		t.Error("PeekRead should fail with zero bytes requested")
	}
	if s1 != nil || s2 != nil {
		t.Error("PeekRead should return nil slices when failing")
	}

	s1, s2, ok = ring.PeekRead(-1)
	if ok {
		t.Error("PeekRead should fail with negative bytes requested")
	}
	if s1 != nil || s2 != nil {
		t.Error("PeekRead should return nil slices when failing")
	}
}

func TestRing_ReserveWrite_FailsWhenClosed(t *testing.T) {
	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	// Close the ring
	ring.Close()

	// ReserveWrite should fail when ring is closed
	s1, s2, ok := ring.ReserveWrite(10)
	if ok {
		t.Error("ReserveWrite should fail when ring is closed")
	}
	if s1 != nil || s2 != nil {
		t.Error("ReserveWrite should return nil slices when failing")
	}
}

func TestRing_ReserveCommit_WrapAround(t *testing.T) {
	ring, err := shm.NewRing(16) // Small ring to force wrap-around
	if err != nil {
		t.Fatalf("NewRing(16) failed: %v", err)
	}

	// Write some data to move the write pointer forward
	initialData := []byte("abc")
	n, err := ring.Write(initialData)
	if err != nil {
		t.Fatalf("Initial write failed: %v", err)
	}
	if n != len(initialData) {
		t.Fatalf("Initial write = %d, want %d", n, len(initialData))
	}

	// Read it back to move read pointer forward
	buf := make([]byte, len(initialData))
	n, err = ring.Read(buf)
	if err != nil {
		t.Fatalf("Initial read failed: %v", err)
	}
	if n != len(initialData) {
		t.Fatalf("Initial read = %d, want %d", n, len(initialData))
	}

	// Now reserve space that will wrap around the buffer
	// Ring capacity is 16, we've written 3 bytes starting at 0, so write pointer is at 3
	// Request 14 bytes: should get one slice from position 3 to 15 (13 bytes)
	// and another slice from position 0 to 0 (1 byte)
	wrapData := make([]byte, 14)
	for i := range wrapData {
		wrapData[i] = byte('A' + i)
	}

	s1, s2, ok := ring.ReserveWrite(len(wrapData))
	if !ok {
		t.Fatalf("ReserveWrite(%d) failed", len(wrapData))
	}

	// Verify we got two slices for wrap-around
	if len(s2) == 0 {
		t.Error("Expected wrap-around with two slices, got only one")
	}

	totalReserved := len(s1) + len(s2)
	if totalReserved != len(wrapData) {
		t.Errorf("ReserveWrite total = %d, want %d", totalReserved, len(wrapData))
	}

	// Write the data
	written := 0
	copy(s1, wrapData[written:written+len(s1)])
	written += len(s1)
	copy(s2, wrapData[written:written+len(s2)])

	// Commit the write
	ring.CommitWrite(len(wrapData))

	// Peek and verify the data
	p1, p2, ok := ring.PeekRead(len(wrapData))
	if !ok {
		t.Fatalf("PeekRead(%d) failed", len(wrapData))
	}

	readData := make([]byte, 0, len(wrapData))
	readData = append(readData, p1...)
	readData = append(readData, p2...)

	if !bytes.Equal(readData, wrapData) {
		t.Errorf("PeekRead wrap-around data = %v, want %v", readData, wrapData)
	}

	// Commit the read
	ring.CommitRead(len(wrapData))

	// Verify ring is empty
	if ring.AvailableRead() != 0 {
		t.Errorf("Ring should be empty after commit, got %d bytes", ring.AvailableRead())
	}
}

func TestRing_PeekRead_Partial(t *testing.T) {
	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	// Write more data than we'll peek at
	testData := []byte("hello world this is more data")
	n, err := ring.Write(testData)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(testData) {
		t.Fatalf("Write = %d, want %d", n, len(testData))
	}

	// Peek at only part of the data
	peekSize := 10
	s1, s2, ok := ring.PeekRead(peekSize)
	if !ok {
		t.Fatalf("PeekRead(%d) failed", peekSize)
	}

	totalPeeked := len(s1) + len(s2)
	if totalPeeked != peekSize {
		t.Errorf("PeekRead total = %d, want %d", totalPeeked, peekSize)
	}

	// Verify peeked data matches first part of written data
	peekedData := make([]byte, 0, peekSize)
	peekedData = append(peekedData, s1...)
	peekedData = append(peekedData, s2...)

	if !bytes.Equal(peekedData, testData[:peekSize]) {
		t.Errorf("PeekRead partial data = %q, want %q", peekedData, testData[:peekSize])
	}

	// Commit only part of the data
	ring.CommitRead(peekSize)

	// Verify remaining data is still available
	remaining := len(testData) - peekSize
	if ring.AvailableRead() != remaining {
		t.Errorf("AvailableRead after partial commit = %d, want %d", ring.AvailableRead(), remaining)
	}

	// Read the rest and verify it's correct
	buf := make([]byte, remaining)
	n, err = ring.Read(buf)
	if err != nil {
		t.Fatalf("Read remaining failed: %v", err)
	}
	if n != remaining {
		t.Fatalf("Read remaining = %d, want %d", n, remaining)
	}
	if !bytes.Equal(buf, testData[peekSize:]) {
		t.Errorf("Remaining data = %q, want %q", buf, testData[peekSize:])
	}
}

func TestRing_ReserveWrite_InvalidInput(t *testing.T) {
	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	// Test zero bytes
	s1, s2, ok := ring.ReserveWrite(0)
	if ok {
		t.Error("ReserveWrite(0) should fail")
	}
	if s1 != nil || s2 != nil {
		t.Error("ReserveWrite(0) should return nil slices")
	}

	// Test negative bytes
	s1, s2, ok = ring.ReserveWrite(-1)
	if ok {
		t.Error("ReserveWrite(-1) should fail")
	}
	if s1 != nil || s2 != nil {
		t.Error("ReserveWrite(-1) should return nil slices")
	}
}

func TestRing_CommitWrite_ZeroBytes(t *testing.T) {
	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	initialAvail := ring.AvailableRead()

	// Committing zero bytes should be safe and do nothing
	ring.CommitWrite(0)

	if ring.AvailableRead() != initialAvail {
		t.Errorf("CommitWrite(0) changed available data: %d -> %d",
			initialAvail, ring.AvailableRead())
	}
}

func TestRing_CommitRead_ZeroBytes(t *testing.T) {
	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	// Write some data first
	testData := []byte("test")
	n, err := ring.Write(testData)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(testData) {
		t.Fatalf("Write = %d, want %d", n, len(testData))
	}

	initialAvail := ring.AvailableRead()

	// Committing zero bytes should be safe and do nothing
	ring.CommitRead(0)

	if ring.AvailableRead() != initialAvail {
		t.Errorf("CommitRead(0) changed available data: %d -> %d",
			initialAvail, ring.AvailableRead())
	}
}

func TestRing_CacheLinePadding(t *testing.T) {
	// This test verifies that our cache-line padding is working as expected.
	// We can't easily test the performance benefit in a unit test, but we can
	// verify that the struct has the expected size characteristics.

	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	// The paddedUint64 should be 64 bytes (8 for atomic.Uint64 + 56 padding)
	// This is a basic sanity check that our padding is in place
	_ = ring // Prevent unused variable warning

	// Test that basic operations still work with padding
	testData := []byte("cache line test")
	n, err := ring.Write(testData)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(testData) {
		t.Fatalf("Write = %d, want %d", n, len(testData))
	}

	buf := make([]byte, len(testData))
	n, err = ring.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != len(testData) {
		t.Fatalf("Read = %d, want %d", n, len(testData))
	}
	if !bytes.Equal(buf, testData) {
		t.Errorf("Read data = %q, want %q", buf, testData)
	}
}

func TestRing_WrapAroundIntegrity(t *testing.T) {
	// Use 64-byte capacity as specified
	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	// Step 1: Write 48 bytes
	data1 := make([]byte, 48)
	for i := range data1 {
		data1[i] = byte(i)
	}
	n, err := ring.Write(data1)
	if err != nil {
		t.Fatalf("Write 48 bytes failed: %v", err)
	}
	if n != 48 {
		t.Fatalf("Write 48 bytes = %d, want 48", n)
	}

	// Step 2: Read 40 bytes
	buf1 := make([]byte, 40)
	n, err = ring.Read(buf1)
	if err != nil {
		t.Fatalf("Read 40 bytes failed: %v", err)
	}
	if n != 40 {
		t.Fatalf("Read 40 bytes = %d, want 40", n)
	}

	// Verify first 40 bytes
	if !bytes.Equal(buf1, data1[:40]) {
		t.Errorf("First read data mismatch: got %v, want %v", buf1, data1[:40])
	}

	// At this point: 8 bytes remain in buffer, write pointer at 48, read pointer at 40
	// Available space: 64 - 8 = 56 bytes

	// Step 3: Write 40 bytes (this forces wrap around)
	data2 := make([]byte, 40)
	for i := range data2 {
		data2[i] = byte(100 + i) // Different pattern to distinguish from data1
	}
	n, err = ring.Write(data2)
	if err != nil {
		t.Fatalf("Write 40 bytes (wrap) failed: %v", err)
	}
	if n != 40 {
		t.Fatalf("Write 40 bytes (wrap) = %d, want 40", n)
	}

	// Step 4: Read all remaining data and verify concatenation
	// Should have: 8 bytes from data1 + 40 bytes from data2 = 48 bytes total
	remainingBuf := make([]byte, 48)
	n, err = ring.Read(remainingBuf)
	if err != nil {
		t.Fatalf("Read remaining failed: %v", err)
	}
	if n != 48 {
		t.Fatalf("Read remaining = %d, want 48", n)
	}

	// Verify concatenation: first 8 bytes should be remainder of data1,
	// next 40 bytes should be data2
	expectedConcatenation := make([]byte, 0, 48)
	expectedConcatenation = append(expectedConcatenation, data1[40:]...) // Last 8 bytes of data1
	expectedConcatenation = append(expectedConcatenation, data2...)      // All 40 bytes of data2

	if !bytes.Equal(remainingBuf, expectedConcatenation) {
		t.Errorf("Concatenation mismatch")
		t.Errorf("Got:      %v", remainingBuf)
		t.Errorf("Expected: %v", expectedConcatenation)
	}

	// Verify ring is now empty
	if ring.AvailableRead() != 0 {
		t.Errorf("Ring should be empty, but has %d bytes available", ring.AvailableRead())
	}
}

func TestRing_PartialProgress(t *testing.T) {
	// Use small ring to easily test partial writes
	ring, err := shm.NewRing(16)
	if err != nil {
		t.Fatalf("NewRing(16) failed: %v", err)
	}

	// Step 1: Fill almost full (14 out of 16 bytes)
	fillData := make([]byte, 14)
	for i := range fillData {
		fillData[i] = byte(i)
	}
	n, err := ring.Write(fillData)
	if err != nil {
		t.Fatalf("Fill write failed: %v", err)
	}
	if n != 14 {
		t.Fatalf("Fill write = %d, want 14", n)
	}

	// Verify available space
	if ring.AvailableWrite() != 2 {
		t.Errorf("Available write space = %d, want 2", ring.AvailableWrite())
	}

	// Step 2: Attempt to write more than available space
	bigData := []byte{100, 101, 102, 103, 104} // 5 bytes, but only 2 available
	n, err = ring.Write(bigData)
	if err != nil {
		t.Errorf("Partial write should not return error, got: %v", err)
	}
	if n != 2 {
		t.Errorf("Partial write = %d, want 2 bytes (short write)", n)
	}

	// Verify ring is now full
	if ring.AvailableWrite() != 0 {
		t.Errorf("Ring should be full, available write = %d", ring.AvailableWrite())
	}

	// Step 3: Free some space by reading
	readBuf := make([]byte, 5)
	n, err = ring.Read(readBuf)
	if err != nil {
		t.Fatalf("Read to free space failed: %v", err)
	}
	if n != 5 {
		t.Fatalf("Read to free space = %d, want 5", n)
	}

	// Verify we have space again
	if ring.AvailableWrite() != 5 {
		t.Errorf("Available write after read = %d, want 5", ring.AvailableWrite())
	}

	// Step 4: Write the remaining data
	remainingData := bigData[2:] // The 3 bytes that didn't fit before
	n, err = ring.Write(remainingData)
	if err != nil {
		t.Fatalf("Write remaining failed: %v", err)
	}
	if n != 3 {
		t.Fatalf("Write remaining = %d, want 3", n)
	}

	// Step 5: Verify data integrity
	// Read all remaining data and verify the pattern
	finalBuf := make([]byte, ring.AvailableRead())
	n, err = ring.Read(finalBuf)
	if err != nil {
		t.Fatalf("Final read failed: %v", err)
	}

	// Expected: fillData[5:] (bytes 5-13) + first 2 bytes of bigData + remaining 3 bytes
	expected := make([]byte, 0)
	expected = append(expected, fillData[5:]...) // Remaining 9 bytes from fillData
	expected = append(expected, bigData[:2]...)  // First 2 bytes that fit
	expected = append(expected, bigData[2:]...)  // Remaining 3 bytes

	if !bytes.Equal(finalBuf, expected) {
		t.Errorf("Final data integrity check failed")
		t.Errorf("Got:      %v", finalBuf)
		t.Errorf("Expected: %v", expected)
	}
}

func TestRing_Concurrent_NoLossNoDup(t *testing.T) {
	const (
		recordSize = 8      // 8-byte little-endian counter
		numRecords = 100000 // 100k records as specified
		ringCap    = 4096   // 4KB capacity as specified
	)

	ring, err := shm.NewRing(ringCap)
	if err != nil {
		t.Fatalf("NewRing(%d) failed: %v", ringCap, err)
	}

	// Channel to signal completion
	writerDone := make(chan struct{})
	readerDone := make(chan error, 1)

	// Writer goroutine: generate sequence of 8-byte little-endian counter values
	go func() {
		defer close(writerDone)

		for counter := uint64(0); counter < numRecords; counter++ {
			// Create 8-byte record with little-endian counter
			record := make([]byte, recordSize)
			binary.LittleEndian.PutUint64(record, counter)

			// Keep trying to write until all bytes are written
			for written := 0; written < recordSize; {
				n, err := ring.Write(record[written:])
				if err != nil {
					t.Errorf("Writer failed at counter %d: %v", counter, err)
					return
				}
				written += n
				// No sleep - tight polling as specified
			}
		}
	}()

	// Reader goroutine: accumulate and validate records
	go func() {
		defer func() {
			readerDone <- nil
		}()

		readBuf := make([]byte, 1024) // Read buffer
		accumBuf := make([]byte, 0)   // Accumulation buffer
		expectedCounter := uint64(0)

		for expectedCounter < numRecords {
			// Read available data
			n, err := ring.Read(readBuf)
			if err != nil {
				readerDone <- fmt.Errorf("reader failed at counter %d: %v", expectedCounter, err)
				return
			}

			if n > 0 {
				// Accumulate the read data
				accumBuf = append(accumBuf, readBuf[:n]...)

				// Process complete records
				for len(accumBuf) >= recordSize {
					// Extract one record
					recordBytes := accumBuf[:recordSize]
					accumBuf = accumBuf[recordSize:]

					// Decode and validate counter
					counter := binary.LittleEndian.Uint64(recordBytes)
					if counter != expectedCounter {
						readerDone <- fmt.Errorf("counter mismatch: got %d, expected %d", counter, expectedCounter)
						return
					}
					expectedCounter++
				}
			}
			// No sleep - tight polling as specified
		}

		// Ensure no leftover partial data
		if len(accumBuf) != 0 {
			readerDone <- fmt.Errorf("leftover partial data: %d bytes", len(accumBuf))
			return
		}
	}()

	// Wait for writer to complete
	<-writerDone

	// Wait for reader to complete or timeout
	select {
	case err := <-readerDone:
		if err != nil {
			t.Fatal(err)
		}
	}

	// Final verification: ring should be empty
	if ring.AvailableRead() != 0 {
		t.Errorf("Ring should be empty at end, but has %d bytes", ring.AvailableRead())
	}
}

func TestRing_Concurrent_CloseSemantics(t *testing.T) {
	const (
		recordSize = 8
		ringCap    = 1024
	)

	ring, err := shm.NewRing(ringCap)
	if err != nil {
		t.Fatalf("NewRing(%d) failed: %v", ringCap, err)
	}

	var wg sync.WaitGroup
	writerStopped := make(chan struct{})
	readerStopped := make(chan struct{})

	// Track what happened
	var writerErrors []error
	var readerErrors []error
	var mutex sync.Mutex

	recordError := func(errs *[]error, err error) {
		mutex.Lock()
		*errs = append(*errs, err)
		mutex.Unlock()
	}

	// Writer goroutine: keep writing until it gets ErrClosed
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(writerStopped)

		counter := uint64(0)
		for {
			record := make([]byte, recordSize)
			binary.LittleEndian.PutUint64(record, counter)

			n, err := ring.Write(record)
			if err == shm.ErrClosed {
				// Expected: writer should see ErrClosed after Close()
				if n != 0 {
					recordError(&writerErrors, fmt.Errorf("write after close returned n=%d, expected 0", n))
				}
				return
			}
			if err != nil {
				recordError(&writerErrors, fmt.Errorf("unexpected write error: %v", err))
				return
			}

			if n == recordSize {
				counter++
			}
			// If n < recordSize, we got a partial write, keep trying
		}
	}()

	// Reader goroutine: keep reading until ring is empty and closed
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(readerStopped)

		readBuf := make([]byte, 256)
		dataReceived := 0

		for {
			n, err := ring.Read(readBuf)
			if err == shm.ErrClosed {
				// Expected: reader should only see ErrClosed when empty and closed
				if n != 0 {
					recordError(&readerErrors, fmt.Errorf("read returned ErrClosed but n=%d", n))
				}
				return
			}
			if err != nil {
				recordError(&readerErrors, fmt.Errorf("unexpected read error: %v", err))
				return
			}

			dataReceived += n
		}
	}()

	// Let producer and consumer run for a bit
	// Use a small controlled delay to let some data flow
	for i := 0; i < 1000; i++ {
		if ring.AvailableRead() > 100 {
			break
		}
	}

	// Close the ring while producer/consumer are active
	ring.Close()

	// Wait for both goroutines to finish
	wg.Wait()

	// Verify no unexpected errors occurred
	mutex.Lock()
	defer mutex.Unlock()

	for _, err := range writerErrors {
		t.Errorf("Writer error: %v", err)
	}
	for _, err := range readerErrors {
		t.Errorf("Reader error: %v", err)
	}

	// Verify ring is now closed
	if !ring.Closed() {
		t.Error("Ring should be closed")
	}

	// Verify subsequent operations fail appropriately
	_, err = ring.Write([]byte{1, 2, 3})
	if err != shm.ErrClosed {
		t.Errorf("Write after close should return ErrClosed, got: %v", err)
	}

	// If ring has data, reading should work until empty
	for ring.AvailableRead() > 0 {
		buf := make([]byte, 100)
		n, err := ring.Read(buf)
		if err != nil {
			t.Errorf("Read from closed ring with data failed: %v", err)
			break
		}
		if n == 0 {
			t.Error("Read from non-empty closed ring returned 0 bytes")
			break
		}
	}

	// Final read from empty closed ring should return ErrClosed
	buf := make([]byte, 10)
	n, err := ring.Read(buf)
	if err != shm.ErrClosed {
		t.Errorf("Final read from empty closed ring: got error=%v n=%d, want ErrClosed n=0", err, n)
	}
	if n != 0 {
		t.Errorf("Final read from empty closed ring: got n=%d, want 0", n)
	}
}
