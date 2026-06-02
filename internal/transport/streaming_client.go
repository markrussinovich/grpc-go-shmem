//go:build linux

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
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
)

// ShmStreamingClient provides bidirectional streaming over shared memory.
// It uses separate goroutines for reading and writing to prevent deadlocks
// when both buffers become full.
type ShmStreamingClient struct {
	seg *Segment
	tx  *ShmRing // client -> server
	rx  *ShmRing // server <- client

	nextID   uint32 // odd stream IDs for client
	streams  map[uint32]*StreamingClientStream
	streamsM sync.Mutex

	writeMu sync.Mutex // serialize frame writes

	readerOnce sync.Once
	readerDone chan struct{}
	// readerStarted gates the Close()-side wait on readerDone. See
	// client.go for the full rationale.
	readerStarted atomic.Bool
	closed     atomic.Bool
}

// StreamingClientStream represents a single bidirectional stream.
type StreamingClientStream struct {
	id     uint32
	ctx    context.Context
	cancel context.CancelFunc
	client *ShmStreamingClient

	// Channels for receiving data from server
	hdrCh chan HeadersV1
	msgCh chan []byte // buffered to allow multiple messages
	trCh  chan TrailersV1
	// trailers holds the received trailers (once). It allows RecvMsg to drain any
	// buffered messages before returning EOF/status.
	trailers atomic.Pointer[TrailersV1]
	errCh    chan error

	// Send coordination
	sendQueue  chan []byte // buffered queue for outgoing messages
	senderDone chan struct{}
	// sendMu serializes SendMsg's `case s.sendQueue <- payload:` send
	// against CloseSend's `close(s.sendQueue)`. Without it, a SendMsg
	// that has already passed the `sendDone.Load()` gate and is mid-
	// select can race with CloseSend's CAS+close, causing
	// "send on closed channel" panic. The mutex is held only across
	// the small select / CAS+close critical sections; the blocking
	// `<-senderDone` in CloseSend runs after release.
	sendMu sync.Mutex

	// Lifecycle
	recvDone atomic.Bool // set when TRAILERS received
	sendDone atomic.Bool // set when client closes send
	done     chan struct{}
	doneOnce sync.Once
}

// NewShmStreamingClient creates a new streaming client over an existing segment.
func NewShmStreamingClient(seg *Segment) *ShmStreamingClient {
	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.B, seg.Mem)
	// Register rings with the segment so the per-data-segment eventfd
	// waker (when enabled) propagates to ring.dataSegWaker; without
	// this, the consumer side parks on eventfd while the producer side
	// only signals via futex — asymmetric wake -> deadlock.
	seg.RegisterRing(tx)
	seg.RegisterRing(rx)
	c := &ShmStreamingClient{
		seg:        seg,
		tx:         tx,
		rx:         rx,
		nextID:     1,
		streams:    make(map[uint32]*StreamingClientStream),
		readerDone: make(chan struct{}),
	}
	return c
}

// Start initiates the background reader goroutine
func (c *ShmStreamingClient) Start() {
	c.startReader()
}

// Close closes the client and underlying segment
func (c *ShmStreamingClient) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}

	// Close rx ring to unblock reader
	_ = c.rx.Close()

	// Wait for reader to exit. BUG FIX (GPT-5.5 bug hunt): only wait
	// if startReader was actually called — otherwise readerDone is
	// never closed and Close hangs forever.
	if c.readerStarted.Load() {
		<-c.readerDone
	}

	// Close all active streams
	c.streamsM.Lock()
	for _, s := range c.streams {
		s.closeWithError(errors.New("client closed"))
	}
	c.streamsM.Unlock()

	return c.seg.Close()
}

// startReader starts the event-driven frame reader
func (c *ShmStreamingClient) startReader() {
	c.readerOnce.Do(func() {
		c.readerStarted.Store(true)
		go func() {
			defer close(c.readerDone)
			ctx := context.Background()

			shmDebugf("StreamingClient: reader goroutine starting")
			for !c.closed.Load() {
				fh, payload, err := readFrame(ctx, c.rx)
				if err != nil {
					if errors.Is(err, ErrRingClosed) || errors.Is(err, io.EOF) {
						shmDebugf("StreamingClient: reader exiting due to closed ring")
						return
					}
					shmDebugf("StreamingClient: reader error: %v", err)
					continue
				}

				// Dispatch frame to appropriate stream
				switch fh.Type {
				case FrameTypeHEADERS:
					hdr, err := decodeHeaders(payload)
					c.dispatchHeaders(fh.StreamID, hdr, err)
				case FrameTypeMESSAGE:
					c.dispatchMessage(fh.StreamID, payload)
				case FrameTypeTRAILERS:
					tr, err := decodeTrailers(payload)
					c.dispatchTrailers(fh.StreamID, tr, err)
				case FrameTypePING:
					// Reply with PONG
					c.writeMu.Lock()
					_ = writeFrame(ctx, c.tx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypePONG}, payload)
					c.writeMu.Unlock()
				case FrameTypeGOAWAY:
					shmDebugf("StreamingClient: received GOAWAY")
					// TODO: handle graceful shutdown
				case FrameTypeCANCEL:
					c.dispatchCancel(fh.StreamID)
				default:
					shmDebugf("StreamingClient: unknown frame type %d", fh.Type)
				}
			}
		}()
	})
}

// NewStream creates a new bidirectional stream.
func (c *ShmStreamingClient) NewStream(ctx context.Context, method, authority string, md []KV) (*StreamingClientStream, error) {
	if c.closed.Load() {
		return nil, errors.New("client is closed")
	}

	// Allocate stream ID
	c.streamsM.Lock()
	streamID := c.nextID
	c.nextID += 2 // client uses odd IDs

	// Create stream context
	streamCtx, cancel := context.WithCancel(ctx)

	// Create stream
	s := &StreamingClientStream{
		id:         streamID,
		ctx:        streamCtx,
		cancel:     cancel,
		client:     c,
		hdrCh:      make(chan HeadersV1, 1),
		msgCh:      make(chan []byte, 16), // buffer multiple messages
		trCh:       make(chan TrailersV1, 1),
		errCh:      make(chan error, 1),
		sendQueue:  make(chan []byte, 16), // buffer outgoing messages
		senderDone: make(chan struct{}),
		done:       make(chan struct{}),
	}

	c.streams[streamID] = s
	c.streamsM.Unlock()

	// Start sender goroutine for this stream
	go c.runStreamSender(s)

	// Send HEADERS frame
	hdr := HeadersV1{
		Version:   1,
		HdrType:   0, // client-initial
		Method:    method,
		Authority: authority,
		Metadata:  md,
	}

	payload := encodeHeaders(hdr)
	fh := FrameHeader{
		StreamID: streamID,
		Type:     FrameTypeHEADERS,
		Flags:    HeadersFlagINITIAL,
	}

	if err := c.writeFrameSafe(streamCtx, fh, payload); err != nil {
		c.streamsM.Lock()
		delete(c.streams, streamID)
		c.streamsM.Unlock()
		cancel()
		return nil, err
	}

	// Ensure reader is running
	c.startReader()

	return s, nil
}

// runStreamSender runs a dedicated sender goroutine for a stream.
// This prevents write operations from blocking the main flow.
func (c *ShmStreamingClient) runStreamSender(s *StreamingClientStream) {
	defer close(s.senderDone)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.done:
			return
		case msg, ok := <-s.sendQueue:
			if !ok {
				return
			}
			// Send message frame. H2 codec on the receive side requires
			// the MESSAGE body to be gRPC LPM-prefixed (1-byte compressed
			// flag + 4-byte big-endian length + body). The user-facing
			// SendMsg / RecvMsg API operates on raw payload bytes, so we
			// wrap on send and strip on receive (dispatchMessage).
			wrapped := make([]byte, 5+len(msg))
			wrapped[0] = 0 // not compressed
			binary.BigEndian.PutUint32(wrapped[1:5], uint32(len(msg)))
			copy(wrapped[5:], msg)
			fh := FrameHeader{
				StreamID: s.id,
				Type:     FrameTypeMESSAGE,
			}
			if err := c.writeFrameSafe(s.ctx, fh, wrapped); err != nil {
				shmDebugf("StreamingClient: failed to send message on stream %d: %v", s.id, err)
				s.closeWithError(err)
				return
			}
		}
	}
}

// writeFrameSafe writes a frame with mutex protection
func (c *ShmStreamingClient) writeFrameSafe(ctx context.Context, fh FrameHeader, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeFrame(ctx, c.tx, fh, payload)
}

// Dispatch methods
func (c *ShmStreamingClient) dispatchHeaders(id uint32, h HeadersV1, err error) {
	c.streamsM.Lock()
	s := c.streams[id]
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
	select {
	case s.hdrCh <- h:
	default:
	}
}

func (c *ShmStreamingClient) dispatchMessage(id uint32, p []byte) {
	c.streamsM.Lock()
	s := c.streams[id]
	c.streamsM.Unlock()
	if s == nil {
		return
	}
	// Strip the gRPC LPM 5-byte prefix added by the sender (the H2 codec
	// passes the LPM through verbatim, so the wrapped header is still
	// present here). If the frame is malformed (too short) drop it.
	body, ok := stripLPMHeader(p)
	if !ok {
		shmDebugf("StreamingClient: dropping malformed MESSAGE on stream %d (len=%d)", id, len(p))
		return
	}
	// Make a copy since the payload buffer may be reused
	msg := append([]byte(nil), body...)
	select {
	case s.msgCh <- msg:
		return
	case <-s.ctx.Done():
		return
	case <-s.done:
		return
	}
}

func (c *ShmStreamingClient) dispatchTrailers(id uint32, tr TrailersV1, err error) {
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
	s.recvDone.Store(true)
	s.trailers.Store(&tr)
	select {
	case s.trCh <- tr:
	default:
	}
}

func (c *ShmStreamingClient) dispatchCancel(id uint32) {
	c.streamsM.Lock()
	s := c.streams[id]
	delete(c.streams, id)
	c.streamsM.Unlock()
	if s == nil {
		return
	}
	s.closeWithError(errors.New("stream cancelled by server"))
}

// Stream methods

// SendMsg sends a message on the stream (non-blocking, queued).
func (s *StreamingClientStream) SendMsg(payload []byte) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.sendDone.Load() {
		return errors.New("send already closed")
	}
	select {
	case s.sendQueue <- payload:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-s.done:
		return errors.New("stream closed")
	}
}

// CloseSend signals that no more messages will be sent.
func (s *StreamingClientStream) CloseSend() error {
	s.sendMu.Lock()
	if !s.sendDone.CompareAndSwap(false, true) {
		s.sendMu.Unlock()
		return errors.New("send already closed")
	}
	if s.client == nil {
		s.sendMu.Unlock()
		return errors.New("client is nil")
	}
	// Preserve ordering: ensure all queued messages are sent before half-closing.
	close(s.sendQueue)
	s.sendMu.Unlock()
	<-s.senderDone
	fh := FrameHeader{StreamID: s.id, Type: FrameTypeHALFCLOSE}
	return s.client.writeFrameSafe(s.ctx, fh, nil)
}

// RecvMsg receives a message from the stream (blocking).
func (s *StreamingClientStream) RecvMsg() ([]byte, error) {
	for {
		// Prefer draining buffered messages before observing trailers.
		select {
		case msg := <-s.msgCh:
			return msg, nil
		default:
		}

		if tr := s.trailers.Load(); tr != nil && len(s.msgCh) == 0 {
			if tr.GRPCStatusCode != 0 {
				return nil, fmt.Errorf("stream ended with status %d: %s", tr.GRPCStatusCode, tr.GRPCStatusMsg)
			}
			return nil, io.EOF
		}

		select {
		case msg := <-s.msgCh:
			return msg, nil
		case tr := <-s.trCh:
			s.trailers.Store(&tr)
			continue
		case err := <-s.errCh:
			return nil, err
		case <-s.ctx.Done():
			return nil, s.ctx.Err()
		case <-s.done:
			return nil, errors.New("stream closed")
		}
	}
}

// RecvHeaders receives the initial headers (blocking).
func (s *StreamingClientStream) RecvHeaders() (HeadersV1, error) {
	select {
	case hdr := <-s.hdrCh:
		return hdr, nil
	case err := <-s.errCh:
		return HeadersV1{}, err
	case <-s.ctx.Done():
		return HeadersV1{}, s.ctx.Err()
	case <-s.done:
		return HeadersV1{}, errors.New("stream closed")
	}
}

// closeWithError closes the stream with an error.
func (s *StreamingClientStream) closeWithError(err error) {
	s.doneOnce.Do(func() {
		if err != nil {
			select {
			case s.errCh <- err:
			default:
			}
		}
		s.cancel()
		close(s.done)
	})
}
