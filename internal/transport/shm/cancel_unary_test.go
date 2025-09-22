//go:build linux && (amd64 || arm64)

package shm

import (
	"context"
	"fmt"
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

	// Server goroutine: read HEADERS and MESSAGE, then expect a CANCEL frame; no response.
	canceledSeen := make(chan struct{}, 1)
	go func() {
		t.Logf("Server: Starting")
		srvRx := NewShmRingFromSegment(seg.A, seg.Mem)
		// Read request HEADERS
		t.Logf("Server: Reading HEADERS...")
		if fh, _, err := readFrame(srvRx, context.Background()); err == nil && fh.Type == FrameTypeHEADERS {
			t.Logf("Server: Got HEADERS, reading more frames...")
			// Read next frame(s) until CANCEL arrives
			for i := 0; i < 3; i++ {
				t.Logf("Server: Reading frame %d...", i+1)
				fh2, _, err := readFrame(srvRx, context.Background())
				if err != nil {
					t.Logf("Server: ReadFrame error: %v", err)
					break
				}
				t.Logf("Server: Got frame type %d", fh2.Type)
				if fh2.Type == FrameTypeCANCEL {
					t.Logf("Server: Saw CANCEL frame!")
					canceledSeen <- struct{}{}
					return
				}
			}
		}
		t.Logf("Server: Exiting without seeing CANCEL")
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
		t.Logf("Client: Starting UnaryCall")
		_, _, _, err := client.UnaryCall(ctx, "/svc.X/Unary", "example.com", nil, payload)
		t.Logf("Client: UnaryCall returned: %v", err)
		errCh <- err
	}()

	// Cancel quickly before server replies
	time.Sleep(10 * time.Millisecond)
	t.Logf("Client: Canceling context...")
	cancel()

	// Client should return with codes.Canceled
	select {
	case err := <-errCh:
		t.Logf("Client: Got result: %v", err)
		if err == nil {
			t.Fatal("expected cancellation error, got nil")
		}
		// Map to gRPC status code Canceled
		if status.FromContextError(err).Code() != codes.Canceled {
			t.Fatalf("expected codes.Canceled, got %v (err=%v)", status.FromContextError(err).Code(), err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not return after cancel")
	}

	// Server observed CANCEL
	t.Logf("Checking if server observed CANCEL...")
	select {
	case <-canceledSeen:
		t.Logf("Server observed CANCEL frame")
	case <-time.After(2 * time.Second):
		t.Fatal("server did not observe CANCEL frame")
	}

	// Clean up resources
	t.Logf("Cleaning up...")
	_ = client.Close()
	t.Logf("=== TestUnary_Cancellation completed ===")
}
