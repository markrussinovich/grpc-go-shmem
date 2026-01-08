//go:build linux

package transport

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/internal/grpcutil"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// ShmServerTransport implements the gRPC ServerTransport interface
// for shared memory communication.
type ShmServerTransport struct {
	// Core state
	segment        *Segment // The shared memory segment
	serverToClient *ShmRing // Ring for server->client data
	clientToServer *ShmRing // Ring for client->server data

	// Connection state
	localAddr  net.Addr
	remoteAddr net.Addr
	peer       *peer.Peer

	// Lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	closed atomic.Bool
	mu     sync.RWMutex

	// Stream management
	streams    map[uint32]*ServerStream
	streamID   uint32 // next stream ID to assign
	handleFunc func(*ServerStream)

	// Error handling
	closeOnce sync.Once
	errCh     chan struct{}

	readerWG sync.WaitGroup
}

// NewShmServerTransport creates a new shared memory server transport.
func NewShmServerTransport(segment *Segment, localAddr, remoteAddr net.Addr) (*ShmServerTransport, error) {
	if segment == nil {
		return nil, errors.New("segment cannot be nil")
	}

	// Create rings for bidirectional communication
	// Ring A: client->server, Ring B: server->client
	clientToServer := NewShmRingFromSegment(segment.A, segment.Mem)
	serverToClient := NewShmRingFromSegment(segment.B, segment.Mem)

	ctx, cancel := context.WithCancel(context.Background())

	t := &ShmServerTransport{
		segment:        segment,
		serverToClient: serverToClient,
		clientToServer: clientToServer,
		localAddr:      localAddr,
		remoteAddr:     remoteAddr,
		peer: &peer.Peer{
			Addr:      remoteAddr,
			LocalAddr: localAddr,
			AuthInfo:  nil, // No auth for shared memory
		},
		ctx:     ctx,
		cancel:  cancel,
		streams: make(map[uint32]*ServerStream),
		errCh:   make(chan struct{}),
	}

	return t, nil
}

// HandleStreams receives incoming streams using the given handler.
// This is typically run in a separate goroutine.
func (t *ShmServerTransport) HandleStreams(ctx context.Context, handle func(*ServerStream)) {
	t.mu.Lock()
	if t.closed.Load() {
		t.mu.Unlock()
		return
	}
	t.handleFunc = handle
	t.mu.Unlock()

	// Start processing incoming data from the client
	t.readerWG.Add(1)
	go func() {
		defer t.readerWG.Done()
		t.processIncomingData(ctx)
	}()

	// Wait for context cancellation or transport closure
	select {
	case <-ctx.Done():
		t.Close(ctx.Err())
	case <-t.errCh:
		// Transport was closed
	}
}

// processIncomingData reads data from the client->server ring and processes gRPC frames
func (t *ShmServerTransport) processIncomingData(ctx context.Context) {
	defer func() {
		if !t.closed.Load() {
			go t.Close(errors.New("incoming data processing ended"))
		}
	}()

	for {
		if t.closed.Load() {
			return
		}
		fh, payload, err := readFrame(t.clientToServer, ctx)
		if err != nil {
			if errors.Is(err, ErrRingClosed) || t.closed.Load() {
				return
			}
			continue
		}

		// Dispatch frames based on type
		switch fh.Type {
		case FrameTypeHEADERS:
			if err := t.handleHeaders(ctx, fh.StreamID, payload); err != nil {
				// Log error but continue processing
				continue
			}
		case FrameTypeMESSAGE:
			t.handleMessage(fh.StreamID, fh.Flags, payload)
		case FrameTypeTRAILERS:
			t.handleTrailers(fh.StreamID, payload)
		case FrameTypeCANCEL:
			t.handleCancel(fh.StreamID)
		default:
			// Unknown frame type, ignore
		}
	}
}

// handleHeaders processes a HEADERS frame and creates a new ServerStream
func (t *ShmServerTransport) handleHeaders(ctx context.Context, streamID uint32, payload []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed.Load() {
		return errors.New("transport closed")
	}

	// Check if stream already exists
	if _, exists := t.streams[streamID]; exists {
		return errors.New("stream already exists")
	}

	// Validate stream ID (client uses odd numbers)
	if streamID%2 != 1 {
		return errors.New("invalid stream ID: must be odd for client-initiated streams")
	}

	// Decode headers using the proper frame format
	hdr, err := decodeHeaders(payload)
	if err != nil {
		return err
	}

	// Convert KV metadata to metadata.MD
	md := make(metadata.MD)
	for _, kv := range hdr.Metadata {
		var vals []string
		for _, v := range kv.Values {
			vals = append(vals, string(v))
		}
		md[kv.Key] = vals
	}

	// Create receive buffer for the stream
	buf := newRecvBuffer()

	// Create the ServerStream
	s := &ServerStream{
		Stream: &Stream{
			id:             streamID,
			method:         hdr.Method,
			buf:            buf,
			sendCompress:   "",
			recvCompress:   "",
			contentSubtype: "",
		},
		st: t,
	}

	// Create context for the stream. If the client provided an RPC deadline,
	// honor it by creating a context with that deadline.
	if hdr.DeadlineUnixNano != 0 {
		const maxInt64AsUint64 = uint64(^uint64(0) >> 1)
		if hdr.DeadlineUnixNano > maxInt64AsUint64 {
			return errors.New("invalid deadline: too large")
		}
		deadline := time.Unix(0, int64(hdr.DeadlineUnixNano))
		s.ctx, s.cancel = context.WithDeadline(ctx, deadline)
	} else {
		s.ctx, s.cancel = context.WithCancel(ctx)
	}
	s.ctxDone = s.ctx.Done()

	// Attach metadata to context if present
	if len(md) > 0 {
		s.ctx = metadata.NewIncomingContext(s.ctx, md)
	}
	// Populate stream fields derived from incoming headers.
	if v := md.Get("grpc-encoding"); len(v) > 0 {
		s.recvCompress = v[0]
	}
	if v := md.Get("grpc-accept-encoding"); len(v) > 0 {
		s.clientAdvertisedCompressors = v[0]
	}
	if v := md.Get("content-type"); len(v) > 0 {
		contentType := strings.ToLower(v[0])
		if contentSubtype, ok := grpcutil.ContentSubtype(contentType); ok {
			s.contentSubtype = contentSubtype
		} else {
			return errors.New("invalid gRPC request content-type")
		}
	}

	// Set requestRead callback for the stream
	// For shared memory, no explicit flow control is needed
	s.requestRead = func(n int) {
		// No-op for shared memory transport
		// Flow control is handled implicitly by the ring buffer
	}

	// Create transport reader for the stream
	s.trReader = &transportReader{
		reader: &recvBufferReader{
			ctx:     s.ctx,
			ctxDone: s.ctxDone,
			recv:    s.buf,
		},
		windowHandler: func(n int) {
			// For shm transport, window handling is implicit via ring buffer
		},
	}

	// Register the stream
	t.streams[streamID] = s

	// Call the handler in a new goroutine
	if t.handleFunc != nil {
		go t.handleFunc(s)
	}

	return nil
}

// handleMessage processes a MESSAGE frame.
// For client->server, the final MESSAGE is indicated by MessageFlagMORE being unset.
func (t *ShmServerTransport) handleMessage(streamID uint32, flags uint8, payload []byte) {
	t.mu.RLock()
	s, exists := t.streams[streamID]
	t.mu.RUnlock()

	if !exists {
		// Stream doesn't exist, drop the message
		return
	}

	// Write the message data to the stream's receive buffer
	// Use mem.Copy to create a buffer from the payload
	buf := mem.Copy(payload, mem.DefaultBufferPool())
	s.write(recvMsg{buffer: buf})

	// If this is the final client message, signal client half-close.
	if flags&MessageFlagMORE == 0 {
		s.write(recvMsg{err: io.EOF})
	}
}

// handleTrailers processes a TRAILERS frame
func (t *ShmServerTransport) handleTrailers(streamID uint32, payload []byte) {
	t.mu.RLock()
	s, exists := t.streams[streamID]
	t.mu.RUnlock()

	if !exists {
		return
	}

	// Decode trailers using the proper frame format
	trailers, err := decodeTrailers(payload)
	if err != nil {
		// Send error to stream
		s.write(recvMsg{err: err})
		return
	}

	// Create status from trailers. For OK, gRPC expects server-side RecvMsg to
	// return io.EOF to indicate client half-close. A nil error here would enqueue
	// an empty recvMsg and can crash downstream readers.
	st := status.New(codes.Code(trailers.GRPCStatusCode), trailers.GRPCStatusMsg)
	var endErr error
	if st.Code() == codes.OK {
		endErr = io.EOF
	} else {
		endErr = st.Err()
	}

	// Signal end-of-client-stream to the stream.
	s.write(recvMsg{err: endErr})

	// Remove stream from active streams
	t.mu.Lock()
	delete(t.streams, streamID)
	t.mu.Unlock()
}

// handleCancel processes a CANCEL frame
func (t *ShmServerTransport) handleCancel(streamID uint32) {
	t.mu.RLock()
	s, exists := t.streams[streamID]
	t.mu.RUnlock()

	if !exists {
		return
	}

	// Cancel the stream context
	s.cancel()

	// Remove from active streams
	t.mu.Lock()
	delete(t.streams, streamID)
	t.mu.Unlock()
}

// Close tears down the transport. Once it is called, the transport
// should not be accessed any more. All the pending streams and their
// handlers will be terminated asynchronously.
func (t *ShmServerTransport) Close(err error) {
	t.closeOnce.Do(func() {
		t.closed.Store(true)

		// Cancel context to stop all goroutines
		t.cancel()

		// Close the rings
		t.serverToClient.Close()
		t.clientToServer.Close()

		// Wait for reader goroutine to exit before unmapping.
		t.readerWG.Wait()

		// Close the segment
		if t.segment != nil {
			t.segment.Close()
		}

		// Terminate all active streams
		t.mu.Lock()
		for _, stream := range t.streams {
			// Signal stream termination
			if stream != nil {
				// TODO: Properly terminate streams
			}
		}
		t.streams = make(map[uint32]*ServerStream)
		t.mu.Unlock()

		// Signal closure
		close(t.errCh)
	})
}

// Peer returns the peer of the server transport.
func (t *ShmServerTransport) Peer() *peer.Peer {
	return t.peer
}

// Drain notifies the client this ServerTransport stops accepting new RPCs.
func (t *ShmServerTransport) Drain(debugData string) {
	// TODO: Implement drain signaling via shared memory
	// For now, just close the transport
	t.Close(errors.New("transport drained: " + debugData))
}

// internalServerTransport methods

// writeHeader writes header metadata for a stream
func (t *ShmServerTransport) writeHeader(s *ServerStream, md metadata.MD) error {
	if t.closed.Load() {
		return ErrConnClosing
	}
	// gRPC may call SendHeader explicitly, and the transport may also need to
	// send implicit headers before the first message. Ensure we only send
	// headers once per stream.
	if s.updateHeaderSent() {
		return nil
	}

	log.Printf("[DEBUG] ShmServerTransport.writeHeader: stream=%d, metadata keys=%v", s.id, len(md))

	// Convert metadata.MD to []KV format
	var kvs []KV
	for k, vals := range md {
		var byteVals [][]byte
		for _, v := range vals {
			byteVals = append(byteVals, []byte(v))
		}
		kvs = append(kvs, KV{Key: k, Values: byteVals})
	}

	// Create HEADERS frame with server-initial type
	hdr := HeadersV1{
		Version:          1,
		HdrType:          1, // server-initial
		Method:           "",
		Authority:        "",
		DeadlineUnixNano: 0,
		Metadata:         kvs,
	}

	payload := encodeHeaders(hdr)

	// Create frame header
	fh := FrameHeader{
		Type:     FrameTypeHEADERS,
		StreamID: s.id,
		Length:   uint32(len(payload)),
	}

	log.Printf("[DEBUG] ShmServerTransport.writeHeader: Writing HEADERS frame, streamID=%d, length=%d", s.id, fh.Length)

	// Write frame to server->client ring
	if err := writeFrame(t.serverToClient, fh, payload, context.Background()); err != nil {
		log.Printf("[ERROR] ShmServerTransport.writeHeader: Failed to write frame: %v", err)
		return err
	}

	log.Printf("[DEBUG] ShmServerTransport.writeHeader: Successfully wrote HEADERS frame")
	return nil
}

func (t *ShmServerTransport) maybeWriteHeader(s *ServerStream) error {
	// If headers were already sent, nothing to do.
	if s.isHeaderSent() {
		return nil
	}
	// Snapshot current outgoing headers under lock.
	s.hdrMu.Lock()
	md := s.header.Copy()
	s.hdrMu.Unlock()
	return t.writeHeader(s, md)
}

// write writes header and data for a stream
func (t *ShmServerTransport) write(s *ServerStream, hdr []byte, data mem.BufferSlice, opts *WriteOptions) error {
	if t.closed.Load() {
		return ErrConnClosing
	}
	if err := t.maybeWriteHeader(s); err != nil {
		return err
	}

	log.Printf("[DEBUG] ShmServerTransport.write: stream=%d, hdr_len=%d, data_slices=%d", s.id, len(hdr), len(data))

	// Materialize the BufferSlice into a contiguous []byte
	var payload []byte
	if len(hdr) > 0 {
		payload = append(payload, hdr...)
	}
	for _, buf := range data {
		payload = append(payload, buf.ReadOnlyData()...)
	}

	log.Printf("[DEBUG] ShmServerTransport.write: total payload=%d bytes", len(payload))

	// Create MESSAGE frame
	fh := FrameHeader{
		Type:     FrameTypeMESSAGE,
		StreamID: s.id,
		Length:   uint32(len(payload)),
	}

	// Write frame to server->client ring
	if err := writeFrame(t.serverToClient, fh, payload, context.Background()); err != nil {
		log.Printf("[ERROR] ShmServerTransport.write: Failed to write frame: %v", err)
		return err
	}

	log.Printf("[DEBUG] ShmServerTransport.write: Successfully wrote MESSAGE frame")
	return nil
}

// writeStatus writes status for a stream (trailers)
func (t *ShmServerTransport) writeStatus(s *ServerStream, st *status.Status) error {
	if t.closed.Load() {
		return ErrConnClosing
	}
	// Ensure idempotence: gRPC may race multiple WriteStatus calls.
	if s.swapState(streamDone) == streamDone {
		return nil
	}
	if err := t.maybeWriteHeader(s); err != nil {
		return err
	}

	log.Printf("[DEBUG] ShmServerTransport.writeStatus: stream=%d, code=%v, msg=%s", s.id, st.Code(), st.Message())

	// Create trailers frame
	trailers := TrailersV1{
		Version:        1,
		GRPCStatusCode: uint32(st.Code()),
		GRPCStatusMsg:  st.Message(),
		Metadata:       nil, // TODO: support trailer metadata
	}

	payload := encodeTrailers(trailers)

	// Create frame header
	fh := FrameHeader{
		Type:     FrameTypeTRAILERS,
		StreamID: s.id,
		Length:   uint32(len(payload)),
	}

	log.Printf("[DEBUG] ShmServerTransport.writeStatus: Writing TRAILERS frame, streamID=%d, length=%d", s.id, fh.Length)

	// Write frame to server->client ring
	if err := writeFrame(t.serverToClient, fh, payload, context.Background()); err != nil {
		log.Printf("[ERROR] ShmServerTransport.writeStatus: Failed to write frame: %v", err)
		return err
	}

	log.Printf("[DEBUG] ShmServerTransport.writeStatus: Successfully wrote TRAILERS frame")

	// Remove stream from active streams
	t.mu.Lock()
	delete(t.streams, s.id)
	t.mu.Unlock()

	return nil
}

// incrMsgRecv increments the message received counter
func (t *ShmServerTransport) incrMsgRecv() {
	// TODO: Implement stats tracking
}
