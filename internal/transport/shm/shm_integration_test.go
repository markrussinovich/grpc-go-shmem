/*
 *
 * Copyright 2025 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apach	// Test data sets - current ring implementation has small capacity limits
	// TODO: investigate why ring capacity is much smaller than expected
	testCases := []struct {
		name string
		data []byte
	}{
		{"small", []byte("hello world")},                       // 11 bytes - works
		{"medium", bytes.Repeat([]byte("x"), 20)},              // 20 bytes - works
		// Note: larger sizes fail with "data larger than ring capacity"
		// This suggests actual usable capacity is much smaller than configured
	}nses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
*/

package shm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// Check if this is a helper process
	if len(os.Args) >= 3 && os.Args[1] == "-test.run=HelperShmServer" {
		// This is the helper server process
		if len(os.Args) < 4 {
			fmt.Fprintf(os.Stderr, "Helper server missing segment name\n")
			os.Exit(1)
		}
		segmentName := os.Args[3]
		os.Exit(runHelperShmServer(segmentName))
	}

	if len(os.Args) >= 3 && os.Args[1] == "-test.run=HelperShmDelayedServer" {
		// This is the delayed server helper for backpressure testing
		if len(os.Args) < 4 {
			fmt.Fprintf(os.Stderr, "Helper delayed server missing segment name\n")
			os.Exit(1)
		}
		segmentName := os.Args[3]
		os.Exit(runHelperShmDelayedServer(segmentName))
	}

	// Normal test execution
	os.Exit(m.Run())
}

// runHelperShmServer implements the server helper process
func runHelperShmServer(segmentName string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Open the segment that the client created
	seg, err := OpenSegment(segmentName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open segment %s: %v\n", segmentName, err)
		return 1
	}
	defer seg.Close()

	// Wait for client to be ready
	if err := seg.WaitForClient(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to wait for client: %v\n", err)
		return 1
	}

	// Mark server as ready
	seg.H.SetServerReady(true)

	// Create server connection
	conn := NewServerConn(seg)
	defer conn.Close()

	// Echo loop: read bytes and write them back
	buffer := make([]byte, 8192)
	for {
		// Check if parent wants us to exit
		select {
		case <-ctx.Done():
			return 0
		default:
		}

		// Try to read from client with context timeout
		n, err := conn.ReadContext(ctx, buffer)
		if err != nil {
			if err == ErrConnectionClosed {
				// Client closed connection, exit gracefully
				return 0
			}
			if err == context.DeadlineExceeded {
				// Context timeout, exit gracefully
				return 0
			}
			fmt.Fprintf(os.Stderr, "Server read error: %v\n", err)
			return 1
		}

		if n == 0 {
			// No data available, continue
			continue
		}

		// Echo the data back to client
		_, err = conn.WriteContext(ctx, buffer[:n])
		if err != nil {
			if err == ErrConnectionClosed {
				return 0
			}
			if err == context.DeadlineExceeded {
				// Context timeout, exit gracefully
				return 0
			}
			fmt.Fprintf(os.Stderr, "Server write error: %v\n", err)
			return 1
		}
	}
}

// runHelperShmDelayedServer implements the delayed server helper for backpressure testing
func runHelperShmDelayedServer(segmentName string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Open the segment that the client created
	seg, err := OpenSegment(segmentName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open segment %s: %v\n", segmentName, err)
		return 1
	}
	defer seg.Close()

	// Wait for client to be ready
	if err := seg.WaitForClient(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to wait for client: %v\n", err)
		return 1
	}

	// Mark server as ready
	seg.H.SetServerReady(true)

	// Create server connection
	conn := NewServerConn(seg)
	defer conn.Close()

	// Wait for signal from parent to start reading
	buffer := make([]byte, 1)
	n, err := os.Stdin.Read(buffer)
	if err != nil || n == 0 {
		fmt.Fprintf(os.Stderr, "Failed to read start signal: %v\n", err)
		return 1
	}

	// Acknowledge that we're starting to read
	fmt.Print("reading_started\n")

	// Now read and echo data normally
	readBuffer := make([]byte, 8192)
	for {
		// Check if parent wants us to exit
		select {
		case <-ctx.Done():
			return 0
		default:
		}

		// Try to read from client with context timeout
		n, err := conn.ReadContext(ctx, readBuffer)
		if err != nil {
			if err == ErrConnectionClosed {
				// Client closed connection, exit gracefully
				return 0
			}
			if err == context.DeadlineExceeded {
				// Context timeout, exit gracefully
				return 0
			}
			fmt.Fprintf(os.Stderr, "Server read error: %v\n", err)
			return 1
		}

		if n == 0 {
			// No data available, continue
			continue
		}

		// Echo the data back to client
		_, err = conn.WriteContext(ctx, readBuffer[:n])
		if err != nil {
			if err == ErrConnectionClosed {
				return 0
			}
			if err == context.DeadlineExceeded {
				// Context timeout, exit gracefully
				return 0
			}
			fmt.Fprintf(os.Stderr, "Server write error: %v\n", err)
			return 1
		}
	}
}

func TestCrossProcessEcho(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only for now")
	}

	segmentName := fmt.Sprintf("test-echo-%d", os.Getpid())

	// Create segment with small capacities (64 KiB per ring)
	seg, err := CreateSegment(segmentName, 65536, 65536)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer seg.Close()

	// Mark client as ready
	seg.H.SetClientReady(true)

	// Spawn server helper process
	cmd := exec.Command(os.Args[0], "-test.run=HelperShmServer", "--", segmentName)
	cmd.Stderr = os.Stderr // Forward stderr for debugging
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start server helper: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	// Wait for server to be ready
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := seg.WaitForServer(ctx); err != nil {
		t.Fatalf("Failed to wait for server: %v", err)
	}

	// Create client connection
	conn := NewClientConn(seg)
	defer conn.Close()

	// Debug: check what the actual ring capacity is
	t.Logf("Testing with ring capacity: %d bytes", 65536)

	// Test data sets - now with larger payloads to test duplex behavior
	// These sizes would cause deadlock with sequential read/write
	testCases := []struct {
		name string
		data []byte
	}{
		{"small", []byte("hello world")},                     // 11 bytes
		{"medium", bytes.Repeat([]byte("x"), 1000)},          // 1KB 
		{"large", bytes.Repeat([]byte("test"), 5000)},        // 20KB
		{"xlarge", bytes.Repeat([]byte("data"), 10000)},      // 40KB - would deadlock without concurrent I/O
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Use concurrent read/write to avoid "dueling full buffers" deadlock
			// The server echoes immediately, so client must read concurrently
			wg := &sync.WaitGroup{}
			wg.Add(2)
			
			var writeErr, readErr error
			received := make([]byte, len(tc.data))

			// Writer goroutine
			go func() {
				defer wg.Done()
				written := 0
				for written < len(tc.data) {
					n, err := conn.WriteContext(ctx, tc.data[written:])
					if err != nil {
						writeErr = fmt.Errorf("write failed: %v", err)
						return
					}
					written += n
				}
				// Signal end of data to server (optional, server handles connection close)
			}()

			// Reader goroutine  
			go func() {
				defer wg.Done()
				read := 0
				for read < len(tc.data) {
					n, err := conn.ReadContext(ctx, received[read:])
					if n > 0 {
						read += n
					}
					if err == io.EOF {
						break
					}
					if err != nil {
						readErr = fmt.Errorf("read failed: %v", err)
						return
					}
				}
			}()

			// Wait for both operations to complete
			wg.Wait()
			
			// Check for errors
			if writeErr != nil {
				t.Fatalf("Write error: %v", writeErr)
			}
			if readErr != nil {
				t.Fatalf("Read error: %v", readErr)
			}

			// Verify data matches exactly
			if !bytes.Equal(tc.data, received) {
				t.Errorf("Data mismatch. Sent %d bytes, received %d bytes", len(tc.data), len(received))
				if len(tc.data) < 100 && len(received) < 100 {
					t.Errorf("Sent: %q", tc.data)
					t.Errorf("Received: %q", received)
				}
			}
		})
	}

	// Test blocking behavior: read before server writes
	t.Run("blocking_behavior", func(t *testing.T) {
		// Use a separate connection for this test
		testSeg, err := CreateSegment(segmentName+"-blocking", 4096, 4096)
		if err != nil {
			t.Fatalf("Failed to create test segment: %v", err)
		}
		defer testSeg.Close()

		testSeg.H.SetClientReady(true)

		// Start server helper for this test
		testCmd := exec.Command(os.Args[0], "-test.run=HelperShmServer", "--", segmentName+"-blocking")
		testCmd.Stderr = os.Stderr
		if err := testCmd.Start(); err != nil {
			t.Fatalf("Failed to start test server: %v", err)
		}
		defer func() {
			testCmd.Process.Kill()
			testCmd.Wait()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := testSeg.WaitForServer(ctx); err != nil {
			t.Fatalf("Failed to wait for test server: %v", err)
		}

		testConn := NewClientConn(testSeg)
		defer testConn.Close()

		// Test that read blocks until data is available
		readDone := make(chan struct{})
		var readErr error
		var readData []byte

		go func() {
			defer close(readDone)
			buf := make([]byte, 10)
			n, err := testConn.ReadContext(ctx, buf)
			readErr = err
			readData = buf[:n]
		}()

		// Give the read goroutine time to start and block
		time.Sleep(100 * time.Millisecond)

		// Verify read hasn't completed yet (should be blocking)
		select {
		case <-readDone:
			t.Error("Read should have blocked until data is available")
		default:
			// Good, read is blocking
		}

		// Now send data - this should unblock the read
		testMsg := []byte("unblock")
		_, err = testConn.WriteContext(ctx, testMsg)
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		// Wait for read to complete with timeout
		select {
		case <-readDone:
			if readErr != nil {
				t.Fatalf("Read failed: %v", readErr)
			}
			if !bytes.Equal(readData, testMsg) {
				t.Errorf("Read data mismatch: got %q, want %q", readData, testMsg)
			}
		case <-time.After(2 * time.Second):
			t.Error("Read did not complete after write - possible deadlock")
		}
	})
}

func TestCrossProcessBackpressure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only for now")
	}

	segmentName := fmt.Sprintf("test-backpressure-%d", os.Getpid())

	// Use small ring capacity (4096 bytes minimum) to test backpressure
	seg, err := CreateSegment(segmentName, 4096, 4096)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer seg.Close()

	seg.H.SetClientReady(true)

	// Start server helper but modify it to delay reads
	cmd := exec.Command(os.Args[0], "-test.run=HelperShmDelayedServer", "--", segmentName)
	cmd.Stderr = os.Stderr

	// Use stdin/stdout for synchronization instead of sleeps
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("Failed to create stdin pipe: %v", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("Failed to create stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start server helper: %v", err)
	}
	defer func() {
		stdinPipe.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := seg.WaitForServer(ctx); err != nil {
		t.Fatalf("Failed to wait for server: %v", err)
	}

	conn := NewClientConn(seg)
	defer conn.Close()

	// Try to write data that will test backpressure - use larger data than ring capacity
	// Now that chunking is implemented, we can write data larger than ring capacity
	ringCapacity := uint64(4096)
	dataSize := int(ringCapacity * 2) // 8192 bytes - will require chunking and should cause backpressure
	largeData := bytes.Repeat([]byte("x"), dataSize)

	// Track if write blocks (doesn't spin)
	writeStarted := make(chan struct{})
	writeCompleted := make(chan struct{})
	var writeErr error

	go func() {
		defer close(writeCompleted)
		close(writeStarted)

		// Write all data at once (connection will block until it can write all)
		n, err := conn.WriteContext(ctx, largeData)
		if err != nil {
			writeErr = err
			return
		}
		if n != len(largeData) {
			writeErr = fmt.Errorf("write returned %d, expected %d", n, len(largeData))
		}
	}()

	// Wait for write to start
	<-writeStarted

	// Give write time to fill the small buffer and block
	time.Sleep(200 * time.Millisecond)

	// Verify write is still in progress (blocked due to backpressure)
	select {
	case <-writeCompleted:
		t.Error("Write completed too quickly - should have blocked on small buffer")
	default:
		// Good, write is blocked due to backpressure
	}

	// Signal server to start reading (unblock the writer)
	fmt.Fprintln(stdinPipe, "start_reading")

	// Wait for server acknowledgment
	response := make([]byte, 100)
	n, err := stdoutPipe.Read(response)
	if err != nil {
		t.Fatalf("Failed to read server response: %v", err)
	}
	if !strings.Contains(string(response[:n]), "reading_started") {
		t.Errorf("Unexpected server response: %s", response[:n])
	}

	// Now write should complete quickly
	select {
	case <-writeCompleted:
		if writeErr != nil {
			t.Fatalf("Write failed: %v", writeErr)
		}
		// Good, write completed after server started reading
	case <-time.After(5 * time.Second):
		t.Error("Write did not complete after server started reading - possible deadlock")
	}
}

// Note: We would need to modify runHelperShmServer or create a separate
// runHelperShmDelayedServer function for the backpressure test, but this
// demonstrates the test structure for event-driven blocking without sleeps.
