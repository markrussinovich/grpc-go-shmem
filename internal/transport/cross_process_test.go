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

/*
 * Cross-process test for shared memory transport.
 * This test verifies that the futex-based synchronization works correctly
 * between separate processes (not just goroutines).
 */

package transport

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// TestCrossProcessRingBuffer tests that the ring buffer synchronization
// works correctly between separate processes.
func TestCrossProcessRingBuffer(t *testing.T) {
	if os.Getenv("GRPC_CROSS_PROCESS_CHILD") != "" {
		// We're the child process - run the child logic
		runCrossProcessChild(t)
		return
	}

	// We're the parent process - create segment and spawn child
	segmentName := fmt.Sprintf("test_cross_process_%d", time.Now().UnixNano())
	ringSize := uint64(64 * 1024) // 64KB per ring

	// Create the segment (ring A for client->server, ring B for server->client)
	seg, err := CreateSegment(segmentName, ringSize, ringSize)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}
	defer func() {
		seg.Close()
		RemoveSegment(segmentName)
	}()

	// Get the rings
	clientToServer := NewShmRingFromSegment(seg.A, seg.Mem)
	serverToClient := NewShmRingFromSegment(seg.B, seg.Mem)

	// Spawn child process with context for timeout
	childCtx, childCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer childCancel()

	cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestCrossProcessRingBuffer$", "-test.v")
	cmd.Env = append(os.Environ(),
		"GRPC_CROSS_PROCESS_CHILD=1",
		"GRPC_CROSS_PROCESS_SEGMENT="+segmentName,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start child process: %v", err)
	}

	// Ensure child is killed if test panics
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}()

	// Parent acts as "client" - writes to clientToServer, reads from serverToClient
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Send test messages
	messages := []string{
		"Hello from parent!",
		"This is message 2",
		"Final message",
	}

	for i, msg := range messages {
		// Write frame: simple 4-byte length prefix + data
		payload := []byte(msg)
		frame := make([]byte, 4+len(payload))
		frame[0] = byte(len(payload))
		frame[1] = byte(len(payload) >> 8)
		frame[2] = byte(len(payload) >> 16)
		frame[3] = byte(len(payload) >> 24)
		copy(frame[4:], payload)

		if err := clientToServer.WriteBlockingContext(ctx, frame); err != nil {
			t.Fatalf("Parent: failed to write message %d: %v", i, err)
		}
		t.Logf("Parent: sent message %d: %q", i, msg)
	}

	// Read echoed messages from child
	for i := range messages {
		// Read length prefix
		lenBuf := make([]byte, 4)
		if _, err := serverToClient.ReadBlockingContext(ctx, lenBuf); err != nil {
			t.Fatalf("Parent: failed to read length %d: %v", i, err)
		}
		msgLen := int(lenBuf[0]) | int(lenBuf[1])<<8 | int(lenBuf[2])<<16 | int(lenBuf[3])<<24

		// Read message body
		msgBuf := make([]byte, msgLen)
		if _, err := serverToClient.ReadBlockingContext(ctx, msgBuf); err != nil {
			t.Fatalf("Parent: failed to read message %d: %v", i, err)
		}

		t.Logf("Parent: received echo %d: %q", i, string(msgBuf))
		expected := "ECHO: " + messages[i]
		if string(msgBuf) != expected {
			t.Errorf("Parent: message %d mismatch: got %q, want %q", i, string(msgBuf), expected)
		}
	}

	// Wait for child to exit with timeout
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("Child process failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatal("Timeout waiting for child process to exit")
	}

	t.Log("Cross-process test passed!")
}

func runCrossProcessChild(t *testing.T) {
	segmentName := os.Getenv("GRPC_CROSS_PROCESS_SEGMENT")
	if segmentName == "" {
		fmt.Println("CHILD: missing segment name")
		os.Exit(1)
	}

	fmt.Printf("CHILD: opening segment %s\n", segmentName)

	// Open the existing segment
	seg, err := OpenSegment(segmentName)
	if err != nil {
		fmt.Printf("CHILD: failed to open segment: %v\n", err)
		os.Exit(1)
	}
	defer seg.Close()

	// Child acts as "server" - reads from clientToServer (A), writes to serverToClient (B)
	clientToServer := NewShmRingFromSegment(seg.A, seg.Mem)
	serverToClient := NewShmRingFromSegment(seg.B, seg.Mem)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Echo 3 messages
	for i := 0; i < 3; i++ {
		// Read length prefix
		lenBuf := make([]byte, 4)
		n, err := clientToServer.ReadBlockingContext(ctx, lenBuf)
		if err != nil {
			fmt.Printf("CHILD: failed to read length %d: %v\n", i, err)
			os.Exit(1)
		}
		if n != 4 {
			fmt.Printf("CHILD: short read for length %d: got %d bytes\n", i, n)
			os.Exit(1)
		}
		msgLen := int(lenBuf[0]) | int(lenBuf[1])<<8 | int(lenBuf[2])<<16 | int(lenBuf[3])<<24

		// Read message body
		msgBuf := make([]byte, msgLen)
		n, err = clientToServer.ReadBlockingContext(ctx, msgBuf)
		if err != nil {
			fmt.Printf("CHILD: failed to read message %d: %v\n", i, err)
			os.Exit(1)
		}
		if n != msgLen {
			fmt.Printf("CHILD: short read for message %d: got %d bytes, want %d\n", i, n, msgLen)
			os.Exit(1)
		}

		fmt.Printf("CHILD: received message %d: %q\n", i, string(msgBuf))

		// Echo back with prefix
		echo := "ECHO: " + string(msgBuf)
		echoPayload := []byte(echo)
		echoFrame := make([]byte, 4+len(echoPayload))
		echoFrame[0] = byte(len(echoPayload))
		echoFrame[1] = byte(len(echoPayload) >> 8)
		echoFrame[2] = byte(len(echoPayload) >> 16)
		echoFrame[3] = byte(len(echoPayload) >> 24)
		copy(echoFrame[4:], echoPayload)

		if err := serverToClient.WriteBlockingContext(ctx, echoFrame); err != nil {
			fmt.Printf("CHILD: failed to write echo %d: %v\n", i, err)
			os.Exit(1)
		}
		fmt.Printf("CHILD: sent echo %d: %q\n", i, echo)
	}

	fmt.Println("CHILD: done, exiting successfully")
	os.Exit(0)
}

// TestCrossProcessGRPC tests full gRPC integration across processes
func TestCrossProcessGRPC(t *testing.T) {
	if os.Getenv("GRPC_CROSS_PROCESS_GRPC_CHILD") != "" {
		runCrossProcessGRPCChild(t)
		return
	}

	// Skip if running in short mode
	if testing.Short() {
		t.Skip("Skipping cross-process gRPC test in short mode")
	}

	// This test requires external binary compilation which is complex
	// For now, just verify the ring buffer works cross-process
	t.Skip("Full gRPC cross-process test requires separate binary")
}

func runCrossProcessGRPCChild(t *testing.T) {
	// Placeholder for full gRPC child process
	port := os.Getenv("GRPC_CROSS_PROCESS_PORT")
	if port == "" {
		os.Exit(1)
	}
	_, _ = strconv.Atoi(port)
	os.Exit(0)
}
