//go:build linux

package transport

import (
	"context"
	"fmt"
	"log"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// / TestUnary_CancellationWithSlowServer verifies that when a client cancels
// while waiting for a slow server, it properly sends a CANCEL frame and returns
// codes.Canceled error.
func TestUnary_CancellationWithSlowServer(t *testing.T) {
	t.Logf("=== Starting TestUnary_CancellationWithSlowServer ===")

	name := fmt.Sprintf("cancel-slow-server-%d", time.Now().UnixNano())
	seg, err := CreateSegment(name, 65536, 65536)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	// Channels for test coordination
	serverReady := make(chan struct{})
	serverReceivedRequest := make(chan struct{})
	cancelFrameSeen := make(chan struct{})
	serverDone := make(chan struct{})

	// Start slow server FIRST and let it get ready
	go func() {
		defer close(serverDone)

		log.Printf("Server: Starting slow server")
		srvRx := NewShmRingFromSegment(seg.A, seg.Mem)
		srvTx := NewShmRingFromSegment(seg.B, seg.Mem)

		// Signal that server is ready to receive
		close(serverReady)

		// Read HEADERS frame with a timeout in case client never sends
		log.Printf("Server: Reading HEADERS...")
		ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
		fh, _, err := readFrame(srvRx, ctx1)
		cancel1()
		if err != nil {
			log.Printf("Server: Failed to read HEADERS: %v", err)
			return
		}
		if fh.Type != FrameTypeHEADERS {
			log.Printf("Server: Expected HEADERS, got type %d", fh.Type)
			return
		}
		streamID := fh.StreamID
		log.Printf("Server: Received HEADERS for stream %d", streamID)

		// Read MESSAGE frame with timeout
		log.Printf("Server: Reading MESSAGE...")
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		fh2, msgPayload, err2 := readFrame(srvRx, ctx2)
		cancel2()
		if err2 != nil {
			log.Printf("Server: Failed to read MESSAGE: %v", err2)
			return
		}
		if fh2.Type != FrameTypeMESSAGE {
			log.Printf("Server: Expected MESSAGE, got type %d", fh2.Type)
			return
		}
		log.Printf("Server: Received MESSAGE (len=%d) for stream %d", len(msgPayload), fh2.StreamID)

		// Signal that request was received
		close(serverReceivedRequest)

		// Simulate slow processing
		log.Printf("Server: Starting slow processing (500ms)...")
		time.Sleep(500 * time.Millisecond)
		log.Printf("Server: Finished slow processing")

		// Try to send response (client should have cancelled by now)
		log.Printf("Server: Attempting to send HEADERS response...")
		respHdr := HeadersV1{Version: 1, HdrType: 1}
		hdrBytes := encodeHeaders(respHdr)
		err3 := writeFrame(srvTx, FrameHeader{
			StreamID: streamID,
			Type:     FrameTypeHEADERS,
		}, hdrBytes, context.Background())

		if err3 != nil {
			log.Printf("Server: Failed to write HEADERS response: %v", err3)
		} else {
			log.Printf("Server: Sent HEADERS response")
		}

		// Check for CANCEL frame
		log.Printf("Server: Checking for CANCEL frame...")
		ctx3, cancel3 := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel3()

		fh3, _, err4 := readFrame(srvRx, ctx3)
		if err4 != nil {
			log.Printf("Server: Error reading next frame: %v", err4)
			return
		}

		if fh3.Type == FrameTypeCANCEL {
			log.Printf("Server: SUCCESS - Received CANCEL frame for stream %d", fh3.StreamID)
			close(cancelFrameSeen)
		} else {
			log.Printf("Server: Expected CANCEL, got frame type %d", fh3.Type)
		}
	}()

	// Wait for server to be ready
	<-serverReady
	log.Printf("Test: Server is ready")

	// Small delay to ensure server is in read state
	time.Sleep(50 * time.Millisecond)

	// Create client
	client := NewShmUnaryClient(seg)

	// Create payload
	payload := make([]byte, 5+3)
	payload[0] = 0 // compression flag
	payload[1] = 0 // length (MSB)
	payload[2] = 0
	payload[3] = 0
	payload[4] = 3 // length (LSB) - 3 bytes
	copy(payload[5:], []byte("abc"))

	log.Printf("Client: Created client and payload")

	// Use WithCancel for explicit cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start a goroutine that will cancel after 200ms
	go func() {
		time.Sleep(200 * time.Millisecond)
		log.Printf("Test: Explicitly cancelling context after 200ms")
		cancel()
	}()

	// Make the call
	log.Printf("Client: Starting UnaryCall (will be cancelled after 200ms)")
	startTime := time.Now()
	_, _, _, err = client.UnaryCall(ctx, "/svc.X/SlowMethod", "example.com", nil, payload)
	elapsed := time.Since(startTime)
	log.Printf("Client: UnaryCall returned after %v with error: %v", elapsed, err)

	// Verify client returned the expected cancellation error
	if err == nil {
		t.Fatal("Expected cancellation error, got nil")
	}

	statusCode := status.FromContextError(err).Code()
	if statusCode != codes.Canceled {
		t.Errorf("Expected codes.Canceled, got %v (err=%v)", statusCode, err)
	} else {
		log.Printf("Client: SUCCESS - Got expected codes.Canceled")
	}

	// Verify timing
	if elapsed > 300*time.Millisecond {
		t.Errorf("Client took too long to cancel: %v", elapsed)
	}

	// Verify server received request
	select {
	case <-serverReceivedRequest:
		log.Printf("Test: Server confirmed it received the request")
	case <-time.After(1 * time.Second):
		t.Error("Server didn't receive request")
	}

	// Verify server saw CANCEL
	select {
	case <-cancelFrameSeen:
		log.Printf("Test: SUCCESS - Server confirmed it received CANCEL frame")
	case <-time.After(2 * time.Second):
		t.Error("Server never saw CANCEL frame")
	}

	// Clean up
	log.Printf("Test: Cleaning up...")
	if err := client.Close(); err != nil {
		log.Printf("Test: client.Close() error: %v", err)
	}

	// Wait for server to complete
	select {
	case <-serverDone:
		log.Printf("Test: Server goroutine completed")
	case <-time.After(2 * time.Second):
		log.Printf("Test: Warning - server didn't complete within 2s")
	}

	log.Printf("=== TestUnary_CancellationWithSlowServer completed successfully ===")
}
