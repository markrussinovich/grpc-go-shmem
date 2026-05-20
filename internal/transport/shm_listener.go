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
	"io"
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

// clientReadyTimeout bounds how long the listener's Accept loop waits
// for an accepted client to map its data segment and signal ClientReady.
// Without a bound, a client that disappears between consuming the
// listener's ACCEPT frame and signalling readiness (process crash,
// kill -9, sandbox restart) would wedge the Accept goroutine
// indefinitely and prevent the listener from serving subsequent
// CONNECT requests. The handshake is sub-millisecond in the common
// case (both endpoints are on the same host); 5s is plenty of
// headroom while still recovering promptly from a dead client.
//
// var rather than const so tests can shrink the wait without
// rebuilding the binary; production code does not mutate it.
var clientReadyTimeout = 5 * time.Second

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
	acceptMu  sync.Mutex     // serializes Accept admission with Close
	acceptWG  sync.WaitGroup // tracks goroutines in Accept touching ctlRx/ctlTx

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
	if err := validateSegmentName(addr.Name); err != nil {
		return nil, err
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
	ctlSeg.RegisterRing(l.ctlRx)
	ctlSeg.RegisterRing(l.ctlTx)

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
	// Serialize with Close() to prevent Add(1) while Wait() is active.
	// Close() holds acceptMu while setting closed, so after this lock
	// either: (a) closed is true and we return immediately, or (b) closed
	// is false and our Add(1) happens-before Close's Wait().
	l.acceptMu.Lock()
	if l.closed.Load() {
		l.acceptMu.Unlock()
		return nil, errors.New("listener closed")
	}
	l.acceptWG.Add(1)
	l.acceptMu.Unlock()
	defer l.acceptWG.Done()

	// Read a CONNECT request from the control ring.
	for {
		fh, payload, err := readCtlFrame(l.ctx, l.ctlRx)
		if err != nil {
			return nil, err
		}
		if fh.Type != FrameTypeCONNECT {
			continue
		}
		connReq, err := decodeConnectRequest(payload)
		if err != nil {
			_ = writeCtlFrame(l.ctx, l.ctlTx, FrameHeader{Type: FrameTypeREJECT}, encodeConnectReject(connectReject{message: err.Error()}))
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
			_ = writeCtlFrame(l.ctx, l.ctlTx, FrameHeader{Type: FrameTypeREJECT}, encodeConnectReject(connectReject{message: err.Error()}))
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
		segment.RegisterRing(readRing)
		segment.RegisterRing(writeRing)

		// Create events for this segment. On Linux, these are no-ops.
		// Must happen before ACCEPT so client's OpenRingEvents finds them.
		readEvents, _ := CreateRingEvents(segmentName, "A")
		writeEvents, _ := CreateRingEvents(segmentName, "B")

		// Attach events to rings
		readRing.SetEvents(readEvents)
		writeRing.SetEvents(writeEvents)

		if err := writeCtlFrame(l.ctx, l.ctlTx, FrameHeader{Type: FrameTypeACCEPT}, encodeConnectResponse(connectResponse{segmentName: segmentName})); err != nil {
			if readEvents != nil {
				readEvents.Close()
			}
			if writeEvents != nil {
				writeEvents.Close()
			}
			segment.Close()
			_ = RemoveSegment(segmentName)
			return nil, err
		}

		// Wait for client to map the segment. Bound this with a timeout
		// so a client that disappears between consuming ACCEPT and
		// signalling ClientReady (process crash, kill -9, network
		// partition for cross-host setups) cannot wedge the listener's
		// Accept loop indefinitely. On timeout we clean up this
		// connection's resources and continue the outer for-loop to
		// service the next CONNECT.
		waitCtx, waitCancel := context.WithTimeout(l.ctx, clientReadyTimeout)
		waitErr := segment.WaitForClient(waitCtx)
		waitCancel()
		if waitErr != nil {
			if readEvents != nil {
				readEvents.Close()
			}
			if writeEvents != nil {
				writeEvents.Close()
			}
			segment.Close()
			_ = RemoveSegment(segmentName)
			// If the listener itself was cancelled, propagate;
			// otherwise this was a per-client timeout and we move on
			// to the next CONNECT instead of failing Accept (which
			// would shut the entire gRPC server down).
			if l.ctx.Err() != nil {
				return nil, waitErr
			}
			continue
		}

		// Resolve the eventfd-waker peer state now that OpenerWakeReady
		// is stably published by the client's setupDataSegWakeForOpener.
		// When the opener obtained a waker (same-process via the in-
		// memory stash OR cross-process via SCM_RIGHTS) both sides keep
		// the eventfd fast path; otherwise the creator drops its waker
		// so both converge on the futex / Windows-events path, avoiding
		// the asymmetric-wake deadlock. MUST run before any goroutine
		// starts using the rings for data-plane reads / writes.
		segment.finalizeDataSegWaker()

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
				_ = RemoveSegment(segmentName)
				return nil, fmt.Errorf("security handshake failed: %v", err)
			}
			conn.authInfo = authInfo
		}

		serverTransport, err := NewShmServerTransport(segment, l.addr, conn.remoteAddr)
		if err != nil {
			if readEvents != nil {
				readEvents.Close()
			}
			if writeEvents != nil {
				writeEvents.Close()
			}
			segment.Close()
			_ = RemoveSegment(segmentName)
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
		// Hold acceptMu while setting closed so no new Accept() can call
		// acceptWG.Add(1) after we observe the counter at zero.
		l.acceptMu.Lock()
		l.closed.Store(true)
		l.acceptMu.Unlock()

		l.cancel()

		if l.ctlSegment != nil {
			// Close rings first — this bumps sequences and wakes any
			// goroutine blocked in Accept()'s readFrame/ReadSlices.
			// The ring's closed flag causes spin loops to exit immediately
			// (checked before each memory access) and futex waiters to
			// wake and see ErrRingClosed.
			if l.ctlRx != nil {
				_ = l.ctlRx.Close()
			}
			if l.ctlTx != nil {
				_ = l.ctlTx.Close()
			}

			// Wait for Accept to finish touching control ring memory.
			// The ring closures above ensure Accept's readCtlFrame returns
			// promptly with an error, so this won't block indefinitely.
			l.acceptWG.Wait()

			l.ctlSegment.Close()
			CloseHandshakeEvents(l.baseName + shmControlSuffix)
			_ = RemoveSegment(l.baseName + shmControlSuffix)
			// Unlink the cross-process control lock file (Linux) so a
			// later listener start does not inherit a stale inode.
			// No-op on Windows where the named mutex is refcounted.
			removeControlLock(l.baseName + shmControlSuffix)

			// Release the listener's reference on the control-ring events.
			// On Linux these are nil; on Windows the refcount in RingEvents
			// keeps them alive until both this Close and the dialer's
			// deferred Close have run, so the SetEvent signals issued by
			// ctlRx.Close above always reach the parked Accept goroutine.
			if l.ctlRxEvents != nil {
				l.ctlRxEvents.Close()
				l.ctlRxEvents = nil
			}
			if l.ctlTxEvents != nil {
				l.ctlTxEvents.Close()
				l.ctlTxEvents = nil
			}
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

// shmConn is the server-side counterpart to the client's shmClientConn
// shim: it satisfies net.Conn so the SHM listener can return values
// through gRPC's standard Accept() contract, while the actual frame
// I/O happens through the embedded ShmServerTransport (accessed via
// ReadRing/WriteRing). As with shmClientConn, the net.Conn surface
// MUST NOT be reached on the SHM hot path; Read and Write return
// io.ErrClosedPipe so any unexpected caller fails fast and visibly.

// Read is intentionally unsupported. See the type doc — callers must
// use the embedded transport instead of going through net.Conn.
func (c *shmConn) Read(_ []byte) (n int, err error) {
	if c.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	return 0, fmt.Errorf("shmConn.Read: %w (use the embedded ShmServerTransport instead)", io.ErrClosedPipe)
}

// Write is intentionally unsupported. See the type doc — callers must
// use the embedded transport instead of going through net.Conn.
func (c *shmConn) Write(_ []byte) (n int, err error) {
	if c.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	return 0, fmt.Errorf("shmConn.Write: %w (use the embedded ShmServerTransport instead)", io.ErrClosedPipe)
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

		// Release the listener-owned ring event refs. These were
		// taken in ShmListener.Accept (one ref each from
		// CreateRingEvents). The server transport holds its own,
		// independent ref pair via NewShmServerTransport and
		// releases them in (*ShmServerTransport).Close above. On
		// Linux these are no-op nil events; on Windows skipping
		// this leaks named-event handles and registry entries per
		// accepted connection. See shm_event_windows.go for the
		// refcount contract.
		if c.readEvents != nil {
			_ = c.readEvents.Close()
			c.readEvents = nil
		}
		if c.writeEvents != nil {
			_ = c.writeEvents.Close()
			c.writeEvents = nil
		}

		// Then close and clean up the segment
		if c.segment != nil {
			c.segment.Close()
		}
		if c.segmentName != "" {
			CloseHandshakeEvents(c.segmentName)
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
