//go:build linux || windows

/*
 *
 * Copyright 2025 gRPC authors.
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
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ShmUnaryClient is a minimal unary client built over SMF v1 and ShmRings.
// It is intended for step-3 bring-up and tests; it does not wire into the
// grpc-go transport.ClientTransport yet.
type ShmUnaryClient struct {
	seg *Segment
	tx  *ShmRing // client -> server
	rx  *ShmRing // server -> client

	nextID   uint32 // odd stream IDs
	streams  map[uint32]*unaryStream
	streamsM sync.Mutex

	writeMu sync.Mutex // serialize frame writes to preserve ordering

	readerOnce sync.Once
	readerDone chan struct{}
	// readerStarted is set inside readerOnce.Do BEFORE the goroutine
	// spawn. Close() reads it to decide whether to wait on
	// readerDone; if startReader was never called, the channel will
	// never close and an unconditional wait would hang Close forever.
	readerStarted atomic.Bool
	closed     atomic.Bool

	// Windows event handles for cross-mapping synchronization
	txEvents *RingEvents
	rxEvents *RingEvents
}

type unaryStream struct {
	hdrCh     chan HeadersV1
	msgCh     chan []byte // delivers exactly one complete gRPC message payload
	trCh      chan TrailersV1
	errCh     chan error
	completed atomic.Bool
}

// NewShmUnaryClient constructs a unary client over an existing segment.
func NewShmUnaryClient(seg *Segment) *ShmUnaryClient {
	segmentName := extractSegmentName(seg.Path)

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.B, seg.Mem)
	seg.RegisterRing(tx)
	seg.RegisterRing(rx)

	// Open events for cross-mapping synchronization (Windows).
	// Client opens events created by server. On Linux, these are no-ops.
	txEvents, _ := OpenRingEvents(segmentName, "A")
	rxEvents, _ := OpenRingEvents(segmentName, "B")

	// Attach events to rings
	tx.SetEvents(txEvents)
	rx.SetEvents(rxEvents)

	c := &ShmUnaryClient{
		seg:        seg,
		tx:         tx,
		rx:         rx,
		nextID:     1,
		streams:    make(map[uint32]*unaryStream),
		readerDone: make(chan struct{}),
		txEvents:   txEvents,
		rxEvents:   rxEvents,
	}
	return c
}

// Close closes the client and underlying segment mapping.
func (c *ShmUnaryClient) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}

	// Close the rx ring to unblock the reader
	_ = c.rx.Close()

	// Unblock the long-lived reader goroutine if it's parked in the
	// per-data-segment eventfd Wait. Closes the eventfd so the Read
	// returns EBADF and the reader exits its outer loop. No-op when
	// the eventfd waker is disabled (see ConfigureShmEventfdWakerForBench).
	if c.seg != nil {
		c.seg.UnblockSameSideParkers()
	}

	// Wait for reader goroutine to exit before closing segment.
	// BUG FIX (GPT-5.5 bug hunt): only wait if startReader was
	// actually called — otherwise readerDone is never closed and
	// Close hangs forever (e.g., NewShmUnaryClient(seg); client.Close()
	// with no intervening UnaryCall).
	if c.readerStarted.Load() {
		<-c.readerDone
	}

	// Close the named events (Windows)
	if c.txEvents != nil {
		c.txEvents.Close()
	}
	if c.rxEvents != nil {
		c.rxEvents.Close()
	}

	return c.seg.Close()
}

// startReader starts the event-driven frame reader once with a client-level context.
func (c *ShmUnaryClient) startReader() {
	c.readerOnce.Do(func() {
		c.readerStarted.Store(true)
		go func() {
			defer close(c.readerDone)

			// Single-threaded demux of frames.
			// Use background context since reader should run until client is closed
			ctx := context.Background()
			shmDebugf("Client: reader goroutine starting")
			for !c.closed.Load() {
				shmDebugf("Client: reader attempting to read frame...")
				fh, payload, err := readFrame(ctx, c.rx)
				if err != nil {
					shmDebugf("Client: reader got error: %v", err)
					// Context cancelled or ring closed
					return
				}
				shmDebugf("Client: reader got frame type %d, streamID %d, payloadLen %d", fh.Type, fh.StreamID, len(payload))
				switch fh.Type {
				case FrameTypeHEADERS:
					shmDebugf("Client: reader dispatching HEADERS for stream %d", fh.StreamID)
					// PR #10: prefer the codec-stashed HeadersV1 struct.
					hdr, err := takeOrDecodeHeaders(c.rx.h2Decoder(), payload)
					c.dispatchHeaders(fh.StreamID, hdr, err)
				case FrameTypeMESSAGE:
					shmDebugf("Client: reader dispatching MESSAGE for stream %d", fh.StreamID)
					// Deliver raw bytes (includes 5-byte gRPC prefix)
					c.dispatchMessage(fh.StreamID, payload)
				case FrameTypeTRAILERS:
					shmDebugf("Client: reader dispatching TRAILERS for stream %d", fh.StreamID)
					tr, err := takeOrDecodeTrailers(c.rx.h2Decoder(), payload)
					c.dispatchTrailers(fh.StreamID, tr, err)
				case FrameTypePING:
					shmDebugf("Client: reader handling PING for stream %d", fh.StreamID)
					// Immediately reply with PONG
					c.writeMu.Lock()
					_ = writeFrame(ctx, c.tx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypePONG}, payload)
					c.writeMu.Unlock()
				case FrameTypeGOAWAY:
					shmDebugf("Client: reader ignoring GOAWAY")
					// Ignore in unary bring-up
				default:
					shmDebugf("Client: reader ignoring unknown frame type %d", fh.Type)
				}
			}
		}()
	})
}

func (c *ShmUnaryClient) dispatchHeaders(id uint32, h HeadersV1, err error) {
	shmDebugf("Client: dispatchHeaders called for stream %d, err=%v", id, err)
	c.streamsM.Lock()
	s := c.streams[id]
	c.streamsM.Unlock()
	if s == nil {
		shmDebugf("Client: dispatchHeaders - no stream found for id %d", id)
		return
	}
	if err != nil {
		shmDebugf("Client: dispatchHeaders - sending error to errCh: %v", err)
		select {
		case s.errCh <- err:
		default:
		}
		return
	}
	shmDebugf("Client: dispatchHeaders - sending headers to hdrCh")
	select {
	case s.hdrCh <- h:
		shmDebugf("Client: dispatchHeaders - headers sent successfully")
	default:
		shmDebugf("Client: dispatchHeaders - hdrCh was full or closed")
	}
}

func (c *ShmUnaryClient) dispatchMessage(id uint32, p []byte) {
	c.streamsM.Lock()
	s := c.streams[id]
	c.streamsM.Unlock()
	if s == nil {
		return
	}
	// Callers (this test-only unary client) operate on the full LPM-
	// prefixed MESSAGE body — the same bytes the producer constructed
	// (compressed flag + length + body). The H2 codec preserves these
	// bytes verbatim on the wire, so we hand the payload through as-is.
	select {
	case s.msgCh <- append([]byte(nil), p...):
	default:
	}
}

func (c *ShmUnaryClient) dispatchTrailers(id uint32, tr TrailersV1, err error) {
	c.streamsM.Lock()
	s := c.streams[id]
	delete(c.streams, id)
	c.streamsM.Unlock()
	if s == nil {
		return
	}
	if err != nil {
		select {
		case s.errCh <- err:
		default:
		}
		return
	}
	s.completed.Store(true)
	select {
	case s.trCh <- tr:
	default:
	}
}

// UnaryCall sends a unary request and waits for the unary response.
// payload must contain the 5-byte gRPC message prefix and message bytes.
func (c *ShmUnaryClient) UnaryCall(ctx context.Context, method, authority string, md []KV, payload []byte) (HeadersV1, []byte, TrailersV1, error) {
	if c.closed.Load() {
		return HeadersV1{}, nil, TrailersV1{}, errors.New("closed")
	}

	c.startReader()

	// Allocate odd stream ID.
	id := atomic.AddUint32(&c.nextID, 2) - 2
	if id == 0 {
		id = 1
	}

	s := &unaryStream{
		hdrCh: make(chan HeadersV1, 1),
		msgCh: make(chan []byte, 1),
		trCh:  make(chan TrailersV1, 1),
		errCh: make(chan error, 1),
	}
	c.streamsM.Lock()
	c.streams[id] = s
	c.streamsM.Unlock()

	// === BEGIN NEW: per-call CANCEL sender goroutine ===
	var sendCancelOnce sync.Once
	done := make(chan struct{})

	sendCancel := func(reason error) {
		// Only send once
		sendCancelOnce.Do(func() {
			shmDebugf("Client: sendCancel called with reason: %v", reason)

			// Check if client is already closed to avoid use-after-free
			if c.closed.Load() {
				shmDebugf("Client: sendCancel - client already closed, returning")
				return
			}

			shmDebugf("Client: sendCancel - acquiring write mutex")
			// Best-effort CANCEL write with a bounded context
			c.writeMu.Lock()
			defer c.writeMu.Unlock()

			// Double-check after acquiring lock
			if c.closed.Load() {
				shmDebugf("Client: sendCancel - client closed after acquiring lock, returning")
				return
			}

			shmDebugf("Client: sendCancel - attempting to write CANCEL frame for stream %d", id)
			cancelCtx, cancelFn := context.WithTimeout(context.Background(), 200*time.Millisecond)
			errCancel := writeFrame(cancelCtx, c.tx, FrameHeader{StreamID: id, Type: FrameTypeCANCEL}, []byte{1})
			cancelFn()

			if errCancel != nil {
				shmDebugf("Client: sendCancel - writeFrame failed: %v, closing tx ring as fallback", errCancel)
				// As a last resort, close the client->server ring to wake the server
				closeErr := c.tx.Close()
				shmDebugf("Client: sendCancel - tx.Close() returned: %v", closeErr)
			} else {
				shmDebugf("Client: sendCancel - CANCEL frame written successfully")
			}

			shmDebugf("Client: sendCancel - removing stream %d from streams map", id)
			// Remove this stream so any late dispatches are ignored
			c.streamsM.Lock()
			delete(c.streams, id)
			c.streamsM.Unlock()

			shmDebugf("Client: sendCancel - signaling error channels")
			// Unblock any waiters on this client-side unary future
			select {
			case s.errCh <- reason:
				shmDebugf("Client: sendCancel - sent reason to errCh")
			default:
				shmDebugf("Client: sendCancel - errCh was full or closed")
			}
			select {
			case s.hdrCh <- HeadersV1{}:
				shmDebugf("Client: sendCancel - sent empty headers to hdrCh")
			default:
				shmDebugf("Client: sendCancel - hdrCh was full or closed")
			}
			select {
			case s.msgCh <- nil:
				shmDebugf("Client: sendCancel - sent nil to msgCh")
			default:
				shmDebugf("Client: sendCancel - msgCh was full or closed")
			}
			select {
			case s.trCh <- TrailersV1{}:
				shmDebugf("Client: sendCancel - sent empty trailers to trCh")
			default:
				shmDebugf("Client: sendCancel - trCh was full or closed")
			}
			shmDebugf("Client: sendCancel - completed successfully")
		})
	}

	go func() {
		shmDebugf("Client: cancel goroutine started for stream %d", id)

		// Event-driven: block on done or ctx.Done() channels
		// No polling needed - ctx.Done() unblocks immediately when context is cancelled
		for {
			select {
			case <-done:
				shmDebugf("Client: cancel goroutine - received done signal for stream %d", id)
				return
			case <-ctx.Done():
				shmDebugf("Client: cancel goroutine - context cancelled for stream %d: %v", id, ctx.Err())
				sendCancel(ctx.Err())
				return
			}
		}
	}()
	// === END NEW ===

	// Send HEADERS. Populate deadline hint if present.
	hdr := HeadersV1{Version: 1, HdrType: 0, Method: method, Authority: authority, Metadata: md}
	if dl, ok := ctx.Deadline(); ok {
		hdr.DeadlineUnixNano = uint64(dl.UnixNano())
	}
	hbytes := encodeHeaders(hdr)
	shmDebugf("Client: about to send HEADERS frame for stream %d", id)
	c.writeMu.Lock()
	if err := writeFrame(ctx, c.tx, FrameHeader{StreamID: id, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL}, hbytes); err != nil {
		c.writeMu.Unlock()
		shmDebugf("Client: HEADERS write failed for stream %d: %v", id, err)
		close(done) // tell cancel goroutine to exit
		return HeadersV1{}, nil, TrailersV1{}, err
	}
	shmDebugf("Client: HEADERS sent successfully for stream %d", id)
	// Send MESSAGE (single frame for unary). The caller is responsible
	// for constructing a gRPC LPM-prefixed body (1-byte compressed flag
	// + 4-byte big-endian length + body) — all callers in this test
	// package already do so. We pass the bytes through verbatim; the
	// H2 codec on the receive side hands the same bytes back to
	// dispatchMessage.
	//
	// Set MessageFlagEndStream so the H2 codec emits the H2
	// END_STREAM flag on the DATA frame. On the receive side this
	// becomes MORE=0 on the surfaced MESSAGE FrameHeader, signalling
	// "this is the only / last request frame" to a peer that breaks
	// its receive loop on MORE=0. Without this, single-frame unary
	// requests look identical to "more frames will follow" and
	// servers loop forever waiting for the next frame.
	shmDebugf("Client: about to send MESSAGE frame for stream %d", id)
	if err := writeFrame(ctx, c.tx, FrameHeader{StreamID: id, Type: FrameTypeMESSAGE, Flags: MessageFlagEndStream}, payload); err != nil {
		c.writeMu.Unlock()
		shmDebugf("Client: MESSAGE write failed for stream %d: %v", id, err)
		close(done)
		return HeadersV1{}, nil, TrailersV1{}, err
	}
	shmDebugf("Client: MESSAGE sent successfully for stream %d", id)
	c.writeMu.Unlock()

	// FAST-PATH: if ctx is already done, send CANCEL immediately
	if err := ctx.Err(); err != nil {
		shmDebugf("Client: context already done after sending frames for stream %d: %v", id, err)
		close(done) // tell cancel goroutine to exit immediately
		sendCancel(err)
		return HeadersV1{}, nil, TrailersV1{}, err
	}

	shmDebugf("Client: starting response wait loop for stream %d", id)
	var rh HeadersV1
	var rm []byte
	var rt TrailersV1
	haveMsg, haveTr := false, false

	for {
		if haveMsg && haveTr {
			shmDebugf("Client: success path - have both message and trailers for stream %d", id)
			close(done) // success path: stop the cancel goroutine
			return rh, rm, rt, nil
		}
		select {
		case <-ctx.Done():
			shmDebugf("Client: context done in wait loop for stream %d: %v", id, ctx.Err())
			sendCancel(ctx.Err())
			close(done)
			return HeadersV1{}, nil, TrailersV1{}, ctx.Err()
		case e := <-s.errCh:
			shmDebugf("Client: received error from errCh for stream %d: %v", id, e)
			close(done)
			return HeadersV1{}, nil, TrailersV1{}, e
		case h := <-s.hdrCh:
			shmDebugf("Client: received headers for stream %d", id)
			rh = h
		case m := <-s.msgCh:
			shmDebugf("Client: received message for stream %d (len=%d)", id, len(m))
			rm = append([]byte(nil), m...)
			haveMsg = true
		case tr := <-s.trCh:
			shmDebugf("Client: received trailers for stream %d", id)
			rt = tr
			haveTr = true
		}
	}
}

// stripLPMHeader removes the 5-byte gRPC length-prefixed-message header
// from a MESSAGE frame body. Returns the body bytes and true on success;
// returns (nil, false) if p is too short or the declared length does not
// match the actual body length.
//
// Used by the test-only ShmUnaryClient / ShmStreamingClient /
// ShmStreamingServer helpers in this package to unwrap MESSAGE bodies on
// receipt. The H2 codec preserves the gRPC LPM 5-byte prefix when it
// reassembles a MESSAGE frame, so test helpers that operate on raw
// user-level payload bytes must strip the prefix before passing data
// to the test.
func stripLPMHeader(p []byte) ([]byte, bool) {
	if len(p) < 5 {
		return nil, false
	}
	declared := binary.BigEndian.Uint32(p[1:5])
	if int(declared) != len(p)-5 {
		return nil, false
	}
	return p[5:], true
}
