//go:build linux && (amd64 || arm64)

package shm

import (
    "fmt"
    "testing"
    "time"
)

func TestEncodeDecodeHeadersAndTrailers(t *testing.T) {
    h := HeadersV1{
        Version:          1,
        HdrType:          0,
        Method:           "/pkg.Service/Method",
        Authority:        "example.com",
        DeadlineUnixNano: 1234567890,
        Metadata: []KV{
            {Key: "grpc-encoding", Values: [][]byte{[]byte("identity")}},
            {Key: "user-agent", Values: [][]byte{[]byte("grpc-go-test")}},
        },
    }
    enc := encodeHeaders(h)
    got, err := decodeHeaders(enc)
    if err != nil {
        t.Fatalf("decodeHeaders error: %v", err)
    }
    if got.HdrType != h.HdrType || got.Method != h.Method || got.Authority != h.Authority || got.DeadlineUnixNano != h.DeadlineUnixNano || len(got.Metadata) != len(h.Metadata) {
        t.Fatalf("headers round-trip mismatch: got=%+v want=%+v", got, h)
    }

    tr := TrailersV1{
        Version:        1,
        GRPCStatusCode: 0,
        GRPCStatusMsg:  "",
        Metadata: []KV{
            {Key: "grpc-status-details-bin", Values: [][]byte{}},
        },
    }
    encT := encodeTrailers(tr)
    gotT, err := decodeTrailers(encT)
    if err != nil {
        t.Fatalf("decodeTrailers error: %v", err)
    }
    if gotT.GRPCStatusCode != tr.GRPCStatusCode || gotT.GRPCStatusMsg != tr.GRPCStatusMsg || len(gotT.Metadata) != len(tr.Metadata) {
        t.Fatalf("trailers round-trip mismatch: got=%+v want=%+v", gotT, tr)
    }
}

func TestWriteReadFrame_HeadersAndTrailers(t *testing.T) {
    name := fmt.Sprintf("frame-ht-%d", time.Now().UnixNano())
    seg, err := CreateSegment(name, 4096, 4096)
    if err != nil {
        t.Fatalf("CreateSegment error: %v", err)
    }
    defer seg.Close()

    ring := NewShmRingFromSegment(seg.A, seg.Mem)

    // Write HEADERS
    hdrPayload := encodeHeaders(HeadersV1{
        Version: 1, HdrType: 0, Method: "/pkg.Svc/M", Authority: "x", Metadata: nil,
    })
    fh := FrameHeader{StreamID: 1, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL}
    if err := writeFrame(ring, fh, hdrPayload); err != nil {
        t.Fatalf("writeFrame HEADERS: %v", err)
    }

    // Read HEADERS
    gotFH, gotPayload, err := readFrame(ring)
    if err != nil {
        t.Fatalf("readFrame HEADERS: %v", err)
    }
    if gotFH.Type != FrameTypeHEADERS || gotFH.StreamID != 1 || gotFH.Flags&HeadersFlagINITIAL == 0 {
        t.Fatalf("unexpected headers fh: %+v", gotFH)
    }
    if _, err := decodeHeaders(gotPayload); err != nil {
        t.Fatalf("decodeHeaders error: %v", err)
    }

    // Write TRAILERS
    trPayload := encodeTrailers(TrailersV1{Version: 1, GRPCStatusCode: 0, GRPCStatusMsg: "", Metadata: nil})
    tfh := FrameHeader{StreamID: 1, Type: FrameTypeTRAILERS, Flags: TrailersFlagEND_STREAM}
    if err := writeFrame(ring, tfh, trPayload); err != nil {
        t.Fatalf("writeFrame TRAILERS: %v", err)
    }
    gotTFH, gotTPayload, err := readFrame(ring)
    if err != nil {
        t.Fatalf("readFrame TRAILERS: %v", err)
    }
    if gotTFH.Type != FrameTypeTRAILERS || gotTFH.StreamID != 1 || gotTFH.Flags&TrailersFlagEND_STREAM == 0 {
        t.Fatalf("unexpected trailers fh: %+v", gotTFH)
    }
    if _, err := decodeTrailers(gotTPayload); err != nil {
        t.Fatalf("decodeTrailers error: %v", err)
    }
}

func TestWriteReadFrame_MessageChunkingWithMoreFlag(t *testing.T) {
    name := fmt.Sprintf("frame-chunk-%d", time.Now().UnixNano())
    seg, err := CreateSegment(name, 4096, 4096)
    if err != nil {
        t.Fatalf("CreateSegment error: %v", err)
    }
    defer seg.Close()
    ring := NewShmRingFromSegment(seg.A, seg.Mem)

    // Create a payload larger than a single chunk we will send in two MESSAGE frames
    orig := make([]byte, 3000)
    for i := range orig { orig[i] = byte(i % 251) }
    part1 := orig[:1700]
    part2 := orig[1700:]

    fh1 := FrameHeader{StreamID: 3, Type: FrameTypeMESSAGE, Flags: MessageFlagMORE}
    if err := writeFrame(ring, fh1, part1); err != nil {
        t.Fatalf("writeFrame part1: %v", err)
    }
    fh2 := FrameHeader{StreamID: 3, Type: FrameTypeMESSAGE, Flags: 0}
    if err := writeFrame(ring, fh2, part2); err != nil {
        t.Fatalf("writeFrame part2: %v", err)
    }

    gfh1, gp1, err := readFrame(ring)
    if err != nil { t.Fatalf("readFrame part1: %v", err) }
    if gfh1.Type != FrameTypeMESSAGE || gfh1.StreamID != 3 || gfh1.Flags&MessageFlagMORE == 0 {
        t.Fatalf("unexpected fh1: %+v", gfh1)
    }
    gfh2, gp2, err := readFrame(ring)
    if err != nil { t.Fatalf("readFrame part2: %v", err) }
    if gfh2.Type != FrameTypeMESSAGE || gfh2.StreamID != 3 || gfh2.Flags&MessageFlagMORE != 0 {
        t.Fatalf("unexpected fh2: %+v", gfh2)
    }
    got := append(gp1, gp2...)
    if len(got) != len(orig) {
        t.Fatalf("reassembled len mismatch: got=%d want=%d", len(got), len(orig))
    }
    for i := range got {
        if got[i] != orig[i] {
            t.Fatalf("reassembled mismatch at %d", i)
        }
    }
}

func TestWriteFrame_PadAlignmentHandled(t *testing.T) {
    name := fmt.Sprintf("frame-pad-%d", time.Now().UnixNano())
    seg, err := CreateSegment(name, 4096, 4096)
    if err != nil {
        t.Fatalf("CreateSegment error: %v", err)
    }
    defer seg.Close()
    ring := NewShmRingFromSegment(seg.A, seg.Mem)

    cap := ring.Capacity()
    // Step write index so that next header would fall with remaining < 16
    // We start at w=0; write one MESSAGE frame with payloadLen = cap-26 so that
    // (16 + payloadLen) % cap = cap-10 -> remaining=10
    payload1 := make([]byte, cap-26)
    fh1 := FrameHeader{StreamID: 9, Type: FrameTypeMESSAGE}
    if err := writeFrame(ring, fh1, payload1); err != nil {
        t.Fatalf("writeFrame payload1: %v", err)
    }

    // Read first frame (big message) before writing the next one to avoid backpressure deadlock.
    if _, _, err := readFrame(ring); err != nil {
        t.Fatalf("readFrame 1: %v", err)
    }

    // Now write a small HEADERS frame; writeFrame should insert PAD internally
    hdrPayload := encodeHeaders(HeadersV1{Version:1, HdrType:0, Method:"/x.y/z", Authority:"a"})
    fh2 := FrameHeader{StreamID: 9, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL}
    if err := writeFrame(ring, fh2, hdrPayload); err != nil {
        t.Fatalf("writeFrame headers after wrap: %v", err)
    }
    // Next non-PAD frame should be HEADERS (PAD is skipped by readFrame)
    gotFH, _, err := readFrame(ring)
    if err != nil {
        t.Fatalf("readFrame 2: %v", err)
    }
    if gotFH.Type != FrameTypeHEADERS {
        t.Fatalf("expected HEADERS after PAD skip, got type=%v", gotFH.Type)
    }
}
