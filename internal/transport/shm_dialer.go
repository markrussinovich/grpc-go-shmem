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
	"net"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"
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

	// Handshaker is the security handshaker for the client.
	// If nil, no security handshake is performed.
	Handshaker *ShmSecurityHandshaker

	// SingleStreamMode requests single-stream optimizations from the server.
	// When enabled and the server agrees, both sides can use inline writes
	// and skip the frame writer queue for reduced latency.
	// Default: false.
	SingleStreamMode bool

	// InitialWindowSize overrides the per-stream HTTP/2 send / receive
	// window for the SHM transport. When <= 0 the SHM-tuned default
	// (maxWindowSize, ~2 GiB, i.e. flow control effectively disabled
	// and the ring buffer is the only backpressure signal) is used.
	// Set non-zero to make grpc.WithInitialWindowSize take effect on
	// the SHM transport — primarily useful for benchmarks that need
	// apples-to-apples comparison against HTTP/2 over TCP / UDS at
	// matched settings. With non-default values the SHM transport
	// behaves like HTTP/2: producer chunks writes under the window;
	// receiver credits WINDOW_UPDATE per DATA frame.
	InitialWindowSize int32

	// InitialConnWindowSize overrides the connection-level HTTP/2
	// send / receive window. Same semantics as InitialWindowSize.
	InitialConnWindowSize int32
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
	if err := validateSegmentName(addr); err != nil {
		return nil, NewShmErrorWithCause(ShmErrInvalidConfig, "invalid segment name", err)
	}
	if opts == nil {
		opts = DefaultDialOptions()
	}

	// Apply timeout. We always install at least the safety-net timeout
	// even when the caller passed ConnectTimeout == 0, because parts of
	// the handshake (writing CONNECT then reading the response on the
	// shared control ring) intentionally use a non-cancellable read
	// context to avoid mid-frame corruption on Ring B. A caller-supplied
	// ctx with no deadline combined with ConnectTimeout=0 would
	// otherwise let DialShm hang forever if the server never replies.
	connectTimeout := opts.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 30 * time.Second
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	// Establish the data segment to use via the server's control segment.
	ctlName := addr + shmControlSuffix
	ctlSeg, err := OpenSegment(ctlName)
	if err != nil {
		return nil, NewShmErrorWithCause(ShmErrSegmentNotFound,
			fmt.Sprintf("open control segment %q", ctlName), err)
	}
	defer ctlSeg.Close()

	// Open handshake events for the control segment (Windows).
	// This must be done before WaitForServer so we can wait on the event.
	// Pair this Open with a deferred Close so the dialer-side ref is
	// released whether DialShm succeeds or fails; without this the
	// Windows registry entry (and its two named-event handles) leaks
	// once per cross-process dial. Same-process: refcount in the shared
	// registry keeps the listener-side entry alive until its own
	// Close decrements to zero.
	_, _ = OpenHandshakeEvents(ctlName)
	defer CloseHandshakeEvents(ctlName)

	if err := ctlSeg.WaitForServer(ctx); err != nil {
		return nil, NewShmErrorWithCause(ShmErrConnectionRefused, "wait for control server", err)
	}

	ctlTx := NewShmRingFromSegment(ctlSeg.A, ctlSeg.Mem)
	ctlRx := NewShmRingFromSegment(ctlSeg.B, ctlSeg.Mem)
	// Defensive: finalizeDataSegWaker is a no-op for control segments
	// (their suffix excludes them from the eventfd waker in
	// setupDataSegWakeFor{Creator,Opener}). The call is kept here so
	// that any future change which allowed wakers on control segments
	// would still observe a stable OpenerWakeReady flag before the
	// rings start carrying control frames.
	ctlSeg.finalizeDataSegWaker()
	ctlSeg.RegisterRing(ctlTx)
	ctlSeg.RegisterRing(ctlRx)

	// Create events for control rings (Windows). On Linux, these are no-ops.
	ctlTxEvents, _ := OpenRingEvents(ctlName, "A")
	ctlRxEvents, _ := OpenRingEvents(ctlName, "B")
	defer func() {
		if ctlTxEvents != nil {
			ctlTxEvents.Close()
		}
		if ctlRxEvents != nil {
			ctlRxEvents.Close()
		}
	}()

	// Attach events to control rings
	ctlTx.SetEvents(ctlTxEvents)
	ctlRx.SetEvents(ctlRxEvents)

	// Acquire the cross-process control-segment lock per gRFC: the
	// control ring is shared among all clients connecting to the same
	// server, so the SPSC invariant on Ring A / Ring B only holds for
	// the duration of one client's CONNECT/ACCEPT exchange. Without
	// this lock two concurrent dialers race on Ring A writes and
	// Ring B reads, potentially stealing each other's response or
	// corrupting ring indices.
	//
	// On Linux this is flock(LOCK_EX) on "<ctlPath>.lock"; on Windows
	// it is LockFileEx on the same sibling file (named mutex would not
	// be safe in Go because mutex ownership is OS-thread-bound). The
	// lock is released after the ACCEPT/REJECT response is fully
	// drained from Ring B, or after a drain attempt if the read was
	// cancelled mid-frame (drain prevents the next dialer from
	// consuming a stale response and re-aligns the ring header).
	releaseCtlLock, err := acquireControlLock(ctx, ctlName)
	if err != nil {
		return nil, NewShmErrorWithCause(ShmErrConnectionRefused, "acquire control lock", err)
	}
	// Track whether we've written CONNECT yet; if so and we later bail
	// out without successfully reading a response, we must drain Ring B
	// before releasing the lock so the next dialer does not consume
	// our abandoned ACCEPT/REJECT. The deferred closure runs only if
	// we have not already released the lock explicitly via the
	// release-and-clear-to-nil dance after a clean response read; on
	// panic, the defer guarantees the lock is released and the ring
	// is drained.
	sentConnect := false
	defer func() {
		if releaseCtlLock == nil {
			return
		}
		if sentConnect {
			// Best-effort drain with a fresh context so the original
			// ctx's cancellation does not abort the drain. The 1-second
			// cap prevents an adversarial / hung server from holding
			// the lock forever.
			drainCtx, drainCancel := context.WithTimeout(context.Background(), time.Second)
			_, _, _ = readCtlFrame(drainCtx, ctlRx)
			drainCancel()
		}
		releaseCtlLock()
	}()

	if err := writeCtlFrame(ctx, ctlTx, FrameHeader{Type: FrameTypeCONNECT}, encodeConnectRequest(connectRequest{
		singleStreamMode: opts.SingleStreamMode,
	})); err != nil {
		return nil, NewShmErrorWithCause(ShmErrConnectionRefused, "send connect request", err)
	}
	sentConnect = true
	// Use a non-cancellable context for the response read, preserving
	// only ctx's deadline. ctx cancellation (Done channel close) during
	// readCtlFrame's two-phase header+payload commit would leave Ring B
	// mid-frame and corrupt the next dialer's read. The outer
	// DialShm ConnectTimeout (and any caller-supplied deadline)
	// still bounds the wait via deadline propagation. Residual risk:
	// a deadline firing between the header commit and payload read
	// could still leave bytes pending; the deferred drain attempts a
	// best-effort cleanup. A wire-level request nonce would close
	// the remaining window; tracked as a gRFC follow-up.
	readCtx := context.Background()
	if d, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		readCtx, cancel = context.WithDeadline(context.Background(), d)
		defer cancel()
	}
	respFH, respPayload, err := readCtlFrame(readCtx, ctlRx)
	if err != nil {
		// Deferred drain + release handles the abandoned-response
		// recovery; just surface the read error.
		return nil, NewShmErrorWithCause(ShmErrConnectionRefused, "read connect response", err)
	}
	// Mark sentConnect=false BEFORE the explicit release so any
	// panic in the release path itself does not trigger a stale
	// drain on already-empty Ring B.
	sentConnect = false
	releaseCtlLock()
	releaseCtlLock = nil
	switch respFH.Type {
	case FrameTypeACCEPT:
		resp, err := decodeConnectResponse(respPayload)
		if err != nil {
			return nil, NewShmErrorWithCause(ShmErrProtocolMismatch, "decode accept", err)
		}
		segName := resp.segmentName
		segment, err := OpenSegment(segName)
		if err != nil {
			return nil, NewShmErrorWithCause(ShmErrSegmentNotFound,
				fmt.Sprintf("open data segment %q", segName), err)
		}

		// Open handshake events for the data segment (Windows).
		// On success ownership transfers to ShmClientTransport.Close
		// which decrements the refcount via CloseHandshakeEvents.
		// On any failure path before that hand-off the segHandshakeOwned
		// flag triggers a local CloseHandshakeEvents so we never leak
		// the dialer-side ref. No-op on Linux.
		_, hsErr := OpenHandshakeEvents(segName)
		segHandshakeOwned := hsErr == nil
		defer func() {
			if segHandshakeOwned {
				CloseHandshakeEvents(segName)
			}
		}()

		// Wait for server readiness via named event (Windows) or futex (Linux).
		if err := segment.WaitForServer(ctx); err != nil {
			segment.Close()
			return nil, NewShmErrorWithCause(ShmErrTimeout, "wait for server ready", err)
		}

		// Signal to the server that the client has mapped the segment.
		// This unblocks the server's WaitForClient in Accept().
		segment.SetClientReadyAndSignal(true)

		// Resolve the eventfd-waker peer state now that OpenerWakeReady
		// is stably published by setupDataSegWakeForOpener. When the
		// opener obtained a waker (same-process via the in-memory stash
		// OR cross-process via SCM_RIGHTS) both sides keep the eventfd
		// fast path; otherwise the creator drops its waker so both
		// converge on the futex / Windows-events path, avoiding the
		// asymmetric-wake deadlock. MUST run before any ring read/write.
		// See Segment.finalizeDataSegWaker for details.
		segment.finalizeDataSegWaker()

		localAddr := &ShmAddr{Name: segName + "_client"}
		remoteAddr := &ShmAddr{Name: segName}

		// Perform security handshake if configured
		var authInfo credentials.AuthInfo
		if opts.Handshaker != nil {
			// Create rings for handshake - client writes to A, reads from B
			txRing := NewShmRingFromSegment(segment.A, segment.Mem)
			rxRing := NewShmRingFromSegment(segment.B, segment.Mem)
			segment.RegisterRing(txRing)
			segment.RegisterRing(rxRing)

			// Open events for rings (Windows). On Linux these are nil.
			// The same named events will be re-opened by
			// NewShmClientTransport below; on Windows each Open bumps
			// the refcount, so we MUST release these handshake-only
			// references whether the handshake succeeds or fails.
			// Otherwise every secured Windows dial leaks one handle
			// per direction.
			txEvents, _ := OpenRingEvents(segName, "A")
			rxEvents, _ := OpenRingEvents(segName, "B")
			txRing.SetEvents(txEvents)
			rxRing.SetEvents(rxEvents)

			hsCtx, hsCancel := context.WithTimeout(ctx, HandshakeTimeout)
			authInfo, err = opts.Handshaker.ClientHandshake(hsCtx, rxRing, txRing)
			hsCancel()
			// Release the handshake-local event refs on both success
			// and failure paths.
			if txEvents != nil {
				txEvents.Close()
			}
			if rxEvents != nil {
				rxEvents.Close()
			}
			if err != nil {
				segment.Close()
				return nil, NewShmErrorWithCause(ShmErrUnknown, "security handshake failed", err)
			}
		}

		clientTransport, err := NewShmClientTransport(segment, localAddr, remoteAddr)
		if err != nil {
			segment.Close()
			return nil, NewShmErrorWithCause(ShmErrUnknown, "failed to create client transport", err)
		}
		clientTransport.singleStreamMode = opts.SingleStreamMode
		// Override flow-control windows when the caller requested
		// non-default sizes. This is how grpc.WithInitialWindowSize /
		// WithInitialConnWindowSize take effect on the SHM transport.
		// With production defaults (opts.* <= 0) the transport keeps
		// its 2 GiB quotas i.e. flow control disabled, ring buffer
		// is the only backpressure. With non-default values both
		// quotas are clamped to the requested sizes so producer
		// chunked write + per-DATA-frame consumer credit (the rest
		// of this commit's machinery) actually exercise the HTTP/2
		// flow-control state machine.
		if opts.InitialConnWindowSize > 0 {
			clientTransport.sendQuotaMu.Lock()
			clientTransport.connSendQuota = int64(opts.InitialConnWindowSize)
			clientTransport.sendQuotaMu.Unlock()
			clientTransport.connInFlow = trInFlow{limit: uint32(opts.InitialConnWindowSize)}
			clientTransport.connInFlow.updateEffectiveWindowSize()
		}
		if opts.InitialWindowSize > 0 {
			// stream quota is initialised per-NewStream; expose the
			// override via the transport's initialStreamWindow field
			// so each new stream picks it up.
			clientTransport.initialStreamWindow = int64(opts.InitialWindowSize)
			clientTransport.initialWindowSize = opts.InitialWindowSize
		}
		// Store auth info on transport
		if authInfo != nil {
			clientTransport.SetAuthInfo(authInfo)
		}
		// Configure keepalive if params are provided.
		clientTransport.ConfigureKeepalive(opts.KeepaliveParams)
		// Transfer ownership of the dialer's handshake-events ref to
		// the transport; ShmClientTransport.Close will release it.
		segHandshakeOwned = false
		return clientTransport, nil
	case FrameTypeREJECT:
		r, err := decodeConnectReject(respPayload)
		if err != nil {
			return nil, NewShmErrorWithCause(ShmErrProtocolMismatch, "connect rejected (decode)", err)
		}
		return nil, NewShmError(ShmErrConnectionRefused, fmt.Sprintf("connect rejected: %s", r.message))
	default:
		return nil, NewShmError(ShmErrProtocolMismatch, fmt.Sprintf("unexpected control frame type %d", respFH.Type))
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

	shmTransport := clientTransport.(*ShmClientTransport)
	return NewShmConn(shmTransport, shmTransport.GetAuthInfo()), nil
}

// NewShmConn wraps an already-dialed ShmClientTransport in a net.Conn that
// also satisfies ClientTransportProvider, so it can flow through gRPC's
// standard ContextDialer / NewClient path. Callers outside this package
// (notably experimental/shm) use this constructor to obtain the shim
// without needing access to the unexported shmClientConn type.
//
// authInfo is the credentials.AuthInfo to expose via the AuthInfo()
// method on the returned conn; pass the value produced by the SHM
// security handshake (typically shmTransport.GetAuthInfo()).
func NewShmConn(t *ShmClientTransport, authInfo credentials.AuthInfo) net.Conn {
	return &shmClientConn{
		transport:  t,
		localAddr:  t.localAddr,
		remoteAddr: t.remoteAddr,
		authInfo:   authInfo,
	}
}

// shmClientConn is an internal shim that lets the shared-memory transport
// flow through gRPC's standard NewClient / ContextDialer path. gRPC's
// ContextDialer signature requires a net.Conn, but the SHM transport is
// not a byte stream — it is a frame-level transport. To bridge that gap
// shmClientConn implements net.Conn purely so the dialer can return it,
// and additionally implements ClientTransportProvider.
//
// On the consumption side, NewHTTP2Client checks for the
// ClientTransportProvider interface immediately after the dialer returns
// and, when present, uses the wrapped ClientTransport directly instead of
// layering HTTP/2 over the conn. As a result Read and Write on the
// net.Conn surface MUST NOT be reached on the SHM hot path; they return
// io.ErrClosedPipe so any unexpected caller fails fast and visibly rather
// than silently hanging.
//
// Close is guarded by a sync.Once so concurrent Close + Read / Write /
// Close calls cannot race on the underlying transport teardown.
type shmClientConn struct {
	transport  *ShmClientTransport
	localAddr  net.Addr
	remoteAddr net.Addr
	closeOnce  sync.Once
	authInfo   credentials.AuthInfo
}

// Read is intentionally unsupported. See the type doc — callers must use
// GetClientTransport() and go through the ClientTransport API.
func (c *shmClientConn) Read(_ []byte) (n int, err error) {
	return 0, fmt.Errorf("shmClientConn.Read: %w (use ClientTransportProvider.GetClientTransport instead)", io.ErrClosedPipe)
}

// Write is intentionally unsupported. See the type doc — callers must use
// GetClientTransport() and go through the ClientTransport API.
func (c *shmClientConn) Write(_ []byte) (n int, err error) {
	return 0, fmt.Errorf("shmClientConn.Write: %w (use ClientTransportProvider.GetClientTransport instead)", io.ErrClosedPipe)
}

// Close implements net.Conn. Concurrency-safe via sync.Once.
func (c *shmClientConn) Close() error {
	c.closeOnce.Do(func() {
		c.transport.Close(errors.New("connection closed"))
	})
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
func (c *shmClientConn) SetDeadline(_ time.Time) error {
	return nil // Shared memory doesn't support deadlines
}

// SetReadDeadline implements net.Conn
func (c *shmClientConn) SetReadDeadline(_ time.Time) error {
	return nil // Shared memory doesn't support deadlines
}

// SetWriteDeadline implements net.Conn
func (c *shmClientConn) SetWriteDeadline(_ time.Time) error {
	return nil // Shared memory doesn't support deadlines
}

// GetClientTransport returns the underlying client transport
func (c *shmClientConn) GetClientTransport() ClientTransport {
	return c.transport
}

// AuthInfo returns the authentication information for this connection.
// This is set after a successful security handshake.
func (c *shmClientConn) AuthInfo() credentials.AuthInfo {
	return c.authInfo
}
