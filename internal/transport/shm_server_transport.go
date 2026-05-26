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
	"container/list"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/internal/grpcutil"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// serverStreamCache holds a cached stream pointer and its ID for lock-free
// lookup in the frame dispatch hot path. Loaded/stored atomically.
type serverStreamCache struct {
	stream   *ServerStream
	streamID uint32
}

// ShmServerTransport implements the gRPC ServerTransport interface
// for shared memory communication.
type ShmServerTransport struct {
	// Core state
	segment        *Segment // The shared memory segment
	serverToClient *ShmRing // Ring for server->client data
	clientToServer *ShmRing // Ring for client->server data

	// Windows event handles for cross-mapping synchronization
	readEvents  *RingEvents
	writeEvents *RingEvents

	// Connection state
	localAddr  net.Addr
	remoteAddr net.Addr
	peer       *peer.Peer

	// Lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	closed atomic.Bool
	// draining indicates the transport has entered a graceful shutdown mode.
	// When draining, new streams are rejected but existing ones may complete.
	draining atomic.Bool
	// drainDebugData is used for error reporting when draining completes.
	drainDebugData string
	// frameWriter serializes writes to the server->client ring via a dedicated
	// goroutine, eliminating lock contention from multiple stream handlers.
	frameWriter *shmFrameWriter
	mu          sync.RWMutex

	// Stream management
	streams    map[uint32]*ServerStream
	handleFunc func(*ServerStream)
	maxStreams uint32

	// cachedStream caches the only active stream pointer for single-stream
	// connections, allowing frame dispatch to skip the map lookup + RLock.
	// Reset to nil when stream count changes (0 or >1).
	//
	// Loaded atomically without t.mu in the frame dispatch hot path.
	// Stored atomically under t.mu in updateStreamCache.
	cachedStream atomic.Pointer[serverStreamCache]

	// singleStreamMode is negotiated via the CONNECT frame. When true,
	// both sides agree to use single-stream optimizations (inline writes,
	// writer loop bypass via inlineMu.TryLock, cachedStream fast path).
	// Automatically disabled when more than one stream is active.
	singleStreamMode bool

	// Flow control
	//
	// connSendQuota and per-stream Stream.sendQuota are atomic.Int64;
	// the acquireSendQuota hot path reserves via two-resource CAS
	// with rollback on conn-CAS failure. sendQuotaMu now guards
	// only the connWaiters FIFO + streamQuotaSignals map (cold
	// register/dispatch path). See ShmClientTransport for the full
	// design rationale.
	sendQuotaMu   sync.Mutex
	connSendQuota atomic.Int64
	// connQuotaSignal is a buffered channel used to BROADCAST conn-level
	// WU credits. notifyQuotaChangeLocked(0) pushes `quotaWaiters` tokens
	// at once so every parked sender wakes in parallel and races for the
	// mutex, avoiding chain-wake serialization through a single slot.
	connQuotaSignal chan struct{}
	// streamQuotaSignals: per-stream wakeup channel (buffer=1) allocated
	// in NewStream-equivalent path and released in closeStream. Stream
	// WU credits a SPECIFIC stream; this guarantees the wakeup reaches
	// the credited stream's writer.
	streamQuotaSignals map[uint32]chan struct{}

	// connWaiters is the FIFO list of streams parked waiting for
	// connection-level send-quota. Mirror of ShmClientTransport's
	// connWaiters; see that field's comment for the SC-Aware Quota
	// Dispatch design. Protected by sendQuotaMu.
	connWaiters *list.List

	connInFlow   trInFlow
	streamInFlow map[uint32]*inFlow

	// BDP estimation and dynamic flow control (RFC A73 Phase 5)
	bdpEst            *shmBDPEstimator
	initialWindowSize int32

	// wuThreshold is the per-transport WindowUpdate emission threshold,
	// captured at construction from shmWindowUpdateThreshold and
	// recomputed whenever t.initialWindowSize changes (BDP estimator).
	// Mirrors the client-side wuThreshold; see ShmClientTransport for
	// the deadlock this avoids when the package-global threshold
	// disagrees with a transport's effective window. Atomic so the
	// WU emit hot path can read without taking sendQuotaMu.
	wuThreshold atomic.Uint32

	// pendingConnWU is the connection-level WindowUpdate accumulator.
	// Atomic so producers (reader callbacks / BDP) Add without
	// contending sendQuotaMu. Per-stream equivalent lives on
	// Stream.pendingWU. See ShmClientTransport for the full design
	// rationale ("WU Lockless Path").
	pendingConnWU atomic.Uint32

	// Error handling
	closeOnce sync.Once
	errCh     chan struct{}
	done      chan struct{} // closed when transport is shutting down

	readerWG sync.WaitGroup

	// Keepalive
	lastRead      int64 // Unix nanos; updated atomically on each received frame
	kp            keepalive.ServerParameters
	kep           keepalive.EnforcementPolicy
	keepaliveDone chan struct{} // closed when keepalive goroutine exits
	// idle is the time when the connection became idle (no active streams).
	// Zero if the connection is not idle.
	idle time.Time
	// lastPingAt is the timestamp of the last PING received, for enforcement.
	lastPingAt time.Time
	// pingStrikes counts policy violations; too many triggers close.
	pingStrikes uint8
}

// updateStreamCache updates the single-stream cache. Must be called with
// t.mu held (Lock or RLock). When exactly one stream is active, cache it
// for lock-free lookup in the frame dispatch hot path.
func (t *ShmServerTransport) updateStreamCache() {
	if len(t.streams) == 1 {
		for id, s := range t.streams {
			t.cachedStream.Store(&serverStreamCache{stream: s, streamID: id})
			break
		}
	} else {
		t.cachedStream.Store(nil)
	}
}

// ApplyServerConfig propagates grpc-level ServerConfig settings into the
// SHM transport's flow-control / lifecycle state machine. It is called by
// transport.NewServerTransport in the ServerTransportProvider fast path
// AFTER the transport has been constructed (by ShmListener.Accept) but
// BEFORE any streams have been accepted. No stream is yet registered at
// this point so direct field writes are race-free.
//
// Why this hook exists: ShmListener.Accept constructs the transport with
// no access to grpc.ServerOption values, so prior to this method the SHM
// server transport always used the package-default shmInitialWindowSize
// (32 MiB) regardless of grpc.InitialWindowSize, silently violating
// HTTP/2 flow-control symmetry and producing a real deadlock when a
// client dialed grpc.WithInitialWindowSize(65535) while the server
// retained the 32 MiB receive window + 8 MiB WindowUpdate threshold:
// the small client window would be exhausted in one 64 KiB write while
// the server accumulated drip credit far below threshold and never
// emitted a WINDOW_UPDATE.
//
// Mutated fields:
//   - initialWindowSize: applied if InitialWindowSize >= defaultWindowSize
//     (matching the stock http2_server.go gate). Drives per-stream
//     inFlow.limit at stream creation and the WindowUpdate emission
//     threshold via wuThreshold below.
//   - wuThreshold: recomputed from the new initialWindowSize so the
//     receiver-side WU emission threshold tracks the effective stream
//     window.
//   - connInFlow.limit: applied only if InitialConnWindowSize > 0 (opt-in
//     clamp). Default behaviour leaves conn limit at maxWindowSize so SHM
//     transports retain the high-throughput "ring is the conn-level
//     back-pressure" property in production.
//   - maxStreams: applied if config.MaxStreams > 0.
//
// Safe to call only before HandleStreams begins serving; ApplyServerConfig
// must not race with any stream-creation path.
func (t *ShmServerTransport) ApplyServerConfig(config *ServerConfig) {
	if config == nil {
		return
	}
	if config.InitialWindowSize >= defaultWindowSize {
		t.initialWindowSize = config.InitialWindowSize
		t.wuThreshold.Store(computeWUThreshold(config.InitialWindowSize))
	}
	if config.InitialConnWindowSize > 0 {
		t.sendQuotaMu.Lock()
		t.connInFlow = trInFlow{limit: uint32(config.InitialConnWindowSize)}
		t.connInFlow.updateEffectiveWindowSize()
		// Keep connSendQuota at its construction default (maxWindowSize).
		// The conn-level receive window (connInFlow.limit) is what governs
		// when the SERVER emits conn-level WindowUpdate frames; the
		// connSendQuota governs how much the server can emit before being
		// blocked by client-issued conn WindowUpdate. Those are
		// independent in our current model; clamping connSendQuota would
		// affect server-to-client direction only, which is not what
		// InitialConnWindowSize implies on the receive side.
		t.sendQuotaMu.Unlock()
	}
	if config.MaxStreams > 0 {
		// maxStreams gates new-stream admission via rejectNewStream; pre-
		// stream mutation is safe here.
		t.maxStreams = config.MaxStreams
	}
}

func (t *ShmServerTransport) sendGoAway(flags uint8, debugData string) {
	if t.closed.Load() || t.serverToClient == nil {
		return
	}
	// Use a background context; the frame writer will handle the write
	// lifetime. We cannot use WithTimeout+defer cancel here because the
	// context would be canceled before the writer goroutine processes it.
	_ = t.frameWriter.enqueue(frameEntry{
		ctx:     context.Background(),
		fh:      FrameHeader{Type: FrameTypeGOAWAY, Flags: flags},
		payload: []byte(debugData),
	})
}

// notifyQuotaChangeLocked wakes waiters that may benefit from the
// just-credited quota. Caller MUST hold sendQuotaMu.
//
// connSendQuota / s.sendQuota are atomic.Int64; the lock here
// guards only the connWaiters FIFO and the per-stream signal-channel
// map.
//
// streamID == 0 (connection-level credit) — SC-Aware Dispatch:
// Walk the connWaiters FIFO; for each waiter w, signal its per-stream
// channel only when atomic conn AND stream quotas both meet w.wanted.
// Stop at the first unsatisfiable head waiter (strict FIFO fairness).
//
// streamID != 0 (stream-level credit): signal only that stream's
// per-stream channel. See ShmClientTransport for full rationale.
func (t *ShmServerTransport) notifyQuotaChangeLocked(streamID uint32) {
	if streamID == 0 {
		connQ := t.connSendQuota.Load()
		for e := t.connWaiters.Front(); e != nil; {
			w := e.Value.(*connWaiter)
			s, sok := t.streams[w.streamID]
			if !sok {
				next := e.Next()
				t.connWaiters.Remove(e)
				e = next
				continue
			}
			streamQ := s.sendQuota.Load()
			if connQ >= w.wanted && streamQ >= w.wanted {
				next := e.Next()
				t.connWaiters.Remove(e)
				if s.connWaiterElem == e {
					s.connWaiterElem = nil
				}
				select {
				case w.signal <- struct{}{}:
				default:
				}
				e = next
				continue
			}
			break
		}
		return
	}
	if ch, ok := t.streamQuotaSignals[streamID]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// unregisterConnWaiterLocked removes the given stream from connWaiters
// if registered. Caller MUST hold sendQuotaMu. Idempotent.
func (t *ShmServerTransport) unregisterConnWaiterLocked(s *ServerStream) {
	if s == nil || s.connWaiterElem == nil {
		return
	}
	t.connWaiters.Remove(s.connWaiterElem)
	s.connWaiterElem = nil
}

// registerConnWaiterLocked adds (or updates wanted on) the given
// stream's conn-quota waiter entry. Caller MUST hold sendQuotaMu.
func (t *ShmServerTransport) registerConnWaiterLocked(s *ServerStream, wanted int64) {
	if s.connWaiterElem != nil {
		w := s.connWaiterElem.Value.(*connWaiter)
		w.wanted = wanted
		return
	}
	w := &connWaiter{
		streamID: s.id,
		wanted:   wanted,
		signal:   t.streamQuotaSignals[s.id],
	}
	s.connWaiterElem = t.connWaiters.PushBack(w)
}

// addSendQuota credits outbound send-quota and wakes parked senders.
// Atomic credit; always takes sendQuotaMu for dispatch to close the
// lost-wakeup race against acquireSendQuota's slow path — see
// ShmClientTransport.addSendQuota for the full rationale.
func (t *ShmServerTransport) addSendQuota(streamID uint32, delta uint32) {
	if delta == 0 {
		return
	}
	if streamID == 0 {
		t.connSendQuota.Add(int64(delta))
	} else {
		if s := t.lookupStream(streamID); s != nil {
			s.sendQuota.Add(int64(delta))
		} else {
			return
		}
	}
	// Wake WL to revisit deferred whole-message entries. Mirror of
	// client addSendQuota.
	select {
	case t.frameWriter.wuRetryWake <- struct{}{}:
	default:
	}
	// Stream-quota dispatch through the connWaiters FIFO. The
	// whole-MESSAGE write path (frameWriter.enqueueMessageAndWait)
	// does NOT register on this FIFO; the zero-copy single-frame
	// proto fast path (writeProto / writeProtoToRing) still uses
	// acquireSendQuota whose slow path parks on
	// streamQuotaSignals, so this dispatch is required whenever a
	// sender is parked there. Near-noop when the FIFO is empty.
	t.sendQuotaMu.Lock()
	t.notifyQuotaChangeLocked(streamID)
	t.sendQuotaMu.Unlock()
}

func (t *ShmServerTransport) acquireSendQuota(ctx context.Context, streamID uint32, n int) error {
	if n == 0 {
		return nil
	}
	s := t.lookupStream(streamID)
	if s == nil {
		return errStreamDone
	}
	want := int64(n)
	for {
		if t.closed.Load() {
			return ErrConnClosing
		}
		// Fast path: lock-free two-resource CAS reservation.
		if tryReserveSendQuota(&t.connSendQuota, &s.sendQuota, want) {
			return nil
		}
		// Slow path: take waiter-list lock, re-check, register or retry.
		t.sendQuotaMu.Lock()
		if t.closed.Load() {
			t.sendQuotaMu.Unlock()
			return ErrConnClosing
		}
		if s.sendQuota.Load() >= want && t.connSendQuota.Load() >= want {
			t.sendQuotaMu.Unlock()
			continue
		}
		t.registerConnWaiterLocked(s, want)
		streamCh := t.streamQuotaSignals[streamID]
		t.sendQuotaMu.Unlock()
		select {
		case <-streamCh:
		case <-ctx.Done():
			t.sendQuotaMu.Lock()
			t.unregisterConnWaiterLocked(s)
			t.sendQuotaMu.Unlock()
			return ContextErr(ctx.Err())
		case <-t.ctx.Done():
			t.sendQuotaMu.Lock()
			t.unregisterConnWaiterLocked(s)
			t.sendQuotaMu.Unlock()
			return ErrConnClosing
		}
	}
}

// acquireUpToSendQuota was retired when chunked-DATA flow-control
// state ownership moved into the writer goroutine. The current
// path is shmFrameWriter.enqueueMessageAndWait → advanceDeferred
// under inlineMu. See client mirror for full rationale.

func (t *ShmServerTransport) sendWindowUpdate(streamID uint32, delta uint32) {
	if streamID == 0 {
		t.sendConnWindowUpdate(delta, false)
		return
	}
	s := t.lookupStream(streamID)
	if s == nil {
		return
	}
	t.sendStreamWindowUpdate(s, delta, false)
}

// sendWindowUpdateForce mirrors the client-side method: bypass the
// shmWindowUpdateThreshold drip batching for maybeAdjust-style
// pre-credit which MUST go out at parse time, otherwise the LPM
// cannot complete under small windows.
func (t *ShmServerTransport) sendWindowUpdateForce(streamID uint32, delta uint32) {
	if streamID == 0 {
		t.sendConnWindowUpdate(delta, true)
		return
	}
	s := t.lookupStream(streamID)
	if s == nil {
		return
	}
	t.sendStreamWindowUpdate(s, delta, true)
}

// sendConnWindowUpdate is the server-side lockless conn WU emission
// path. See ShmClientTransport.sendConnWindowUpdate for the full
// design rationale ("WU Lockless Path"). Briefly: Add+threshold
// check+Swap(0)+emit; Swap=0 means a concurrent producer carried
// our bytes. errFrameWriterFull restores credit and signals
// wuRetryWake so the writer loop's next iteration drains the
// pending atomic before processing the next batch.
func (t *ShmServerTransport) sendConnWindowUpdate(delta uint32, force bool) {
	if delta == 0 || t.closed.Load() {
		return
	}
	newVal := t.pendingConnWU.Add(delta)
	if !force && newVal < t.wuThreshold.Load() {
		return
	}
	v := t.pendingConnWU.Swap(0)
	if v == 0 {
		return
	}
	t.emitWindowUpdateFrame(0, v, nil)
}

// sendStreamWindowUpdate is the server-side lockless stream WU
// emission path. Mirror of the client version; per-stream pending
// accumulator lives on Stream.pendingWU.
func (t *ShmServerTransport) sendStreamWindowUpdate(s *ServerStream, delta uint32, force bool) {
	if delta == 0 || s == nil || t.closed.Load() {
		return
	}
	if s.getState() == streamDone {
		return
	}
	newVal := s.pendingWU.Add(delta)
	if !force && newVal < t.wuThreshold.Load() {
		return
	}
	v := s.pendingWU.Swap(0)
	if v == 0 {
		return
	}
	if s.getState() == streamDone {
		return
	}
	t.emitWindowUpdateFrame(s.id, v, s)
}

// emitWindowUpdateFrame writes a WINDOW_UPDATE frame via the non-
// blocking writer path. On errFrameWriterFull the captured value is
// restored to the appropriate pending accumulator and wuRetryWake is
// signalled so the writer loop drains pending atomics on its next
// tick. Pass nil for `s` on conn-level (streamID=0) frames.
func (t *ShmServerTransport) emitWindowUpdateFrame(streamID uint32, v uint32, s *ServerStream) {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, v)
	err := t.frameWriter.enqueueOrInlineNonBlocking(frameEntry{
		ctx:     context.Background(),
		fh:      FrameHeader{Type: FrameTypeWindowUpdate, StreamID: streamID},
		payload: buf,
	})
	if err == errFrameWriterFull {
		shmWUFramesBackpressured.Add(1)
		if streamID == 0 {
			t.pendingConnWU.Add(v)
		} else if s != nil && s.getState() != streamDone {
			s.pendingWU.Add(v)
		}
		select {
		case t.frameWriter.wuRetryWake <- struct{}{}:
		default:
		}
	}
}

// drainPendingWUForWriter is the writer-loop callback registered via
// frameWriter.setDrainPendingWUFn. See client-side
// drainPendingWUForWriter for the full contract; this is the
// server-side mirror.
func (t *ShmServerTransport) drainPendingWUForWriter() {
	if t.closed.Load() {
		return
	}
	if v := t.pendingConnWU.Swap(0); v > 0 {
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, v)
		_ = writeFrame(context.Background(), t.frameWriter.tx,
			FrameHeader{Type: FrameTypeWindowUpdate, StreamID: 0}, buf)
	}
	t.mu.RLock()
	if len(t.streams) == 0 {
		t.mu.RUnlock()
		return
	}
	snapshot := make([]*ServerStream, 0, len(t.streams))
	for _, s := range t.streams {
		snapshot = append(snapshot, s)
	}
	t.mu.RUnlock()
	for _, s := range snapshot {
		if s.getState() == streamDone {
			s.pendingWU.Store(0)
			continue
		}
		if v := s.pendingWU.Swap(0); v > 0 {
			buf := make([]byte, 4)
			binary.BigEndian.PutUint32(buf, v)
			_ = writeFrame(context.Background(), t.frameWriter.tx,
				FrameHeader{Type: FrameTypeWindowUpdate, StreamID: s.id}, buf)
		}
	}
}

// piggybackWUForWriter is the per-chunk piggyback callback for the
// server transport. See client-side piggybackWUForWriter for the
// full contract; this is the server-side mirror.
//
// LOCK ORDERING: runs WITH inlineMu held; may acquire t.mu.RLock via
// lookupStream slow path. The reverse order (t.mu.Lock → inlineMu)
// is never used in the codebase — verified safe.
func (t *ShmServerTransport) piggybackWUForWriter(streamID uint32) {
	if t.closed.Load() {
		return
	}
	if v := t.pendingConnWU.Swap(0); v > 0 {
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, v)
		_ = writeFrame(context.Background(), t.frameWriter.tx,
			FrameHeader{Type: FrameTypeWindowUpdate, StreamID: 0}, buf)
	}
	if streamID == 0 {
		return
	}
	s := t.lookupStream(streamID)
	if s == nil || s.getState() == streamDone {
		return
	}
	if v := s.pendingWU.Swap(0); v > 0 {
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, v)
		_ = writeFrame(context.Background(), t.frameWriter.tx,
			FrameHeader{Type: FrameTypeWindowUpdate, StreamID: streamID}, buf)
	}
}

// updateFlowControl updates the incoming flow control windows for the
// transport and all active streams based on the current BDP estimation.
func (t *ShmServerTransport) updateFlowControl(n uint32) {
	t.mu.Lock()
	t.initialWindowSize = int32(n)
	t.mu.Unlock()
	t.wuThreshold.Store(computeWUThreshold(int32(n)))
	t.mu.Lock()
	for _, s := range t.streams {
		s.fc.newLimit(n)
	}
	t.mu.Unlock()

	// Send connection-level window update
	if wu := t.connInFlow.newLimit(n); wu > 0 {
		t.sendWindowUpdate(0, wu)
	}
}

// sendBDPPing sends a BDP estimation ping to the client.
func (t *ShmServerTransport) sendBDPPing() {
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

func (t *ShmServerTransport) rejectNewStream(streamID uint32, msg string) {
	if t.closed.Load() || t.serverToClient == nil {
		return
	}
	// Best-effort send GOAWAY as a signal to stop creating new streams.
	t.sendGoAway(GoAwayFlagDRAINING, msg)

	trailers := encodeTrailers(TrailersV1{Version: 1, GRPCStatusCode: uint32(codes.Unavailable), GRPCStatusMsg: msg})
	fh := FrameHeader{Type: FrameTypeTRAILERS, StreamID: streamID, Length: uint32(len(trailers))}

	_ = t.frameWriter.enqueue(frameEntry{
		ctx:     context.Background(),
		fh:      fh,
		payload: trailers,
	})
}

// NewShmServerTransport creates a new shared memory server transport.
func NewShmServerTransport(segment *Segment, localAddr, remoteAddr net.Addr) (*ShmServerTransport, error) {
	if segment == nil {
		return nil, errors.New("segment cannot be nil")
	}

	// Extract segment name for event naming
	segmentName := extractSegmentName(segment.Path)

	// Create rings for bidirectional communication
	// Ring A: client->server, Ring B: server->client
	clientToServer := NewShmRingFromSegment(segment.A, segment.Mem)
	serverToClient := NewShmRingFromSegment(segment.B, segment.Mem)

	segment.RegisterRing(clientToServer)
	segment.RegisterRing(serverToClient)

	// Create events for cross-mapping synchronization (Windows).
	// Server creates events. On Linux, these are no-ops returning nil events.
	readEvents, _ := CreateRingEvents(segmentName, "A")
	writeEvents, _ := CreateRingEvents(segmentName, "B")

	// Attach events to rings
	clientToServer.SetEvents(readEvents)
	serverToClient.SetEvents(writeEvents)

	ctx, cancel := context.WithCancel(context.Background())

	t := &ShmServerTransport{
		segment:        segment,
		serverToClient: serverToClient,
		clientToServer: clientToServer,
		readEvents:     readEvents,
		writeEvents:    writeEvents,
		localAddr:      localAddr,
		remoteAddr:     remoteAddr,
		peer: &peer.Peer{
			Addr:      remoteAddr,
			LocalAddr: localAddr,
			AuthInfo:  nil, // No auth for shared memory
		},
		ctx:                ctx,
		cancel:             cancel,
		streams:            make(map[uint32]*ServerStream),
		streamInFlow:       make(map[uint32]*inFlow),
		errCh:              make(chan struct{}),
		connQuotaSignal:    make(chan struct{}, 1024),
		connWaiters:        list.New(),
		streamQuotaSignals: make(map[uint32]chan struct{}),
		done:               make(chan struct{}),
		keepaliveDone:      make(chan struct{}),
	}
	// Start the dedicated frame writer goroutine for the server→client ring.
	t.frameWriter = newShmFrameWriter(serverToClient)
	// Register the lockless WU drain callback. See client-side
	// drainPendingWUForWriter for the contract. Must be installed
	// BEFORE any reader/sender goroutine can call sendWindowUpdate
	// and observe an errFrameWriterFull restore; constructor order
	// here is safe because HandleStreams (the trigger for inbound
	// frame parsing) has not yet been called.
	t.frameWriter.setDrainPendingWUFn(t.drainPendingWUForWriter)
	// Register the per-chunk piggyback callback. See client-side
	// piggybackWUForWriter for the contract.
	t.frameWriter.setPiggybackWUFn(t.piggybackWUForWriter)

	// Publish conn-quota pointer to WL for CAS-reserve in
	// processWholeMessage / retryDeferred (mirror of client).
	t.frameWriter.setConnQuotaPtr(&t.connSendQuota)

	// Initialize flow control windows.
	t.connSendQuota.Store(int64(maxWindowSize))
	t.connInFlow = trInFlow{limit: uint32(maxWindowSize)}
	t.connInFlow.updateEffectiveWindowSize()

	// Initialize BDP estimation for dynamic flow control (RFC A73 Phase 5).
	// SHM uses a much larger initial window (32MB) than HTTP/2 (64KB) because
	// local memory has near-zero RTT and high bandwidth.
	t.initialWindowSize = int32(shmInitialWindowSize)
	// Per-transport WU emission threshold. See computeWUThreshold for the
	// deadlock-correctness note. Atomic for lockless WU emit path.
	t.wuThreshold.Store(computeWUThreshold(t.initialWindowSize))
	t.bdpEst = newShmBDPEstimator(uint32(shmInitialWindowSize), t.updateFlowControl)

	max := segment.H.MaxStreams()
	if max == 0 {
		max = uint32(math.MaxUint32)
	}
	t.maxStreams = max

	return t, nil
}

// HandleStreams receives incoming streams using the given handler.
// This is typically run in a separate goroutine.
func (t *ShmServerTransport) HandleStreams(ctx context.Context, handle func(*ServerStream)) {
	t.mu.Lock()
	if t.closed.Load() {
		t.mu.Unlock()
		return
	}
	t.handleFunc = handle
	// Add to WaitGroup while holding lock to prevent race with Close().
	// Close() checks closed flag and then calls readerWG.Wait(), so we must
	// ensure Add(1) happens before closed is set to true.
	t.readerWG.Add(1)
	t.mu.Unlock()

	// The reader and all stream contexts should be canceled when either the
	// HandleStreams context is canceled or the transport is closed.
	procCtx, procCancel := context.WithCancel(ctx)
	defer procCancel()
	go func() {
		select {
		case <-t.ctx.Done():
			procCancel()
		case <-procCtx.Done():
		}
	}()

	// Start processing incoming data from the client
	go func() {
		defer t.readerWG.Done()
		t.processIncomingData(procCtx)
	}()

	// Wait for context cancellation or transport closure
	select {
	case <-ctx.Done():
		t.Close(ctx.Err())
	case <-t.errCh:
		// Transport was closed
	}
}

// processIncomingData reads data from the client->server ring and processes gRPC frames
func (t *ShmServerTransport) processIncomingData(ctx context.Context) {
	if shmDebugEnabled {
		shmDebugf("[DEBUG] ShmServerTransport.processIncomingData: STARTED, ring=%p", t.clientToServer)
	}
	// Install the per-DATA-frame flow-control callback on the H2
	// decoder for the client→server ring. The codec invokes this
	// every time it consumes an H2 DATA frame body, BEFORE the bytes
	// are handed to the per-stream lpmAccumulator. Crediting at this
	// point — rather than waiting until handleMessage assembles a
	// complete LPM — decouples HTTP/2 connection flow control from
	// gRPC message reassembly and lets a producer with a small
	// per-stream send window drain a multi-DATA-frame LPM without
	// deadlocking on a WindowUpdate that never arrives.
	t.clientToServer.h2Decoder().onDataFrame = t.onDataFrameReceived
	// onMessageStart drives receiver-side pre-credit for multi-DATA-
	// frame LPMs. See the matching client-side comment for rationale.
	t.clientToServer.h2Decoder().onMessageStart = t.onMessageStart
	defer func() {
		if shmDebugEnabled {
			shmDebugf("[DEBUG] ShmServerTransport.processIncomingData: EXITING")
		}
		if !t.closed.Load() {
			go t.Close(errors.New("incoming data processing ended"))
		}
	}()

	// messageBurst counts MESSAGE frames delivered without a cooperative
	// yield. Local to this goroutine — no synchronisation needed and no
	// risk of races with handlers invoked from elsewhere (e.g. tests).
	// Reset to 0 after every yield. See the FrameTypeMESSAGE case below
	// for the burst-cap rationale.
	var messageBurst int

	for {
		if t.closed.Load() {
			if shmDebugEnabled {
				shmDebugf("[DEBUG] ShmServerTransport.processIncomingData: transport closed, exiting")
			}
			return
		}
		if shmDebugEnabled {
			shmDebugf("[DEBUG] ShmServerTransport.processIncomingData: waiting for frame from client... widx=%d, ridx=%d", t.clientToServer.header().WriteIndex(), t.clientToServer.header().ReadIndex())
		}
		fh, payloadBuf, err := readFrameView(ctx, t.clientToServer)
		if err != nil {
			if shmDebugEnabled {
				shmDebugf("[DEBUG] ShmServerTransport.processIncomingData: readFrameView error: %v", err)
			}
			if errors.Is(err, ErrRingClosed) || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || t.closed.Load() {
				return
			}
			continue
		}
		if shmDebugEnabled {
			shmDebugf("[DEBUG] ShmServerTransport.processIncomingData: received frame type=%d, streamID=%d, length=%d", fh.Type, fh.StreamID, fh.Length)
		}

		// Update last read timestamp for keepalive tracking.
		atomic.StoreInt64(&t.lastRead, time.Now().UnixNano())

		payloadTransferred := false
		release := func() {
			if shmDebugEnabled {
				shmDebugf("[DEBUG] ShmServerTransport.processIncomingData: release() called, payloadTransferred=%v, payloadBuf=%v", payloadTransferred, payloadBuf)
			}
			if !payloadTransferred && payloadBuf != nil {
				payloadBuf.Free()
				payloadBuf = nil
			}
		}

		var payload []byte
		if payloadBuf != nil {
			payload = payloadBuf.ReadOnlyData()
		}

		// Dispatch frames based on type
		switch fh.Type {
		case FrameTypeHEADERS:
			if err := t.handleHeaders(ctx, fh.StreamID, payload); err != nil {
				// Log error but continue processing
				release()
				continue
			}
			release()
		case FrameTypeGOAWAY:
			// Client is draining or closing the connection.
			// Enter draining mode so we stop accepting new streams, and close once
			// all active streams complete.
			if fh.Flags&GoAwayFlagIMMEDIATE != 0 {
				dbg := string(payload)
				if dbg == "" {
					dbg = "received GOAWAY (immediate)"
				}
				release()
				go t.Close(errors.New(dbg))
				return
			}

			// Record draining state once.
			if t.draining.CompareAndSwap(false, true) {
				t.mu.Lock()
				t.drainDebugData = string(payload)
				if t.drainDebugData == "" {
					t.drainDebugData = "received GOAWAY (draining)"
				}
				active := len(t.streams)
				t.mu.Unlock()

				if active == 0 {
					release()
					go t.Close(errors.New("transport drained: received GOAWAY (draining)"))
					return
				}
			}
			release()
			continue
		case FrameTypeMESSAGE:
			// Transfer ownership to the stream to avoid copying when possible.
			var sz uint32
			var delivered bool
			if payloadBuf != nil {
				payloadTransferred = true
				sz, delivered = t.handleMessageBuffer(fh.StreamID, fh.Flags, payloadBuf)
				payloadBuf = nil
			} else {
				sz, delivered = t.handleMessage(fh.StreamID, fh.Flags, payload)
			}
			// Co-locate the handler G on this M (see ShmClientTransport
			// processIncomingData for the wakep-avoidance rationale).
			//
			// Gated on (1) the ring being drained, (2) a bounded burst
			// limit, and (3) payload size. At N=1000+ streams with tiny
			// payloads the reader's ring keeps producing fresh frames
			// from many different streams; yielding 1000 times per RPC
			// round forces the scheduler through 1000 park/unpark
			// cycles. Keep draining for small payloads. For medium /
			// large payloads the parallel app goroutine work outweighs
			// the wakep cost, so always yield.
			if delivered {
				messageBurst++
				if sz > shmYieldSkipMaxPayload ||
					messageBurst >= shmServerMaxMessageBurst ||
					!t.clientToServer.HasPendingData() {
					runtime.Gosched()
					messageBurst = 0
				}
			}
		case FrameTypeHALFCLOSE:
			// Client signalled it is done sending. The H2 codec emits this
			// after an initial HEADERS frame whose source H2 frame
			// carried END_STREAM (zero-message client-streaming
			// request). Without this case, such a stream would hang
			// waiting for a MESSAGE that never arrives.
			t.handleHalfClose(fh.StreamID)
			release()
		case FrameTypeTRAILERS:
			t.handleTrailers(fh.StreamID, payload)
			release()
		case FrameTypeCANCEL:
			t.handleCancel(fh.StreamID)
			release()
		case FrameTypePING:
			// Handle PING with enforcement policy.
			t.handlePing(ctx, payload)
			release()
		case FrameTypeWindowUpdate:
			if len(payload) >= 4 {
				// RFC 7540 §6.9.1: increment is big-endian. Senders
				// (sendWindowUpdate above) write BigEndian so this matches.
				delta := binary.BigEndian.Uint32(payload[:4])
				t.addSendQuota(fh.StreamID, delta)
			}
			release()
			continue
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
		default:
			// Unknown frame type, ignore
			release()
		}
	}
}

// handleHeaders processes a HEADERS frame and creates a new ServerStream
func (t *ShmServerTransport) handleHeaders(ctx context.Context, streamID uint32, payload []byte) error {
	// Fast-path checks under lock.
	t.mu.Lock()
	if t.closed.Load() {
		t.mu.Unlock()
		return errors.New("transport closed")
	}
	if t.draining.Load() {
		msg := "transport is draining"
		t.mu.Unlock()
		t.rejectNewStream(streamID, msg)
		return nil
	}
	if uint32(len(t.streams)) >= t.maxStreams {
		t.mu.Unlock()
		t.rejectNewStream(streamID, "max concurrent streams exceeded")
		return nil
	}
	// Check if stream already exists.
	if _, exists := t.streams[streamID]; exists {
		t.mu.Unlock()
		return errors.New("stream already exists")
	}
	// Validate stream ID (client uses odd numbers).
	if streamID%2 != 1 {
		t.mu.Unlock()
		return errors.New("invalid stream ID: must be odd for client-initiated streams")
	}
	t.mu.Unlock()

	// Decode headers using the proper frame format.
	hdr, err := decodeHeaders(payload)
	if err != nil {
		return err
	}

	// Convert KV metadata to metadata.MD
	md := make(metadata.MD)
	for _, kv := range hdr.Metadata {
		var vals []string
		for _, v := range kv.Values {
			vals = append(vals, string(v))
		}
		md[kv.Key] = vals
	}

	// Create the ServerStream
	s := &ServerStream{
		Stream: Stream{
			id:             streamID,
			method:         hdr.Method,
			sendCompress:   "",
			recvCompress:   "",
			contentSubtype: "",
		},
		st: t,
	}
	s.Stream.buf.init()
	// Per-stream inFlow limit: use the configured initial window so
	// maybeAdjust / onRead thresholds are sized realistically.
	ws := t.initialWindowSize
	if ws <= 0 {
		ws = int32(shmInitialWindowSize)
	}
	s.fc = inFlow{limit: uint32(ws)}

	// Create context for the stream. If the client provided an RPC deadline,
	// honor it by creating a context with that deadline.
	if hdr.DeadlineUnixNano != 0 {
		const maxInt64AsUint64 = uint64(^uint64(0) >> 1)
		if hdr.DeadlineUnixNano > maxInt64AsUint64 {
			return errors.New("invalid deadline: too large")
		}
		deadline := time.Unix(0, int64(hdr.DeadlineUnixNano))
		s.ctx, s.cancel = context.WithDeadline(ctx, deadline)
	} else {
		s.ctx, s.cancel = context.WithCancel(ctx)
	}
	s.ctxDone = s.ctx.Done()

	// Attach metadata to context if present
	if len(md) > 0 {
		s.ctx = metadata.NewIncomingContext(s.ctx, md)
	}
	// Populate stream fields derived from incoming headers.
	if v := md.Get("grpc-encoding"); len(v) > 0 {
		s.recvCompress = v[0]
	}
	if v := md.Get("grpc-accept-encoding"); len(v) > 0 {
		s.clientAdvertisedCompressors = v[0]
	}
	if v := md.Get("content-type"); len(v) > 0 {
		contentType := strings.ToLower(v[0])
		if contentSubtype, ok := grpcutil.ContentSubtype(contentType); ok {
			s.contentSubtype = contentSubtype
		} else {
			return errors.New("invalid gRPC request content-type")
		}
	}

	// Set up readRequester for the stream
	s.readRequester = s
	s.ctxDone = s.ctx.Done()

	// Create transport reader for the stream
	s.trReader = transportReader{
		reader: recvBufferReader{
			ctx:     s.ctx,
			ctxDone: s.ctxDone,
			recv:    &s.buf,
		},
		windowHandler: s,
	}

	// Register the stream (re-check draining/closed).
	t.mu.Lock()
	if t.closed.Load() {
		t.mu.Unlock()
		return errors.New("transport closed")
	}
	if t.draining.Load() {
		t.mu.Unlock()
		t.rejectNewStream(streamID, "transport is draining")
		return nil
	}
	t.streams[streamID] = s
	t.streamInFlow[streamID] = &s.fc
	// Update single-stream cache.
	if len(t.streams) == 1 {
		t.cachedStream.Store(&serverStreamCache{stream: s, streamID: streamID})
	} else {
		t.cachedStream.Store(nil)
	}
	// Clear idle time when we have active streams.
	t.idle = time.Time{}
	// Reset ping strikes when streams become active.
	t.pingStrikes = 0
	h := t.handleFunc
	t.mu.Unlock()

	// Initialize send quota for this stream (protected by sendQuotaMu, not mu).
	//
	// Clamp to t.initialWindowSize. The writer goroutine chunks
	// outbound bytes by per-grant CAS (advanceDeferred), so a MESSAGE
	// whose `5 + bodyLen` exceeds the current stream window still
	// progresses: each grant emits a partial chunk under MORE and the
	// remainder is deferred until WINDOW_UPDATE credit arrives. An
	// atomic reservation of the full `5 + bodyLen` would deadlock
	// when the window is smaller than that total (e.g. body == window).
	t.sendQuotaMu.Lock()
	// Per-stream send quota lives on the Stream as atomic.Int64
	// (s.sendQuota via embedded Stream).
	s.sendQuota.Store(int64(ws))
	// Per-stream wakeup channel: see the client-side mirror in
	// shm_client_transport.go for the design rationale.
	t.streamQuotaSignals[streamID] = make(chan struct{}, 1)
	t.sendQuotaMu.Unlock()

	// Call the handler in a new goroutine.
	if h != nil {
		go h(s)
	}

	return nil
}

// handleMessage processes a MESSAGE frame.
// For client->server, the final MESSAGE is indicated by MessageFlagMORE being unset.
// handleHalfClose surfaces a client half-close on streamID without
// any associated message payload. Used when the codec needs to
// signal "client done sending" out-of-band — currently only when an
// initial HEADERS frame carried H2's END_STREAM flag (zero-message
// client-streaming request, RFC 7540 §6.2 + gRFC G2). The normal
// path, where MORE=0 on the last MESSAGE drives EOF, is handled in
// handleMessage and is unaffected.
func (t *ShmServerTransport) handleHalfClose(streamID uint32) {
	var s *ServerStream
	if c := t.cachedStream.Load(); c != nil && c.streamID == streamID {
		s = c.stream
	} else {
		t.mu.RLock()
		var exists bool
		s, exists = t.streams[streamID]
		t.mu.RUnlock()
		if !exists {
			return
		}
	}
	s.write(recvMsg{err: io.EOF})
}

func (t *ShmServerTransport) handleMessage(streamID uint32, flags uint8, payload []byte) (uint32, bool) {
	// Fast path: if we have a cached single stream, skip map lookup + RLock.
	var s *ServerStream
	if c := t.cachedStream.Load(); c != nil && c.streamID == streamID {
		s = c.stream
	} else {
		t.mu.RLock()
		var exists bool
		s, exists = t.streams[streamID]
		t.mu.RUnlock()
		if !exists {
			return 0, false
		}
	}

	sz := uint32(len(payload))

	// BDP estimation: track bytes received and trigger BDP ping if needed
	var sendBDPPing bool
	if t.bdpEst != nil {
		sendBDPPing = t.bdpEst.add(sz)
	}

	// NOTE: connection + stream flow-control credit is NOT done here.
	// onDataFrameReceived (installed on the h2 decoder in
	// processIncomingData) credits per H2 DATA frame at receipt time,
	// which is the correct decoupling point for multi-DATA-frame
	// LPMs. Crediting again here would double-count.

	// Send BDP ping if BDP estimator requests it
	if sendBDPPing {
		// Send window update before BDP ping to avoid excessive ping detection
		if wu := t.connInFlow.reset(); wu > 0 {
			t.sendWindowUpdate(0, wu)
		}
		t.sendBDPPing()
	}

	// Write the message data to the stream's receive buffer
	// Use mem.Copy to create a buffer from the payload
	buf := mem.Copy(payload, mem.DefaultBufferPool())
	s.write(recvMsg{buffer: buf})

	// If this is the final client message, signal client half-close.
	if flags&MessageFlagMORE == 0 {
		s.write(recvMsg{err: io.EOF})
	}
	return sz, true
}

// handleMessageBuffer mirrors handleMessage but transfers ownership of the
// provided ring-backed buffer to the stream to avoid copying.
func (t *ShmServerTransport) handleMessageBuffer(streamID uint32, flags uint8, buf mem.Buffer) (uint32, bool) {
	if buf == nil {
		return 0, false
	}
	payload := buf.ReadOnlyData()
	// Fast path: if we have a cached single stream, skip map lookup + RLock.
	var s *ServerStream
	if c := t.cachedStream.Load(); c != nil && c.streamID == streamID {
		s = c.stream
	} else {
		var exists bool
		t.mu.RLock()
		s, exists = t.streams[streamID]
		t.mu.RUnlock()
		if !exists {
			buf.Free()
			return 0, false
		}
	}

	sz := uint32(len(payload))

	// BDP estimation: track bytes received and trigger BDP ping if needed
	var sendBDPPing bool
	if t.bdpEst != nil {
		sendBDPPing = t.bdpEst.add(sz)
	}

	// NOTE: flow-control credit is already done per H2 DATA frame in
	// onDataFrameReceived. See handleMessage for rationale.

	// Send BDP ping if BDP estimator requests it
	if sendBDPPing {
		if wu := t.connInFlow.reset(); wu > 0 {
			t.sendWindowUpdate(0, wu)
		}
		t.sendBDPPing()
	}

	s.write(recvMsg{buffer: buf})

	if flags&MessageFlagMORE == 0 {
		s.write(recvMsg{err: io.EOF})
	}
	return sz, true
}

// handlePing processes a PING frame, sends PONG, and enforces keepalive policy.
func (t *ShmServerTransport) handlePing(ctx context.Context, payload []byte) {
	if t.closed.Load() {
		return
	}
	// Send PONG. Copy payload because the caller releases the underlying
	// ring memory immediately after this returns.
	pongPayload := make([]byte, len(payload))
	copy(pongPayload, payload)
	_ = t.frameWriter.enqueue(frameEntry{
		ctx:     ctx,
		fh:      FrameHeader{Type: FrameTypePONG},
		payload: pongPayload,
	})

	now := time.Now()
	defer func() {
		t.mu.Lock()
		t.lastPingAt = now
		t.mu.Unlock()
	}()

	// Check enforcement policy.
	t.mu.Lock()
	ns := len(t.streams)
	lastPing := t.lastPingAt
	t.mu.Unlock()

	if ns < 1 && !t.kep.PermitWithoutStream {
		// Keepalive shouldn't be active; this ping should have come after
		// at least defaultPingTimeout.
		if !lastPing.IsZero() && lastPing.Add(defaultPingTimeout).After(now) {
			t.mu.Lock()
			t.pingStrikes++
			t.mu.Unlock()
		}
	} else {
		// Check if keepalive policy is respected.
		if !lastPing.IsZero() && lastPing.Add(t.kep.MinTime).After(now) {
			t.mu.Lock()
			t.pingStrikes++
			t.mu.Unlock()
		}
	}

	t.mu.Lock()
	strikes := t.pingStrikes
	t.mu.Unlock()
	if strikes > maxPingStrikes {
		// Send GOAWAY and close.
		t.sendGoAway(GoAwayFlagIMMEDIATE, "too_many_pings")
		go t.Close(errors.New("too many pings from client"))
	}
}

// handleTrailers processes a TRAILERS frame
func (t *ShmServerTransport) handleTrailers(streamID uint32, payload []byte) {
	t.mu.RLock()
	s, exists := t.streams[streamID]
	t.mu.RUnlock()

	if !exists {
		return
	}

	// Decode trailers using the proper frame format
	trailers, err := decodeTrailers(payload)
	if err != nil {
		// Send error to stream
		s.write(recvMsg{err: err})
		return
	}

	// Create status from trailers. For OK, gRPC expects server-side RecvMsg to
	// return io.EOF to indicate client half-close. A nil error here would enqueue
	// an empty recvMsg and can crash downstream readers.
	st := status.New(codes.Code(trailers.GRPCStatusCode), trailers.GRPCStatusMsg)
	var endErr error
	if st.Code() == codes.OK {
		endErr = io.EOF
	} else {
		endErr = st.Err()
	}

	// Signal end-of-client-stream to the stream.
	s.write(recvMsg{err: endErr})

	// Clear any pending stream-level WU credit. The atomic was
	// guarded by streamDone-state checks in the emit paths, but a
	// late drip producer could still Add before observing the final
	// state below. Storing 0 here makes the cleanup observable.
	s.pendingWU.Store(0)

	// Remove stream send quota (protected by sendQuotaMu).
	// Per-stream quota lives on s.sendQuota; no map cleanup needed.
	// The lock guards only the signal channel + waiter list.
	t.sendQuotaMu.Lock()
	t.unregisterConnWaiterLocked(s)
	if ch, ok := t.streamQuotaSignals[streamID]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
		delete(t.streamQuotaSignals, streamID)
	}
	t.sendQuotaMu.Unlock()

	// Remove stream from active streams and finish draining if needed.
	var shouldClose bool
	var dbg string
	t.mu.Lock()
	delete(t.streams, streamID)
	delete(t.streamInFlow, streamID)
	t.updateStreamCache()
	// Mark idle when no more active streams.
	if len(t.streams) == 0 {
		t.idle = time.Now()
	}
	if t.draining.Load() && len(t.streams) == 0 && !t.closed.Load() {
		shouldClose = true
		dbg = t.drainDebugData
	}
	t.mu.Unlock()
	if shouldClose {
		go t.Close(errors.New("transport drained: " + dbg))
	}
}

// handleCancel processes a CANCEL frame
func (t *ShmServerTransport) handleCancel(streamID uint32) {
	t.mu.RLock()
	s, exists := t.streams[streamID]
	t.mu.RUnlock()

	if !exists {
		return
	}

	// Cancel the stream context
	s.cancel()

	// Clear any pending stream-level WU credit before removing the
	// stream below.
	s.pendingWU.Store(0)

	// Remove stream send quota (protected by sendQuotaMu).
	// Per-stream quota lives on s.sendQuota; no map cleanup.
	t.sendQuotaMu.Lock()
	t.unregisterConnWaiterLocked(s)
	if ch, ok := t.streamQuotaSignals[streamID]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
		delete(t.streamQuotaSignals, streamID)
	}
	t.sendQuotaMu.Unlock()
	// Wake the writer so any deferred message pinned to this stream
	// sees ctx cancellation (s.cancel() above) on the next
	// retryDeferred and is drained out.
	select {
	case t.frameWriter.wuRetryWake <- struct{}{}:
	default:
	}

	// Remove from active streams and finish draining if needed.
	var shouldClose bool
	var dbg string
	t.mu.Lock()
	delete(t.streams, streamID)
	delete(t.streamInFlow, streamID)
	t.updateStreamCache()
	// Mark idle when no more active streams.
	if len(t.streams) == 0 {
		t.idle = time.Now()
	}
	if t.draining.Load() && len(t.streams) == 0 && !t.closed.Load() {
		shouldClose = true
		dbg = t.drainDebugData
	}
	t.mu.Unlock()
	if shouldClose {
		go t.Close(errors.New("transport drained: " + dbg))
	}
}

// Close tears down the transport. Once it is called, the transport
// should not be accessed any more. All the pending streams and their
// handlers will be terminated asynchronously.
func (t *ShmServerTransport) Close(err error) {
	t.closeOnce.Do(func() {
		// Hold mu while setting closed to prevent race with HandleStreams
		// which checks closed and calls readerWG.Add(1) under the same lock.
		t.mu.Lock()
		t.closed.Store(true)
		t.mu.Unlock()
		if err == nil {
			err = ErrConnClosing
		}

		// Signal the keepalive goroutine to exit.
		close(t.done)

		// If the underlying segment was already closed (e.g., listener teardown
		// raced this Close path), avoid touching unmapped memory in the rings.
		segClosed := t.segment != nil && t.segment.closed.Load()

		t.sendQuotaMu.Lock()
		// Broadcast: wake every parked waiter so they observe `closed`
		// and return ErrConnClosing instead of sleeping forever.
		t.notifyQuotaChangeLocked(0)
		t.sendQuotaMu.Unlock()

		// Best-effort GOAWAY before tearing down rings.
		// Non-blocking: if the channel is full (writer stuck on ring write),
		// skip GOAWAY to avoid deadlocking Close.
		if t.serverToClient != nil && !segClosed {
			t.frameWriter.tryEnqueueNonBlocking(frameEntry{
				ctx:     context.Background(),
				fh:      FrameHeader{Type: FrameTypeGOAWAY, Flags: GoAwayFlagIMMEDIATE},
				payload: []byte("server closing"),
			})
		}

		// Snapshot and terminate all active streams.
		var streams []*ServerStream
		t.mu.Lock()
		for _, s := range t.streams {
			streams = append(streams, s)
		}
		t.streams = make(map[uint32]*ServerStream)
		t.mu.Unlock()
		for _, s := range streams {
			if s == nil {
				continue
			}
			if s.cancel != nil {
				s.cancel()
			}
			s.write(recvMsg{err: err})
			s.drainRecvBuffer()
		}

		// Cancel context to stop all goroutines
		t.cancel()

		// Close the rings FIRST so any writeFrame blocked inside the writer
		// goroutine gets ErrRingClosed and unblocks. Then close the frame
		// writer to drain remaining entries and stop the goroutine.
		if !segClosed {
			t.serverToClient.Close()
			t.clientToServer.Close()
		}
		t.frameWriter.close()

		// Close the named events (Windows)
		if t.readEvents != nil {
			t.readEvents.Close()
		}
		if t.writeEvents != nil {
			t.writeEvents.Close()
		}

		// Stop the reader goroutine. Under the eventfd waker the reader
		// is parked in shmDataSegWaker.WaitForChange (an *os.File.Read
		// on the eventfd via Go netpoll); ring.Close above set
		// hdr.Closed but the parker has no way to observe that without
		// a wake. Closing the eventfd makes Read return EBADF, which
		// WaitForChange surfaces as ErrRingClosed, and the reader exits.
		// No-op on per-address eventfd / futex paths.
		if t.segment != nil {
			t.segment.UnblockSameSideParkers()
		}

		// Wait for reader goroutine to exit before unmapping.
		t.readerWG.Wait()

		// Close the segment
		if t.segment != nil {
			t.segment.Close()
		}

		// Signal closure
		close(t.errCh)
	})
}

// Peer returns the peer of the server transport.
func (t *ShmServerTransport) Peer() *peer.Peer {
	return t.peer
}

// Drain notifies the client this ServerTransport stops accepting new RPCs.
func (t *ShmServerTransport) Drain(debugData string) {
	if t.closed.Load() {
		return
	}
	if !t.draining.CompareAndSwap(false, true) {
		return
	}

	// Record debug data for the eventual close.
	t.mu.Lock()
	t.drainDebugData = debugData
	active := len(t.streams)
	t.mu.Unlock()

	// Notify peer we're draining.
	t.sendGoAway(GoAwayFlagDRAINING, debugData)

	// If there are no active streams, close immediately.
	if active == 0 {
		go t.Close(errors.New("transport drained: " + debugData))
	}
}

// internalServerTransport methods

// writeHeader writes header metadata for a stream
func (t *ShmServerTransport) writeHeader(s *ServerStream, md metadata.MD) error {
	if t.closed.Load() {
		return ErrConnClosing
	}
	// gRPC may call SendHeader explicitly, and the transport may also need to
	// send implicit headers before the first message. Ensure we only send
	// headers once per stream.
	if s.updateHeaderSent() {
		return nil
	}

	if shmDebugEnabled {
		shmDebugf("[DEBUG] ShmServerTransport.writeHeader: stream=%d, metadata keys=%v", s.id, len(md))
	}

	// Convert metadata.MD to []KV format
	var kvs []KV
	for k, vals := range md {
		var byteVals [][]byte
		for _, v := range vals {
			byteVals = append(byteVals, []byte(v))
		}
		kvs = append(kvs, KV{Key: k, Values: byteVals})
	}

	// Add grpc-encoding if sendCompress is set (like HTTP/2 does)
	if s.sendCompress != "" {
		kvs = append(kvs, KV{Key: "grpc-encoding", Values: [][]byte{[]byte(s.sendCompress)}})
	}

	// Create HEADERS frame with server-initial type
	hdr := HeadersV1{
		Version:          1,
		HdrType:          1, // server-initial
		Method:           "",
		Authority:        "",
		DeadlineUnixNano: 0,
		Metadata:         kvs,
	}

	payload := encodeHeaders(hdr)

	// Create frame header
	fh := FrameHeader{
		Type:     FrameTypeHEADERS,
		StreamID: s.id,
		Length:   uint32(len(payload)),
	}

	if shmDebugEnabled {
		shmDebugf("[DEBUG] ShmServerTransport.writeHeader: Writing HEADERS frame, streamID=%d, length=%d", s.id, fh.Length)
	}

	// Write frame via the dedicated writer goroutine.
	// HEADERS must be synchronous so the caller knows if it failed.
	if err := t.frameWriter.enqueueAndWait(frameEntry{
		ctx:     context.Background(),
		fh:      fh,
		payload: payload,
	}); err != nil {
		if shmDebugEnabled {
			shmDebugf("[ERROR] ShmServerTransport.writeHeader: Failed to write frame: %v", err)
		}
		return err
	}

	if shmDebugEnabled {
		shmDebugf("[DEBUG] ShmServerTransport.writeHeader: Successfully wrote HEADERS frame")
	}
	return nil
}

func (t *ShmServerTransport) maybeWriteHeader(s *ServerStream) error {
	// If headers were already sent, nothing to do.
	if s.isHeaderSent() {
		return nil
	}
	// Snapshot current outgoing headers under lock.
	s.hdrMu.Lock()
	md := s.header.Copy()
	s.hdrMu.Unlock()
	return t.writeHeader(s, md)
}

// writeProto serializes a proto.Message directly into the ring buffer,
// bypassing the standard encode→copy path. Returns (true, err) if handled,
// (false, nil) if the caller should fall back to the standard path.
//
// Only handles contiguous (non-wrap-around) writes. Wrap-around, non-proto
// messages, and oversized messages fall back.
func (t *ShmServerTransport) writeProto(s *ServerStream, msg any, _ *WriteOptions) (bool, error) {
	pm, ok := msg.(protoMessage)
	if !ok {
		return false, nil
	}
	if t.closed.Load() {
		return false, ErrConnClosing
	}

	// Check stream is still active before doing work.
	if s.getState() != streamActive {
		return false, errStreamDone
	}

	if err := t.maybeWriteHeader(s); err != nil {
		return false, err
	}

	pSize := protoSize(pm)
	ringSize := h2FrameHeaderSize + 5 + pSize // total bytes in ring (H2 header + gRPC LPM + proto)
	quotaSize := 5 + pSize                    // flow-control size (matches receiver accounting)

	// Skip ZC if the message is too large for a single frame.
	if uint64(ringSize) > t.serverToClient.Capacity()/3 {
		return false, nil
	}

	// Skip ZC when the message exceeds the current send window —
	// acquireSendQuota is atomic on quotaSize and deadlocks when the
	// stream window is smaller. The fallback write() path hands the
	// whole MESSAGE to the writer goroutine, which chunks under
	// flow control via advanceDeferred.
	//
	// Lockless quota inspect via atomic Loads. Advisory —
	// acquireSendQuota below does the actual reservation.
	if s.sendQuota.Load() < int64(quotaSize) || t.connSendQuota.Load() < int64(quotaSize) {
		atomic.AddUint64(&shmZCWriteSkipQuota, 1)
		return false, nil
	}

	// Flow control: account only the gRPC payload (5-byte LPM + proto body).
	// The 9-byte H2 frame header is NOT included in WINDOW_UPDATE.
	if err := t.acquireSendQuota(s.ctx, s.id, quotaSize); err != nil {
		return false, err
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
		// Atomic refund; always dispatch under the mutex to close
		// the lost-wakeup race vs acquireSendQuota. Also wake WL
		// to revisit deferred whole-message senders whose quota gap
		// may now be satisfiable.
		t.connSendQuota.Add(int64(quotaSize))
		s.sendQuota.Add(int64(quotaSize))
		select {
		case t.frameWriter.wuRetryWake <- struct{}{}:
		default:
		}
		t.sendQuotaMu.Lock()
		t.notifyQuotaChangeLocked(0)
		t.sendQuotaMu.Unlock()
		return false, nil
	}
	ok2, err := writeProtoToRing(s.ctx, t.serverToClient, s.id, pm, pSize, 0)
	t.frameWriter.inlineMu.Unlock()
	t.frameWriter.closeMu.RUnlock()
	if !ok2 {
		// ZC didn't handle — release quota for fallback path.
		// Always dispatch under the mutex. Also wake WL to revisit
		// deferred whole-message senders whose quota gap may now be
		// satisfiable.
		t.connSendQuota.Add(int64(quotaSize))
		s.sendQuota.Add(int64(quotaSize))
		select {
		case t.frameWriter.wuRetryWake <- struct{}{}:
		default:
		}
		t.sendQuotaMu.Lock()
		t.notifyQuotaChangeLocked(0)
		t.sendQuotaMu.Unlock()
		return false, err
	}
	return true, err
}

// write writes header and data for a stream.
//
// The writer goroutine owns outbound flow-control state and chunks
// the message across per-window grants. Server-side MESSAGE frames
// never carry the HTTP/2 END_STREAM bit; end-of-stream is
// communicated via TRAILERS (writeStatus). Therefore isLast is
// always false at this call site.
func (t *ShmServerTransport) write(s *ServerStream, hdr []byte, data mem.BufferSlice, _ *WriteOptions) error {
	if t.closed.Load() {
		return ErrConnClosing
	}
	if err := t.maybeWriteHeader(s); err != nil {
		return err
	}

	if shmDebugEnabled {
		shmDebugf("[DEBUG] ShmServerTransport.write: stream=%d, hdr_len=%d, data_bytes=%d", s.id, len(hdr), data.Len())
	}
	return t.frameWriter.enqueueMessageAndWait(s.ctx, &s.Stream, hdr, data, false)
}

// writeStatus writes status for a stream (trailers)
func (t *ShmServerTransport) writeStatus(s *ServerStream, st *status.Status) error {
	if t.closed.Load() {
		return ErrConnClosing
	}
	// Ensure idempotence: gRPC may race multiple WriteStatus calls.
	if s.swapState(streamDone) == streamDone {
		return nil
	}
	// Clear any pending stream-level WU credit. After swapState the emit
	// paths drop further restores; this prevents a late producer from
	// re-adding credit and leaving a stale value in the atomic when
	// the stream is removed from t.streams below.
	s.pendingWU.Store(0)
	if err := t.maybeWriteHeader(s); err != nil {
		return err
	}

	if shmDebugEnabled {
		shmDebugf("[DEBUG] ShmServerTransport.writeStatus: stream=%d, code=%v, msg=%s", s.id, st.Code(), st.Message())
	}

	// Snapshot trailer metadata.
	s.hdrMu.Lock()
	trMD := s.trailer.Copy()
	s.hdrMu.Unlock()

	var kvs []KV
	for k, vals := range trMD {
		var byteVals [][]byte
		for _, v := range vals {
			byteVals = append(byteVals, []byte(v))
		}
		kvs = append(kvs, KV{Key: k, Values: byteVals})
	}

	// Create trailers frame
	trailers := TrailersV1{
		Version:        1,
		GRPCStatusCode: uint32(st.Code()),
		GRPCStatusMsg:  st.Message(),
		Metadata:       kvs,
	}

	payload := encodeTrailers(trailers)

	// Create frame header
	fh := FrameHeader{
		Type:     FrameTypeTRAILERS,
		StreamID: s.id,
		Length:   uint32(len(payload)),
	}

	if shmDebugEnabled {
		shmDebugf("[DEBUG] ShmServerTransport.writeStatus: Writing TRAILERS frame, streamID=%d, length=%d", s.id, fh.Length)
	}

	// Write frame via the dedicated writer goroutine.
	if err := t.frameWriter.enqueueAndWait(frameEntry{
		ctx:     context.Background(),
		fh:      fh,
		payload: payload,
	}); err != nil {
		if shmDebugEnabled {
			shmDebugf("[ERROR] ShmServerTransport.writeStatus: Failed to write frame: %v", err)
		}
		return err
	}

	if shmDebugEnabled {
		shmDebugf("[DEBUG] ShmServerTransport.writeStatus: Successfully wrote TRAILERS frame")
	}

	// Remove stream send quota (protected by sendQuotaMu).
	// Per-stream quota lives on s.sendQuota; no map cleanup.
	t.sendQuotaMu.Lock()
	t.unregisterConnWaiterLocked(s)
	if ch, ok := t.streamQuotaSignals[s.id]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
		delete(t.streamQuotaSignals, s.id)
	}
	t.sendQuotaMu.Unlock()
	// Wake the writer goroutine so any deferred whole-message entry
	// pinned to this stream observes streamDone on its next
	// retryDeferred pass and is drained out with errStreamDone.
	select {
	case t.frameWriter.wuRetryWake <- struct{}{}:
	default:
	}

	// Remove stream from active streams and finish draining if needed.
	var shouldClose bool
	var dbg string
	t.mu.Lock()
	delete(t.streams, s.id)
	delete(t.streamInFlow, s.id)
	t.updateStreamCache()
	// Mark idle when no more active streams.
	if len(t.streams) == 0 {
		t.idle = time.Now()
	}
	if t.draining.Load() && len(t.streams) == 0 && !t.closed.Load() {
		shouldClose = true
		dbg = t.drainDebugData
	}
	t.mu.Unlock()
	if shouldClose {
		go t.Close(errors.New("transport drained: " + dbg))
	}

	return nil
}

// incrMsgRecv increments the message received counter
func (t *ShmServerTransport) incrMsgRecv() {
	// Channelz metrics are not wired for shm transport; keep a no-op to satisfy interface expectations.
}

// adjustWindow sends out extra window update over the initial window size
// of stream if the application is requesting data larger in size than
// the window.
func (t *ShmServerTransport) adjustWindow(s *ServerStream, n uint32) {
	if w := s.fc.maybeAdjust(n); w > 0 {
		t.sendWindowUpdate(s.id, w)
	}
}

// updateWindow is the application-consumption drip-credit path. See
// the matching ShmClientTransport.updateWindow for the design
// rationale (App-Recv drives stream-level WU via inFlow.onRead; the
// pre-credit-on-LPM-header path is in onMessageStart).
func (t *ShmServerTransport) updateWindow(s *ServerStream, n uint32) {
	if n == 0 {
		return
	}
	if w := s.fc.onRead(n); w > 0 {
		t.sendWindowUpdate(s.id, w)
	}
}

// lookupStream resolves a stream id to its ServerStream using the
// cachedStream MRU fast path then falling back to the streams map
// under RLock.
func (t *ShmServerTransport) lookupStream(streamID uint32) *ServerStream {
	if c := t.cachedStream.Load(); c != nil && c.streamID == streamID {
		return c.stream
	}
	t.mu.RLock()
	s := t.streams[streamID]
	t.mu.RUnlock()
	return s
}

// onMessageStart is invoked by the h2 codec the moment it parses a
// new gRPC LPM's 5-byte header (multi-DATA-frame case only). lpmSize
// is `5 + bodyLen`; the message is NOT yet fully assembled in the
// accumulator. The transport asks both inFlow.maybeAdjust (stream
// pre-credit) and trInFlow.maybeAdjust (conn pre-credit) whether the
// peer needs an upfront WINDOW_UPDATE to admit the rest of the LPM
// and, if so, emits each via the force path (bypassing the limit/4
// drip-credit batching threshold).
//
// See ShmClientTransport.onMessageStart for the full design rationale.
// Server-side rationale is identical: SHM's lpmAccumulator hides the
// partial LPM from the app, so the receiver drives pre-credit on
// both stream and conn windows. Conn pre-credit is required to avoid
// the 64-cycle conn-quota refill stall when an LPM (e.g. 1 MiB) is
// larger than fair-default's 65535-byte conn window.
//
// Runs on the reader goroutine; MUST NOT block on the producer.
func (t *ShmServerTransport) onMessageStart(streamID uint32, lpmSize uint32) {
	if lpmSize == 0 {
		return
	}
	s := t.lookupStream(streamID)
	if s == nil {
		return
	}
	if w := s.fc.maybeAdjust(lpmSize); w > 0 {
		shmStreamPreCreditEmitted.Add(uint64(w))
		t.sendWindowUpdateForce(streamID, w)
	}
	if cw := t.connInFlow.maybeAdjust(lpmSize); cw > 0 {
		shmConnPreCreditEmitted.Add(uint64(cw))
		t.sendWindowUpdateForce(0, cw)
	}
}

// onDataFrameReceived runs at parse-time for each H2 DATA frame
// (BEFORE the body is fed to lpmAccumulator). It performs:
//
//   - Connection-level accounting via trInFlow.onData: bytes first
//     repay any pre-credit debt left by onMessageStart, then
//     accumulate towards the limit/4 drip threshold. Returns the WU
//     delta to emit (already netted against debt).
//   - Stream-level inFlow.onData accounting; NO stream WU here.
//     Stream credit is driven by onMessageStart (pre-credit once
//     LPM size is known) and updateWindow (drip credit as app
//     consumes).
//
// Runs on the reader goroutine; MUST NOT block on the producer.
func (t *ShmServerTransport) onDataFrameReceived(streamID uint32, size uint32) {
	if size == 0 {
		return
	}
	if residual := t.connInFlow.settleDebt(size); residual > 0 {
		t.sendWindowUpdate(0, residual)
	}
	s := t.lookupStream(streamID)
	if s == nil {
		return
	}
	// Stream-level: track pendingData and enforce the receive
	// window per RFC 7540 §6.9.1 and §5.2.2. A compliant SHM
	// sender (using this transport) cannot produce over-window
	// bytes because the writer goroutine CAS-deducts BOTH quotas
	// before emit and refunds on rollback; only a buggy or
	// non-conforming peer can reach this error. Terminate the
	// stream locally and propagate the failure to the application
	// handler via context cancellation, which surfaces as a gRPC
	// status to the peer in the resulting TRAILERS frame. Stock
	// HTTP/2 server (http2_server.go) closes with
	// http2.ErrCodeFlowControl; the SHM equivalent is the
	// cancellation path below.
	if err := s.fc.onData(size); err != nil {
		shmDebugf("[ERROR] ShmServerTransport.onDataFrameReceived: stream=%d flow-control violation (%v); cancelling stream", streamID, err)
		s.cancel()
	}
}

// ConfigureKeepalive sets keepalive parameters and starts the keepalive
// goroutine.
func (t *ShmServerTransport) ConfigureKeepalive(kp keepalive.ServerParameters, kep keepalive.EnforcementPolicy) {
	// Apply defaults matching HTTP/2 transport.
	if kp.MaxConnectionIdle == 0 {
		kp.MaxConnectionIdle = defaultMaxConnectionIdle
	}
	if kp.MaxConnectionAge == 0 {
		kp.MaxConnectionAge = defaultMaxConnectionAge
	}
	if kp.MaxConnectionAgeGrace == 0 {
		kp.MaxConnectionAgeGrace = defaultMaxConnectionAgeGrace
	}
	if kp.Time == 0 {
		kp.Time = defaultServerKeepaliveTime
	}
	if kp.Timeout == 0 {
		kp.Timeout = defaultServerKeepaliveTimeout
	}
	if kep.MinTime == 0 {
		kep.MinTime = defaultKeepalivePolicyMinTime
	}
	t.kp = kp
	t.kep = kep
	// Mark connection as initially idle.
	t.mu.Lock()
	t.idle = time.Now()
	t.mu.Unlock()
	go t.keepalive()
}

// sendPing sends a PING frame with 8-byte opaque data.
func (t *ShmServerTransport) sendPing() error {
	var data [8]byte
	binary.LittleEndian.PutUint64(data[:], uint64(time.Now().UnixNano()))
	return t.frameWriter.enqueueAndWait(frameEntry{
		ctx:     context.Background(),
		fh:      FrameHeader{Type: FrameTypePING},
		payload: data[:],
	})
}

// keepalive monitors connection health and enforces server-side policies:
// - MaxConnectionIdle: close after being idle too long.
// - MaxConnectionAge: close after connection exists too long.
// - Time/Timeout: send PINGs to check liveness.
func (t *ShmServerTransport) keepalive() {
	defer close(t.keepaliveDone)

	// True iff a ping has been sent, and no data has been received since then.
	outstandingPing := false
	// Amount of time remaining before which we should receive an ACK for the
	// last sent ping.
	kpTimeoutLeft := time.Duration(0)
	// Records the last value of t.lastRead before we go block on the timer.
	prevNano := time.Now().UnixNano()

	idleTimer := time.NewTimer(t.kp.MaxConnectionIdle)
	ageTimer := time.NewTimer(t.kp.MaxConnectionAge)
	kpTimer := time.NewTimer(t.kp.Time)
	defer func() {
		idleTimer.Stop()
		ageTimer.Stop()
		kpTimer.Stop()
	}()

	for {
		select {
		case <-idleTimer.C:
			t.mu.Lock()
			idle := t.idle
			if idle.IsZero() {
				// Connection is not idle.
				t.mu.Unlock()
				idleTimer.Reset(t.kp.MaxConnectionIdle)
				continue
			}
			val := t.kp.MaxConnectionIdle - time.Since(idle)
			t.mu.Unlock()
			if val <= 0 {
				// Connection has been idle for MaxConnectionIdle; drain.
				t.Drain("max_idle")
				return
			}
			idleTimer.Reset(val)
		case <-ageTimer.C:
			// Connection age exceeded; drain.
			t.Drain("max_age")
			ageTimer.Reset(t.kp.MaxConnectionAgeGrace)
			select {
			case <-ageTimer.C:
				// Grace period expired; close.
				t.Close(errors.New("max connection age grace exceeded"))
			case <-t.done:
			}
			return
		case <-kpTimer.C:
			lastRead := atomic.LoadInt64(&t.lastRead)
			if lastRead > prevNano {
				// There has been read activity.
				outstandingPing = false
				kpTimer.Reset(time.Duration(lastRead) + t.kp.Time - time.Duration(time.Now().UnixNano()))
				prevNano = lastRead
				continue
			}
			if outstandingPing && kpTimeoutLeft <= 0 {
				t.Close(errors.New("keepalive ping not acked within timeout"))
				return
			}
			if !outstandingPing {
				if err := t.sendPing(); err != nil {
					t.Close(errors.New("keepalive failed to send ping"))
					return
				}
				kpTimeoutLeft = t.kp.Timeout
				outstandingPing = true
			}
			sleepDuration := min(t.kp.Time, kpTimeoutLeft)
			kpTimeoutLeft -= sleepDuration
			kpTimer.Reset(sleepDuration)
		case <-t.done:
			return
		}
	}
}
