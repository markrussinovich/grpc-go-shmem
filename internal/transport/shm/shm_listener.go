//go:build linux && (amd64 || arm64)

package shm

import (
    "context"
    "errors"
    "fmt"
    "net"
    "sync"
    "sync/atomic"
    "time"

    "google.golang.org/grpc/internal/transport"
)

// ShmAddr represents a shared memory network address
type ShmAddr struct {
	Name string // Segment name/identifier
}

// Network returns the network type
func (a *ShmAddr) Network() string {
	return "shm"
}

// String returns the string representation of the address
func (a *ShmAddr) String() string {
	return a.Name
}

// ShmListener implements net.Listener for shared memory connections
type ShmListener struct {
    addr     *ShmAddr
    baseName string // Base name for segment creation
    connID   uint64 // Atomic counter for connection IDs
    segment  *Segment

	// Lifecycle management
	ctx       context.Context
	cancel    context.CancelFunc
	closed    atomic.Bool
	closeOnce sync.Once

	// Connection handling
	connCh   chan net.Conn
	acceptCh chan *shmConn
	mu       sync.RWMutex

	// Configuration
	segmentSize uint64
	ringASize   uint64
	ringBSize   uint64
}

// shmConn represents a shared memory connection
type shmConn struct {
	segment    *Segment
	localAddr  net.Addr
	remoteAddr net.Addr
	transport  *ShmServerTransport

	// Connection state
	established atomic.Bool
	closed      atomic.Bool
	closeOnce   sync.Once
}

// NewShmListener creates a new shared memory listener
func NewShmListener(addr *ShmAddr, segmentSize, ringASize, ringBSize uint64) (*ShmListener, error) {
	if addr == nil {
		return nil, errors.New("address cannot be nil")
	}

	ctx, cancel := context.WithCancel(context.Background())

	l := &ShmListener{
		addr:        addr,
		baseName:    addr.Name,
		ctx:         ctx,
		cancel:      cancel,
		connCh:      make(chan net.Conn, 10), // Buffer for incoming connections
		acceptCh:    make(chan *shmConn, 10),
		segmentSize: segmentSize,
		ringASize:   ringASize,
		ringBSize:   ringBSize,
	}

    // Server owns lifetime: create a single segment now and mark server ready.
    seg, err := CreateSegment(addr.Name, ringASize, ringBSize)
    if err != nil {
        cancel()
        return nil, fmt.Errorf("create segment: %w", err)
    }
    l.segment = seg
    l.segment.H.SetServerReady(true)

    return l, nil
}

// acceptLoop continuously monitors for new connection requests
func (l *ShmListener) acceptLoop() {}

// checkForConnections looks for new segment creation requests
func (l *ShmListener) checkForConnections() error { return nil }

// handlePotentialConnection processes a potential new connection
func (l *ShmListener) handlePotentialConnection(segmentName string) (*shmConn, error) {
    // Single-connection mode: wait for client to set ClientReady on our precreated segment.
    segment := l.segment
    // Block event-driven until client ready.
    if err := segment.WaitForClient(l.ctx); err != nil {
        return nil, err
    }
    conn := &shmConn{
        segment:    segment,
        localAddr:  l.addr,
        remoteAddr: &ShmAddr{Name: segmentName + "_client"},
    }
    serverTransport, err := NewShmServerTransport(segment, l.addr, conn.remoteAddr)
    if err != nil {
        return nil, fmt.Errorf("failed to create server transport: %v", err)
    }

    conn.transport = serverTransport
    conn.established.Store(true)
    return conn, nil
}

// Accept waits for and returns the next connection to the listener
func (l *ShmListener) Accept() (net.Conn, error) {
    if l.closed.Load() {
        return nil, errors.New("listener closed")
    }
    // Use single-connection blocking accept using event-driven handshake.
    conn, err := l.handlePotentialConnection(l.addr.Name)
    if err != nil {
        return nil, err
    }
    return conn, nil
}

// Close closes the listener
func (l *ShmListener) Close() error {
	l.closeOnce.Do(func() {
		l.closed.Store(true)
		l.cancel()
		close(l.acceptCh)
	})
	return nil
}

// Addr returns the listener's network address
func (l *ShmListener) Addr() net.Addr {
	return l.addr
}

// shmConn net.Conn implementation

// Read reads data from the connection
func (c *shmConn) Read(b []byte) (n int, err error) {
	if c.closed.Load() {
		return 0, errors.New("connection closed")
	}

	// For shared memory, reading is handled by the transport layer
	// This is a placeholder implementation
	return 0, errors.New("direct read not supported, use transport layer")
}

// Write writes data to the connection
func (c *shmConn) Write(b []byte) (n int, err error) {
	if c.closed.Load() {
		return 0, errors.New("connection closed")
	}

	// For shared memory, writing is handled by the transport layer
	// This is a placeholder implementation
	return 0, errors.New("direct write not supported, use transport layer")
}

// Close closes the connection
func (c *shmConn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)

		if c.transport != nil {
			c.transport.Close(errors.New("connection closed"))
		}

		if c.segment != nil {
			c.segment.Close()
		}
	})
	return nil
}

// LocalAddr returns the local network address
func (c *shmConn) LocalAddr() net.Addr {
	return c.localAddr
}

// RemoteAddr returns the remote network address
func (c *shmConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

// SetDeadline sets the read and write deadlines
func (c *shmConn) SetDeadline(t time.Time) error {
	// Shared memory connections don't support deadlines in the traditional sense
	return nil
}

// SetReadDeadline sets the deadline for future Read calls
func (c *shmConn) SetReadDeadline(t time.Time) error {
	// Shared memory connections don't support deadlines in the traditional sense
	return nil
}

// SetWriteDeadline sets the deadline for future Write calls
func (c *shmConn) SetWriteDeadline(t time.Time) error {
	// Shared memory connections don't support deadlines in the traditional sense
	return nil
}

// GetServerTransport returns the server transport for this connection
func (c *shmConn) GetServerTransport() transport.ServerTransport {
	return c.transport
}
