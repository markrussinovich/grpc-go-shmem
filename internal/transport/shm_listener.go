//go:build linux

package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

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
	connID   atomic.Uint64 // Atomic counter for connection IDs

	// Lifecycle management
	ctx       context.Context
	cancel    context.CancelFunc
	closed    atomic.Bool
	closeOnce sync.Once

	// Connection handling
	mu sync.RWMutex
	activeSegments map[string]*Segment // Track active segments for cleanup
	nextSegment    *Segment            // Pre-created segment waiting for next client
	nextSegmentName string

	// Configuration
	segmentSize uint64
	ringASize   uint64
	ringBSize   uint64
}

// shmConn represents a shared memory connection
type shmConn struct {
	segment     *Segment
	segmentName string
	listener    *ShmListener
	localAddr   net.Addr
	remoteAddr  net.Addr
	transport   *ShmServerTransport

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
		addr:           addr,
		baseName:       addr.Name,
		ctx:            ctx,
		cancel:         cancel,
		activeSegments: make(map[string]*Segment),
		segmentSize:    segmentSize,
		ringASize:      ringASize,
		ringBSize:      ringBSize,
	}

	// Pre-create the first segment so it's ready when client connects
	if err := l.prepareNextSegment(); err != nil {
		cancel()
		return nil, err
	}

	return l, nil
}

// prepareNextSegment pre-creates a segment for the next incoming connection
func (l *ShmListener) prepareNextSegment() error {
	connID := l.connID.Add(1)
	segmentName := fmt.Sprintf("%s_conn_%d", l.baseName, connID)

	segment, err := CreateSegment(segmentName, l.ringASize, l.ringBSize)
	if err != nil {
		return fmt.Errorf("create segment: %w", err)
	}

	// Mark server ready immediately
	segment.H.SetServerReady(true)

	l.nextSegment = segment
	l.nextSegmentName = segmentName

	return nil
}

// handlePotentialConnection processes a potential new connection
func (l *ShmListener) handlePotentialConnection() (*shmConn, error) {
	// Use the pre-created segment
	segment := l.nextSegment
	segmentName := l.nextSegmentName

	if segment == nil {
		return nil, errors.New("no segment available")
	}

	// Clear the next segment so we don't reuse it
	l.nextSegment = nil
	l.nextSegmentName = ""

	// Pre-create the next segment for the next connection BEFORE waiting for client
	// This ensures it's ready when the next client tries to connect
	go func() {
		if err := l.prepareNextSegment(); err != nil {
			// Log error but don't fail - listener can still serve existing connections
			fmt.Printf("Warning: failed to prepare next segment: %v\n", err)
		}
	}()

	// Wait for client to connect (event-driven, no polling)
	if err := segment.WaitForClient(l.ctx); err != nil {
		segment.Close()
		return nil, err
	}

	// Track the segment
	l.mu.Lock()
	l.activeSegments[segmentName] = segment
	l.mu.Unlock()

	// Create connection with this segment
	conn := &shmConn{
		segment:     segment,
		segmentName: segmentName,
		listener:    l,
		localAddr:   l.addr,
		remoteAddr:  &ShmAddr{Name: segmentName + "_client"},
	}

	// Create server transport for this connection
	serverTransport, err := NewShmServerTransport(segment, l.addr, conn.remoteAddr)
	if err != nil {
		l.mu.Lock()
		delete(l.activeSegments, segmentName)
		l.mu.Unlock()
		segment.Close()
		return nil, fmt.Errorf("failed to create server transport: %v", err)
	}

	conn.transport = serverTransport
	conn.established.Store(true)

	return conn, nil
}

// Accept waits for and returns the next connection to the listener
// Creates a new segment for each connection, similar to TCP socket model
func (l *ShmListener) Accept() (net.Conn, error) {
	if l.closed.Load() {
		return nil, errors.New("listener closed")
	}

	// Create new segment and wait for client connection
	conn, err := l.handlePotentialConnection()
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

		// Clean up the next segment if it exists
		if l.nextSegment != nil {
			l.nextSegment.Close()
			l.nextSegment = nil
		}

		// Clean up all active segments
		l.mu.Lock()
		for _, segment := range l.activeSegments {
			segment.Close()
		}
		l.activeSegments = make(map[string]*Segment)
		l.mu.Unlock()
	})
	return nil
}

// Addr returns the listener's network address
func (l *ShmListener) Addr() net.Addr {
	return l.addr
}

// GetNextSegment returns the pre-created segment waiting for next client (for testing)
func (l *ShmListener) GetNextSegment() *Segment {
	return l.nextSegment
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

		// Close transport first (graceful shutdown)
		if c.transport != nil {
			c.transport.Close(errors.New("connection closed"))
		}

		// Remove from listener's active segments
		if c.listener != nil && c.segmentName != "" {
			c.listener.mu.Lock()
			delete(c.listener.activeSegments, c.segmentName)
			c.listener.mu.Unlock()
		}

		// Then close and clean up the segment
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
func (c *shmConn) GetServerTransport() ServerTransport {
	return c.transport
}
