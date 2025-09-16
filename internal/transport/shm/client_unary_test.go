//go:build linux && (amd64 || arm64)

package shm

import (
    "context"
    "fmt"
    "testing"
    "time"
)

// Test a unary round-trip using SMF v1 framing and the ShmUnaryClient.
func TestShmUnaryClient_Echo(t *testing.T) {
    name := fmt.Sprintf("shm-unary-%d", time.Now().UnixNano())
    seg, err := CreateSegment(name, 65536, 65536)
    if err != nil { t.Fatalf("CreateSegment: %v", err) }

    // Server goroutine: simple echo handler over frames
    done := make(chan struct{})
    go func() {
        defer close(done)
        srvRx := NewShmRingFromSegment(seg.A, seg.Mem) // client->server
        srvTx := NewShmRingFromSegment(seg.B, seg.Mem) // server->client
        // Read HEADERS
        fh, pl, err := readFrame(srvRx)
        if err != nil { t.Errorf("server read headers: %v", err); return }
        if fh.Type != FrameTypeHEADERS { t.Errorf("expected HEADERS, got %v", fh.Type); return }
        if _, err := decodeHeaders(pl); err != nil { t.Errorf("decode headers: %v", err); return }

        // Read MESSAGE
        fh2, msg, err := readFrame(srvRx)
        if err != nil { t.Errorf("server read message: %v", err); return }
        if fh2.Type != FrameTypeMESSAGE { t.Errorf("expected MESSAGE, got %v", fh2.Type); return }

        // Write HEADERS (server-initial)
        h := HeadersV1{Version:1, HdrType:1, Authority:"srv", Metadata: []KV{{Key:"x-reply", Values: [][]byte{[]byte("yes")}}}}
        if err := writeFrame(srvTx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL}, encodeHeaders(h)); err != nil {
            t.Errorf("server write headers: %v", err); return
        }
        // Echo MESSAGE
        if err := writeFrame(srvTx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeMESSAGE}, msg); err != nil {
            t.Errorf("server write message: %v", err); return
        }
        // Write TRAILERS OK
        tr := TrailersV1{Version:1, GRPCStatusCode:0}
        if err := writeFrame(srvTx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeTRAILERS, Flags: TrailersFlagEND_STREAM}, encodeTrailers(tr)); err != nil {
            t.Errorf("server write trailers: %v", err); return
        }
    }()

    client := NewShmUnaryClient(seg)

    // Make a gRPC-style MESSAGE: 5-byte prefix + payload
    payload := make([]byte, 5+7)
    payload[0] = 0 // uncompressed
    // length=7
    payload[1] = 7; payload[2] = 0; payload[3] = 0; payload[4] = 0
    copy(payload[5:], []byte("hello!!"))

    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancel()
    hdr, msg, tr, err := client.UnaryCall(ctx, "/echo.Echo/Echo", "example.com", nil, payload)
    if err != nil { t.Fatalf("UnaryCall error: %v", err) }
    if hdr.HdrType != 1 { t.Fatalf("expected server-initial headers, got %v", hdr.HdrType) }
    // Verify echoed metadata
    found := false
    for _, kv := range hdr.Metadata {
        if kv.Key == "x-reply" && len(kv.Values) == 1 && string(kv.Values[0]) == "yes" { found = true; break }
    }
    if !found { t.Fatalf("expected x-reply metadata in headers") }
    if tr.GRPCStatusCode != 0 { t.Fatalf("expected OK status, got %d", tr.GRPCStatusCode) }
    if len(msg) != len(payload) { t.Fatalf("echo length mismatch: got %d want %d", len(msg), len(payload)) }
    for i := range msg { if msg[i] != payload[i] { t.Fatalf("echo data mismatch at %d", i) } }

    <-done
    _ = client.Close()
    _ = seg.Close()
}
