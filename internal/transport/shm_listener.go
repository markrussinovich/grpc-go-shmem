//go:build linux || windows

/*
 *
 * Copyright 2025 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package transport

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
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
	baseName string        // Base name for segment creation
	connID   atomic.Uint64 // Atomic counter for connection IDs

	ctlSegment  *Segment
	ctlRx       *ShmRing    // client->server control
	ctlTx       *ShmRing    // server->client control
	ctlRxEvents *RingEvents // Events for control rings (Windows)
	ctlTxEvents *RingEvents

	// Lifecycle management
	ctx       context.Context
	cancel    context.CancelFunc
	closed    atomic.Bool
	closeOnce sync.Once

	// Connection handling
	mu             sync.RWMutex
	activeSegments map[string]*shmConn // Track active connections for cleanup

	// Configuration
	segmentSize uint64
	ringASize   uint64
	ringBSize   uint64
	maxStreams  uint32

	// Keepalive configuration for server transports
	kp  keepalive.ServerParameters
	kep keepalive.EnforcementPolicy

	// Security handshake configuration
	handshaker *ShmSecurityHandshaker
}

// shmConn represents a shared memory connection
type shmConn struct {
	segment     *Segment
	segmentName string
	listener    *ShmListener
	localAddr   net.Addr
	remoteAddr  net.Addr
	transport   *ShmServerTransport

	// Rings with events for cross-mapping synchronization
	readRing    *ShmRing
	writeRing   *ShmRing
	readEvents  *RingEvents
	writeEvents *RingEvents

	// Connection state
	established      atomic.Bool
	closed           atomic.Bool
	closeOnce        sync.Once
	singleStreamMode bool

	// Security handshake result
	authInfo credentials.AuthInfo
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
		activeSegments: make(map[string]*shmConn),
		segmentSize:    segmentSize,
		ringASize:      ringASize,
		ringBSize:      ringBSize,
		maxStreams:     uint32(math.MaxUint32),
	}

	// Create the control-plane segment used to establish connections without any
	// dial-side polling.
	ctlName := l.baseName + shmControlSuffix

	// Proactively clean up any stale segment from a previous run before creating.
	// This handles the case where the server crashed without proper cleanup.
	if SegmentExists(ctlName) {
		_ = RemoveSegment(ctlName)
	}

	ctlSeg, err := CreateSegment(ctlName, MinRingCapacity, MinRingCapacity)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create control segment: %w", err)
	}

	// Create handshake events for the control segment (Windows).
	// This must be done before SetServerReady so clients can wait on the event.
	ctlEventName := l.baseName + shmControlSuffix
	_, _ = CreateHandshakeEvents(ctlEventName)

	// Signal server ready with event
	ctlSeg.SetServerReadyAndSignal(true)

	// Ring A is client->server; ring B is server->client.
	l.ctlSegment = ctlSeg
	l.ctlRx = NewShmRingFromSegment(ctlSeg.A, ctlSeg.Mem)
	l.ctlTx = NewShmRingFromSegment(ctlSeg.B, ctlSeg.Mem)

	// Create events for control rings (Windows). On Linux, these are no-ops.
	l.ctlRxEvents, _ = CreateRingEvents(ctlEventName, "A")
	l.ctlTxEvents, _ = CreateRingEvents(ctlEventName, "B")

	// Attach events to control rings
	l.ctlRx.SetEvents(l.ctlRxEvents)
	l.ctlTx.SetEvents(l.ctlTxEvents)

	return l, nil
}

// SetMaxStreams configures the max concurrent streams for new connections.
// A value of 0 indicates unlimited.
func (l *ShmListener) SetMaxStreams(max uint32) {
	if max == 0 {
		max = uint32(math.MaxUint32)
	}
	atomic.StoreUint32(&l.maxStreams, max)
}

// Accept waits for and returns the next connection to the listener
// Creates a new segment for each connection, similar to TCP socket model
func (l *ShmListener) Accept() (net.Conn, error) {
	if l.closed.Load() {
		return nil, errors.New("listener closed")
	}

	// Read a CONNECT request from the control ring.
	for {
		fh, payload, err := readFrame(l.ctx, l.ctlRx)
		if err != nil {
			return nil, err
		}
		if fh.Type != FrameTypeCONNECT {
			continue
		}
		connReq, err := decodeConnectRequest(payload)
		if err != nil {
			_ = writeFrame(l.ctx, l.ctlTx, FrameHeader{Type: FrameTypeREJECT}, encodeConnectReject(connectReject{message: err.Error()}))
			continue
		}

		connID := l.connID.Add(1)
		segmentName := fmt.Sprintf("%s_conn_%d", l.baseName, connID)

		// Proactively clean up any stale segment from a previous run.
		if SegmentExists(segmentName) {
			_ = RemoveSegment(segmentName)
		}

		segment, err := CreateSegment(segmentName, l.ringASize, l.ringBSize)
		if err != nil {
			_ = writeFrame(l.ctx, l.ctlTx, FrameHeader{Type: FrameTypeREJECT}, encodeConnectReject(connectReject{message: err.Error()}))
			continue
		}
		segment.H.SetMaxStreams(atomic.LoadUint32(&l.maxStreams))

		// Create handshake events for the data segment (Windows).
		_, _ = CreateHandshakeEvents(segmentName)
		segment.SetServerReadyAndSignal(true)

		// Create rings and events BEFORE sending ACCEPT, so events exist
		// when client opens the segment and creates its transport.
		readRing := NewShmRingFromSegment(segment.A, segment.Mem)
		writeRing := NewShmRingFromSegment(segment.B, segment.Mem)

		// Create events for this segment. On Linux, these are no-ops.
		// Must happen before ACCEPT so client's OpenRingEvents finds them.
		readEvents, _ := CreateRingEvents(segmentName, "A")
		writeEvents, _ := CreateRingEvents(segmentName, "B")

		// Attach events to rings
		readRing.SetEvents(readEvents)
		writeRing.SetEvents(writeEvents)

		if err := writeFrame(l.ctx, l.ctlTx, FrameHeader{Type: FrameTypeACCEPT}, encodeConnectResponse(connectResponse{segmentName: segmentName})); err != nil {
			if readEvents != nil {
				readEvents.Close()
			}
			if writeEvents != nil {
				writeEvents.Close()
			}
			segment.Close()
			return nil, err
		}

		// Wait for client to map the segment.
		if err := segment.WaitForClient(l.ctx); err != nil {
			if readEvents != nil {
				readEvents.Close()
			}
			if writeEvents != nil {
				writeEvents.Close()
			}
			segment.Close()
			return nil, err
		}

		conn := &shmConn{
			segment:          segment,
			segmentName:      segmentName,
			listener:         l,
			localAddr:        l.addr,
			remoteAddr:       &ShmAddr{Name: segmentName + "_client"},
			readRing:         readRing,
			writeRing:        writeRing,
			readEvents:       readEvents,
			writeEvents:      writeEvents,
			singleStreamMode: connReq.singleStreamMode,
		}

		// Perform security handshake if configured
		l.mu.RLock()
		handshaker := l.handshaker
		l.mu.RUnlock()
		if handshaker != nil {
			hsCtx, hsCancel := context.WithTimeout(l.ctx, HandshakeTimeout)
			authInfo, err := handshaker.ServerHandshake(hsCtx, readRing, writeRing)
			hsCancel()
			if err != nil {
				if readEvents != nil {
					readEvents.Close()
				}
				if writeEvents != nil {
					writeEvents.Close()
				}
				segment.Close()
				return nil, fmt.Errorf("security handshake failed: %v", err)
			}
			conn.authInfo = authInfo
		}

		serverTransport, err := NewShmServerTransport(segment, l.addr, conn.remoteAddr)
		if err != nil {
			l.mu.Lock()
			delete(l.activeSegments, segmentName)
			l.mu.Unlock()
			segment.Close()
			return nil, fmt.Errorf("failed to create server transport: %v", err)
		}
		// Configure keepalive on the server transport.
		serverTransport.ConfigureKeepalive(l.kp, l.kep)
		serverTransport.singleStreamMode = conn.singleStreamMode
		conn.transport = serverTransport
		conn.established.Store(true)
		l.mu.Lock()
		l.activeSegments[segmentName] = conn
		l.mu.Unlock()
		return conn, nil
	}
}

// Close closes the listener
func (l *ShmListener) Close() error {
	l.closeOnce.Do(func() {
		l.closed.Store(true)
		l.cancel()

		if l.ctlSegment != nil {
			// Wake any goroutine blocked in Accept() waiting for a CONNECT frame.
			// The listener context cancellation alone cannot interrupt a futex wait
			// without a deadline, so we must explicitly close the rings (which bumps
			// sequences and futex-wakes waiters) before unmapping the segment.
			// Note: We don't nil these pointers because Accept() might still be
			// reading l.ctlRx concurrently. The Close() on the ring will cause
			// the read to fail, which is the desired behavior.
			if l.ctlRx != nil {
				_ = l.ctlRx.Close()
			}
			if l.ctlTx != nil {
				_ = l.ctlTx.Close()
			}

			l.ctlSegment.Close()
			_ = RemoveSegment(l.baseName + shmControlSuffix)
		}

		// Clean up all active connections to ensure rings close before unmapping.
		l.mu.Lock()
		conns := make([]*shmConn, 0, len(l.activeSegments))
		for _, c := range l.activeSegments {
			conns = append(conns, c)
		}
		l.activeSegments = make(map[string]*shmConn)
		l.mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return nil
}

// Addr returns the listener's network address
func (l *ShmListener) Addr() net.Addr {
	return l.addr
}

// SetKeepaliveParams sets the keepalive parameters for server transports
// created by this listener. This must be called before Accept is called.
func (l *ShmListener) SetKeepaliveParams(kp keepalive.ServerParameters, kep keepalive.EnforcementPolicy) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.kp = kp
	l.kep = kep
}

// SetHandshaker sets the security handshaker for server transports.
// This must be called before Accept is called.
// If nil, no security handshake is performed.
func (l *ShmListener) SetHandshaker(h *ShmSecurityHandshaker) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.handshaker = h
}

// shmConn net.Conn implementation

// Read reads data from the connection
func (c *shmConn) Read(_ []byte) (n int, err error) {
	if c.closed.Load() {
		return 0, errors.New("connection closed")
	}

	// For shared memory, reading is handled by the transport layer
	// This is a placeholder implementation
	return 0, errors.New("direct read not supported, use transport layer")
}

// Write writes data to the connection
func (c *shmConn) Write(_ []byte) (n int, err error) {
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
		if c.segmentName != "" {
			_ = RemoveSegment(c.segmentName)
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
func (c *shmConn) SetDeadline(_ time.Time) error {
	// Shared memory connections don't support deadlines in the traditional sense
	return nil
}

// SetReadDeadline sets the deadline for future Read calls
func (c *shmConn) SetReadDeadline(_ time.Time) error {
	// Shared memory connections don't support deadlines in the traditional sense
	return nil
}

// SetWriteDeadline sets the deadline for future Write calls
func (c *shmConn) SetWriteDeadline(_ time.Time) error {
	// Shared memory connections don't support deadlines in the traditional sense
	return nil
}

// GetServerTransport returns the server transport for this connection
func (c *shmConn) GetServerTransport() ServerTransport {
	return c.transport
}

// ReadRing returns the read ring (client->server) with events attached.
// For tests that need direct ring access.
func (c *shmConn) ReadRing() *ShmRing {
	return c.readRing
}

// WriteRing returns the write ring (server->client) with events attached.
// For tests that need direct ring access.
func (c *shmConn) WriteRing() *ShmRing {
	return c.writeRing
}

// AuthInfo returns the authentication information for this connection.
// This is set after a successful security handshake.
func (c *shmConn) AuthInfo() credentials.AuthInfo {
	return c.authInfo
}
