package main

import (
	"fmt"
	"log"

	"google.golang.org/grpc/internal/transport/shm"
)

func main() {
	// Create a segment just like in the tests
	seg, err := shm.CreateSegment("debug-capacity", 65536, 65536)
	if err != nil {
		log.Fatalf("Failed to create segment: %v", err)
	}
	defer seg.Close()

	// Create a ring and check its capacity
	ring := shm.NewShmRingFromSegment(seg.A, seg.Mem)
	
	fmt.Printf("Configured capacity: 65536 bytes\n")
	fmt.Printf("Actual ring capacity: %d bytes\n", ring.Capacity())
	
	// Try to determine the actual usable space
	testSizes := []int{10, 20, 30, 40, 50, 100, 200, 500, 1000}
	
	for _, size := range testSizes {
		data := make([]byte, size)
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
}