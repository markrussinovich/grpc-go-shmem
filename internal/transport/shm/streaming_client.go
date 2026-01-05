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

package shm

import (
	"context"
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
	streams  map[uint32]*streamingClientStream
	streamsM sync.Mutex

	writeMu sync.Mutex // serialize frame writes

	readerOnce sync.Once
	readerDone chan struct{}
	closed     atomic.Bool
}

// streamingClientStream represents a single bidirectional stream
type streamingClientStream struct {
	id     uint32
	ctx    context.Context
	cancel context.CancelFunc

	// Channels for receiving data from server
	hdrCh chan HeadersV1
	msgCh chan []byte      // buffered to allow multiple messages
	trCh  chan TrailersV1
	errCh chan error

	// Send coordination
	sendMu    sync.Mutex
	sendQueue chan []byte // buffered queue for outgoing messages

	// Lifecycle
	recvDone atomic.Bool // set when TRAILERS received
	sendDone atomic.Bool // set when client closes send
	done     chan struct{}
	doneOnce sync.Once
}

// NewShmStreamingClient creates a new streaming client over an existing segment.
func NewShmStreamingClient(seg *Segment) *ShmStreamingClient {
	c := &ShmStreamingClient{
		seg:        seg,
		tx:         NewShmRingFromSegment(seg.A, seg.Mem),
		rx:         NewShmRingFromSegment(seg.B, seg.Mem),
		nextID:     1,
		streams:    make(map[uint32]*streamingClientStream),
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

	// Wait for reader to exit
	<-c.readerDone

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
		go func() {
			defer close(c.readerDone)
			ctx := context.Background()
			
			log.Printf("StreamingClient: reader goroutine starting")
			for !c.closed.Load() {
				fh, payload, err := readFrame(c.rx, ctx)
				if err != nil {
					if errors.Is(err, ErrRingClosed) || errors.Is(err, io.EOF) {
						log.Printf("StreamingClient: reader exiting due to closed ring")
						return
					}
					log.Printf("StreamingClient: reader error: %v", err)
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
					_ = writeFrame(c.tx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypePONG}, payload, ctx)
					c.writeMu.Unlock()
				case FrameTypeGOAWAY:
					log.Printf("StreamingClient: received GOAWAY")
					// TODO: handle graceful shutdown
				case FrameTypeCANCEL:
					c.dispatchCancel(fh.StreamID)
				default:
					log.Printf("StreamingClient: unknown frame type %d", fh.Type)
				}
			}
		}()
	})
}

// NewStream creates a new bidirectional stream
func (c *ShmStreamingClient) NewStream(ctx context.Context, method, authority string, md []KV) (*streamingClientStream, error) {
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
	s := &streamingClientStream{
		id:        streamID,
		ctx:       streamCtx,
		cancel:    cancel,
		hdrCh:     make(chan HeadersV1, 1),
		msgCh:     make(chan []byte, 16), // buffer multiple messages
		trCh:      make(chan TrailersV1, 1),
		errCh:     make(chan error, 1),
		sendQueue: make(chan []byte, 16), // buffer outgoing messages
		done:      make(chan struct{}),
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

	if err := c.writeFrameSafe(fh, payload, streamCtx); err != nil {
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

// metadataToKV converts metadata []KV - just returns as-is since test uses []KV directly
func metadataToKV(md []KV) []KV {
	return md
}

// runStreamSender runs a dedicated sender goroutine for a stream.
// This prevents write operations from blocking the main flow.
func (c *ShmStreamingClient) runStreamSender(s *streamingClientStream) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.done:
			return
		case msg := <-s.sendQueue:
			// Send message frame
			fh := FrameHeader{
				StreamID: s.id,
				Type:     FrameTypeMESSAGE,
			}
			if err := c.writeFrameSafe(fh, msg, s.ctx); err != nil {
				log.Printf("StreamingClient: failed to send message on stream %d: %v", s.id, err)
				s.closeWithError(err)
				return
			}
		}
	}
}

// writeFrameSafe writes a frame with mutex protection
func (c *ShmStreamingClient) writeFrameSafe(fh FrameHeader, payload []byte, ctx context.Context) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeFrame(c.tx, fh, payload, ctx)
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
	// Make a copy since the payload buffer may be reused
	msg := append([]byte(nil), p...)
	select {
	case s.msgCh <- msg:
	default:
		// Drop message if buffer full (shouldn't happen with large buffer)
		log.Printf("StreamingClient: dropped message on stream %d (buffer full)", id)
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

// SendMsg sends a message on the stream (non-blocking, queued)
func (s *streamingClientStream) SendMsg(payload []byte) error {
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

// CloseSend signals that no more messages will be sent
func (s *streamingClientStream) CloseSend() error {
	if !s.sendDone.CompareAndSwap(false, true) {
		return errors.New("send already closed")
	}
	// Note: In a full implementation, we'd send end-of-stream indicator
	// For now, trailers from server will close the stream
	return nil
}

// RecvMsg receives a message from the stream (blocking)
func (s *streamingClientStream) RecvMsg() ([]byte, error) {
	select {
	case msg := <-s.msgCh:
		return msg, nil
	case tr := <-s.trCh:
		// Stream ended
		if tr.GRPCStatusCode != 0 {
			return nil, fmt.Errorf("stream ended with status %d: %s", tr.GRPCStatusCode, tr.GRPCStatusMsg)
		}
		return nil, io.EOF
	case err := <-s.errCh:
		return nil, err
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case <-s.done:
		return nil, errors.New("stream closed")
	}
}

// RecvHeaders receives the initial headers (blocking)
func (s *streamingClientStream) RecvHeaders() (HeadersV1, error) {
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

// closeWithError closes the stream with an error
func (s *streamingClientStream) closeWithError(err error) {
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
