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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/internal/grpcutil"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

// clientStreamCache holds a cached stream pointer and its ID for lock-free
// lookup in the frame dispatch hot path. Loaded/stored atomically.
type clientStreamCache struct {
	stream   *ClientStream
	streamID uint32
}

// ShmClientTransport implements the gRPC ClientTransport interface
// for shared memory communication.
type ShmClientTransport struct {
	// Core state
	segment        *Segment // The shared memory segment
	clientToServer *ShmRing // Ring for client->server data
	serverToClient *ShmRing // Ring for server->client data
	segmentName    string   // Segment identifier for cleanup

	// Windows event handles for cross-mapping synchronization
	readEvents  *RingEvents
	writeEvents *RingEvents

	// Connection state
	localAddr  net.Addr
	remoteAddr net.Addr

	// Lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	closed atomic.Bool
	// draining indicates GracefulClose or server GOAWAY has been initiated.
	// When draining, NewStream must fail and the transport should close once all
	// active streams finish.
	draining atomic.Bool
	// frameWriter serializes writes to the client->server ring via a dedicated
	// goroutine, eliminating races between concurrent stream writers.
	frameWriter *shmFrameWriter
	mu          sync.RWMutex

	// Stream management
	streams  map[uint32]*ClientStream
	streamID uint32 // next stream ID to assign

	// cachedStream caches the only active stream for single-stream connections,
	// allowing frame dispatch to skip the map lookup + RLock.
	//
	// Loaded atomically without t.mu in the frame dispatch hot path.
	// Stored atomically under t.mu when the stream set changes.
	cachedStream atomic.Pointer[clientStreamCache]

	// singleStreamMode is negotiated via the CONNECT frame. When true,
	// the client requested single-stream optimizations and the transport
	// uses inline writes via inlineMu.TryLock and cachedStream fast path.
	singleStreamMode bool

	// Flow control (outbound send windows)
	sendQuotaMu           sync.Mutex
	connSendQuota         int64
	streamSendQuota       map[uint32]int64
	quotaSignal           chan struct{}
	streamInFlow          map[uint32]*inFlow
	connInFlow            trInFlow
	maxConcurrentStreams  uint32
	streamQuota           int64
	streamsQuotaAvailable chan struct{}
	waitingStreams        uint32

	// BDP estimation and dynamic flow control (RFC A73 Phase 5)
	bdpEst            *shmBDPEstimator
	initialWindowSize int32

	// initialStreamWindow is the per-stream send-quota value applied
	// to each new stream's streamSendQuota at NewStream time. When 0
	// (default), the transport uses maxWindowSize (~2 GiB) i.e. flow
	// control disabled — production behavior. When set non-zero by
	// DialShm reading opts.InitialWindowSize, the producer-side
	// chunked write path (acquireUpToSendQuota) actually enforces
	// the window.
	initialStreamWindow int64

	// WindowUpdate batching: accumulate deltas and flush when threshold exceeded.
	pendingConnWU   uint32            // accumulated connection-level WindowUpdate delta
	pendingStreamWU map[uint32]uint32 // accumulated per-stream WindowUpdate deltas

	// Error handling
	closeOnce sync.Once
	errCh     chan struct{}
	goAwayCh  chan struct{}

	goAwayOnce         sync.Once
	goAwayReason       GoAwayReason
	goAwayDebugMessage string

	readerWG sync.WaitGroup

	// Keepalive
	lastRead         int64 // Unix nanos; updated atomically on each received frame
	kp               keepalive.ClientParameters
	keepaliveEnabled bool
	keepaliveDone    chan struct{} // closed when keepalive goroutine exits
	// kpDormancyCond signals the keepalive goroutine to exit dormant state.
	// Guarded by mu.
	kpDormancyCond *sync.Cond
	kpDormant      bool

	// onClose is a callback invoked when the transport is closed.
	// This is used by ClientConn/addrConn to track connectivity state.
	// RFC A73: Required for proper subchannel lifecycle management.
	onClose func(GoAwayInfo)

	// authInfo stores the authentication information from security handshake.
	authInfo credentials.AuthInfo
}

func (t *ShmClientTransport) setGoAwayReason(flags uint8, debug string) {
	t.goAwayOnce.Do(func() {
		// shm GOAWAY frames do not carry an HTTP/2 error code or debug data.
		// Mirror the http2 client default when a GOAWAY is received.
		t.goAwayReason = GoAwayNoReason
		if debug == "" {
			if flags&GoAwayFlagIMMEDIATE != 0 {
				t.goAwayDebugMessage = "received GOAWAY (immediate)"
				return
			}
			t.goAwayDebugMessage = "received GOAWAY (draining)"
			return
		}
		// Prefer peer-provided debug string when present.
		t.goAwayDebugMessage = debug
	})
}

// notifyQuotaChangeLocked wakes ONE waiter (if any is parked) so it
// can recheck its quota condition. Successful acquirers chain-wake
// the next parker via the same signal at their return point, so a
// single WindowUpdate cascades through all satisfiable waiters.
//
// quotaSignal is a buffered-1 channel: a notify while no waiter is
// parked just buffers; the next acquirer consumes it and proceeds.
// A notify while the buffer is already full drops, which is fine
// because the prior pending notify is sufficient to wake one parker
// (and they chain-wake the next).
//
// This replaces the previous close-and-recreate broadcast pattern,
// which woke EVERY parker on every WindowUpdate. At N=1000
// concurrent streams with the fair-default 64 KiB HTTP/2 connection
// window, the broadcast caused a thundering herd that burned ~22%
// of CPU in runtime.futex (verified by pprof).
func (t *ShmClientTransport) notifyQuotaChangeLocked() {
	select {
	case t.quotaSignal <- struct{}{}:
	default:
	}
}

func (t *ShmClientTransport) addSendQuota(streamID uint32, delta uint32) {
	if delta == 0 {
		return
	}
	if shmNoWU() {
		// v3.4 P1a: no quota tracking; nothing to add.
		return
	}
	t.sendQuotaMu.Lock()
	if streamID == 0 {
		t.connSendQuota += int64(delta)
	} else {
		if _, ok := t.streamSendQuota[streamID]; ok {
			t.streamSendQuota[streamID] += int64(delta)
		}
	}
	t.notifyQuotaChangeLocked()
	t.sendQuotaMu.Unlock()
}

func (t *ShmClientTransport) acquireSendQuota(ctx context.Context, streamID uint32, n int) error {
	if n == 0 {
		return nil
	}
	if shmNoWU() {
		// v3.4 P1a: skip quota; ring backpressure is the only limit.
		shmQuotaSkips.Add(1)
		if t.closed.Load() {
			return ErrConnClosing
		}
		return nil
	}
	t.sendQuotaMu.Lock()
	for {
		if t.closed.Load() {
			t.sendQuotaMu.Unlock()
			return ErrConnClosing
		}
		connOK := t.connSendQuota >= int64(n)
		streamOK := false
		if q, ok := t.streamSendQuota[streamID]; ok && q >= int64(n) {
			streamOK = true
		}
		if connOK && streamOK {
			t.connSendQuota -= int64(n)
			t.streamSendQuota[streamID] -= int64(n)
			// Chain-wake: signal one more parker so cascading WUs reach
			// every satisfiable waiter without thundering herd.
			select {
			case t.quotaSignal <- struct{}{}:
			default:
			}
			t.sendQuotaMu.Unlock()
			return nil
		}
		ch := t.quotaSignal
		t.sendQuotaMu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ContextErr(ctx.Err())
		case <-t.ctx.Done():
			return ErrConnClosing
		}
		t.sendQuotaMu.Lock()
	}
}

// acquireUpToSendQuota acquires up to `want` bytes of send quota,
// returning whatever is currently available on both the stream and
// connection windows (min of the two, capped at want). Blocks only
// when zero bytes are available on either window; on a nil error it
// returns got > 0.
//
// This is the correct primitive for writing a MESSAGE whose total
// size may exceed the per-stream HTTP/2 flow-control window.
// acquireSendQuota waits for `n` bytes atomically and deadlocks when
// n > stream window because the receiver never gets data to consume,
// never sends WINDOW_UPDATE, and the window never grows.
// acquireUpToSendQuota lets the caller drain the window in chunks,
// give the receiver a chance to credit it back, then continue.
//
// Callers MUST loop until they have written the full message, and
// MUST set MessageFlagMORE / clear MessageFlagEndStream on
// intermediate chunks so the receiver's lpmAccumulator stitches the
// DATA frames into one LPM. Only the final chunk carries the caller's
// intended EndStream signal.
func (t *ShmClientTransport) acquireUpToSendQuota(ctx context.Context, streamID uint32, want int) (got int, err error) {
	if want <= 0 {
		return 0, nil
	}
	if shmNoWU() {
		// v3.4 P1a: skip quota; grant the full request immediately.
		// Ring backpressure (downstream) enforces real limits.
		shmQuotaSkips.Add(1)
		if t.closed.Load() {
			return 0, ErrConnClosing
		}
		return want, nil
	}
	t.sendQuotaMu.Lock()
	for {
		if t.closed.Load() {
			t.sendQuotaMu.Unlock()
			return 0, ErrConnClosing
		}
		avail := t.connSendQuota
		q, ok := t.streamSendQuota[streamID]
		if !ok {
			// Stream not registered — caller violated the contract
			// (must NewStream before write). Surface as errStreamDone
			// rather than block forever on a quota that will never
			// be created.
			t.sendQuotaMu.Unlock()
			return 0, errStreamDone
		}
		if q < avail {
			avail = q
		}
		if avail > 0 {
			grant := avail
			if grant > int64(want) {
				grant = int64(want)
			}
			t.connSendQuota -= grant
			t.streamSendQuota[streamID] -= grant
			// Chain-wake: see notifyQuotaChangeLocked.
			select {
			case t.quotaSignal <- struct{}{}:
			default:
			}
			t.sendQuotaMu.Unlock()
			return int(grant), nil
		}
		ch := t.quotaSignal
		t.sendQuotaMu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return 0, ContextErr(ctx.Err())
		case <-t.ctx.Done():
			return 0, ErrConnClosing
		}
		t.sendQuotaMu.Lock()
	}
}

func (t *ShmClientTransport) sendWindowUpdate(streamID uint32, delta uint32) {
	if delta == 0 || t.closed.Load() {
		return
	}
	if shmNoWU() {
		// v3.4 P1a: do not emit WU frames between SHM peers.
		shmWUFramesElided.Add(1)
		return
	}
	// Batch WindowUpdate deltas: only send a frame when the accumulated
	// delta exceeds shmWindowUpdateThreshold (8 MB). This dramatically
	// reduces the number of control frames on the wire.
	t.sendQuotaMu.Lock()
	if streamID == 0 {
		t.pendingConnWU += delta
		if t.pendingConnWU < uint32(shmWindowUpdateThreshold) {
			t.sendQuotaMu.Unlock()
			return
		}
		delta = t.pendingConnWU
		t.pendingConnWU = 0
	} else {
		t.pendingStreamWU[streamID] += delta
		if t.pendingStreamWU[streamID] < uint32(shmWindowUpdateThreshold) {
			t.sendQuotaMu.Unlock()
			return
		}
		delta = t.pendingStreamWU[streamID]
		delete(t.pendingStreamWU, streamID)
	}
	t.sendQuotaMu.Unlock()

	buf := make([]byte, 4)
	// RFC 7540 §6.9.1: WINDOW_UPDATE Window Size Increment is a 31-bit
	// big-endian unsigned integer. Match the spec so the codec's
	// validate-non-zero check (which reads BigEndian) sees the
	// correct value, and so an external HTTP/2 peer parsing this
	// frame interprets the increment correctly.
	binary.BigEndian.PutUint32(buf, delta)
	// enqueueOrInline: writes the WU frame directly on this goroutine if
	// the frame writer is idle (the common case on the receive path,
	// since the writer parks waiting for app traffic). Falls back to
	// async enqueue under contention. Saves one goroutine hop +
	// scheduler wake per WU; matters under fair-default flow control
	// where the producer stalls one round-trip per ~16 KiB consumed.
	_ = t.frameWriter.enqueueOrInline(frameEntry{
		ctx:     context.Background(),
		fh:      FrameHeader{Type: FrameTypeWindowUpdate, StreamID: streamID},
		payload: buf,
	})
}

// updateFlowControl updates the incoming flow control windows for the
// transport and all active streams based on the current BDP estimation.
// This mirrors HTTP/2's dynamic window adjustment behavior.
func (t *ShmClientTransport) updateFlowControl(n uint32) {
	t.mu.Lock()
	t.initialWindowSize = int32(n)
	for _, s := range t.streams {
		s.fc.newLimit(n)
	}
	t.mu.Unlock()

	// Send connection-level window update
	if wu := t.connInFlow.newLimit(n); wu > 0 {
		t.sendWindowUpdate(0, wu)
	}
}

// sendBDPPing sends a BDP estimation ping to the server.
func (t *ShmClientTransport) sendBDPPing() {
	if t.closed.Load() {
		return
	}
	t.bdpEst.timesnap()
	_ = t.frameWriter.enqueue(frameEntry{
		ctx:     context.Background(),
		fh:      FrameHeader{Type: FrameTypePING, Flags: PingFlagBDP},
		payload: bdpPing.data[:],
	})
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

	// Extract segment name for event naming
	segmentName := extractSegmentName(segment.Path)

	// Create rings for bidirectional communication
	// Ring A: client->server, Ring B: server->client
	clientToServer := NewShmRingFromSegment(segment.A, segment.Mem)
	serverToClient := NewShmRingFromSegment(segment.B, segment.Mem)

	// Tag both rings with the segment path so the same-process wake
	// registry (SHM_INPROC_WAKE=1 experimental path) can match the
	// producer's signalData with the consumer's waitForData by
	// (segmentID, byte-offset) instead of by vaddr. Different mmap
	// calls of the same /dev/shm file return different virtual
	// addresses, so vaddr-keying fails.
	clientToServer.SetSegmentID(segment.Path)
	serverToClient.SetSegmentID(segment.Path)
	segment.RegisterRing(clientToServer)
	segment.RegisterRing(serverToClient)

	// Open events for cross-mapping synchronization (Windows).
	// Client opens events created by the server. On Linux, these are no-ops.
	writeEvents, _ := OpenRingEvents(segmentName, "A")
	readEvents, _ := OpenRingEvents(segmentName, "B")

	// Attach events to rings
	clientToServer.SetEvents(writeEvents)
	serverToClient.SetEvents(readEvents)

	ctx, cancel := context.WithCancel(context.Background())

	segName := ""
	if addr, ok := remoteAddr.(*ShmAddr); ok {
		segName = addr.Name
	}

	t := &ShmClientTransport{
		segment:        segment,
		clientToServer: clientToServer,
		serverToClient: serverToClient,
		segmentName:    segName,
		readEvents:     readEvents,
		writeEvents:    writeEvents,
		localAddr:      localAddr,
		remoteAddr:     remoteAddr,
		ctx:            ctx,
		cancel:         cancel,
		streams:        make(map[uint32]*ClientStream),

		streamSendQuota:       make(map[uint32]int64),
		streamInFlow:          make(map[uint32]*inFlow),
		pendingStreamWU:       make(map[uint32]uint32),
		errCh:                 make(chan struct{}),
		goAwayCh:              make(chan struct{}),
		quotaSignal:           make(chan struct{}),
		streamsQuotaAvailable: make(chan struct{}, 1),
		keepaliveDone:         make(chan struct{}),
	}
	// Start the dedicated frame writer goroutine for the client→server ring.
	t.frameWriter = newShmFrameWriter(clientToServer)
	// Surface async write failures (SHM_NO_WU=1 fire-and-forget MESSAGE
	// path, and async HEADERS/GOAWAY) by tearing down the transport.
	// Without this hook the writer goroutine would silently drop bytes
	// after data.Free(), and the peer would wait forever for a MESSAGE
	// that was never sent. The handler runs in a fresh goroutine so it
	// can safely call Close (which waits for the writer goroutine that
	// is currently invoking the callback). Close is guarded by
	// closeOnce so concurrent invocations are idempotent.
	t.frameWriter.setAsyncErrorHandler(func(err error) {
		// Context cancellation on the per-stream context is benign —
		// the stream is gone and the client doesn't need the bytes.
		// Ring closed errors mean we are already tearing down.
		if err == context.Canceled || err == context.DeadlineExceeded {
			return
		}
		if t.closed.Load() {
			return
		}
		go t.Close(fmt.Errorf("shm client: async write failed: %w", err))
	})

	// Initialize dormancy condition variable.
	t.kpDormancyCond = sync.NewCond(&t.mu)
	// Initialize connection-level flow control windows to the HTTP/2 maximum.
	t.connSendQuota = int64(maxWindowSize)
	t.connInFlow = trInFlow{limit: uint32(maxWindowSize)}
	t.connInFlow.updateEffectiveWindowSize()

	// Initialize BDP estimation for dynamic flow control (RFC A73 Phase 5).
	// SHM uses a much larger initial window (32MB) than HTTP/2 (64KB) because
	// local memory has near-zero RTT and high bandwidth.
	t.initialWindowSize = int32(shmInitialWindowSize)
	t.bdpEst = newShmBDPEstimator(uint32(shmInitialWindowSize), t.updateFlowControl)

	max := segment.H.MaxStreams()
	if max == 0 {
		max = uint32(math.MaxUint32)
	}
	t.maxConcurrentStreams = max
	t.streamQuota = int64(max)

	// Start processing incoming data from the server (test hook guarded)
	if enableClientReader.Load() {
		t.readerWG.Add(1)
		go func() {
			defer t.readerWG.Done()
			t.processIncomingData(t.ctx)
		}()
	}

	return t, nil
}

// SetOnClose sets the callback to be invoked when the transport is closed.
// RFC A73: This integrates with gRPC's ClientConn connectivity state management.
func (t *ShmClientTransport) SetOnClose(f func(GoAwayInfo)) {
	t.onClose = f
}

// ConfigureKeepalive sets keepalive parameters and starts the keepalive
// goroutine if Time != infinity.
func (t *ShmClientTransport) ConfigureKeepalive(kp keepalive.ClientParameters) {
	// Apply defaults matching HTTP/2 transport.
	if kp.Time == 0 {
		kp.Time = defaultClientKeepaliveTime
	}
	if kp.Timeout == 0 {
		kp.Timeout = defaultClientKeepaliveTimeout
	}
	t.kp = kp
	if kp.Time != infinity {
		t.keepaliveEnabled = true
		go t.keepalive()
	}
}

// SetAuthInfo sets the authentication information from security handshake.
func (t *ShmClientTransport) SetAuthInfo(authInfo credentials.AuthInfo) {
	t.mu.Lock()
	t.authInfo = authInfo
	t.mu.Unlock()
}

// GetAuthInfo returns the authentication information from security handshake.
func (t *ShmClientTransport) GetAuthInfo() credentials.AuthInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.authInfo
}

// processIncomingData reads data from the server->client ring and processes gRPC frames
func (t *ShmClientTransport) processIncomingData(ctx context.Context) {
	if shmDebugEnabled {
		shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: STARTED")
	}
	// Install the per-DATA-frame flow-control callback on the H2
	// decoder for the server→client ring. See the matching block in
	// ShmServerTransport.processIncomingData for the design rationale
	// (decouple H2 conn-flow credit from gRPC LPM reassembly so
	// multi-DATA-frame responses don't deadlock under a small
	// per-stream send window).
	t.serverToClient.h2Decoder().onDataFrame = t.onDataFrameReceived
	defer func() {
		if shmDebugEnabled {
			shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: EXITING")
		}
		if !t.closed.Load() {
			go t.Close(errors.New("incoming data processing ended"))
		}
	}()

	// Bounded burst counter: number of MESSAGE frames delivered since the
	// last cooperative yield. When the ring keeps producing data the reader
	// stays on-CPU to drain it, but we cap the burst so app goroutines that
	// just got data on their recvBuffer get a chance to run and post their
	// next Send. See shmClientMaxMessageBurst doc-comment for the cap value
	// rationale.
	messageBurst := 0

	for {
		if t.closed.Load() {
			if shmDebugEnabled {
				shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: transport closed, exiting")
			}
			return
		}
		if shmDebugEnabled {
			shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: waiting for frame from server...")
		}
		// Event-driven: block on next frame from rx ring.
		fh, payloadBuf, err := readFrameView(ctx, t.serverToClient)
		if err != nil {
			if shmDebugEnabled {
				shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: readFrame error: %v", err)
			}
			if errors.Is(err, io.EOF) {
				return
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			if errors.Is(err, ErrRingClosed) || t.closed.Load() {
				return
			}
			continue
		}
		if shmDebugEnabled {
			shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: received frame type=%d, streamID=%d, length=%d", fh.Type, fh.StreamID, fh.Length)
		}

		// Update last read timestamp for keepalive tracking.
		atomic.StoreInt64(&t.lastRead, time.Now().UnixNano())

		payloadTransferred := false
		release := func() {
			if !payloadTransferred && payloadBuf != nil {
				payloadBuf.Free()
				payloadBuf = nil
			}
		}

		var payload []byte
		if payloadBuf != nil {
			payload = payloadBuf.ReadOnlyData()
		}

		// Transport-level frames are not associated with a particular stream.
		switch fh.Type {
		case FrameTypeGOAWAY:
			var dbg string
			if len(payload) > 0 {
				dbg = string(payload)
			}
			t.setGoAwayReason(fh.Flags, dbg)
			t.draining.Store(true)
			select {
			case <-t.goAwayCh:
				// already closed
			default:
				close(t.goAwayCh)
			}
			if fh.Flags&GoAwayFlagIMMEDIATE != 0 {
				release()
				go t.Close(errors.New("received GOAWAY (immediate)"))
				return
			}
			t.mu.RLock()
			active := len(t.streams)
			t.mu.RUnlock()
			if active == 0 {
				release()
				go t.Close(errors.New("received GOAWAY (draining) with no active streams"))
				return
			}
			release()
			continue
		case FrameTypeWindowUpdate:
			if shmNoWU() {
				// v3.4 P1a: SHM peers MUST NOT emit WU. If we get one, ignore.
				shmWUFramesIgnored.Add(1)
				release()
				continue
			}
			if len(payload) >= 4 {
				// RFC 7540 §6.9.1: increment is big-endian. Senders
				// (sendWindowUpdate above) write BigEndian so this matches.
				delta := binary.BigEndian.Uint32(payload[:4])
				t.addSendQuota(fh.StreamID, delta)
			}
			release()
			continue
		}

		// Dispatch frame to appropriate stream.
		// Fast path: if we have a cached single stream, skip map lookup + RLock.
		var stream *ClientStream
		if c := t.cachedStream.Load(); c != nil && c.streamID == fh.StreamID {
			stream = c.stream
		} else {
			t.mu.RLock()
			var ok bool
			stream, ok = t.streams[fh.StreamID]
			t.mu.RUnlock()
			if !ok {
				release()
				continue
			}
		}

		// Handle different frame types
		switch fh.Type {
		case FrameTypeHEADERS:
			// Server sent headers (response headers)
			h, err := decodeHeaders(payload)
			if err != nil {
				release()
				stream.write(recvMsg{err: err})
				continue
			}

			// Populate the received header metadata.
			md := make(metadata.MD)
			for _, kv := range h.Metadata {
				vals := make([]string, 0, len(kv.Values))
				for _, v := range kv.Values {
					vals = append(vals, string(v))
				}
				md[kv.Key] = vals
			}
			if v := md.Get("grpc-encoding"); len(v) > 0 {
				stream.recvCompress = v[0]
			}
			if v := md.Get("content-type"); len(v) > 0 {
				if contentSubtype, ok := grpcutil.ContentSubtype(v[0]); ok {
					stream.contentSubtype = contentSubtype
				} else {
					release()
					stream.write(recvMsg{err: errors.New("transport: received unexpected content-type")})
					continue
				}
			}
			stream.header = md
			stream.headerValid = true
			stream.noHeaders = false

			// Signal that headers have been received
			if atomic.CompareAndSwapUint32(&stream.headerChanClosed, 0, 1) {
				close(stream.headerChan)
			}
			release()

		case FrameTypeMESSAGE:
			// Server sent a message. Apply inbound flow control before delivering.
			if shmDebugEnabled {
				shmDebugf("[DEBUG] ShmClientTransport: MESSAGE handler entered for stream %d, payload size=%d", fh.StreamID, len(payload))
			}
			sz := uint32(len(payload))

			// BDP estimation: track bytes received and trigger BDP ping if needed
			var sendBDPPing bool
			if t.bdpEst != nil {
				sendBDPPing = t.bdpEst.add(sz)
			}

			// NOTE: connection + stream flow-control credit is done
			// per H2 DATA frame in onDataFrameReceived (installed on
			// the h2 decoder above). Crediting here would double-
			// count. See onDataFrameReceived for the design rationale.

			// Send BDP ping if BDP estimator requests it
			if sendBDPPing {
				// Send window update before BDP ping to avoid excessive ping detection
				if wu := t.connInFlow.reset(); wu > 0 {
					t.sendWindowUpdate(0, wu)
				}
				t.sendBDPPing()
			}

			// Transfer ownership of the ring-backed buffer to the stream for zero-copy delivery.
			if payloadBuf != nil {
				if shmDebugEnabled {
					shmDebugf("[DEBUG] ShmClientTransport: MESSAGE delivering payloadBuf (len=%d) to stream %d", payloadBuf.Len(), fh.StreamID)
				}
				payloadTransferred = true
				stream.write(recvMsg{buffer: payloadBuf})
				payloadBuf = nil
			} else {
				if shmDebugEnabled {
					shmDebugf("[DEBUG] ShmClientTransport: MESSAGE delivering copied payload (len=%d) to stream %d", len(payload), fh.StreamID)
				}
				buf := mem.Copy(payload, mem.DefaultBufferPool())
				stream.write(recvMsg{buffer: buf})
			}
			// Yield to the app goroutine that was just goready'd by the channel
			// send. The recvBuffer's channel put places the receiver G on the
			// current P's local runq head; without a Gosched the runtime's
			// wakep then tries to find an idle M on another P to run the
			// woken G in parallel — which costs a futex syscall on Linux.
			// For ping-pong RPCs the parallelism is illusory (the reader has
			// nothing else to do until the server replies, which itself waits
			// on the app's next Send), so co-locating the two Gs on this M
			// strictly wins. The runtime.Gosched is a cooperative yield, not
			// a spin: it costs no CPU when no other G is runnable.
			//
			// AT HIGH STREAM CONCURRENCY (N=1000+), this unconditional yield
			// costs N park/unpark cycles per RPC round. Skip the yield when
			// more frames are immediately ready in the ring — keep draining
			// instead of round-tripping through the scheduler. The ping-pong
			// win is preserved because in the 1-stream case the ring is
			// almost always empty after the MESSAGE is delivered. A burst
			// cap (shmClientMaxMessageBurst) bounds how many frames the
			// reader will process without yielding so that app goroutines
			// waiting on recvBuffer don't starve.
			//
			// SIZE-AWARE: only the small-payload case wins from skipping
			// the yield. At medium payloads (e.g. N=100 streams sending
			// 64 KiB messages) the parallel app goroutine work outweighs
			// the wakep cost — let work-stealing pick up the recvBuffer
			// reader on another P. Always yield when the just-delivered
			// payload is above shmYieldSkipMaxPayload bytes.
			messageBurst++
			yield := sz > shmYieldSkipMaxPayload ||
				messageBurst >= shmClientMaxMessageBurst ||
				!t.serverToClient.HasPendingData()
			if yield {
				runtime.Gosched()
				messageBurst = 0
			}
			if shmDebugEnabled {
				shmDebugf("[DEBUG] ShmClientTransport: MESSAGE delivered to stream %d", fh.StreamID)
			}

		case FrameTypePING:
			// Respond with PONG carrying the same opaque data.
			pongPayload := make([]byte, len(payload))
			copy(pongPayload, payload)
			_ = t.frameWriter.enqueue(frameEntry{
				ctx:     context.Background(),
				fh:      FrameHeader{Type: FrameTypePONG, Flags: fh.Flags},
				payload: pongPayload,
			})
			release()

		case FrameTypePONG:
			// Check if this is a BDP ping acknowledgment
			if t.bdpEst != nil && len(payload) >= 8 {
				var data [8]byte
				copy(data[:], payload[:8])
				if data == bdpPing.data {
					t.bdpEst.calculate()
				}
			}
			release()
			continue

		case FrameTypeTRAILERS:
			// Server sent trailers (end of stream)
			tr, err := decodeTrailers(payload)
			if err != nil {
				release()
				t.closeStream(stream, err, false, 0, nil, nil, false)
			} else {
				// Convert metadata from protocol format to map
				trailerMap := make(map[string][]string)
				for _, kv := range tr.Metadata {
					trailerMap[kv.Key] = make([]string, len(kv.Values))
					for i, v := range kv.Values {
						trailerMap[kv.Key][i] = string(v)
					}
				}

				// Convert status
				var st *status.Status
				if tr.GRPCStatusCode != 0 {
					st = status.New(codes.Code(tr.GRPCStatusCode), tr.GRPCStatusMsg)
					err = st.Err()
				} else {
					st = status.New(codes.OK, "")
					err = io.EOF
				}

				// Close the stream with trailers
				t.closeStream(stream, err, false, 0, st, trailerMap, true)
			}
			release()

		case FrameTypeCANCEL:
			// Server cancelled the stream
			stream.write(recvMsg{err: context.Canceled})
			release()

		default:
			// Unknown frame type - ignore
			release()
		}
	}
}

// Close tears down this transport. Once it returns, the transport
// should not be accessed any more. The caller must make sure this
// is called only once.
func (t *ShmClientTransport) Close(err error) {
	t.closeOnce.Do(func() {
		// Mark closed early so late closeStream calls won't attempt to write to the
		// rings while teardown is in progress.
		t.closed.Store(true)
		segClosed := t.segment != nil && t.segment.closed.Load()
		t.sendQuotaMu.Lock()
		t.notifyQuotaChangeLocked()
		t.sendQuotaMu.Unlock()

		// Best-effort GOAWAY before tearing down rings.
		// Non-blocking: if the channel is full (writer stuck on ring write),
		// skip GOAWAY to avoid deadlocking Close.
		if t.clientToServer != nil && !segClosed {
			t.frameWriter.tryEnqueueNonBlocking(frameEntry{
				ctx:     context.Background(),
				fh:      FrameHeader{Type: FrameTypeGOAWAY, Flags: GoAwayFlagIMMEDIATE},
				payload: []byte("client closing"),
			})
		}

		// Cancel context to stop background reader goroutine and keepalive.
		t.cancel()

		// Close the rings FIRST so any writeFrame blocked inside the writer
		// goroutine gets ErrRingClosed and unblocks. This must happen before
		// waiting for keepalive, because keepalive's sendPing uses
		// enqueueAndWait which blocks on the writer goroutine.
		if !segClosed {
			if t.clientToServer != nil {
				_ = t.clientToServer.Close()
			}
			if t.serverToClient != nil {
				_ = t.serverToClient.Close()
			}
		}
		t.frameWriter.close()

		// Wake up the keepalive goroutine if it's dormant, so it can exit.
		t.mu.Lock()
		if t.kpDormant {
			t.kpDormancyCond.Signal()
		}
		t.mu.Unlock()

		// Wait for keepalive goroutine to exit.
		if t.keepaliveEnabled && t.keepaliveDone != nil {
			<-t.keepaliveDone
		}

		// Terminate all active streams before unmapping the segment.
		t.mu.Lock()
		streams := make([]*ClientStream, 0, len(t.streams))
		for _, stream := range t.streams {
			if stream != nil {
				streams = append(streams, stream)
			}
		}
		t.mu.Unlock()
		for _, stream := range streams {
			t.closeStream(stream, err, false, 0, status.Convert(err), nil, false)
		}

		// Stop the reader goroutine. Under SHM_DATASEG_WAKE the reader
		// is parked in shmDataSegWaker.WaitForChange (an *os.File.Read
		// on the eventfd via Go netpoll); ring.Close above set
		// hdr.Closed but the parker has no way to observe that without
		// a wake. Closing the eventfd makes Read return EBADF, which
		// WaitForChange surfaces as ErrRingClosed, and the reader
		// outer-loop exits. No-op on the per-address eventfd / futex
		// path (signal* above already woke same-side parkers there).
		if t.segment != nil {
			t.segment.UnblockSameSideParkers()
		}

		t.readerWG.Wait()

		// Close the named events (Windows)
		if t.readEvents != nil {
			t.readEvents.Close()
		}
		if t.writeEvents != nil {
			t.writeEvents.Close()
		}

		// Close the segment last and unlink the backing file.
		if t.segment != nil {
			_ = t.segment.Close()
		}
		if t.segmentName != "" {
			_ = RemoveSegment(t.segmentName)
		}

		// Signal closure
		close(t.errCh)

		// RFC A73: Invoke onClose callback to notify ClientConn of transport closure.
		// This allows the addrConn to update connectivity state properly.
		if t.onClose != nil {
			t.onClose(GoAwayInfo{Reason: t.goAwayReason})
		}
	})
}

// GracefulClose starts to tear down the transport: the transport will stop
// accepting new RPCs and NewStream will return error. Once all streams are
// finished, the transport will close.
//
// It does not block.
func (t *ShmClientTransport) GracefulClose() {
	// Mirror http2 client semantics: move into draining, which prevents new
	// streams from being created. Close the transport only after the last active
	// stream completes.
	if t.closed.Load() {
		return
	}
	if !t.draining.CompareAndSwap(false, true) {
		return
	}

	// Best-effort notify the peer we're draining.
	if t.clientToServer != nil {
		_ = t.frameWriter.enqueue(frameEntry{
			ctx:     context.Background(),
			fh:      FrameHeader{Type: FrameTypeGOAWAY, Flags: GoAwayFlagDRAINING},
			payload: []byte("draining"),
		})
	}

	// If there are no active streams, close immediately.
	t.mu.RLock()
	active := len(t.streams)
	t.mu.RUnlock()
	if active == 0 {
		t.Close(errors.New("no active streams left to process while draining"))
	}
}

// NewStream creates a Stream for an RPC.
func (t *ShmClientTransport) NewStream(ctx context.Context, callHdr *CallHdr, handler stats.Handler) (*ClientStream, error) {
	if t.closed.Load() || t.draining.Load() {
		return nil, &NewStreamError{Err: ErrConnClosing, AllowTransparentRetry: true}
	}

	firstTry := true
	var ch chan struct{}
	var s *ClientStream
	var streamID uint32
	var transportDrainRequired bool
	for {
		t.mu.Lock()
		if t.closed.Load() || t.draining.Load() {
			t.mu.Unlock()
			return nil, &NewStreamError{Err: ErrConnClosing, AllowTransparentRetry: true}
		}
		if t.streamQuota <= 0 {
			if firstTry {
				t.waitingStreams++
			}
			ch = t.streamsQuotaAvailable
			t.mu.Unlock()
			firstTry = false
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, &NewStreamError{Err: ContextErr(ctx.Err())}
			case <-t.goAwayCh:
				return nil, &NewStreamError{Err: errStreamDrain, AllowTransparentRetry: true}
			case <-t.ctx.Done():
				return nil, &NewStreamError{Err: ErrConnClosing, AllowTransparentRetry: true}
			}
		}
		if !firstTry {
			t.waitingStreams--
		}
		t.streamQuota--

		// Assign stream ID (client uses odd IDs, starting from 1)
		streamID = t.streamID
		if streamID == 0 {
			streamID = 1
		}
		t.streamID = streamID + 2 // Increment by 2 to maintain odd IDs
		// Drain client transport if nextID > MaxStreamID which signals gRPC that
		// the connection is closed and a new one must be created for subsequent RPCs.
		transportDrainRequired = t.streamID > MaxStreamID

		// Create the client stream
		s = &ClientStream{
			Stream: Stream{
				id:             streamID,
				ctx:            ctx,
				method:         callHdr.Method,
				sendCompress:   callHdr.SendCompress,
				contentSubtype: callHdr.ContentSubtype,
			},
			ct:           t, // Set the client transport (now an interface, no unsafe needed)
			done:         make(chan struct{}),
			headerChan:   make(chan struct{}),
			doneFunc:     callHdr.DoneFunc,
			statsHandler: handler,
		}
		s.Stream.buf.init()
		s.fc = inFlow{limit: uint32(maxWindowSize)}
		if t.initialStreamWindow > 0 && t.initialStreamWindow < int64(maxWindowSize) {
			s.fc = inFlow{limit: uint32(t.initialStreamWindow)}
		}
		s.readRequester = s

		// Set up transport reader for this stream
		s.trReader = transportReader{
			reader: recvBufferReader{
				ctx:          s.ctx,
				ctxDone:      s.ctx.Done(),
				recv:         &s.buf,
				clientStream: s,
			},
			windowHandler: s,
		}

		// Register the stream
		t.streams[streamID] = s
		// Update single-stream cache.
		if len(t.streams) == 1 {
			t.cachedStream.Store(&clientStreamCache{stream: s, streamID: streamID})
		} else {
			t.cachedStream.Store(nil)
		}
		t.sendQuotaMu.Lock()
		streamWindow := int64(maxWindowSize)
		if t.initialStreamWindow > 0 {
			streamWindow = t.initialStreamWindow
		}
		t.streamSendQuota[streamID] = streamWindow
		t.sendQuotaMu.Unlock()
		t.streamInFlow[streamID] = &s.fc
		if t.streamQuota > 0 && t.waitingStreams > 0 {
			select {
			case t.streamsQuotaAvailable <- struct{}{}:
			default:
			}
		}
		// Wake up the keepalive goroutine if it's dormant, so it can start
		// monitoring the now-active connection.
		if t.kpDormant {
			t.kpDormancyCond.Signal()
		}

		t.mu.Unlock()

		break
	}

	// Send HEADERS frame to initiate the stream
	var deadlineUnixNano uint64
	if deadline, ok := ctx.Deadline(); ok {
		if unixNano := deadline.UnixNano(); unixNano > 0 {
			deadlineUnixNano = uint64(unixNano)
		}
	}
	var kvs []KV
	hasKey := func(key string) bool {
		for _, kv := range kvs {
			if kv.Key == key {
				return true
			}
		}
		return false
	}
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		for k, vals := range md {
			byteVals := make([][]byte, 0, len(vals))
			for _, v := range vals {
				byteVals = append(byteVals, []byte(v))
			}
			kvs = append(kvs, KV{Key: k, Values: byteVals})
		}
	}
	// Add gRPC-required/expected metadata fields if not already present.
	if !hasKey("content-type") {
		kvs = append(kvs, KV{Key: "content-type", Values: [][]byte{[]byte(grpcutil.ContentType(callHdr.ContentSubtype))}})
	}
	registeredCompressors := grpcutil.RegisteredCompressors()
	if callHdr.SendCompress != "" {
		if !hasKey("grpc-encoding") {
			kvs = append(kvs, KV{Key: "grpc-encoding", Values: [][]byte{[]byte(callHdr.SendCompress)}})
		}
		if !grpcutil.IsCompressorNameRegistered(callHdr.SendCompress) {
			if registeredCompressors != "" {
				registeredCompressors += ","
			}
			registeredCompressors += callHdr.SendCompress
		}
	}
	if registeredCompressors != "" && !hasKey("grpc-accept-encoding") {
		kvs = append(kvs, KV{Key: "grpc-accept-encoding", Values: [][]byte{[]byte(registeredCompressors)}})
	}
	hdr := HeadersV1{
		Version:          1,
		HdrType:          0, // client-initial
		Method:           callHdr.Method,
		Authority:        callHdr.Host,
		DeadlineUnixNano: deadlineUnixNano,
		Metadata:         kvs,
	}

	payload := encodeHeaders(hdr)
	fh := FrameHeader{
		StreamID: streamID,
		Type:     FrameTypeHEADERS,
		Flags:    HeadersFlagINITIAL,
	}

	if err := t.frameWriter.enqueueAndWait(frameEntry{
		ctx:     ctx,
		fh:      fh,
		payload: payload,
	}); err != nil {
		t.mu.Lock()
		delete(t.streams, streamID)
		t.streamQuota++
		if t.streamQuota > 0 && t.waitingStreams > 0 {
			select {
			case t.streamsQuotaAvailable <- struct{}{}:
			default:
			}
		}
		t.mu.Unlock()
		// If draining was initiated concurrently and there are no streams left,
		// ensure the transport completes draining.
		if t.draining.Load() {
			t.mu.RLock()
			active := len(t.streams)
			t.mu.RUnlock()
			if active == 0 {
				go t.Close(errors.New("draining with no active streams"))
			}
		}
		return nil, &NewStreamError{Err: err, AllowTransparentRetry: true}
	}

	// If stream ID exhaustion requires draining, initiate graceful close.
	// This mirrors http2Client behavior.
	if transportDrainRequired {
		t.GracefulClose()
	}

	return s, nil
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
func (t *ShmClientTransport) GetGoAwayReason() (GoAwayReason, string) {
	if !t.draining.Load() {
		return GoAwayInvalid, ""
	}
	return t.goAwayReason, t.goAwayDebugMessage
}

// RemoteAddr returns the remote network address.
func (t *ShmClientTransport) RemoteAddr() net.Addr {
	return t.remoteAddr
}

// Peer returns the peer information for this transport.
func (t *ShmClientTransport) Peer() *peer.Peer {
	return &peer.Peer{
		Addr:      t.remoteAddr,
		AuthInfo:  nil, // Shared memory transport does not use authentication
		LocalAddr: t.localAddr,
	}
}

// incrMsgRecv increments the message received counter.
// This is called by ClientStream.Read() when a message is successfully read.
func (t *ShmClientTransport) incrMsgRecv() {
	// For shm transport, we don't track channelz metrics yet
	// This is a no-op for now, but maintains compatibility with ClientStream
}

// adjustWindow sends out extra window update over the initial window size
// of stream if the application is requesting data larger in size than
// the window.
func (t *ShmClientTransport) adjustWindow(s *ClientStream, n uint32) {
	if w := s.fc.maybeAdjust(n); w > 0 {
		t.sendWindowUpdate(s.id, w)
	}
}

// updateWindow adjusts the inbound quota for the stream.
// Window updates will be sent out when the cumulative quota
// exceeds the corresponding threshold.
func (t *ShmClientTransport) updateWindow(s *ClientStream, n uint32) {
	if w := s.fc.onRead(n); w > 0 {
		t.sendWindowUpdate(s.id, w)
	}
}

// onDataFrameReceived is the per-DATA-frame flow-control callback
// the H2 decoder fires on the reader goroutine (see
// processIncomingData where this is installed on the server→client
// ring's hpackDecoderHolder). Same design as the server side: credit
// HTTP/2 connection + stream windows as soon as we've seen the bytes
// on the wire, decoupled from gRPC LPM reassembly. Without this, a
// multi-DATA-frame response message larger than the per-stream send
// window will deadlock the server because the client's recv-side
// lpmAccumulator is buffering the partial LPM and won't trigger
// handleMessage → connInFlow.onData until the whole LPM completes.
//
// `size` is the on-wire DATA payload length (h2fh.Length); auto-acked
// via onRead because the SHM transport treats the ring buffer (not
// HTTP/2 flow control) as the real backpressure signal.
func (t *ShmClientTransport) onDataFrameReceived(streamID uint32, size uint32) {
	if size == 0 {
		return
	}
	// Refill the connection + stream windows by exactly `size`. The
	// sendWindowUpdate batching layer (shmWindowUpdateThreshold)
	// keeps the WINDOW_UPDATE rate sane; we don't gate through
	// trInFlow.onData / inFlow.onData here because their limit/4
	// thresholds are tied to maxWindowSize and won't fire under
	// configurations where the producer's per-stream send window is
	// smaller than the receive limit. See the matching method in
	// shm_server_transport.go for the design rationale.
	t.sendWindowUpdate(0, size)
	t.sendWindowUpdate(streamID, size)
}

// closeStream closes the given stream and cleans up resources.
// This is called by ClientStream.Close() to terminate the stream.
func (t *ShmClientTransport) closeStream(s *ClientStream, err error, rst bool, _ http2.ErrCode, st *status.Status, mdata map[string][]string, _ bool) {
	// Set stream state to done
	if s.swapState(streamDone) == streamDone {
		// Already done, wait for first closer to finish
		<-s.done
		return
	}

	// Update status and trailers
	s.status = st
	if len(mdata) > 0 {
		s.trailer = mdata
	}

	// Signal error to readers. This must happen BEFORE closing headerChan
	// so that gRPC can read any buffered data before seeing the error.
	// For graceful close (eosReceived=true), err is io.EOF which signals
	// the reader that the stream ended normally.
	if err != nil {
		s.write(recvMsg{err: err})
	}

	// Close header channel if not already closed
	if atomic.CompareAndSwapUint32(&s.headerChanClosed, 0, 1) {
		s.noHeaders = true
		close(s.headerChan)
	}

	// Remove stream from active streams map and return stream quota.
	var shouldClose bool
	t.mu.Lock()
	delete(t.streams, s.id)
	// Update single-stream cache.
	if len(t.streams) == 1 {
		for id, cs := range t.streams {
			t.cachedStream.Store(&clientStreamCache{stream: cs, streamID: id})
			break
		}
	} else {
		t.cachedStream.Store(nil)
	}
	t.sendQuotaMu.Lock()
	delete(t.streamSendQuota, s.id)
	delete(t.pendingStreamWU, s.id)
	t.sendQuotaMu.Unlock()
	delete(t.streamInFlow, s.id)
	t.streamQuota++
	if t.streamQuota > 0 && t.waitingStreams > 0 {
		select {
		case t.streamsQuotaAvailable <- struct{}{}:
		default:
		}
	}
	shouldClose = t.draining.Load() && len(t.streams) == 0 && !t.closed.Load()
	t.mu.Unlock()

	// Send CANCEL frame if requested
	if rst && !t.closed.Load() {
		fh := FrameHeader{
			StreamID: s.id,
			Type:     FrameTypeCANCEL,
			Flags:    0,
		}
		// Best effort - ignore errors since stream is closing anyway
		_ = t.frameWriter.enqueue(frameEntry{
			ctx:     context.Background(),
			fh:      fh,
			payload: nil,
		})
	}

	// Close the done channel to unblock waiters
	close(s.done)

	if shouldClose {
		go t.Close(errors.New("transport drained"))
	}

	// Call doneFunc if present
	if s.doneFunc != nil {
		s.doneFunc()
	}
}

// writeProto serializes a proto.Message directly into the ring buffer,
// bypassing the standard encode→copy path.
func (t *ShmClientTransport) writeProto(s *ClientStream, msg any, opts *WriteOptions) (bool, error) {
	pm, ok := msg.(protoMessage)
	if !ok {
		return false, nil
	}
	if t.closed.Load() {
		return false, ErrConnClosing
	}

	// Do NOT check/modify stream state here. If ZC fails, the caller falls
	// back to write() which does its own CAS. Doing it here would leave the
	// stream in streamWriteDone, causing the fallback to return errStreamDone.

	// Check stream is active (read-only — no CAS yet).
	if s.getState() != streamActive {
		return false, errStreamDone
	}

	pSize := protoSize(pm)
	ringSize := h2FrameHeaderSize + 5 + pSize // total bytes in ring (H2 header + gRPC LPM + proto)
	quotaSize := 5 + pSize                    // flow-control size (matches receiver WINDOW_UPDATE accounting)

	// Skip ZC if the message is too large for a single frame.
	// Must check before acquiring flow control quota to avoid double-acquire
	// when the caller falls back to the standard write path.
	if uint64(ringSize) > t.clientToServer.Capacity()/3 {
		return false, nil
	}

	// Skip ZC when the message wouldn't fit in the current per-stream
	// send window. acquireSendQuota is atomic on `quotaSize` bytes —
	// when quotaSize exceeds the stream window it would deadlock
	// (the receiver never gets the data so never sends a WindowUpdate
	// big enough to admit it). The fallback write() path handles
	// over-window messages by chunking under flow control, so just
	// return false and let it run.
	if !shmNoWU() {
		t.sendQuotaMu.Lock()
		streamQ, hasStreamQ := t.streamSendQuota[s.id]
		if !hasStreamQ || streamQ < int64(quotaSize) || t.connSendQuota < int64(quotaSize) {
			t.sendQuotaMu.Unlock()
			atomic.AddUint64(&shmZCWriteSkipQuota, 1)
			return false, nil
		}
		t.sendQuotaMu.Unlock()
	}
	// In shmNoWU mode, skip the window pre-check; ring backpressure handles it.

	// Flow control: account only the gRPC payload (5-byte LPM header + proto
	// body). The 9-byte H2 frame header is a transport-level concern and
	// is NOT included in WINDOW_UPDATE accounting on the receive side.
	if err := t.acquireSendQuota(s.ctx, s.id, quotaSize); err != nil {
		return false, err
	}

	// Set frame flags based on the caller's "last message" signal:
	//
	//   - MessageFlagMORE: signals "more frames follow on this stream".
	//     The server's handleMessage uses MORE=0 on incoming MESSAGE
	//     to detect client half-close.
	//   - MessageFlagEndStream: signals "this is the last message I
	//     will send on this stream". writeProtoToRingH2 maps this to
	//     H2's END_STREAM bit on the emitted DATA frame; the
	//     server-side H2 reader translates END_STREAM back to MORE=0
	//     so the same handleMessage MORE=0 EOF logic fires.
	var frameFlags uint8
	if opts != nil && !opts.Last {
		frameFlags = MessageFlagMORE
	} else {
		frameFlags = MessageFlagEndStream
	}

	// Acquire the frame writer's inline mutex to serialize with writeLoop.
	// writeProtoToRing writes directly to the ring, bypassing the frame
	// writer channel. Without this lock, concurrent control frame writes
	// (PING, WINDOW_UPDATE, etc.) would violate the SPSC ring invariant.
	//
	// closeMu.RLock prevents close() from completing (and the transport
	// from unmapping the segment) while we're writing to the ring.
	t.frameWriter.closeMu.RLock()
	if t.frameWriter.closed.Load() {
		t.frameWriter.closeMu.RUnlock()
		return true, ErrConnClosing
	}
	if !t.frameWriter.inlineMu.TryLock() {
		t.frameWriter.closeMu.RUnlock()
		atomic.AddUint64(&shmZCWriteSkipInlineBusy, 1)
		// Writer goroutine is busy — release quota and let caller fall back
		// to the standard write path (which goes through enqueueAndWait).
		t.sendQuotaMu.Lock()
		t.connSendQuota += int64(quotaSize)
		if _, ok := t.streamSendQuota[s.id]; ok {
			t.streamSendQuota[s.id] += int64(quotaSize)
		}
		t.notifyQuotaChangeLocked()
		t.sendQuotaMu.Unlock()
		return false, nil
	}
	ok2, err := writeProtoToRing(s.ctx, t.clientToServer, s.id, pm, pSize, frameFlags)
	t.frameWriter.inlineMu.Unlock()
	t.frameWriter.closeMu.RUnlock()
	if !ok2 {
		// ZC didn't handle the write (insufficient contiguous space, or
		// an error from ReserveWrite/marshal). Release quota so the
		// fallback standard write path can re-acquire without leak.
		t.sendQuotaMu.Lock()
		t.connSendQuota += int64(quotaSize)
		if _, ok := t.streamSendQuota[s.id]; ok {
			t.streamSendQuota[s.id] += int64(quotaSize)
		}
		t.notifyQuotaChangeLocked()
		t.sendQuotaMu.Unlock()
		return false, err
	}
	if err != nil {
		return true, err
	}

	// ZC succeeded — transition stream state if this is the last message.
	if opts != nil && opts.Last {
		if !s.compareAndSwapState(streamActive, streamWriteDone) {
			// Race: stream was closed concurrently. Data is already written
			// to the ring which is harmless (reader will process it).
			return true, errStreamDone
		}
	}
	return true, nil
}

// write writes data to the stream via the shared memory transport.
// This is called by ClientStream.Write() to send data.
func (t *ShmClientTransport) write(s *ClientStream, hdr []byte, data mem.BufferSlice, opts *WriteOptions) error {
	if shmDebugEnabled {
		shmDebugf("[DEBUG] ShmClientTransport.write: stream=%d, hdr_len=%d, data_bytes=%d, ring=%p", s.id, len(hdr), data.Len(), t.clientToServer)
	}
	// Check if transport is closed
	if t.closed.Load() {
		if shmDebugEnabled {
			shmDebugf("[DEBUG] ShmClientTransport.write: transport closed")
		}
		return ErrConnClosing
	}

	// Check stream state
	if opts != nil && opts.Last {
		// Last message - transition to write done state
		if !s.compareAndSwapState(streamActive, streamWriteDone) {
			if shmDebugEnabled {
				shmDebugf("[DEBUG] ShmClientTransport.write: stream done (Last=true)")
			}
			return errStreamDone
		}
	} else if s.getState() != streamActive {
		if shmDebugEnabled {
			shmDebugf("[DEBUG] ShmClientTransport.write: stream not active")
		}
		return errStreamDone
	}

	payloadLen := len(hdr) + data.Len()

	// Flow-control aware write. The producer's send quota may be
	// smaller than the full message (when grpc.WithInitialWindowSize
	// lowers it, or — once that option is plumbed through — at the
	// HTTP/2 default of 65535 B). The legacy acquireSendQuota call
	// waits atomically for `payloadLen` bytes which deadlocks when
	// payloadLen > stream window. Instead we loop:
	//
	//   - acquire up to `remaining` bytes of currently-available
	//     quota (blocks only if both windows are zero),
	//   - send that many bytes as one MESSAGE frame carrying
	//     MessageFlagMORE so the receiver's lpmAccumulator stitches
	//     it together with the rest of the LPM,
	//   - repeat until the full payload has been written.
	//
	// Only the LAST chunk carries the caller's intended EndStream /
	// MORE flag. Intermediate chunks always set MessageFlagMORE so
	// the H2 codec emits DATA frames without END_STREAM.
	//
	// Fast path: when the first acquireUpToSendQuota grant covers
	// the full payload (the common case in production where the
	// stream quota is maxWindowSize = 2 GiB), we preserve the
	// existing vectored single-frame path through writeFrameBuffers
	// and avoid the materialisation step. The slow path materialises
	// hdr+data once into a contiguous byte slice so we can slice
	// across the hdr/segment boundary without ad-hoc indexing; for
	// messages large enough to span the window this extra copy is
	// dominated by the actual send time.
	if shmDebugEnabled {
		shmDebugf("[DEBUG] ShmClientTransport.write: acquiring send quota for %d bytes", payloadLen)
	}
	got, err := t.acquireUpToSendQuota(s.ctx, s.id, payloadLen)
	if err != nil {
		if shmDebugEnabled {
			shmDebugf("[DEBUG] ShmClientTransport.write: acquireUpToSendQuota failed: %v", err)
		}
		return err
	}

	if got == payloadLen {
		// Fast path: full payload fits in the currently-available
		// window. Use the vectored frame writer to avoid materialising
		// hdr+data into a contiguous buffer.
		fh := FrameHeader{StreamID: s.id, Type: FrameTypeMESSAGE}
		if opts != nil && !opts.Last {
			fh.Flags = MessageFlagMORE
		} else {
			fh.Flags = MessageFlagEndStream
		}
		if shmDebugEnabled {
			shmDebugf("[DEBUG] ShmClientTransport.write: writing single frame (fast path)")
		}
		// v3.4 P1a-async: under SHM_NO_WU mode, fire-and-forget the
		// MESSAGE frame. Take ownership via data.Ref() so the caller's
		// `defer data.Free()` does NOT prematurely release buffers
		// the writer goroutine still needs.
		//
		// Errors are surfaced asynchronously: a failed ring write closes
		// the transport, which marks all streams errored; the next stream
		// operation observes the error. This matches stock grpc-go's
		// loopyWriter pattern (sender does not wait for socket write ack).
		if shmNoWU() {
			data.Ref()
			if err := t.frameWriter.enqueue(frameEntry{
				ctx:      s.ctx,
				fh:       fh,
				hdr:      hdr,
				data:     data,
				freeData: true, // writer Free()s; balances our Ref()
			}); err != nil {
				// enqueue failed (transport closed): writer won't see
				// the entry, so balance the Ref() here.
				data.Free()
				return err
			}
			return nil
		}
		if err := t.frameWriter.enqueueAndWait(frameEntry{
			ctx:  s.ctx,
			fh:   fh,
			hdr:  hdr,
			data: data,
		}); err != nil {
			if shmDebugEnabled {
				shmDebugf("[ERROR] ShmClientTransport.write: frame write failed: %v", err)
			}
			return err
		}
		if shmDebugEnabled {
			shmDebugf("[DEBUG] ShmClientTransport.write: frame written successfully")
		}
		return nil
	}

	// Slow path: payload spans multiple flow-control chunks.
	if shmDebugEnabled {
		shmDebugf("[DEBUG] ShmClientTransport.write: chunked write, got %d / %d on first acquire", got, payloadLen)
	}
	// v3.4 P5: emit each per-window iteration directly from a vecCursor
	// over (hdr || data BufferSlice) so the producer never has to
	// materialise the LPM into one contiguous buffer first. The
	// pre-P5 code allocated a payloadLen-sized slice from shmLpmPool
	// and copied hdr+data into it once before iterating — that
	// materialise step costs one full payload-size memcpy per RPC,
	// which at fair-default's 65535-byte initial window and 16 MB
	// LargeUnary contributes ~3 ms of producer-side latency.
	//
	// The cursor's invariant is that emitMessageInlineVec advances it
	// by exactly the requested chunk length on success, so we can
	// walk the entire (hdr || data) once across however many
	// flow-control grants we need. Errors abort early; the cursor's
	// position is then meaningless but we don't reuse it.
	cur := vecCursor{lpmHdr: hdr, data: data}
	off := 0
	for {
		end := off + got
		isLast := end == payloadLen
		fh := FrameHeader{StreamID: s.id, Type: FrameTypeMESSAGE}
		switch {
		case !isLast:
			// Intermediate chunk: receiver concatenates into one LPM.
			fh.Flags = MessageFlagMORE
		case opts != nil && !opts.Last:
			// Final chunk of THIS message; more messages to follow on
			// the stream. MessageFlagMORE here means "LPM done, more
			// messages later" — same encoding the codec uses to keep
			// END_STREAM off the wire.
			fh.Flags = MessageFlagMORE
		default:
			// Final chunk of the LAST message on the stream.
			fh.Flags = MessageFlagEndStream
		}
		if err := t.frameWriter.emitMessageInlineVec(s.ctx, fh, &cur, got); err != nil {
			if shmDebugEnabled {
				shmDebugf("[ERROR] ShmClientTransport.write: chunk write failed at off=%d: %v", off, err)
			}
			return err
		}
		off = end
		if isLast {
			if shmDebugEnabled {
				shmDebugf("[DEBUG] ShmClientTransport.write: chunked write complete (%d bytes)", payloadLen)
			}
			return nil
		}
		got, err = t.acquireUpToSendQuota(s.ctx, s.id, payloadLen-off)
		if err != nil {
			if shmDebugEnabled {
				shmDebugf("[DEBUG] ShmClientTransport.write: acquireUpToSendQuota failed mid-chunk: %v", err)
			}
			return err
		}
	}
}

// sendPing sends a PING frame with 8-byte opaque data.
func (t *ShmClientTransport) sendPing() error {
	// Check if transport is closed before attempting to write.
	if t.closed.Load() {
		return ErrConnClosing
	}
	var data [8]byte
	// Use current time nanos as opaque payload (not strictly required, just convenient).
	binary.LittleEndian.PutUint64(data[:], uint64(time.Now().UnixNano()))
	return t.frameWriter.enqueueAndWait(frameEntry{
		ctx:     t.ctx,
		fh:      FrameHeader{Type: FrameTypePING},
		payload: data[:],
	})
}

// keepalive monitors connection health and sends periodic PING frames.
// It follows the gRPC keepalive semantics:
// - Send PING after kp.Time of inactivity.
// - Close connection if no PONG within kp.Timeout.
// - Go dormant if no active streams and !PermitWithoutStream.
func (t *ShmClientTransport) keepalive() {
	var err error
	defer func() {
		close(t.keepaliveDone)
		if err != nil {
			t.Close(err)
		}
	}()

	// True iff a ping has been sent, and no data has been received since then.
	outstandingPing := false
	// Amount of time remaining before which we should receive an ACK for the
	// last sent ping.
	timeoutLeft := time.Duration(0)
	// Records the last value of t.lastRead before we go block on the timer.
	prevNano := time.Now().UnixNano()
	timer := time.NewTimer(t.kp.Time)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			lastRead := atomic.LoadInt64(&t.lastRead)
			if lastRead > prevNano {
				// There has been read activity since the last time we were here.
				outstandingPing = false
				// Next timer should fire at kp.Time seconds from lastRead time.
				timer.Reset(time.Duration(lastRead) + t.kp.Time - time.Duration(time.Now().UnixNano()))
				prevNano = lastRead
				continue
			}
			if outstandingPing && timeoutLeft <= 0 {
				err = connectionErrorf(true, nil, "keepalive ping failed to receive ACK within timeout")
				return
			}
			t.mu.Lock()
			if t.closed.Load() {
				// Transport is closing; exit.
				t.mu.Unlock()
				return
			}
			if len(t.streams) < 1 && !t.kp.PermitWithoutStream {
				// If a ping was sent out previously (because there were active
				// streams at that point) which wasn't acked and its timeout
				// hadn't fired, but we got here and are about to go dormant,
				// we should make sure that we unconditionally send a ping once
				// we awaken.
				outstandingPing = false
				t.kpDormant = true
				t.kpDormancyCond.Wait()
			}
			t.kpDormant = false
			t.mu.Unlock()

			// We get here either because we were dormant and a new stream was
			// created which unblocked the Wait() call, or because the
			// keepalive timer expired. In both cases, we need to send a ping.
			if !outstandingPing {
				if pingErr := t.sendPing(); pingErr != nil {
					// Failed to send ping; connection may be broken.
					err = connectionErrorf(true, pingErr, "keepalive failed to send ping")
					return
				}
				timeoutLeft = t.kp.Timeout
				outstandingPing = true
			}
			// The amount of time to sleep here is the minimum of kp.Time and
			// timeoutLeft. This will ensure that we wait only for kp.Time
			// before sending out the next ping (for cases where the ping is
			// acked).
			sleepDuration := min(t.kp.Time, timeoutLeft)
			timeoutLeft -= sleepDuration
			timer.Reset(sleepDuration)
		case <-t.ctx.Done():
			// Transport is shutting down.
			return
		}
	}
}

// Compile-time check to ensure ShmClientTransport implements clientTransport.
var _ clientTransport = (*ShmClientTransport)(nil)
