//go:build linux && (amd64 || arm64)

package shm

import (
    "context"
    "fmt"
    "runtime"
    "testing"
    "time"
)

// Test backpressure and blocking semantics with small rings and large messages.
func TestUnary_BackpressureAndBlocking(t *testing.T) {
    // Small rings: 64 KiB each
    const ringCap = 64 * 1024
    name := fmt.Sprintf("pressure-%d", time.Now().UnixNano())
    seg, err := CreateSegment(name, ringCap, ringCap)
    if err != nil { t.Fatalf("CreateSegment: %v", err) }
    defer seg.Close()

    cliTx := NewShmRingFromSegment(seg.A, seg.Mem) // client->server
    cliRx := NewShmRingFromSegment(seg.B, seg.Mem) // server->client

    // Server: slow reader simulating processing delay.
    serverDone := make(chan struct{})
    go func() {
        defer close(serverDone)
        // Delay to allow writer to fill ring and exercise backpressure
        time.Sleep(50 * time.Millisecond)
        // Read request HEADERS
        fh, _, err := readFrame(cliTx)
        if err != nil || fh.Type != FrameTypeHEADERS {
            t.Errorf("server read headers err=%v type=%v", err, fh.Type)
            return
        }
        // Slow read loop for MESSAGE chunks; reassemble
        var acc []byte
        for {
            fhm, p, err := readFrame(cliTx)
            if err != nil { t.Errorf("server read message: %v", err); return }
            if fhm.Type != FrameTypeMESSAGE { t.Errorf("expected MESSAGE, got %v", fhm.Type); return }
            acc = append(acc, p...)
            if fhm.Flags&MessageFlagMORE == 0 { break }
        }
        // Respond: HEADERS, then MESSAGE (single frame ok), then TRAILERS OK
        if err := writeFrame(cliRx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL}, encodeHeaders(HeadersV1{Version:1, HdrType:1})); err != nil {
            t.Errorf("server write headers: %v", err); return
        }
        if err := writeFrame(cliRx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeMESSAGE}, acc); err != nil {
            t.Errorf("server write message: %v", err); return
        }
        if err := writeFrame(cliRx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeTRAILERS, Flags: TrailersFlagEND_STREAM}, encodeTrailers(TrailersV1{Version:1, GRPCStatusCode:0})); err != nil {
            t.Errorf("server write trailers: %v", err); return
        }
    }()

    // Large request: 256 KiB
    const total = 256 * 1024
    payload := make([]byte, 5+total)
    payload[0] = 0
    payload[1] = byte(total & 0xFF)
    payload[2] = byte((total >> 8) & 0xFF)
    payload[3] = byte((total >> 16) & 0xFF)
    payload[4] = byte((total >> 24) & 0xFF)
    for i := 0; i < total; i++ { payload[5+i] = byte(i % 251) }

    // Client performs unary call using ShmUnaryClient; this will internally block
    // on WriteAll due to small ring capacity and large payload.
    client := NewShmUnaryClient(seg)
    beforeG := runtime.NumGoroutine()
    start := time.Now()
    hdr, msg, tr, err := client.UnaryCall(context.Background(), "/svc.P/Pressure", "example.com", nil, payload)
    if err != nil { t.Fatalf("UnaryCall error: %v", err) }
    if hdr.HdrType != 1 { t.Fatalf("expected server-initial headers, got %v", hdr.HdrType) }
    if tr.GRPCStatusCode != 0 { t.Fatalf("expected OK status, got %d", tr.GRPCStatusCode) }
    if time.Since(start) < 30*time.Millisecond {
        t.Fatalf("unary call finished too quickly; expected blocking due to backpressure")
    }

    // No polling check: goroutine count change small
    afterG := runtime.NumGoroutine()
    if afterG > beforeG+2 { // a small slack for goroutine scheduling
        t.Fatalf("suspicious goroutine growth (polling?): before=%d after=%d", beforeG, afterG)
    }

    // Validate round-trip payload
    if len(msg) != len(payload) {
        t.Fatalf("response len mismatch: got=%d want=%d", len(msg), len(payload))
    }
    for i := range msg { if msg[i] != payload[i] { t.Fatalf("response mismatch at %d", i) } }

    <-serverDone
}
