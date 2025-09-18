//go:build linux && (amd64 || arm64)

package shm

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Test a unary server listener + transport handling one unary RPC using SMF v1.
func TestShmServerUnary_Echo(t *testing.T) {
	t.Skip("SHM server listener end-to-end requires client factory (Step 5);")
	url := fmt.Sprintf("shm://unary-%d?cap=65536", time.Now().UnixNano())
	lis, err := NewURLListener(url)
	if err != nil {
		t.Fatalf("NewURLListener: %v", err)
	}
	defer lis.Close()

	// Accept one connection and serve unary echo over frames.
	served := make(chan struct{})
	go func() {
		defer close(served)
		conn, err := lis.Accept()
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		sc, ok := conn.(*shmConn)
		if !ok {
			t.Errorf("unexpected conn type")
			return
		}
		st := sc.transport

		// Read initial HEADERS and MESSAGE from client
		fh, pl, err := readFrame(st.clientToServer)
		if err != nil {
			t.Errorf("server read headers: %v", err)
			return
		}
		if fh.Type != FrameTypeHEADERS {
			t.Errorf("expected HEADERS, got %v", fh.Type)
			return
		}
		if _, err := decodeHeaders(pl); err != nil {
			t.Errorf("decode headers: %v", err)
			return
		}

		fh2, msg, err := readFrame(st.clientToServer)
		if err != nil {
			t.Errorf("server read message: %v", err)
			return
		}
		if fh2.Type != FrameTypeMESSAGE {
			t.Errorf("expected MESSAGE, got %v", fh2.Type)
			return
		}

		// Send server HEADERS, then echo MESSAGE, then TRAILERS OK
		h := HeadersV1{Version: 1, HdrType: 1, Metadata: []KV{{Key: "x-srv", Values: [][]byte{[]byte("ok")}}}}
		if err := writeFrame(st.serverToClient, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL}, encodeHeaders(h)); err != nil {
			t.Errorf("server write headers: %v", err)
			return
		}
		if err := writeFrame(st.serverToClient, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeMESSAGE}, msg); err != nil {
			t.Errorf("server write message: %v", err)
			return
		}
		tr := TrailersV1{Version: 1, GRPCStatusCode: 0}
		if err := writeFrame(st.serverToClient, FrameHeader{StreamID: fh.StreamID, Type: FrameTypeTRAILERS, Flags: TrailersFlagEND_STREAM}, encodeTrailers(tr)); err != nil {
			t.Errorf("server write trailers: %v", err)
			return
		}
	}()

	// Client: open the pre-created server segment and handshake.
	name := lis.Addr().String()
	seg, err := OpenSegment(name)
	if err != nil {
		t.Fatalf("OpenSegment: %v", err)
	}
	defer seg.Close()
	seg.H.SetClientReady(true)
	ctxWait, cancelWait := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWait()
	if err := seg.WaitForServer(ctxWait); err != nil {
		t.Fatalf("WaitForServer: %v", err)
	}
	cli := NewShmUnaryClient(seg)

	// Prepare a small gRPC-framed message
	payload := make([]byte, 5+3)
	payload[0] = 0
	payload[1] = 3
	payload[2] = 0
	payload[3] = 0
	payload[4] = 0
	copy(payload[5:], []byte("hey"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hdr, msg, tr, err := cli.UnaryCall(ctx, "/echo.Echo/Echo", name, nil, payload)
	if err != nil {
		t.Fatalf("UnaryCall error: %v", err)
	}
	if hdr.HdrType != 1 {
		t.Fatalf("expected server-initial, got %v", hdr.HdrType)
	}
	if tr.GRPCStatusCode != 0 {
		t.Fatalf("expected status OK, got %d", tr.GRPCStatusCode)
	}
	if len(msg) != len(payload) {
		t.Fatalf("len mismatch: %d vs %d", len(msg), len(payload))
	}
	for i := range msg {
		if msg[i] != payload[i] {
			t.Fatalf("data mismatch at %d", i)
		}
	}
	_ = cli.Close()

	<-served
}
