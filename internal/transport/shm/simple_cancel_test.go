//go:build linux

package shm

import (
	"context"
	"fmt"
	"log"
	"testing"
	"time"
)

// TestSimpleCancellation tests cancellation with a simple context.WithCancel
func TestSimpleCancellation(t *testing.T) {
	log.Printf("=== Starting TestSimpleCancellation ===")
	name := fmt.Sprintf("simple-cancel-%d", time.Now().UnixNano())
	seg, err := CreateSegment(name, 65536, 65536)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	// Server goroutine: read HEADERS and MESSAGE, then expect a CANCEL frame
	canceledSeen := make(chan struct{}, 1)
	go func() {
		log.Printf("Server: Starting")
		srvRx := NewShmRingFromSegment(seg.A, seg.Mem)

		// Read request HEADERS with timeout
		log.Printf("Server: Reading HEADERS...")
		readCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		fh, _, err := readFrame(srvRx, readCtx)
		cancel()

		if err != nil {
			log.Printf("Server: Failed to read HEADERS: %v", err)
			return
		}
		if fh.Type != FrameTypeHEADERS {
			log.Printf("Server: Expected HEADERS, got type %d", fh.Type)
			return
		}

		log.Printf("Server: Got HEADERS (streamID=%d), reading more frames...", fh.StreamID)
		// Read next frame(s) until CANCEL arrives
		for i := 0; i < 3; i++ {
			log.Printf("Server: Reading frame %d...", i+1)
			readCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			fh2, payload, err := readFrame(srvRx, readCtx)
			cancel()

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

	// Use a simple cancellable context WITHOUT timeout
	ctx, cancel := context.WithCancel(context.Background())

	// Invoke in a goroutine to allow early cancel
	errCh := make(chan error, 1)
	go func() {
		log.Printf("Client: Starting UnaryCall")
		_, _, _, err := client.UnaryCall(ctx, "/svc.X/Unary", "example.com", nil, payload)
		log.Printf("Client: UnaryCall returned: %v", err)
		errCh <- err
	}()

	// Let the client send HEADERS and MESSAGE frames, then cancel
	log.Printf("Client: Sleeping briefly to let frames be sent...")
	time.Sleep(2 * time.Millisecond)
	log.Printf("Client: About to call cancel()...")
	cancel()
	log.Printf("Client: cancel() returned")

	// Client should return with cancellation error
	log.Printf("Client: Waiting for UnaryCall result...")
	select {
	case err := <-errCh:
		log.Printf("Client: Got result: %v", err)
		if err == nil {
			t.Fatal("expected cancellation error, got nil")
		}
	case <-time.After(2 * time.Second):
		log.Printf("Client: TIMEOUT waiting for UnaryCall result!")
		t.Fatal("client did not return after cancel")
	}

	// Server observed CANCEL
	log.Printf("Checking if server observed CANCEL...")
	select {
	case <-canceledSeen:
		log.Printf("Server observed CANCEL frame")
	case <-time.After(2 * time.Second):
		log.Printf("Server: TIMEOUT waiting for CANCEL frame!")
		t.Fatal("server did not observe CANCEL frame")
	}

	// Clean up resources
	log.Printf("Cleaning up...")
	_ = client.Close()
	log.Printf("=== TestSimpleCancellation completed ===")
}
