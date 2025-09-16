//go:build linux && (amd64 || arm64)

package shm

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/internal/transport"
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
	streams    map[uint32]*transport.ServerStream
	streamID   uint32 // next stream ID to assign
	handleFunc func(*transport.ServerStream)

	// Error handling
	closeOnce sync.Once
	errCh     chan struct{}
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
		streams: make(map[uint32]*transport.ServerStream),
		errCh:   make(chan struct{}),
	}

	return t, nil
}

// HandleStreams receives incoming streams using the given handler.
// This is typically run in a separate goroutine.
func (t *ShmServerTransport) HandleStreams(ctx context.Context, handle func(*transport.ServerStream)) {
	t.mu.Lock()
	if t.closed.Load() {
		t.mu.Unlock()
		return
	}
	t.handleFunc = handle
	t.mu.Unlock()

	// Start processing incoming data from the client
	go t.processIncomingData(ctx)

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
			t.Close(errors.New("incoming data processing ended"))
		}
	}()

	// Buffer for reading frames
	buf := make([]byte, 64*1024) // 64KB buffer

	for {
		// Check for cancellation first
		select {
		case <-ctx.Done():
			return
		case <-t.ctx.Done():
			return
		default:
		}

		if t.closed.Load() {
			return
		}

		// Read data from client->server ring with timeout by using context
		// We'll implement a non-blocking check first
		if t.clientToServer.IsEmpty() {
			// Brief sleep to avoid busy waiting
			select {
			case <-ctx.Done():
				return
			case <-t.ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
				continue
			}
		}

		// Try to read data (this might still block, but less likely)
		n, err := t.clientToServer.ReadBlocking(buf)
		if err != nil {
			if errors.Is(err, ErrRingClosed) || t.closed.Load() {
				return // Ring closed, exit gracefully
			}
			// Other errors - continue with brief pause
			select {
			case <-ctx.Done():
				return
			case <-t.ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
				continue
			}
		}

		if n > 0 {
			// Process the received data as gRPC frames
			if err := t.processFrameData(buf[:n]); err != nil {
				// Log error but continue processing
				continue
			}
		}
	}
}

// processFrameData processes incoming gRPC frame data
func (t *ShmServerTransport) processFrameData(data []byte) error {
	// TODO: Implement gRPC frame parsing and routing to streams
	// For now, this is a placeholder that will be implemented in the next step
	return nil
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
		t.streams = make(map[uint32]*transport.ServerStream)
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
func (t *ShmServerTransport) writeHeader(s *transport.ServerStream, md metadata.MD) error {
	if t.closed.Load() {
		return transport.ErrConnClosing
	}

	// TODO: Implement header frame writing
	return nil
}

// write writes header and data for a stream
func (t *ShmServerTransport) write(s *transport.ServerStream, hdr []byte, data mem.BufferSlice, opts *transport.WriteOptions) error {
	if t.closed.Load() {
		return transport.ErrConnClosing
	}

	// TODO: Implement data frame writing
	return nil
}

// writeStatus writes status for a stream
func (t *ShmServerTransport) writeStatus(s *transport.ServerStream, st *status.Status) error {
	if t.closed.Load() {
		return transport.ErrConnClosing
	}

	// TODO: Implement status frame writing
	return nil
}

// incrMsgRecv increments the message received counter
func (t *ShmServerTransport) incrMsgRecv() {
	// TODO: Implement stats tracking
}
