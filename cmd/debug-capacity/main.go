package main

import (
	"fmt"
	"log"
	"os"

	"google.golang.org/grpc/internal/transport/shm"
)

func main() {
	// Only run on Linux
	if os.Getenv("GOOS") == "windows" || os.Getenv("GOOS") == "darwin" {
		fmt.Println("Skipping on non-Linux platform")
		return
	}

	// Create a segment just like in the tests
	seg, err := shm.CreateSegment("debug-capacity-v3", 65536, 65536)
	if err != nil {
		log.Fatalf("Failed to create segment: %v", err)
	}
	defer seg.Close()

	// Create a ring and check its capacity
	ring := shm.NewShmRingFromSegment(seg.A, seg.Mem)

	fmt.Printf("=== Ring Capacity Analysis ===\n")
	fmt.Printf("Configured capacity: 65536 bytes\n")
	fmt.Printf("Ring A capacity: %d bytes\n", seg.A.Capacity())
	fmt.Printf("Ring B capacity: %d bytes\n", seg.B.Capacity())

	// Show segment details
	fmt.Printf("\n=== Segment Details ===\n")
	fmt.Printf("Total memory size: %d bytes\n", len(seg.Mem))

	// Try to determine the actual usable space
	fmt.Printf("\n=== Single Write Tests ===\n")
	testSizes := []int{10, 20, 30, 40, 50, 100, 200, 500, 1000, 5000, 10000, 32768, 65000, 65536}

	for _, size := range testSizes {
		data := make([]byte, size)
		// Fill with pattern to ensure it's written
		for i := range data {
			data[i] = byte(i % 256)
		}

		err := ring.WriteBlocking(data)
		if err != nil {
			fmt.Printf("Size %d bytes: FAIL (%v)\n", size, err)
			break
		} else {
			fmt.Printf("Size %d bytes: OK\n", size)
			// Read it back to clear the ring
			readData := make([]byte, size)
			ring.ReadBlocking(readData)
		}
	}

	// Test filling the buffer gradually to see backpressure
	fmt.Printf("\n=== Backpressure Test ===\n")
	chunkSize := 1000
	totalWritten := 0

	for i := 0; i < 100; i++ { // Try up to 100KB
		data := make([]byte, chunkSize)
		for j := range data {
			data[j] = byte((i + j) % 256)
		}

		err := ring.WriteBlocking(data)
		if err != nil {
			fmt.Printf("Failed after %d bytes written (%d chunks): %v\n", totalWritten, i, err)
			break
		}
		totalWritten += chunkSize
		fmt.Printf("Written %d bytes (%d chunks)\n", totalWritten, i+1)

		// Don't read back - let buffer fill up
	}
}
