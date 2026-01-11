//go:build linux

package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc/keepalive"
)

// DialOptions contains options for dialing a shared memory connection
type DialOptions struct {
	// SegmentSize is the total size of the shared memory segment
	SegmentSize uint64

	// RingASize is the size of ring A (client->server)
	RingASize uint64

	// RingBSize is the size of ring B (server->client)
	RingBSize uint64

	// Timeout for connection establishment
	ConnectTimeout time.Duration

	// KeepaliveParams stores the keepalive parameters for the client.
	KeepaliveParams keepalive.ClientParameters
}

// DefaultDialOptions returns sensible defaults for dialing
func DefaultDialOptions() *DialOptions {
	return &DialOptions{
		SegmentSize:    DefaultSegmentSize,
		RingASize:      DefaultRingASize,
		RingBSize:      DefaultRingBSize,
		ConnectTimeout: 30 * time.Second,
	}
}

// DialShm creates a new shared memory connection to the given address
func DialShm(ctx context.Context, addr string, opts *DialOptions) (ClientTransport, error) {
	if opts == nil {
		opts = DefaultDialOptions()
	}

	// Apply timeout
	if opts.ConnectTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.ConnectTimeout)
		defer cancel()
	}

	// Establish the data segment to use via the server's control segment.
	ctlName := addr + shmControlSuffix
	ctlSeg, err := OpenSegment(ctlName)
	if err != nil {
		return nil, fmt.Errorf("open control segment %q: %w", ctlName, err)
	}
	defer ctlSeg.Close()
	if err := ctlSeg.WaitForServer(ctx); err != nil {
		return nil, fmt.Errorf("wait for control server: %w", err)
	}

	ctlTx := NewShmRingFromSegment(ctlSeg.A, ctlSeg.Mem)
	ctlRx := NewShmRingFromSegment(ctlSeg.B, ctlSeg.Mem)

	if err := writeFrame(ctlTx, FrameHeader{Type: FrameTypeCONNECT}, encodeConnectRequest(connectRequest{}), ctx); err != nil {
		return nil, fmt.Errorf("send connect request: %w", err)
	}
	respFH, respPayload, err := readFrame(ctlRx, ctx)
	if err != nil {
		return nil, fmt.Errorf("read connect response: %w", err)
	}
	switch respFH.Type {
	case FrameTypeACCEPT:
		resp, err := decodeConnectResponse(respPayload)
		if err != nil {
			return nil, fmt.Errorf("decode accept: %w", err)
		}
		segName := resp.segmentName
		segment, err := OpenSegment(segName)
		if err != nil {
			return nil, fmt.Errorf("open data segment %q: %w", segName, err)
		}
		// Wait for server readiness via futex (event-driven).
		if err := segment.WaitForServer(ctx); err != nil {
			segment.Close()
			return nil, fmt.Errorf("wait for server ready: %w", err)
		}

		localAddr := &ShmAddr{Name: segName + "_client"}
		remoteAddr := &ShmAddr{Name: segName}
		clientTransport, err := NewShmClientTransport(segment, localAddr, remoteAddr)
		if err != nil {
			segment.Close()
			return nil, fmt.Errorf("failed to create client transport: %v", err)
		}
		// Configure keepalive if params are provided.
		clientTransport.ConfigureKeepalive(opts.KeepaliveParams)
		return clientTransport, nil
	case FrameTypeREJECT:
		r, err := decodeConnectReject(respPayload)
		if err != nil {
			return nil, fmt.Errorf("connect rejected (decode): %w", err)
		}
		return nil, fmt.Errorf("connect rejected: %s", r.message)
	default:
		return nil, fmt.Errorf("unexpected control frame type %d", respFH.Type)
	}
}

// ShmDialer provides a dialer function for gRPC
type ShmDialer struct {
	opts *DialOptions
}

// NewShmDialer creates a new shared memory dialer
func NewShmDialer(opts *DialOptions) *ShmDialer {
	if opts == nil {
		opts = DefaultDialOptions()
	}
	return &ShmDialer{opts: opts}
}

// Dial creates a new connection
func (d *ShmDialer) Dial(ctx context.Context, addr string) (net.Conn, error) {
	// For shared memory, we bypass the net.Conn interface and return
	// a connection that can provide the transport directly
	clientTransport, err := DialShm(ctx, addr, d.opts)
	if err != nil {
		return nil, err
	}

	// Wrap the transport in a connection-like interface
	return &shmClientConn{
		transport:  clientTransport.(*ShmClientTransport),
		localAddr:  clientTransport.(*ShmClientTransport).localAddr,
		remoteAddr: clientTransport.(*ShmClientTransport).remoteAddr,
	}, nil
}

// shmClientConn wraps the client transport as a net.Conn
type shmClientConn struct {
	transport  *ShmClientTransport
	localAddr  net.Addr
	remoteAddr net.Addr
	closed     bool
}

// Read implements net.Conn - not used directly in gRPC
func (c *shmClientConn) Read(b []byte) (n int, err error) {
	if c.closed {
		return 0, errors.New("connection closed")
	}
	return 0, errors.New("direct read not supported, use transport layer")
}

// Write implements net.Conn - not used directly in gRPC
func (c *shmClientConn) Write(b []byte) (n int, err error) {
	if c.closed {
		return 0, errors.New("connection closed")
	}
	return 0, errors.New("direct write not supported, use transport layer")
}

// Close implements net.Conn
func (c *shmClientConn) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	c.transport.Close(errors.New("connection closed"))
	return nil
}

// LocalAddr implements net.Conn
func (c *shmClientConn) LocalAddr() net.Addr {
	return c.localAddr
}

// RemoteAddr implements net.Conn
func (c *shmClientConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

// SetDeadline implements net.Conn
func (c *shmClientConn) SetDeadline(t time.Time) error {
	return nil // Shared memory doesn't support deadlines
}

// SetReadDeadline implements net.Conn
func (c *shmClientConn) SetReadDeadline(t time.Time) error {
	return nil // Shared memory doesn't support deadlines
}

// SetWriteDeadline implements net.Conn
func (c *shmClientConn) SetWriteDeadline(t time.Time) error {
	return nil // Shared memory doesn't support deadlines
}

// GetClientTransport returns the underlying client transport
func (c *shmClientConn) GetClientTransport() ClientTransport {
	return c.transport
}
