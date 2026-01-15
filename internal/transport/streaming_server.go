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
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
)

// ShmStreamingServer provides bidirectional streaming over shared memory.
// It uses separate goroutines for reading and writing to prevent deadlocks.
type ShmStreamingServer struct {
	seg *Segment
	tx  *ShmRing // server -> client
	rx  *ShmRing // client -> server

	streams  map[uint32]*streamingServerStream
	streamsM sync.Mutex

	writeMu sync.Mutex // serialize frame writes

	readerOnce sync.Once
	readerDone chan struct{}
	closed     atomic.Bool

	// Handler for new streams
	handler func(*streamingServerStream)
}

// streamingServerStream represents a single bidirectional stream on the server
type streamingServerStream struct {
	id     uint32
	method string
	ctx    context.Context
	cancel context.CancelFunc

	// Channels for receiving data from client
	hdrCh chan HeadersV1
	msgCh chan []byte // buffered to allow multiple messages
	errCh chan error

	// Send coordination
	sendQueue  chan []byte // buffered queue for outgoing messages
	senderDone chan struct{}

	// Lifecycle
	recvDone atomic.Bool // set when client closes send
	sendDone atomic.Bool // set when server closes send
	done     chan struct{}
	doneOnce sync.Once

	// Reference to server for sending
	server *ShmStreamingServer
}

// NewShmStreamingServer creates a new streaming server over an existing segment.
func NewShmStreamingServer(seg *Segment) *ShmStreamingServer {
	s := &ShmStreamingServer{
		seg:        seg,
		tx:         NewShmRingFromSegment(seg.B, seg.Mem), // server uses ring B for sending
		rx:         NewShmRingFromSegment(seg.A, seg.Mem), // server uses ring A for receiving
		streams:    make(map[uint32]*streamingServerStream),
		readerDone: make(chan struct{}),
	}
	return s
}

// Serve starts the server and processes incoming streams
func (s *ShmStreamingServer) Serve(ctx context.Context, handler func(*streamingServerStream)) error {
	s.handler = handler
	s.startReader()

	// Wait for context cancellation
	<-ctx.Done()

	// Close
	return s.Close()
}

// Close closes the server
func (s *ShmStreamingServer) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}

	// Close rx ring to unblock reader
	_ = s.rx.Close()

	// Wait for reader to exit
	<-s.readerDone

	// Close all active streams
	s.streamsM.Lock()
	for _, stream := range s.streams {
		stream.closeWithError(errors.New("server closed"))
	}
	s.streamsM.Unlock()

	return s.seg.Close()
}

// startReader starts the event-driven frame reader
func (s *ShmStreamingServer) startReader() {
	s.readerOnce.Do(func() {
		go func() {
			defer close(s.readerDone)
			ctx := context.Background()

			log.Printf("StreamingServer: reader goroutine starting")
			for !s.closed.Load() {
				fh, payload, err := readFrame(ctx, s.rx)
				if err != nil {
					if errors.Is(err, ErrRingClosed) || errors.Is(err, io.EOF) {
						log.Printf("StreamingServer: reader exiting due to closed ring")
						return
					}
					log.Printf("StreamingServer: reader error: %v", err)
					continue
				}

				// Dispatch frame to appropriate stream
				switch fh.Type {
				case FrameTypeHEADERS:
					hdr, err := decodeHeaders(payload)
					if err != nil {
						log.Printf("StreamingServer: failed to decode headers: %v", err)
						continue
					}
					s.handleNewStream(fh.StreamID, hdr)
				case FrameTypeMESSAGE:
					s.dispatchMessage(fh.StreamID, payload)
				case FrameTypeHALFCLOSE:
					s.dispatchHalfClose(fh.StreamID)
				case FrameTypeCANCEL:
					s.dispatchCancel(fh.StreamID)
				case FrameTypePING:
					// Reply with PONG
					s.writeMu.Lock()
					_ = writeFrame(ctx, s.tx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypePONG}, payload)
					s.writeMu.Unlock()
				default:
					log.Printf("StreamingServer: unknown frame type %d", fh.Type)
				}
			}
		}()
	})
}

// handleNewStream creates and dispatches a new stream to the handler
func (s *ShmStreamingServer) handleNewStream(streamID uint32, hdr HeadersV1) {
	s.streamsM.Lock()
	// Check if stream already exists
	if _, exists := s.streams[streamID]; exists {
		s.streamsM.Unlock()
		log.Printf("StreamingServer: stream %d already exists", streamID)
		return
	}

	// Create stream context
	streamCtx, cancel := context.WithCancel(context.Background())

	// Create stream
	stream := &streamingServerStream{
		id:         streamID,
		method:     hdr.Method,
		ctx:        streamCtx,
		cancel:     cancel,
		hdrCh:      make(chan HeadersV1, 1),
		msgCh:      make(chan []byte, 16), // buffer multiple messages
		errCh:      make(chan error, 1),
		sendQueue:  make(chan []byte, 16), // buffer outgoing messages
		senderDone: make(chan struct{}),
		done:       make(chan struct{}),
		server:     s,
	}

	// Store initial headers
	select {
	case stream.hdrCh <- hdr:
	default:
	}

	s.streams[streamID] = stream
	s.streamsM.Unlock()

	// Start sender goroutine for this stream
	go s.runStreamSender(stream)

	// Dispatch to handler in a new goroutine to avoid blocking reader
	go func() {
		if s.handler != nil {
			s.handler(stream)
		}
	}()
}

// runStreamSender runs a dedicated sender goroutine for a stream.
// This prevents write operations from blocking the main flow.
func (s *ShmStreamingServer) runStreamSender(stream *streamingServerStream) {
	defer close(stream.senderDone)
	for {
		select {
		case <-stream.ctx.Done():
			return
		case <-stream.done:
			return
		case msg, ok := <-stream.sendQueue:
			if !ok {
				return
			}
			// Send message frame
			fh := FrameHeader{
				StreamID: stream.id,
				Type:     FrameTypeMESSAGE,
			}
			if err := s.writeFrameSafe(stream.ctx, fh, msg); err != nil {
				log.Printf("StreamingServer: failed to send message on stream %d: %v", stream.id, err)
				stream.closeWithError(err)
				return
			}
		}
	}
}

// writeFrameSafe writes a frame with mutex protection
func (s *ShmStreamingServer) writeFrameSafe(ctx context.Context, fh FrameHeader, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeFrame(ctx, s.tx, fh, payload)
}

// Dispatch methods
func (s *ShmStreamingServer) dispatchMessage(id uint32, p []byte) {
	s.streamsM.Lock()
	stream := s.streams[id]
	s.streamsM.Unlock()
	if stream == nil {
		log.Printf("StreamingServer: no stream found for id %d", id)
		return
	}
	// Make a copy since the payload buffer may be reused
	msg := append([]byte(nil), p...)
	select {
	case stream.msgCh <- msg:
		return
	case <-stream.ctx.Done():
		return
	case <-stream.done:
		return
	}
}

func (s *ShmStreamingServer) dispatchCancel(id uint32) {
	s.streamsM.Lock()
	stream := s.streams[id]
	delete(s.streams, id)
	s.streamsM.Unlock()
	if stream == nil {
		return
	}
	stream.closeWithError(errors.New("stream cancelled by client"))
}

func (s *ShmStreamingServer) dispatchHalfClose(id uint32) {
	s.streamsM.Lock()
	stream := s.streams[id]
	s.streamsM.Unlock()
	if stream == nil {
		return
	}
	stream.recvDone.Store(true)
}

// Stream methods

// SendHeaders sends initial headers to the client
func (s *streamingServerStream) SendHeaders(md []KV) error {
	hdr := HeadersV1{
		Version:  1,
		HdrType:  1, // server-initial
		Metadata: md,
	}

	payload := encodeHeaders(hdr)
	fh := FrameHeader{
		StreamID: s.id,
		Type:     FrameTypeHEADERS,
	}

	return s.server.writeFrameSafe(s.ctx, fh, payload)
}

// SendMsg sends a message on the stream (non-blocking, queued)
func (s *streamingServerStream) SendMsg(payload []byte) error {
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

// SendTrailers sends trailers and closes the stream
func (s *streamingServerStream) SendTrailers(statusCode uint32, statusMsg string, md []KV) error {
	if !s.sendDone.CompareAndSwap(false, true) {
		return errors.New("send already closed")
	}
	// Preserve ordering: ensure all queued messages are flushed before trailers.
	close(s.sendQueue)
	<-s.senderDone

	tr := TrailersV1{
		Version:        1,
		GRPCStatusCode: statusCode,
		GRPCStatusMsg:  statusMsg,
		Metadata:       md,
	}

	payload := encodeTrailers(tr)
	fh := FrameHeader{
		StreamID: s.id,
		Type:     FrameTypeTRAILERS,
		Flags:    TrailersFlagEndStream,
	}

	err := s.server.writeFrameSafe(s.ctx, fh, payload)

	// Remove from server's stream map
	s.server.streamsM.Lock()
	delete(s.server.streams, s.id)
	s.server.streamsM.Unlock()

	s.closeWithError(nil)
	return err
}

// RecvMsg receives a message from the stream (blocking)
func (s *streamingServerStream) RecvMsg() ([]byte, error) {
	select {
	case msg := <-s.msgCh:
		return msg, nil
	default:
	}
	if s.recvDone.Load() && len(s.msgCh) == 0 {
		return nil, io.EOF
	}

	select {
	case msg := <-s.msgCh:
		return msg, nil
	case err := <-s.errCh:
		return nil, err
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case <-s.done:
		return nil, errors.New("stream closed")
	}
}

// RecvHeaders receives the initial headers (blocking)
func (s *streamingServerStream) RecvHeaders() (HeadersV1, error) {
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

// Method returns the method name
func (s *streamingServerStream) Method() string {
	return s.method
}

// Context returns the stream context
func (s *streamingServerStream) Context() context.Context {
	return s.ctx
}

// closeWithError closes the stream with an error
func (s *streamingServerStream) closeWithError(err error) {
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
