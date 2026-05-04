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
	"context"
	"fmt"
	"testing"
	"time"
)

// TestH2NegotiatedDial verifies that when both client and server advertise
// HTTP/2, the negotiated wire format is propagated to both sides' data rings
// and a ping-pong RPC succeeds.
func TestH2NegotiatedDial(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer testCancel()

	name := fmt.Sprintf("h2neg-%d", time.Now().UnixNano())
	addrStr := fmt.Sprintf("shm://%s?cap=65536", name)

	lis, err := newShmServerFactory(addrStr)
	if err != nil {
		t.Fatalf("server factory: %v", err)
	}
	lis.SetSupportedWireFormats([]WireFormat{WireFormatHTTP2, WireFormatCustom16})
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

		// Server's data rings should be H2-mode after negotiation.
		if got := conn.ReadRing().WireFormat(); got != WireFormatHTTP2 {
			t.Errorf("server readRing wire: got %v want H2", got)
		}
		if got := conn.WriteRing().WireFormat(); got != WireFormatHTTP2 {
			t.Errorf("server writeRing wire: got %v want H2", got)
		}

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

	// Client dials with H2 advertised first.
	enableClientReader.Store(false)
	defer enableClientReader.Store(true)

	addr, err := ParseAddress(addrStr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts := &DialOptions{
		SegmentSize:          DefaultSegmentSize,
		RingASize:            addr.Cap,
		RingBSize:            addr.Cap,
		ConnectTimeout:       5 * time.Second,
		SupportedWireFormats: []WireFormat{WireFormatHTTP2, WireFormatCustom16},
	}
	ctIface, err := DialShm(testCtx, addr.Name, opts)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ct := ctIface.(*ShmClientTransport)
	defer ct.Close(fmt.Errorf("test done"))

	// Client's data rings should be H2-mode after negotiation.
	if got := ct.clientToServer.WireFormat(); got != WireFormatHTTP2 {
		t.Errorf("client clientToServer wire: got %v want H2", got)
	}
	if got := ct.serverToClient.WireFormat(); got != WireFormatHTTP2 {
		t.Errorf("client serverToClient wire: got %v want H2", got)
	}

	// Send headers + message; read server response frames.
	streamID := uint32(1)
	if err := writeFrame(testCtx, ct.clientToServer, FrameHeader{
		StreamID: streamID, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL,
	}, encodeHeaders(HeadersV1{Version: 1, HdrType: 0, Method: "/svc/Hi"})); err != nil {
		t.Fatalf("client write headers: %v", err)
	}
	payload := []byte("hello h2 world")
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
			tr, err := decodeTrailers(body)
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

// TestH2FallbackToCustom16 verifies that a client advertising H2 against a
// server that does NOT advertise H2 falls back to Custom16 successfully.
func TestH2FallbackToCustom16(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer testCancel()

	name := fmt.Sprintf("h2fb-%d", time.Now().UnixNano())
	addrStr := fmt.Sprintf("shm://%s?cap=65536", name)

	lis, err := newShmServerFactory(addrStr)
	if err != nil {
		t.Fatalf("server factory: %v", err)
	}
	// Note: NOT calling SetSupportedWireFormats — server defaults to
	// Custom16 only.
	defer lis.Close()

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
		// Server should fall back to Custom16.
		if got := conn.ReadRing().WireFormat(); got != WireFormatCustom16 {
			t.Errorf("server wire: got %v want C16 (fallback)", got)
		}
		// Drain one frame to keep the goroutine alive a moment.
		_, _, _ = readFrame(testCtx, conn.ReadRing())
	}()

	enableClientReader.Store(false)
	defer enableClientReader.Store(true)

	addr, _ := ParseAddress(addrStr)
	opts := &DialOptions{
		SegmentSize:          DefaultSegmentSize,
		RingASize:            addr.Cap,
		RingBSize:            addr.Cap,
		ConnectTimeout:       5 * time.Second,
		SupportedWireFormats: []WireFormat{WireFormatHTTP2}, // client wants H2
	}
	ctIface, err := DialShm(testCtx, addr.Name, opts)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ct := ctIface.(*ShmClientTransport)
	defer ct.Close(fmt.Errorf("test done"))

	// Should fall back to Custom16 because server doesn't advertise H2.
	if got := ct.clientToServer.WireFormat(); got != WireFormatCustom16 {
		t.Errorf("client wire: got %v want C16 (fallback)", got)
	}

	// Send a single frame to wake the server.
	_ = writeFrame(testCtx, ct.clientToServer, FrameHeader{
		StreamID: 1, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL,
	}, encodeHeaders(HeadersV1{Version: 1, HdrType: 0, Method: "/svc/X"}))
	<-serverDone
}
