//go:build linux || windows

/*
 *
 * Copyright 2026 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package transport

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc/mem"
)

// TestShmDial_E2E verifies a basic ping-pong RPC over the shared-memory
// transport via the public dial / accept paths. The on-ring wire format
// is HTTP/2 (the only supported format).
func TestShmDial_E2E(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer testCancel()

	name := fmt.Sprintf("h2neg-%d", time.Now().UnixNano())
	addrStr := fmt.Sprintf("shm://%s?cap=65536", name)

	lis, err := newShmServerFactory(addrStr)
	if err != nil {
		t.Fatalf("server factory: %v", err)
	}
	defer lis.Close()

	// Server responder goroutine.
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		c, err := lis.Accept()
		if err != nil {
			t.Errorf("server accept: %v", err)
			return
		}
		defer c.Close()
		conn := c.(*shmConn)

		// Echo headers + message + trailers.
		fh, _, err := readFrame(testCtx, conn.ReadRing())
		if err != nil {
			t.Errorf("server read headers: %v", err)
			return
		}
		if fh.Type != FrameTypeHEADERS {
			t.Errorf("server: expected HEADERS, got %v", fh.Type)
			return
		}
		fhMsg, msg, err := readFrame(testCtx, conn.ReadRing())
		if err != nil {
			t.Errorf("server read msg: %v", err)
			return
		}
		_ = writeFrame(testCtx, conn.WriteRing(), FrameHeader{
			StreamID: fh.StreamID, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL,
		}, encodeHeaders(HeadersV1{Version: 1, HdrType: 1}))
		_ = writeFrame(testCtx, conn.WriteRing(), FrameHeader{
			StreamID: fhMsg.StreamID, Type: FrameTypeMESSAGE,
		}, msg)
		_ = writeFrame(testCtx, conn.WriteRing(), FrameHeader{
			StreamID: fhMsg.StreamID, Type: FrameTypeTRAILERS, Flags: TrailersFlagEndStream,
		}, encodeTrailers(TrailersV1{Version: 1, GRPCStatusCode: 0}))
	}()

	// Client dials.
	enableClientReader.Store(false)
	defer enableClientReader.Store(true)

	addr, err := ParseAddress(addrStr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts := &DialOptions{
		SegmentSize:    DefaultSegmentSize,
		RingASize:      addr.Cap,
		RingBSize:      addr.Cap,
		ConnectTimeout: 5 * time.Second,
	}
	ctIface, err := DialShm(testCtx, addr.Name, opts)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ct := ctIface.(*ShmClientTransport)
	defer ct.Close(fmt.Errorf("test done"))

	// Send headers + message; read server response frames.
	streamID := uint32(1)
	if err := writeFrame(testCtx, ct.clientToServer, FrameHeader{
		StreamID: streamID, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL,
	}, encodeHeaders(HeadersV1{Version: 1, HdrType: 0, Method: "/svc/Hi"})); err != nil {
		t.Fatalf("client write headers: %v", err)
	}
	// MESSAGE bodies must include the gRPC LPM prefix (5 bytes) so the
	// H2 reader's lpmAccumulator can identify a complete message.
	body := []byte("hello h2 world")
	payload := make([]byte, 5+len(body))
	payload[0] = 0
	binary.BigEndian.PutUint32(payload[1:5], uint32(len(body)))
	copy(payload[5:], body)
	if err := writeFrame(testCtx, ct.clientToServer, FrameHeader{
		StreamID: streamID, Type: FrameTypeMESSAGE,
	}, payload); err != nil {
		t.Fatalf("client write msg: %v", err)
	}

	// Read 3 response frames: HEADERS, MESSAGE, TRAILERS.
	for i := 0; i < 3; i++ {
		fh, body, err := readFrame(testCtx, ct.serverToClient)
		if err != nil {
			t.Fatalf("client read[%d]: %v", i, err)
		}
		switch fh.Type {
		case FrameTypeHEADERS:
			// Server-initial; nothing to verify deeply here.
		case FrameTypeMESSAGE:
			if string(body) != string(payload) {
				t.Errorf("echo mismatch: got %q want %q", body, payload)
			}
		case FrameTypeTRAILERS:
			tr, err := takeOrDecodeTrailers(ct.serverToClient.h2Decoder(), body)
			if err != nil {
				t.Fatalf("decodeTrailers: %v", err)
			}
			if tr.GRPCStatusCode != 0 {
				t.Errorf("status: got %d want 0", tr.GRPCStatusCode)
			}
		default:
			t.Errorf("unexpected frame %v", fh.Type)
		}
	}
	<-serverDone
}

// TestH2ClientStreaming_TwoMessagesBeforeEndStream verifies the
// codec's MORE-flag derivation is wired correctly end-to-end through
// the production server transport: a client sends two complete LPMs
// with END_STREAM on the LAST DATA frame (the canonical shape from
// stock grpc-go HTTP/2 / grpc-java / grpc-c++) and the server-side
// ring reader returns BOTH messages with MORE flags such that
// ShmServerTransport.handleMessage's MORE=0 EOF logic fires only
// after the second message.
//
// Pre-fix the codec emitted MORE=0 on every MESSAGE so the server
// transport observed io.EOF after the FIRST message and silently
// dropped the second.
func TestH2ClientStreaming_TwoMessagesBeforeEndStream(t *testing.T) {
	segName := fmt.Sprintf("h2cs-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, 1<<20, 1<<20)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Build two LPMs; emit them as DATA[lpm1] (no END_STREAM) +
	// DATA[lpm2] (END_STREAM). This is the shape stock grpc-go's
	// HTTP/2 transport emits for a 2-message client-streaming send.
	body1 := []byte("first")
	body2 := []byte("second")
	build := func(b []byte) []byte {
		out := make([]byte, 5+len(b))
		out[0] = 0
		binary.BigEndian.PutUint32(out[1:5], uint32(len(b)))
		copy(out[5:], b)
		return out
	}
	lpm1 := build(body1)
	lpm2 := build(body2)

	// Inject DATA #1 (no END_STREAM).
	{
		var hdr [h2FrameHeaderSize]byte
		encodeH2FrameHeaderTo(&hdr, H2FrameHeader{
			Length: uint32(len(lpm1)), Type: H2FrameDATA, Flags: 0, StreamID: 1,
		})
		res, _ := tx.ReserveWrite(ctx, h2FrameHeaderSize+len(lpm1))
		copy(res.First, hdr[:])
		copy(res.First[h2FrameHeaderSize:], lpm1)
		_ = res.Commit(h2FrameHeaderSize + len(lpm1))
	}
	// Inject DATA #2 with END_STREAM.
	{
		var hdr [h2FrameHeaderSize]byte
		encodeH2FrameHeaderTo(&hdr, H2FrameHeader{
			Length: uint32(len(lpm2)), Type: H2FrameDATA, Flags: H2FlagEndStream, StreamID: 1,
		})
		res, _ := tx.ReserveWrite(ctx, h2FrameHeaderSize+len(lpm2))
		copy(res.First, hdr[:])
		copy(res.First[h2FrameHeaderSize:], lpm2)
		_ = res.Commit(h2FrameHeaderSize + len(lpm2))
	}

	// First read: MESSAGE lpm1, MORE=1 (no END_STREAM on source DATA).
	fh1, got1, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame 1: %v", err)
	}
	if fh1.Type != FrameTypeMESSAGE || !bytes.Equal(got1, lpm1) {
		t.Fatalf("first frame: type=%d body=%q", fh1.Type, got1)
	}
	if fh1.Flags&MessageFlagMORE == 0 {
		t.Errorf("first frame flags: got MORE=0, want MORE=1 (more frames coming)")
	}

	// Second read: MESSAGE lpm2, MORE=0 (END_STREAM source).
	fh2, got2, err := readFrame(ctx, rx)
	if err != nil {
		t.Fatalf("readFrame 2: %v", err)
	}
	if fh2.Type != FrameTypeMESSAGE || !bytes.Equal(got2, lpm2) {
		t.Fatalf("second frame: type=%d body=%q", fh2.Type, got2)
	}
	if fh2.Flags&MessageFlagMORE != 0 {
		t.Errorf("second frame flags: got MORE=1, want MORE=0 (last message)")
	}
}

// TestH2ServerTransport_HeadersEndStream_HandlerSeesEOF drives the
// fix-2 codepath end-to-end through the production
// ShmServerTransport.processIncomingData dispatcher: an H2 client
// that sends a single HEADERS frame with END_STREAM (zero-message
// client streaming, the canonical "no request payload" shape) must
// surface BOTH the new stream to the server's RPC handler AND
// io.EOF on the handler's first Read so it can transition to
// sending the response without hanging.
//
// Pre-fix behavior: the codec emitted only FrameTypeHEADERS and
// the handler's recv buffer never received io.EOF, so any code
// expecting "client done sending" semantics (e.g., grpc.Server's
// processStreamingRPC reading the request before invoking the
// service handler) blocked indefinitely until the deadline.
//
// This test sits one layer above the codec-level
// TestH2InitialHeadersEndStream_EmitsHalfClose: it verifies the
// synthetic FrameTypeHALFCLOSE actually drives recvMsg{err:
// io.EOF} into the stream's receive buffer through the real
// ShmServerTransport dispatch loop, including the cachedStream
// fast path.
func TestH2ServerTransport_HeadersEndStream_HandlerSeesEOF(t *testing.T) {
	segName := fmt.Sprintf("h2srveof-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, 1<<20, 1<<20)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	seg.H.SetServerReady(true)

	srvAddr := &ShmAddr{Name: segName + "_s"}
	cliAddr := &ShmAddr{Name: segName + "_c"}
	st, err := NewShmServerTransport(seg, srvAddr, cliAddr)
	if err != nil {
		t.Fatalf("NewShmServerTransport: %v", err)
	}
	defer st.Close(nil)

	// Force H2 wire on both rings — bypassing the negotiation
	// handshake keeps this test focused on the codec/dispatch
	// integration rather than wire-format selection.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Capture the first Read result observed by the RPC handler.
	type readResult struct {
		data mem.BufferSlice
		err  error
	}
	resCh := make(chan readResult, 1)

	go st.HandleStreams(ctx, func(s *ServerStream) {
		// Single Read should return io.EOF immediately because
		// HEADERS+END_STREAM signals "no request payload, half-close
		// now". A pre-fix transport blocks here until ctx expires.
		buf, err := s.Read(1)
		select {
		case resCh <- readResult{data: buf, err: err}:
		default:
		}
		// Send a minimal status reply so the handler returns and the
		// stream cleans up gracefully (the test doesn't read the
		// response, but writeStatus has its own ring write so it must
		// succeed without panicking).
		_ = st.writeStatus(s, nil)
	})

	// Inject a single HEADERS frame with END_STREAM containing the
	// minimal pseudo-headers gRPC requires for a server to accept the
	// request. HPACK-encoded so the H2 codec can decode it through
	// the production hpackDecoderHolder.
	hpackBlock := hpackEncodeForTest(t,
		hpack.HeaderField{Name: ":method", Value: "POST"},
		hpack.HeaderField{Name: ":scheme", Value: "http"},
		hpack.HeaderField{Name: ":path", Value: "/svc/EmptyReq"},
		hpack.HeaderField{Name: ":authority", Value: "localhost"},
		hpack.HeaderField{Name: "te", Value: "trailers"},
		hpack.HeaderField{Name: "content-type", Value: "application/grpc"},
	)
	var h2hdr [h2FrameHeaderSize]byte
	encodeH2FrameHeaderTo(&h2hdr, H2FrameHeader{
		Length:   uint32(len(hpackBlock)),
		Type:     H2FrameHEADERS,
		Flags:    H2FlagEndHeaders | H2FlagEndStream,
		StreamID: 1,
	})
	res, err := st.clientToServer.ReserveWrite(ctx, h2FrameHeaderSize+len(hpackBlock))
	if err != nil {
		t.Fatalf("ReserveWrite: %v", err)
	}
	copy(res.First, h2hdr[:])
	copy(res.First[h2FrameHeaderSize:], hpackBlock)
	if err := res.Commit(h2FrameHeaderSize + len(hpackBlock)); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	select {
	case r := <-resCh:
		if r.data != nil {
			r.data.Free()
		}
		if !errors.Is(r.err, io.EOF) {
			t.Fatalf("handler Read err: got %v want io.EOF", r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not observe io.EOF within 3s — server is hanging on a half-close that never arrived")
	}
}
