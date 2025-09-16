//go:build linux && (amd64 || arm64)

package shm

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	
	"google.golang.org/grpc/internal/transport"
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
	streams  map[uint32]*transport.ClientStream
	streamID uint32 // next stream ID to assign

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
		segment:        segment,
		clientToServer: clientToServer,
		serverToClient: serverToClient,
		localAddr:      localAddr,
		remoteAddr:     remoteAddr,
		ctx:            ctx,
		cancel:         cancel,
		streams:        make(map[uint32]*transport.ClientStream),
		errCh:          make(chan struct{}),
		goAwayCh:       make(chan struct{}),
	}

    // Start processing incoming data from the server (test hook guarded)
    if enableClientReader.Load() {
        go t.processIncomingData()
    }

	return t, nil
}

// processIncomingData reads data from the server->client ring and processes gRPC frames
func (t *ShmClientTransport) processIncomingData() {
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
        fh, payload, err := readFrame(t.serverToClient)
        if err != nil {
            if errors.Is(err, ErrRingClosed) || t.closed.Load() {
                return
            }
            continue
        }
        _ = fh
        _ = payload
        // TODO: dispatch to streams once stream management is in place.
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
			// Signal stream termination
			if stream != nil {
				// TODO: Properly terminate streams
			}
		}
		t.streams = make(map[uint32]*transport.ClientStream)
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
func (t *ShmClientTransport) NewStream(ctx context.Context, callHdr *transport.CallHdr) (*transport.ClientStream, error) {
	if t.closed.Load() {
		return nil, transport.ErrConnClosing
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Assign stream ID
	streamID := t.streamID
	t.streamID++

	// TODO: Create actual client stream
	// For now, return a placeholder
	_ = streamID // Use the variable to avoid compiler error
	return nil, errors.New("NewStream not fully implemented yet")
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
func (t *ShmClientTransport) GetGoAwayReason() (transport.GoAwayReason, string) {
	// TODO: Implement proper GoAway reason tracking
	return transport.GoAwayInvalid, "shared memory transport closed"
}

// RemoteAddr returns the remote network address.
func (t *ShmClientTransport) RemoteAddr() net.Addr {
	return t.remoteAddr
}
