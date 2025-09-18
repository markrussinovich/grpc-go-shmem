//go:build linux && (amd64 || arm64)

package shm

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestUnary_MetadataAndStatus(t *testing.T) {
	name := fmt.Sprintf("md-unary-%d", time.Now().UnixNano())
	seg, err := CreateSegment(name, 65536, 65536)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	// Client metadata with binary keys and duplicates
	cliMD := []KV{
		{Key: "grpc-encoding", Values: [][]byte{[]byte("identity")}},
		{Key: "grpc-accept-encoding", Values: [][]byte{[]byte("gzip,identity")}},
		{Key: "x-foo", Values: [][]byte{[]byte("bar"), []byte("baz")}},
		{Key: "x-foo-bin", Values: [][]byte{{0x00, 0x01, 0xFE}}},
	}

	// Server goroutine validates incoming metadata and echoes it back in headers.
	done := make(chan struct{})
	go func() {
		defer close(done)
		srvRx := NewShmRingFromSegment(seg.A, seg.Mem) // client->server
		srvTx := NewShmRingFromSegment(seg.B, seg.Mem) // server->client

		// Read HEADERS
		fh, pl, err := readFrame(srvRx)
		if err != nil {
			t.Errorf("server read headers: %v", err)
			return
		}
		if fh.Type != FrameTypeHEADERS {
			t.Errorf("expected HEADERS, got %v", fh.Type)
			return
		}
		hdr, err := decodeHeaders(pl)
		if err != nil {
			t.Errorf("decode headers: %v", err)
			return
		}
		if hdr.HdrType != 0 || hdr.Method != "/svc.M/Echo" || hdr.Authority != "example.com" {
			t.Errorf("unexpected request hdr: %+v", hdr)
			return
		}
		if hdr.DeadlineUnixNano == 0 {
			t.Errorf("missing deadline hint")
		}
		// Validate metadata exact bytes
		if !mdEqual(hdr.Metadata, cliMD) {
			t.Errorf("metadata mismatch: got=%+v want=%+v", hdr.Metadata, cliMD)
		}

		// Read MESSAGE
		fh2, msg, err := readFrame(srvRx)
		if err != nil {
			t.Errorf("server read msg: %v", err)
			return
		}
		if fh2.Type != FrameTypeMESSAGE {
			t.Errorf("expected MESSAGE, got %v", fh2.Type)
			return
		}

		// Respond HEADERS echoing metadata back (plus server header)
		echoMD := append([]KV{}, cliMD...)
		echoMD = append(echoMD, KV{Key: "x-srv", Values: [][]byte{[]byte("ok")}})
		respH := HeadersV1{Version: 1, HdrType: 1, Metadata: echoMD}
		if err := writeFrame(srvTx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL}, encodeHeaders(respH)); err != nil {
			t.Errorf("write headers: %v", err)
			return
		}
		// Echo MESSAGE
		if err := writeFrame(srvTx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeMESSAGE}, msg); err != nil {
			t.Errorf("write msg: %v", err)
			return
		}
		// Send TRAILERS with status OK and custom trailer
		tr := TrailersV1{Version: 1, GRPCStatusCode: 0, Metadata: []KV{{Key: "x-tr", Values: [][]byte{[]byte("trail-ok")}}, {Key: "x-tr-bin", Values: [][]byte{{0xEE, 0xFF}}}}}
		if err := writeFrame(srvTx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeTRAILERS, Flags: TrailersFlagEND_STREAM}, encodeTrailers(tr)); err != nil {
			t.Errorf("write trailers: %v", err)
			return
		}
	}()

	client := NewShmUnaryClient(seg)
	// Prepare a message (5-byte prefix + body)
	payload := make([]byte, 5+4)
	payload[0] = 0
	payload[1] = 4
	payload[2] = 0
	payload[3] = 0
	payload[4] = 0
	copy(payload[5:], []byte("ping"))

	// Set deadline to populate deadline hint
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hdr, msg, tr, err := client.UnaryCall(ctx, "/svc.M/Echo", "example.com", cliMD, payload)
	if err != nil {
		t.Fatalf("UnaryCall error: %v", err)
	}

	// Client Header(): check echoed metadata (including binary), and server header
	if !mdContains(hdr.Metadata, cliMD) {
		t.Fatalf("header missing echoed metadata: got=%+v want>=%+v", hdr.Metadata, cliMD)
	}
	if !mdContains(hdr.Metadata, []KV{{Key: "x-srv", Values: [][]byte{[]byte("ok")}}}) {
		t.Fatalf("missing server header x-srv:ok in %v", hdr.Metadata)
	}
	// Response payload matches exactly
	if len(msg) != len(payload) {
		t.Fatalf("payload len mismatch: got=%d want=%d", len(msg), len(payload))
	}
	for i := range msg {
		if msg[i] != payload[i] {
			t.Fatalf("payload mismatch at %d", i)
		}
	}
	// Client Trailer(): OK status and custom trailer keys
	if tr.GRPCStatusCode != 0 {
		t.Fatalf("expected OK status, got %d", tr.GRPCStatusCode)
	}
	if !mdContains(tr.Metadata, []KV{{Key: "x-tr", Values: [][]byte{[]byte("trail-ok")}}}) {
		t.Fatalf("missing trailer x-tr:trail-ok in %v", tr.Metadata)
	}
	if !mdContains(tr.Metadata, []KV{{Key: "x-tr-bin", Values: [][]byte{{0xEE, 0xFF}}}}) {
		t.Fatalf("missing trailer x-tr-bin in %v", tr.Metadata)
	}
	_ = client.Close()

	<-done
}

func mdEqual(a, b []KV) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key {
			return false
		}
		if len(a[i].Values) != len(b[i].Values) {
			return false
		}
		for j := range a[i].Values {
			if string(a[i].Values[j]) != string(b[i].Values[j]) {
				return false
			}
		}
	}
	return true
}

func mdContains(have []KV, want []KV) bool {
	// Treat keys as case-sensitive raw bytes per our encoding, exact match required.
	for _, w := range want {
		found := false
		for _, h := range have {
			if h.Key != w.Key {
				continue
			}
			if len(h.Values) != len(w.Values) {
				continue
			}
			eq := true
			for i := range h.Values {
				if string(h.Values[i]) != string(w.Values[i]) {
					eq = false
					break
				}
			}
			if eq {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
