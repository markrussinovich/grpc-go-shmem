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
    name := fmt.Sprintf("cancel-unary-%d", time.Now().UnixNano())
    seg, err := CreateSegment(name, 65536, 65536)
    if err != nil { t.Fatalf("CreateSegment: %v", err) }
    defer seg.Close()

    // Server goroutine: read HEADERS and MESSAGE, then expect a CANCEL frame; no response.
    canceledSeen := make(chan struct{}, 1)
    go func() {
        srvRx := NewShmRingFromSegment(seg.A, seg.Mem)
        // Read request HEADERS
        if fh, _, err := readFrame(srvRx); err == nil && fh.Type == FrameTypeHEADERS {
            // Read next frame(s) until CANCEL arrives
            for i := 0; i < 3; i++ {
                fh2, _, err := readFrame(srvRx)
                if err != nil { break }
                if fh2.Type == FrameTypeCANCEL {
                    canceledSeen <- struct{}{}
                    return
                }
            }
        }
    }()

    client := NewShmUnaryClient(seg)
    payload := make([]byte, 5+3)
    payload[0] = 0; payload[1] = 3; payload[2] = 0; payload[3] = 0; payload[4] = 0
    copy(payload[5:], []byte("abc"))

    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    // Invoke in a goroutine to allow early cancel
    errCh := make(chan error, 1)
    go func() {
        _, _, _, err := client.UnaryCall(ctx, "/svc.X/Unary", "example.com", nil, payload)
        errCh <- err
    }()

    // Cancel quickly before server replies
    time.Sleep(10 * time.Millisecond)
    cancel()

    // Client should return with codes.Canceled
    select {
    case err := <-errCh:
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
    select {
    case <-canceledSeen:
    case <-time.After(2 * time.Second):
        t.Fatal("server did not observe CANCEL frame")
    }
}

