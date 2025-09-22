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

// TestDebugCancellation has extensive debug output to trace cancellation behavior
func TestDebugCancellation(t *testing.T) {
	log.Printf("=== Starting TestDebugCancellation ===")
	name := fmt.Sprintf("debug-cancel-%d", time.Now().UnixNano())
	seg, err := CreateSegment(name, 65536, 65536)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	// Server goroutine: read HEADERS and MESSAGE, then expect a CANCEL frame; no response.
	canceledSeen := make(chan struct{}, 1)
	go func() {
		log.Printf("Server: Starting")
		srvRx := NewShmRingFromSegment(seg.A, seg.Mem)
		// Read request HEADERS
		log.Printf("Server: Reading HEADERS...")
		if fh, _, err := readFrame(srvRx, context.Background()); err == nil && fh.Type == FrameTypeHEADERS {
			log.Printf("Server: Got HEADERS (streamID=%d), reading more frames...", fh.StreamID)
			// Read next frame(s) until CANCEL arrives
			for i := 0; i < 3; i++ {
				log.Printf("Server: Reading frame %d...", i+1)
				fh2, payload, err := readFrame(srvRx, context.Background())
				if err != nil {
					log.Printf("Server: ReadFrame error: %v", err)
					break
				}
				log.Printf("Server: Got frame type %d (streamID=%d, payloadLen=%d)", fh2.Type, fh2.StreamID, len(payload))
				if fh2.Type == FrameTypeCANCEL {
					log.Printf("Server: Saw CANCEL frame!")
					canceledSeen <- struct{}{}
					return
				}
			}
		} else {
			log.Printf("Server: Failed to read HEADERS: %v", err)
		}
		log.Printf("Server: Exiting without seeing CANCEL")
	}()

	client := NewShmUnaryClient(seg)
	payload := make([]byte, 5+3)
	payload[0] = 0
	payload[1] = 3
	payload[2] = 0
	payload[3] = 0
	payload[4] = 0
	copy(payload[5:], []byte("abc"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	// Invoke in a goroutine to allow early cancel
	errCh := make(chan error, 1)
	go func() {
		log.Printf("Client: Starting UnaryCall")
		_, _, _, err := client.UnaryCall(ctx, "/svc.X/Unary", "example.com", nil, payload)
		log.Printf("Client: UnaryCall returned: %v", err)
		errCh <- err
	}()

	// Cancel quickly before server replies
	log.Printf("Client: Sleeping 1ms before cancel...")
	time.Sleep(1 * time.Millisecond)
	log.Printf("Client: Canceling context NOW...")
	cancel()
	log.Printf("Client: Context canceled")

	// Give the cancel goroutine a moment to react
	time.Sleep(10 * time.Millisecond)
	log.Printf("Client: After cancel delay")

	// Client should return with codes.Canceled
	log.Printf("Client: Waiting for UnaryCall result...")
	select {
	case err := <-errCh:
		log.Printf("Client: Got result: %v", err)
		if err == nil {
			t.Fatal("expected cancellation error, got nil")
		}
		// Map to gRPC status code Canceled
		if status.FromContextError(err).Code() != codes.Canceled {
			t.Fatalf("expected codes.Canceled, got %v (err=%v)", status.FromContextError(err).Code(), err)
		}
	case <-time.After(5 * time.Second):
		log.Printf("Client: TIMEOUT waiting for UnaryCall result!")
		t.Fatal("client did not return after cancel")
	}

	// Server observed CANCEL
	log.Printf("Checking if server observed CANCEL...")
	select {
	case <-canceledSeen:
		log.Printf("Server observed CANCEL frame")
	case <-time.After(5 * time.Second):
		log.Printf("Server: TIMEOUT waiting for CANCEL frame!")
		t.Fatal("server did not observe CANCEL frame")
	}

	// Clean up resources
	log.Printf("Cleaning up...")
	_ = client.Close()
	log.Printf("=== TestDebugCancellation completed ===")
}
