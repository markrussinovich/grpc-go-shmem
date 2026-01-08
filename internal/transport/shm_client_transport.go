//go:build linux

package transport

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"

	"golang.org/x/net/http2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
)

// ShmClientTransport implements the gRPC ClientTransport interface
// for shared memory communication.
type ShmClientTransport struct {
	// Core state
	segment        *Segment // The shared memory segment
	clientToServer *ShmRing // Ring for client->server data
	serverToClient *ShmRing // Ring for server->client data

	// Connection state
	localAddr  net.Addr
	remoteAddr net.Addr

	// Lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	closed atomic.Bool
	// draining indicates GracefulClose or server GOAWAY has been initiated.
	// When draining, NewStream must fail and the transport should close once all
	// active streams finish.
	draining atomic.Bool
	mu       sync.RWMutex

	// Stream management
	streams         map[uint32]*ClientStream
	streamTransport map[*ClientStream]*ShmClientTransport // Track transport for each stream
	streamID        uint32                                // next stream ID to assign

	// Error handling
	closeOnce sync.Once
	errCh     chan struct{}
	goAwayCh  chan struct{}

	readerWG sync.WaitGroup
}

// test hook: allow disabling the background reader in tests to avoid
// interference when a different client is used on the same segment.
var enableClientReader atomic.Bool

func init() { enableClientReader.Store(true) }

// NewShmClientTransport creates a new shared memory client transport.
func NewShmClientTransport(segment *Segment, localAddr, remoteAddr net.Addr) (*ShmClientTransport, error) {
	if segment == nil {
		return nil, errors.New("segment cannot be nil")
	}

	// Create rings for bidirectional communication
	// Ring A: client->server, Ring B: server->client
	clientToServer := NewShmRingFromSegment(segment.A, segment.Mem)
	serverToClient := NewShmRingFromSegment(segment.B, segment.Mem)

	ctx, cancel := context.WithCancel(context.Background())

	t := &ShmClientTransport{
		segment:         segment,
		clientToServer:  clientToServer,
		serverToClient:  serverToClient,
		localAddr:       localAddr,
		remoteAddr:      remoteAddr,
		ctx:             ctx,
		cancel:          cancel,
		streams:         make(map[uint32]*ClientStream),
		streamTransport: make(map[*ClientStream]*ShmClientTransport),
		errCh:           make(chan struct{}),
		goAwayCh:        make(chan struct{}),
	}

	// Start processing incoming data from the server (test hook guarded)
	if enableClientReader.Load() {
		t.readerWG.Add(1)
		go func() {
			defer t.readerWG.Done()
			t.processIncomingData(t.ctx)
		}()
	}

	return t, nil
}

// processIncomingData reads data from the server->client ring and processes gRPC frames
func (t *ShmClientTransport) processIncomingData(ctx context.Context) {
	log.Printf("[DEBUG] ShmClientTransport.processIncomingData: STARTED")
	defer func() {
		log.Printf("[DEBUG] ShmClientTransport.processIncomingData: EXITING")
		if !t.closed.Load() {
			go t.Close(errors.New("incoming data processing ended"))
		}
	}()

	for {
		if t.closed.Load() {
			log.Printf("[DEBUG] ShmClientTransport.processIncomingData: transport closed, exiting")
			return
		}
		log.Printf("[DEBUG] ShmClientTransport.processIncomingData: waiting for frame from server...")
		// Event-driven: block on next frame from rx ring.
		fh, payload, err := readFrame(t.serverToClient, ctx)
		if err != nil {
			log.Printf("[DEBUG] ShmClientTransport.processIncomingData: readFrame error: %v", err)
			if errors.Is(err, io.EOF) {
				return
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			if errors.Is(err, ErrRingClosed) || t.closed.Load() {
				return
			}
			continue
		}
		log.Printf("[DEBUG] ShmClientTransport.processIncomingData: received frame type=%d, streamID=%d, length=%d", fh.Type, fh.StreamID, fh.Length)

		// Transport-level frames are not associated with a particular stream.
		switch fh.Type {
		case FrameTypeGOAWAY:
			// Server is draining or closing the connection.
			// Treat this as a signal to stop creating new streams.
			t.draining.Store(true)
			select {
			case <-t.goAwayCh:
				// already closed
			default:
				close(t.goAwayCh)
			}
			// If server requests immediate close, tear down the transport.
			if fh.Flags&GoAwayFlagIMMEDIATE != 0 {
				go t.Close(errors.New("received GOAWAY (immediate)"))
				return
			}
			// Otherwise, close when the last active stream completes.
			t.mu.RLock()
			active := len(t.streams)
			t.mu.RUnlock()
			if active == 0 {
				go t.Close(errors.New("received GOAWAY (draining) with no active streams"))
				return
			}
			continue
		}

		// Dispatch frame to appropriate stream
		t.mu.RLock()
		stream, ok := t.streams[fh.StreamID]
		t.mu.RUnlock()

		if !ok {
			// Stream not found - might have been closed
			continue
		}

		// Handle different frame types
		switch fh.Type {
		case FrameTypeHEADERS:
			// Server sent headers (response headers)
			_, err := decodeHeaders(payload)
			if err != nil {
				stream.write(recvMsg{err: err})
				continue
			}

			// Signal that headers have been received
			if atomic.CompareAndSwapUint32(&stream.headerChanClosed, 0, 1) {
				close(stream.headerChan)
			}

		case FrameTypeMESSAGE:
			// Server sent a message
			// Copy payload to avoid using stale buffer data
			buf := mem.Copy(payload, mem.DefaultBufferPool())
			stream.write(recvMsg{buffer: buf})

		case FrameTypeTRAILERS:
			// Server sent trailers (end of stream)
			tr, err := decodeTrailers(payload)
			if err != nil {
				t.closeStream(stream, err, false, 0, nil, nil, false)
			} else {
				// Convert metadata from protocol format to map
				trailerMap := make(map[string][]string)
				for _, kv := range tr.Metadata {
					trailerMap[kv.Key] = make([]string, len(kv.Values))
					for i, v := range kv.Values {
						trailerMap[kv.Key][i] = string(v)
					}
				}

				// Convert status
				var st *status.Status
				if tr.GRPCStatusCode != 0 {
					st = status.New(codes.Code(tr.GRPCStatusCode), tr.GRPCStatusMsg)
					err = st.Err()
				} else {
					st = status.New(codes.OK, "")
					err = io.EOF
				}

				// Close the stream with trailers
				t.closeStream(stream, err, false, 0, st, trailerMap, true)
			}

		case FrameTypeCANCEL:
			// Server cancelled the stream
			stream.write(recvMsg{err: context.Canceled})

		default:
			// Unknown frame type - ignore
		}
	}
}

// processFrameData processes incoming gRPC frame data
func (t *ShmClientTransport) processFrameData(data []byte) error {
	// TODO: Implement gRPC frame parsing and routing to streams
	// For now, this is a placeholder that will be implemented in the next step
	return nil
}

// Close tears down this transport. Once it returns, the transport
// should not be accessed any more. The caller must make sure this
// is called only once.
func (t *ShmClientTransport) Close(err error) {
	t.closeOnce.Do(func() {
		// Mark closed early so late closeStream calls won't attempt to write to the
		// rings while teardown is in progress.
		t.closed.Store(true)

		// Cancel context to stop background reader goroutine.
		t.cancel()

		// Terminate all active streams before closing/unmapping the segment.
		// This prevents concurrent stream Close paths from touching unmapped ring
		// memory.
		t.mu.Lock()
		streams := make([]*ClientStream, 0, len(t.streams))
		for _, stream := range t.streams {
			if stream != nil {
				streams = append(streams, stream)
			}
		}
		t.mu.Unlock()
		for _, stream := range streams {
			t.closeStream(stream, err, false, 0, status.Convert(err), nil, false)
		}

		// Close the rings and wait for the background reader to exit before
		// unmapping.
		if t.clientToServer != nil {
			_ = t.clientToServer.Close()
		}
		if t.serverToClient != nil {
			_ = t.serverToClient.Close()
		}
		t.readerWG.Wait()

		// Close the segment last.
		if t.segment != nil {
			_ = t.segment.Close()
		}

		// Signal closure
		close(t.errCh)
	})
}

// GracefulClose starts to tear down the transport: the transport will stop
// accepting new RPCs and NewStream will return error. Once all streams are
// finished, the transport will close.
//
// It does not block.
func (t *ShmClientTransport) GracefulClose() {
	// Mirror http2 client semantics: move into draining, which prevents new
	// streams from being created. Close the transport only after the last active
	// stream completes.
	if t.closed.Load() {
		return
	}
	if !t.draining.CompareAndSwap(false, true) {
		return
	}

	// Best-effort notify the peer we're draining.
	if t.clientToServer != nil {
		_ = writeFrame(t.clientToServer, FrameHeader{Type: FrameTypeGOAWAY, Flags: GoAwayFlagDRAINING}, nil, context.Background())
	}

	// If there are no active streams, close immediately.
	t.mu.RLock()
	active := len(t.streams)
	t.mu.RUnlock()
	if active == 0 {
		t.Close(errors.New("no active streams left to process while draining"))
	}
}

// NewStream creates a Stream for an RPC.
func (t *ShmClientTransport) NewStream(ctx context.Context, callHdr *CallHdr) (*ClientStream, error) {
	if t.closed.Load() || t.draining.Load() {
		return nil, &NewStreamError{Err: ErrConnClosing, AllowTransparentRetry: true}
	}

	t.mu.Lock()
	if t.closed.Load() || t.draining.Load() {
		t.mu.Unlock()
		return nil, &NewStreamError{Err: ErrConnClosing, AllowTransparentRetry: true}
	}
	// Assign stream ID (client uses odd IDs, starting from 1)
	streamID := t.streamID
	if streamID == 0 {
		streamID = 1
	}
	t.streamID = streamID + 2 // Increment by 2 to maintain odd IDs

	// Create the client stream
	s := &ClientStream{
		Stream: &Stream{
			id:             streamID,
			ctx:            ctx,
			method:         callHdr.Method,
			sendCompress:   callHdr.SendCompress,
			buf:            newRecvBuffer(),
			contentSubtype: callHdr.ContentSubtype,
		},
		ct:         t, // Set the client transport (now an interface, no unsafe needed)
		done:       make(chan struct{}),
		headerChan: make(chan struct{}),
		doneFunc:   callHdr.DoneFunc,
	}

	// Set up transport reader for this stream
	s.trReader = &transportReader{
		reader: &recvBufferReader{
			ctx:     s.ctx,
			ctxDone: s.ctx.Done(),
			recv:    s.buf,
			closeStream: func(err error) {
				s.Close(err)
			},
		},
		windowHandler: func(n int) {
			// Flow control: for shm transport, we don't need traditional flow control
			// as the ring buffer already provides backpressure
		},
	}

	// Set requestRead callback (required by Stream.ReadMessageHeader)
	// For shared memory transport, flow control is handled by the ring buffer
	s.requestRead = func(n int) {
		// No-op: shared memory transport doesn't need explicit flow control
	}

	// Register the stream
	t.streams[streamID] = s
	t.streamTransport[s] = t
	t.mu.Unlock()

	// Send HEADERS frame to initiate the stream
	var deadlineUnixNano uint64
	if deadline, ok := ctx.Deadline(); ok {
		if unixNano := deadline.UnixNano(); unixNano > 0 {
			deadlineUnixNano = uint64(unixNano)
		}
	}
	hdr := HeadersV1{
		Version:          1,
		HdrType:          0, // client-initial
		Method:           callHdr.Method,
		Authority:        callHdr.Host,
		DeadlineUnixNano: deadlineUnixNano,
		Metadata:         nil, // TODO: extract metadata from context
	}

	payload := encodeHeaders(hdr)
	fh := FrameHeader{
		StreamID: streamID,
		Type:     FrameTypeHEADERS,
		Flags:    HeadersFlagINITIAL,
	}

	if err := writeFrame(t.clientToServer, fh, payload, ctx); err != nil {
		t.mu.Lock()
		delete(t.streams, streamID)
		delete(t.streamTransport, s)
		t.mu.Unlock()
		// If draining was initiated concurrently and there are no streams left,
		// ensure the transport completes draining.
		if t.draining.Load() {
			t.mu.RLock()
			active := len(t.streams)
			t.mu.RUnlock()
			if active == 0 {
				go t.Close(errors.New("draining with no active streams"))
			}
		}
		return nil, &NewStreamError{Err: err, AllowTransparentRetry: true}
	}

	return s, nil
}

// Error returns a channel that is closed when some I/O error
// happens. Typically the caller should have a goroutine to monitor
// this in order to take action (e.g., close the current transport
// and create a new one) in error case. It should not return nil
// once the transport is initiated.
func (t *ShmClientTransport) Error() <-chan struct{} {
	return t.errCh
}

// GoAway returns a channel that is closed when ClientTransport
// receives the draining signal from the server (e.g., GOAWAY frame in
// HTTP/2).
func (t *ShmClientTransport) GoAway() <-chan struct{} {
	return t.goAwayCh
}

// GetGoAwayReason returns the reason why GoAway frame was received, along
// with a human readable string with debug info.
func (t *ShmClientTransport) GetGoAwayReason() (GoAwayReason, string) {
	// TODO: Implement proper GoAway reason tracking
	return GoAwayInvalid, "shared memory transport closed"
}

// RemoteAddr returns the remote network address.
func (t *ShmClientTransport) RemoteAddr() net.Addr {
	return t.remoteAddr
}

// incrMsgRecv increments the message received counter.
// This is called by ClientStream.Read() when a message is successfully read.
func (t *ShmClientTransport) incrMsgRecv() {
	// For shm transport, we don't track channelz metrics yet
	// This is a no-op for now, but maintains compatibility with ClientStream
}

// closeStream closes the given stream and cleans up resources.
// This is called by ClientStream.Close() to terminate the stream.
func (t *ShmClientTransport) closeStream(s *ClientStream, err error, rst bool, rstCode http2.ErrCode, st *status.Status, mdata map[string][]string, eosReceived bool) {
	// Set stream state to done
	if s.swapState(streamDone) == streamDone {
		// Already done, wait for first closer to finish
		<-s.done
		return
	}

	// Update status and trailers
	s.status = st
	if len(mdata) > 0 {
		s.trailer = mdata
	}

	// Signal error to readers if present
	if err != nil {
		s.write(recvMsg{err: err})
	}

	// Close header channel if not already closed
	if atomic.CompareAndSwapUint32(&s.headerChanClosed, 0, 1) {
		s.noHeaders = true
		close(s.headerChan)
	}

	// Remove stream from active streams map
	var shouldClose bool
	t.mu.Lock()
	delete(t.streams, s.id)
	delete(t.streamTransport, s)
	shouldClose = t.draining.Load() && len(t.streams) == 0 && !t.closed.Load()
	t.mu.Unlock()

	// Send CANCEL frame if requested
	if rst && !t.closed.Load() {
		fh := FrameHeader{
			StreamID: s.id,
			Type:     FrameTypeCANCEL,
			Flags:    0,
		}
		// Best effort - ignore errors since stream is closing anyway
		_ = writeFrame(t.clientToServer, fh, nil, context.Background())
	}

	// Close the done channel to unblock waiters
	close(s.done)

	if shouldClose {
		go t.Close(errors.New("transport drained"))
	}

	// Call doneFunc if present
	if s.doneFunc != nil {
		s.doneFunc()
	}
}

// write writes data to the stream via the shared memory transport.
// This is called by ClientStream.Write() to send data.
func (t *ShmClientTransport) write(s *ClientStream, hdr []byte, data mem.BufferSlice, opts *WriteOptions) error {
	// Check if transport is closed
	if t.closed.Load() {
		return ErrConnClosing
	}

	// Check stream state
	if opts.Last {
		// Last message - transition to write done state
		if !s.compareAndSwapState(streamActive, streamWriteDone) {
			return errStreamDone
		}
	} else if s.getState() != streamActive {
		return errStreamDone
	}

	// Combine header and data into a single payload
	var payload []byte
	if hdr != nil {
		payload = append(payload, hdr...)
	}
	if data.Len() > 0 {
		// Materialize the BufferSlice into a contiguous byte slice
		payload = append(payload, data.Materialize()...)
	}

	// Write MESSAGE frame. MessageFlagMORE indicates more data will follow.
	fh := FrameHeader{
		StreamID: s.id,
		Type:     FrameTypeMESSAGE,
		Flags:    0,
	}
	if opts != nil && !opts.Last {
		fh.Flags = MessageFlagMORE
	}

	if err := writeFrame(t.clientToServer, fh, payload, s.ctx); err != nil {
		return err
	}

	return nil
}

// Compile-time check to ensure ShmClientTransport implements clientTransport.
var _ clientTransport = (*ShmClientTransport)(nil)
