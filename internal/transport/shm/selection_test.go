//go:build linux && (amd64 || arm64)

package shm

import (
    "context"
    "fmt"
    "testing"
    "time"
)

// A minimal selection test that prefers shm:// over tcp:// when both are present.
func TestSelection_ChoosesSHM_and_ExecutesUnary(t *testing.T) {
    name := fmt.Sprintf("sel-%d", time.Now().UnixNano())
    raw := fmt.Sprintf("shm://%s?cap=65536", name)

    // Server factory
    lis, err := newShmServerFactory(raw)
    if err != nil { t.Fatalf("server factory: %v", err) }
    defer lis.Close()

    // Server responder goroutine (HEADERS→MESSAGE→TRAILERS OK)
    go func() {
        // Use rings directly from listener's segment; server factory already
        // created the segment and marked server ready.
        seg := lis.segment
        srvRx := NewShmRingFromSegment(seg.A, seg.Mem)
        srvTx := NewShmRingFromSegment(seg.B, seg.Mem)
        fh, pl, err := readFrame(srvRx)
        if err != nil { t.Errorf("server read headers: %v", err); return }
        if fh.Type != FrameTypeHEADERS { t.Errorf("expected HEADERS"); return }
        if _, err := decodeHeaders(pl); err != nil { t.Errorf("decode headers: %v", err); return }
        fh2, msg, err := readFrame(srvRx)
        if err != nil { t.Errorf("server read msg: %v", err); return }
        if fh2.Type != FrameTypeMESSAGE { t.Errorf("expected MESSAGE"); return }
        // Respond
        h := HeadersV1{Version:1, HdrType:1}
        _ = writeFrame(srvTx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL}, encodeHeaders(h))
        _ = writeFrame(srvTx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeMESSAGE}, msg)
        tr := TrailersV1{Version:1, GRPCStatusCode:0}
        _ = writeFrame(srvTx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeTRAILERS, Flags: TrailersFlagEND_STREAM}, encodeTrailers(tr))
    }()

    // Registry-like selection: prefer shm over tcp when present.
    addrs := []string{"tcp://127.0.0.1:0", raw}
    var chosen string
    for _, a := range addrs {
        if _, err := ParseAddress(a); err == nil {
            chosen = a; break
        }
    }
    if chosen == "" { t.Fatal("shm address not chosen") }

    // Client factory (disable transport reader to avoid interference).
    enableClientReader.Store(false)
    defer enableClientReader.Store(true)
    ct, err := newShmClientFactory(context.Background(), chosen)
    if err != nil { t.Fatalf("client factory: %v", err) }

    // Unary call over the shared memory segment
    time.Sleep(10 * time.Millisecond)
    seg := ct.(*ShmClientTransport).segment
    cli := NewShmUnaryClient(seg)
    payload := make([]byte, 5+3)
    payload[0] = 0; payload[1] = 3; payload[2] = 0; payload[3] = 0; payload[4] = 0
    copy(payload[5:], []byte("hey"))
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancel()
    _, msg, tr, err := cli.UnaryCall(ctx, "/echo.Svc/Echo", name, nil, payload)
    if err != nil { t.Fatalf("UnaryCall error: %v", err) }
    if tr.GRPCStatusCode != 0 { t.Fatalf("expected OK status, got %d", tr.GRPCStatusCode) }
    if len(msg) != len(payload) { t.Fatalf("len mismatch: got %d want %d", len(msg), len(payload)) }
    for i := range msg { if msg[i] != payload[i] { t.Fatalf("data mismatch at %d", i) } }

    // Verify selection counters observed shm path
    if shmServerListenCount.Load() == 0 { t.Fatalf("server factory counter not incremented") }
    if shmClientConnectCount.Load() == 0 { t.Fatalf("client factory counter not incremented") }
}
