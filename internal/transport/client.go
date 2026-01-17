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
	"errors"
	"log"
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
	closed     atomic.Bool
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
	c := &ShmUnaryClient{
		seg:        seg,
		tx:         NewShmRingFromSegment(seg.A, seg.Mem),
		rx:         NewShmRingFromSegment(seg.B, seg.Mem),
		nextID:     1,
		streams:    make(map[uint32]*unaryStream),
		readerDone: make(chan struct{}),
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

	// Wait for reader goroutine to exit before closing segment
	<-c.readerDone

	return c.seg.Close()
}

// startReader starts the event-driven frame reader once with a client-level context.
func (c *ShmUnaryClient) startReader() {
	c.readerOnce.Do(func() {
		go func() {
			defer close(c.readerDone)

			// Single-threaded demux of frames.
			// Use background context since reader should run until client is closed
			ctx := context.Background()
			log.Printf("Client: reader goroutine starting")
			for !c.closed.Load() {
				log.Printf("Client: reader attempting to read frame...")
				fh, payload, err := readFrame(ctx, c.rx)
				if err != nil {
					log.Printf("Client: reader got error: %v", err)
					// Context cancelled or ring closed
					return
				}
				log.Printf("Client: reader got frame type %d, streamID %d, payloadLen %d", fh.Type, fh.StreamID, len(payload))
				switch fh.Type {
				case FrameTypeHEADERS:
					log.Printf("Client: reader dispatching HEADERS for stream %d", fh.StreamID)
					hdr, err := decodeHeaders(payload)
					c.dispatchHeaders(fh.StreamID, hdr, err)
				case FrameTypeMESSAGE:
					log.Printf("Client: reader dispatching MESSAGE for stream %d", fh.StreamID)
					// Deliver raw bytes (includes 5-byte gRPC prefix)
					c.dispatchMessage(fh.StreamID, payload)
				case FrameTypeTRAILERS:
					log.Printf("Client: reader dispatching TRAILERS for stream %d", fh.StreamID)
					tr, err := decodeTrailers(payload)
					c.dispatchTrailers(fh.StreamID, tr, err)
				case FrameTypePING:
					log.Printf("Client: reader handling PING for stream %d", fh.StreamID)
					// Immediately reply with PONG
					c.writeMu.Lock()
					_ = writeFrame(ctx, c.tx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypePONG}, payload)
					c.writeMu.Unlock()
				case FrameTypeGOAWAY:
					log.Printf("Client: reader ignoring GOAWAY")
					// Ignore in unary bring-up
				default:
					log.Printf("Client: reader ignoring unknown frame type %d", fh.Type)
				}
			}
		}()
	})
}

func (c *ShmUnaryClient) dispatchHeaders(id uint32, h HeadersV1, err error) {
	log.Printf("Client: dispatchHeaders called for stream %d, err=%v", id, err)
	c.streamsM.Lock()
	s := c.streams[id]
	c.streamsM.Unlock()
	if s == nil {
		log.Printf("Client: dispatchHeaders - no stream found for id %d", id)
		return
	}
	if err != nil {
		log.Printf("Client: dispatchHeaders - sending error to errCh: %v", err)
		select {
		case s.errCh <- err:
		default:
		}
		return
	}
	log.Printf("Client: dispatchHeaders - sending headers to hdrCh")
	select {
	case s.hdrCh <- h:
		log.Printf("Client: dispatchHeaders - headers sent successfully")
	default:
		log.Printf("Client: dispatchHeaders - hdrCh was full or closed")
	}
}

func (c *ShmUnaryClient) dispatchMessage(id uint32, p []byte) {
	c.streamsM.Lock()
	s := c.streams[id]
	c.streamsM.Unlock()
	if s == nil {
		return
	}
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
			log.Printf("Client: sendCancel called with reason: %v", reason)

			// Check if client is already closed to avoid use-after-free
			if c.closed.Load() {
				log.Printf("Client: sendCancel - client already closed, returning")
				return
			}

			log.Printf("Client: sendCancel - acquiring write mutex")
			// Best-effort CANCEL write with a bounded context
			c.writeMu.Lock()
			defer c.writeMu.Unlock()

			// Double-check after acquiring lock
			if c.closed.Load() {
				log.Printf("Client: sendCancel - client closed after acquiring lock, returning")
				return
			}

			log.Printf("Client: sendCancel - attempting to write CANCEL frame for stream %d", id)
			cancelCtx, cancelFn := context.WithTimeout(context.Background(), 200*time.Millisecond)
			errCancel := writeFrame(cancelCtx, c.tx, FrameHeader{StreamID: id, Type: FrameTypeCANCEL}, []byte{1})
			cancelFn()

			if errCancel != nil {
				log.Printf("Client: sendCancel - writeFrame failed: %v, closing tx ring as fallback", errCancel)
				// As a last resort, close the client->server ring to wake the server
				closeErr := c.tx.Close()
				log.Printf("Client: sendCancel - tx.Close() returned: %v", closeErr)
			} else {
				log.Printf("Client: sendCancel - CANCEL frame written successfully")
			}

			log.Printf("Client: sendCancel - removing stream %d from streams map", id)
			// Remove this stream so any late dispatches are ignored
			c.streamsM.Lock()
			delete(c.streams, id)
			c.streamsM.Unlock()

			log.Printf("Client: sendCancel - signaling error channels")
			// Unblock any waiters on this client-side unary future
			select {
			case s.errCh <- reason:
				log.Printf("Client: sendCancel - sent reason to errCh")
			default:
				log.Printf("Client: sendCancel - errCh was full or closed")
			}
			select {
			case s.hdrCh <- HeadersV1{}:
				log.Printf("Client: sendCancel - sent empty headers to hdrCh")
			default:
				log.Printf("Client: sendCancel - hdrCh was full or closed")
			}
			select {
			case s.msgCh <- nil:
				log.Printf("Client: sendCancel - sent nil to msgCh")
			default:
				log.Printf("Client: sendCancel - msgCh was full or closed")
			}
			select {
			case s.trCh <- TrailersV1{}:
				log.Printf("Client: sendCancel - sent empty trailers to trCh")
			default:
				log.Printf("Client: sendCancel - trCh was full or closed")
			}
			log.Printf("Client: sendCancel - completed successfully")
		})
	}

	go func() {
		log.Printf("Client: cancel goroutine started for stream %d", id)

		// Event-driven: block on done or ctx.Done() channels
		// No polling needed - ctx.Done() unblocks immediately when context is cancelled
		for {
			select {
			case <-done:
				log.Printf("Client: cancel goroutine - received done signal for stream %d", id)
				return
			case <-ctx.Done():
				log.Printf("Client: cancel goroutine - context cancelled for stream %d: %v", id, ctx.Err())
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
	log.Printf("Client: about to send HEADERS frame for stream %d", id)
	c.writeMu.Lock()
	if err := writeFrame(ctx, c.tx, FrameHeader{StreamID: id, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL}, hbytes); err != nil {
		c.writeMu.Unlock()
		log.Printf("Client: HEADERS write failed for stream %d: %v", id, err)
		close(done) // tell cancel goroutine to exit
		return HeadersV1{}, nil, TrailersV1{}, err
	}
	log.Printf("Client: HEADERS sent successfully for stream %d", id)
	// Send MESSAGE (single frame for unary)
	log.Printf("Client: about to send MESSAGE frame for stream %d", id)
	if err := writeFrame(ctx, c.tx, FrameHeader{StreamID: id, Type: FrameTypeMESSAGE}, payload); err != nil {
		c.writeMu.Unlock()
		log.Printf("Client: MESSAGE write failed for stream %d: %v", id, err)
		close(done)
		return HeadersV1{}, nil, TrailersV1{}, err
	}
	log.Printf("Client: MESSAGE sent successfully for stream %d", id)
	c.writeMu.Unlock()

	// FAST-PATH: if ctx is already done, send CANCEL immediately
	if err := ctx.Err(); err != nil {
		log.Printf("Client: context already done after sending frames for stream %d: %v", id, err)
		close(done) // tell cancel goroutine to exit immediately
		sendCancel(err)
		return HeadersV1{}, nil, TrailersV1{}, err
	}

	log.Printf("Client: starting response wait loop for stream %d", id)
	var rh HeadersV1
	var rm []byte
	var rt TrailersV1
	haveMsg, haveTr := false, false

	for {
		if haveMsg && haveTr {
			log.Printf("Client: success path - have both message and trailers for stream %d", id)
			close(done) // success path: stop the cancel goroutine
			return rh, rm, rt, nil
		}
		select {
		case <-ctx.Done():
			log.Printf("Client: context done in wait loop for stream %d: %v", id, ctx.Err())
			sendCancel(ctx.Err())
			close(done)
			return HeadersV1{}, nil, TrailersV1{}, ctx.Err()
		case e := <-s.errCh:
			log.Printf("Client: received error from errCh for stream %d: %v", id, e)
			close(done)
			return HeadersV1{}, nil, TrailersV1{}, e
		case h := <-s.hdrCh:
			log.Printf("Client: received headers for stream %d", id)
			rh = h
		case m := <-s.msgCh:
			log.Printf("Client: received message for stream %d (len=%d)", id, len(m))
			rm = append([]byte(nil), m...)
			haveMsg = true
		case tr := <-s.trCh:
			log.Printf("Client: received trailers for stream %d", id)
			rt = tr
			haveTr = true
		}
	}
}
