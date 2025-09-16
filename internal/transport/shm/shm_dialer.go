//go:build linux && (amd64 || arm64)

package shm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc/internal/transport"
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
func DialShm(ctx context.Context, addr string, opts *DialOptions) (transport.ClientTransport, error) {
	if opts == nil {
		opts = DefaultDialOptions()
	}

	// Apply timeout
	if opts.ConnectTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.ConnectTimeout)
		defer cancel()
	}

	// Generate a unique segment name for this connection
	segmentName := fmt.Sprintf("grpc_shm_%s_%d", addr, time.Now().UnixNano())

	// Create the shared memory segment
	segment, err := CreateSegment(segmentName, opts.RingASize, opts.RingBSize)
	if err != nil {
		return nil, fmt.Errorf("failed to create segment: %v", err)
	}

	// Mark client as ready
	segment.H.SetClientReady(true)

	// Wait for server to be ready
	if err := waitForServerReady(ctx, segment); err != nil {
		segment.Close()
		return nil, fmt.Errorf("failed to establish connection: %v", err)
	}

	// Create client transport
	localAddr := &ShmAddr{Name: segmentName + "_client"}
	remoteAddr := &ShmAddr{Name: addr}

	clientTransport, err := NewShmClientTransport(segment, localAddr, remoteAddr)
	if err != nil {
		segment.Close()
		return nil, fmt.Errorf("failed to create client transport: %v", err)
	}

	return clientTransport, nil
}

// waitForServerReady waits for the server to mark itself as ready
func waitForServerReady(ctx context.Context, segment *Segment) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if segment.H.ServerReady() {
				return nil
			}
		}
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
func (c *shmClientConn) GetClientTransport() transport.ClientTransport {
	return c.transport
}
