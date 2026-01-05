//go:build linux

package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"golang.org/x/net/http2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
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
	mu     sync.RWMutex

	// Stream management
	streams         map[uint32]*ClientStream
	streamTransport map[*ClientStream]*ShmClientTransport // Track transport for each stream
	streamID        uint32                                // next stream ID to assign

	// Error handling
	closeOnce sync.Once
	errCh     chan struct{}
	goAwayCh  chan struct{}
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
		go t.processIncomingData(context.Background())
	}

	return t, nil
}

// processIncomingData reads data from the server->client ring and processes gRPC frames
func (t *ShmClientTransport) processIncomingData(ctx context.Context) {
	defer func() {
		if !t.closed.Load() {
			t.Close(errors.New("incoming data processing ended"))
		}
	}()

	for {
		if t.closed.Load() {
			return
		}
		// Event-driven: block on next frame from rx ring.
		fh, payload, err := readFrame(t.serverToClient, ctx)
		if err != nil {
			if errors.Is(err, ErrRingClosed) || t.closed.Load() {
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
			// Wrap payload in mem.Buffer
			buf := mem.NewBuffer(&payload, nil)
			stream.write(recvMsg{buffer: buf})
			
		case FrameTypeTRAILERS:
			// Server sent trailers (end of stream)
			tr, err := decodeTrailers(payload)
			if err != nil {
				stream.write(recvMsg{err: err})
			} else {
				// Store trailer metadata
				stream.trailer = metadata.MD{}
				for _, kv := range tr.Metadata {
					stream.trailer[kv.Key] = make([]string, len(kv.Values))
					for i, v := range kv.Values {
						stream.trailer[kv.Key][i] = string(v)
					}
				}
				
				// Convert status to error if not OK
				if tr.GRPCStatusCode != 0 {
					err = status.Error(codes.Code(tr.GRPCStatusCode), tr.GRPCStatusMsg)
				} else {
					err = io.EOF
				}
				stream.write(recvMsg{err: err})
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
		t.closed.Store(true)

		// Cancel context to stop all goroutines
		t.cancel()

		// Close the rings
		t.clientToServer.Close()
		t.serverToClient.Close()

		// Close the segment
		if t.segment != nil {
			t.segment.Close()
		}

		// Terminate all active streams
		t.mu.Lock()
		for _, stream := range t.streams {
			if stream != nil {
				// Close the stream with the transport error
				stream.write(recvMsg{err: err})
				close(stream.done)
				if atomic.CompareAndSwapUint32(&stream.headerChanClosed, 0, 1) {
					close(stream.headerChan)
				}
			}
		}
		t.streams = make(map[uint32]*ClientStream)
		t.streamTransport = make(map[*ClientStream]*ShmClientTransport)
		t.mu.Unlock()

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
	// TODO: Implement graceful close signaling
	t.Close(errors.New("graceful close requested"))
}

// NewStream creates a Stream for an RPC.
func (t *ShmClientTransport) NewStream(ctx context.Context, callHdr *CallHdr) (*ClientStream, error) {
	if t.closed.Load() {
		return nil, ErrConnClosing
	}

	t.mu.Lock()
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
	
	// Register the stream
	t.streams[streamID] = s
	t.streamTransport[s] = t
	t.mu.Unlock()

	// Send HEADERS frame to initiate the stream
	hdr := HeadersV1{
		Version:          1,
		HdrType:          0, // client-initial
		Method:           callHdr.Method,
		Authority:        callHdr.Host,
		DeadlineUnixNano: 0, // TODO: extract from ctx if present
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
		return nil, err
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
	t.mu.Lock()
	delete(t.streams, s.id)
	delete(t.streamTransport, s)
	t.mu.Unlock()

	// Send CANCEL frame if requested
	if rst {
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

	// Write MESSAGE frame
	fh := FrameHeader{
		StreamID: s.id,
		Type:     FrameTypeMESSAGE,
		Flags:    0,
	}
	if opts.Last {
		fh.Flags = MessageFlagMORE // Indicate more data coming (trailers)
	}

	if err := writeFrame(t.clientToServer, fh, payload, s.ctx); err != nil {
		return err
	}

	// If this is the last write, send trailers
	if opts.Last {
		tr := TrailersV1{
			GRPCStatusCode: 0, // OK
			GRPCStatusMsg:  "",
			Metadata:       nil,
		}
		trPayload := encodeTrailers(tr)
		trFh := FrameHeader{
			StreamID: s.id,
			Type:     FrameTypeTRAILERS,
			Flags:    TrailersFlagEND_STREAM,
		}
		if err := writeFrame(t.clientToServer, trFh, trPayload, s.ctx); err != nil {
			return err
		}
	}

	return nil
}

// Compile-time check to ensure ShmClientTransport implements clientTransport.
var _ clientTransport = (*ShmClientTransport)(nil)

