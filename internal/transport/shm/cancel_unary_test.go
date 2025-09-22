//go:build linux

package shm

import (
	"context"
	"fmt"
	"log"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestUnary_Cancellation verifies client sends a single CANCEL and server observes
// cancellation; client returns an error consistent with grpc-go (codes.Canceled).
func TestUnary_Cancellation(t *testing.T) {
	t.Logf("=== Starting TestUnary_Cancellation ===")
	name := fmt.Sprintf("cancel-unary-%d", time.Now().UnixNano())
	seg, err := CreateSegment(name, 65536, 65536)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	// Server goroutine: read HEADERS and MESSAGE, send HEADERS response, then expect a CANCEL frame.
	canceledSeen := make(chan struct{}, 1)
	serverDone := make(chan struct{}, 1)
	go func() {
		defer close(serverDone)
		log.Printf("Server: Starting goroutine")
		srvRx := NewShmRingFromSegment(seg.A, seg.Mem)
		srvTx := NewShmRingFromSegment(seg.B, seg.Mem)

		// Read request HEADERS with timeout to avoid race detector hangs
		log.Printf("Server: About to read HEADERS frame...")
		headersCtx, headersCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer headersCancel()
		if fh, _, err := readFrame(srvRx, headersCtx); err == nil && fh.Type == FrameTypeHEADERS {
			log.Printf("Server: Successfully read HEADERS frame (streamID=%d)", fh.StreamID)

			// Read MESSAGE frame with timeout
			log.Printf("Server: About to read MESSAGE frame...")
			messageCtx, messageCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer messageCancel()
			if fh2, _, err2 := readFrame(srvRx, messageCtx); err2 == nil && fh2.Type == FrameTypeMESSAGE {
				log.Printf("Server: Successfully read MESSAGE frame (streamID=%d)", fh2.StreamID)

				// Send HEADERS response to unblock client reader
				log.Printf("Server: Sending HEADERS response...")
				respHdr := HeadersV1{Version: 1, HdrType: 1} // Response headers
				hdrBytes := encodeHeaders(respHdr)
				err3 := writeFrame(srvTx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeHEADERS}, hdrBytes, context.Background())
				if err3 != nil {
					log.Printf("Server: Failed to send HEADERS response: %v", err3)
				} else {
					log.Printf("Server: HEADERS response sent successfully")
				}

				// Now wait for CANCEL frame
				for i := 0; i < 10; i++ {
					log.Printf("Server: Attempting to read CANCEL frame %d...", i+1)

					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					fh3, payload, err := readFrame(srvRx, ctx)
					cancel()

					if err != nil {
						log.Printf("Server: ReadFrame error on attempt %d: %v", i+1, err)
						if ctx.Err() == context.DeadlineExceeded {
							log.Printf("Server: Timeout waiting for CANCEL frame, continuing...")
							continue
						}
						break
					}

					log.Printf("Server: Got frame type %d (streamID=%d, payloadLen=%d)", fh3.Type, fh3.StreamID, len(payload))

					if fh3.Type == FrameTypeCANCEL {
						log.Printf("Server: SUCCESS - Received CANCEL frame!")
						canceledSeen <- struct{}{}
						return
					} else {
						log.Printf("Server: Received unexpected frame type %d, continuing...", fh3.Type)
					}
				}
			} else {
				log.Printf("Server: Failed to read MESSAGE: err=%v", err2)
			}
		} else {
			log.Printf("Server: Failed to read HEADERS: err=%v", err)
		}
		log.Printf("Server: Exiting without seeing CANCEL frame")
	}()

	client := NewShmUnaryClient(seg)
	payload := make([]byte, 5+3)
	payload[0] = 0
	payload[1] = 3
	payload[2] = 0
	payload[3] = 0
	payload[4] = 0
	copy(payload[5:], []byte("abc"))

	log.Printf("Client: Created client and payload")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // Increased timeout
	defer cancel()

	// Invoke in a goroutine to allow early cancel
	errCh := make(chan error, 1)
	resultCh := make(chan struct{}, 1)

	go func() {
		log.Printf("Client: Starting UnaryCall goroutine")
		_, _, _, err := client.UnaryCall(ctx, "/svc.X/Unary", "example.com", nil, payload)
		log.Printf("Client: UnaryCall returned with error: %v", err)
		errCh <- err
		resultCh <- struct{}{}
	}()

	// Allow client to send initial frames before canceling
	// Under race detector, operations are much slower, so wait longer
	delay := 100 * time.Millisecond
	if testing.Short() {
		delay = 50 * time.Millisecond
	} else {
		// Assume we might be under race detector or other slow conditions
		delay = 500 * time.Millisecond
	}

	log.Printf("Client: Waiting %v before cancel...", delay)
	time.Sleep(delay)
	log.Printf("Client: Sleep completed")

	log.Printf("Client: About to cancel context...")
	cancel()
	log.Printf("Client: Context cancelled")

	// Client should return with codes.Canceled
	log.Printf("Client: Waiting for UnaryCall to complete...")
	select {
	case err := <-errCh:
		log.Printf("Client: UnaryCall completed with result: %v", err)
		if err == nil {
			t.Fatal("expected cancellation error, got nil")
		}
		// Map to gRPC status code Canceled
		statusCode := status.FromContextError(err).Code()
		log.Printf("Client: Error mapped to gRPC status code: %v", statusCode)
		if statusCode != codes.Canceled {
			t.Fatalf("expected codes.Canceled, got %v (err=%v)", statusCode, err)
		}
		log.Printf("Client: SUCCESS - Got expected codes.Canceled")
	case <-time.After(8 * time.Second): // Increased timeout
		log.Printf("Client: ERROR - UnaryCall did not return after cancel within timeout")
		t.Fatal("client did not return after cancel")
	}

	// Server observed CANCEL
	log.Printf("Checking if server observed CANCEL frame...")
	select {
	case <-canceledSeen:
		log.Printf("Server: SUCCESS - Observed CANCEL frame")
	case <-time.After(5 * time.Second): // Increased timeout
		log.Printf("Server: ERROR - Did not observe CANCEL frame within timeout")

		// Check if server goroutine is still running
		select {
		case <-serverDone:
			log.Printf("Server: Server goroutine has completed")
		default:
			log.Printf("Server: Server goroutine is still running")
		}

		t.Fatal("server did not observe CANCEL frame")
	}

	// Clean up resources
	log.Printf("Test: Cleaning up...")
	closeErr := client.Close()
	log.Printf("Test: Client.Close() returned: %v", closeErr)

	// Wait for server goroutine to complete
	select {
	case <-serverDone:
		log.Printf("Test: Server goroutine completed successfully")
	case <-time.After(2 * time.Second):
		log.Printf("Test: WARNING - Server goroutine did not complete within 2s")
	}

	log.Printf("=== TestUnary_Cancellation completed ===")
}
