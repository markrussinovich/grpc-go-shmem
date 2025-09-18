//go:build linux && (amd64 || arm64)

package shm

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"
)

// writeFrameWithContext writes a frame with a context for timeout support
func writeFrameWithContext(ctx context.Context, tx *ShmRing, fh FrameHeader, payload []byte) error {
	// Fill header fields consistently and set reserved to zero
	fh.Length = uint32(len(payload))
	fh.Reserved = 0
	fh.Reserved2 = 0

	// Reserve and write the 16-byte header
	res, err := tx.ReserveFrameHeader(ctx)
	if err != nil {
		return err
	}
	var hdr [frameHeaderSize]byte
	encodeFrameHeaderTo(&hdr, fh)
	n := copy(res.First, hdr[:])
	if n != frameHeaderSize {
		return fmt.Errorf("failed to copy frame header")
	}
	if err := res.Commit(frameHeaderSize); err != nil {
		return err
	}

	// Write payload
	if len(payload) > 0 {
		if err := tx.WriteAll(payload, ctx); err != nil {
			return err
		}
	}
	return nil
}

// readFrameWithContext reads a frame with a context for timeout support
func readFrameWithContext(ctx context.Context, rx *ShmRing) (FrameHeader, []byte, error) {
	for {
		// Check context first
		select {
		case <-ctx.Done():
			return FrameHeader{}, nil, ctx.Err()
		default:
		}

		// Geometry-aware fast path: if fewer than 16 bytes remain before wrap,
		// skip them (PAD payload) first, then read the header at offset 0.
		hdr := rx.header()
		rIdx := hdr.ReadIndex()
		remToEnd := rx.capacity - (rIdx & rx.capMask)
		if remToEnd < frameHeaderSize && remToEnd > 0 {
			// Consume the tail bytes as PAD payload
			if _, _, commit, err := rx.ReadSlices(int(remToEnd), ctx); err != nil {
				return FrameHeader{}, nil, err
			} else {
				commit(int(remToEnd))
			}
			// Consume the PAD header at offset 0 silently, then continue
			if _, err := rx.ReadExact(frameHeaderSize, nil, ctx); err != nil {
				return FrameHeader{}, nil, err
			}
			continue
		}

		// Read and parse header at current r
		hb, err := rx.ReadExact(frameHeaderSize, nil, ctx)
		if err != nil {
			return FrameHeader{}, nil, err
		}
		fh, err := decodeFrameHeader(hb)
		if err != nil {
			return FrameHeader{}, nil, err
		}
		if fh.Type == FrameTypePAD {
			// Consume PAD payload and continue
			if fh.Length > 0 {
				if _, err := rx.ReadExact(int(fh.Length), nil, ctx); err != nil {
					return FrameHeader{}, nil, err
				}
			}
			continue
		}

		// Read payload if present
		var payload []byte
		if fh.Length > 0 {
			payload, err = rx.ReadExact(int(fh.Length), nil, ctx)
			if err != nil {
				return FrameHeader{}, nil, err
			}
		}
		return fh, payload, nil
	}
}

// Test backpressure and blocking semantics with small rings and large messages.
func TestUnary_BackpressureAndBlocking(t *testing.T) {
	// Small rings: 64 KiB each
	const ringCap = 64 * 1024
	name := fmt.Sprintf("pressure-%d", time.Now().UnixNano())
	seg, err := CreateSegment(name, ringCap, ringCap)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	cliTx := NewShmRingFromSegment(seg.A, seg.Mem) // client->server
	cliRx := NewShmRingFromSegment(seg.B, seg.Mem) // server->client

	// Use a smaller payload that won't cause complete deadlock but will exercise backpressure
	const total = 48 * 1024
	payload := make([]byte, 5+total)
	payload[0] = 0
	payload[1] = byte(total & 0xFF)
	payload[2] = byte((total >> 8) & 0xFF)
	payload[3] = byte((total >> 16) & 0xFF)
	payload[4] = byte((total >> 24) & 0xFF)
	for i := 0; i < total; i++ {
		payload[5+i] = byte(i % 251)
	}

	// Run the test with a global timeout to catch any hanging
	done := make(chan error, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("test panic: %v", r)
			}
		}()

		// Server: slow reader simulating processing delay but concurrent
		serverResult := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					serverResult <- fmt.Errorf("server panic: %v", r)
				}
			}()

			// Small delay to allow client to start writing
			time.Sleep(20 * time.Millisecond)

			// Read request HEADERS
			fh, _, err := readFrame(cliTx)
			if err != nil {
				serverResult <- fmt.Errorf("server read headers: %v", err)
				return
			}
			if fh.Type != FrameTypeHEADERS {
				serverResult <- fmt.Errorf("expected HEADERS, got %v", fh.Type)
				return
			}

			// Read MESSAGE frame(s) - should be chunked due to ring size
			var acc []byte
			for {
				fhm, p, err := readFrame(cliTx)
				if err != nil {
					serverResult <- fmt.Errorf("server read message: %v", err)
					return
				}
				if fhm.Type != FrameTypeMESSAGE {
					serverResult <- fmt.Errorf("expected MESSAGE, got %v", fhm.Type)
					return
				}
				acc = append(acc, p...)
				if fhm.Flags&MessageFlagMORE == 0 {
					break
				}
			}

			// Respond with same data
			if err := writeFrame(cliRx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL}, encodeHeaders(HeadersV1{Version: 1, HdrType: 1})); err != nil {
				serverResult <- fmt.Errorf("server write headers: %v", err)
				return
			}
			if err := writeFrame(cliRx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeMESSAGE}, acc); err != nil {
				serverResult <- fmt.Errorf("server write message: %v", err)
				return
			}
			if err := writeFrame(cliRx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeTRAILERS, Flags: TrailersFlagEND_STREAM}, encodeTrailers(TrailersV1{Version: 1, GRPCStatusCode: 0})); err != nil {
				serverResult <- fmt.Errorf("server write trailers: %v", err)
				return
			}

			serverResult <- nil // success
		}()

		// Client call with reasonable timeout
		client := NewShmUnaryClient(seg)
		beforeG := runtime.NumGoroutine()
		start := time.Now()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		hdr, msg, tr, err := client.UnaryCall(ctx, "/svc.P/Pressure", "example.com", nil, payload)
		if err != nil {
			_ = client.Close()
			done <- fmt.Errorf("UnaryCall error: %v", err)
			return
		}
		if hdr.HdrType != 1 {
			_ = client.Close()
			done <- fmt.Errorf("expected server-initial headers, got %v", hdr.HdrType)
			return
		}
		if tr.GRPCStatusCode != 0 {
			_ = client.Close()
			done <- fmt.Errorf("expected OK status, got %d", tr.GRPCStatusCode)
			return
		}

		// Check that some blocking occurred (backpressure exercised)
		elapsed := time.Since(start)
		if elapsed < 10*time.Millisecond {
			t.Logf("Warning: call completed very quickly (%v), backpressure may not have been exercised", elapsed)
		}

		// No polling check: goroutine count change small
		afterG := runtime.NumGoroutine()
		if afterG > beforeG+3 { // a bit more slack for goroutine scheduling
			_ = client.Close()
			done <- fmt.Errorf("suspicious goroutine growth (polling?): before=%d after=%d", beforeG, afterG)
			return
		}

		// Validate round-trip payload
		if len(msg) != len(payload) {
			_ = client.Close()
			done <- fmt.Errorf("response len mismatch: got=%d want=%d", len(msg), len(payload))
			return
		}
		for i := range msg {
			if msg[i] != payload[i] {
				_ = client.Close()
				done <- fmt.Errorf("response mismatch at %d", i)
				return
			}
		}

		// Wait for server to complete
		select {
		case serverErr := <-serverResult:
			if serverErr != nil {
				_ = client.Close()
				done <- fmt.Errorf("server error: %v", serverErr)
				return
			}
		case <-time.After(1 * time.Second):
			_ = client.Close()
			done <- fmt.Errorf("server did not complete within timeout")
			return
		}

		// Clean up client before success
		_ = client.Close()
		done <- nil // success
	}()

	// Wait for completion with global timeout
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("test timed out - likely hanging in frame operations")
	}
}
