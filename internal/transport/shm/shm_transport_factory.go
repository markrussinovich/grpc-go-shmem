//go:build linux && (amd64 || arm64)

package shm

import (
	"context"
	"errors"
	"net"

	"google.golang.org/grpc/internal/transport"
)

// Common errors
var (
	ErrInvalidConnection = errors.New("invalid connection type")
)

// ServerTransportFactory creates server transports for shared memory connections
type ServerTransportFactory struct {
	listener *ShmListener
}

// NewServerTransportFactory creates a new server transport factory
func NewServerTransportFactory(addr string, segmentSize, ringASize, ringBSize uint64) (*ServerTransportFactory, error) {
	shmAddr := &ShmAddr{Name: addr}
	listener, err := NewShmListener(shmAddr, segmentSize, ringASize, ringBSize)
	if err != nil {
		return nil, err
	}

	return &ServerTransportFactory{
		listener: listener,
	}, nil
}

// Accept waits for and returns the next connection transport
func (f *ServerTransportFactory) Accept() (transport.ServerTransport, error) {
	conn, err := f.listener.Accept()
	if err != nil {
		return nil, err
	}

	// Extract the server transport from the connection
	shmConn, ok := conn.(*shmConn)
	if !ok {
		conn.Close()
		return nil, ErrInvalidConnection
	}

	return shmConn.transport, nil
}

// Close closes the transport factory
func (f *ServerTransportFactory) Close() error {
	return f.listener.Close()
}

// Addr returns the factory's address
func (f *ServerTransportFactory) Addr() net.Addr {
	return f.listener.Addr()
}

// ClientTransportFactory creates client transports for shared memory connections
type ClientTransportFactory struct {
	dialer *ShmDialer
}

// NewClientTransportFactory creates a new client transport factory
func NewClientTransportFactory(opts *DialOptions) *ClientTransportFactory {
	return &ClientTransportFactory{
		dialer: NewShmDialer(opts),
	}
}

// NewTransport creates a new client transport to the given address
func (f *ClientTransportFactory) NewTransport(ctx context.Context, addr string) (transport.ClientTransport, error) {
	return DialShm(ctx, addr, f.dialer.opts)
}

// ShmTransportOptions contains options for shared memory transport
type ShmTransportOptions struct {
	// Server options
	ServerSegmentSize uint64
	ServerRingASize   uint64
	ServerRingBSize   uint64

	// Client options
	ClientDialOptions *DialOptions
}

// DefaultShmTransportOptions returns default transport options
func DefaultShmTransportOptions() *ShmTransportOptions {
	return &ShmTransportOptions{
		ServerSegmentSize: DefaultSegmentSize,
		ServerRingASize:   DefaultRingASize,
		ServerRingBSize:   DefaultRingBSize,
		ClientDialOptions: DefaultDialOptions(),
	}
}

// CreateShmServerTransportFactory creates a factory for server transports
func CreateShmServerTransportFactory(addr string, opts *ShmTransportOptions) (*ServerTransportFactory, error) {
	if opts == nil {
		opts = DefaultShmTransportOptions()
	}

	return NewServerTransportFactory(addr, opts.ServerSegmentSize, opts.ServerRingASize, opts.ServerRingBSize)
}

// CreateShmClientTransportFactory creates a factory for client transports
func CreateShmClientTransportFactory(opts *ShmTransportOptions) *ClientTransportFactory {
	if opts == nil {
		opts = DefaultShmTransportOptions()
	}

	return NewClientTransportFactory(opts.ClientDialOptions)
}
