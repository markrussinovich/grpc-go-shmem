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
	"io"
	"log"
	"math"
	"net"
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

// ShmServerTransport implements the gRPC ServerTransport interface
// for shared memory communication.

// pendingServerMsg holds a buffered MESSAGE frame (and optional HEADERS frame)
// from write(Last=true), to be coalesced with TRAILERS in writeStatus().
type pendingServerMsg struct {
	frames []coalescedFrame
}
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
	// writeMu serializes writes to the server->client ring.
	writeMu sync.Mutex
	mu      sync.RWMutex

	// Stream management
	streams    map[uint32]*ServerStream
	handleFunc func(*ServerStream)
	maxStreams uint32

	// Flow control
	sendQuotaMu     sync.Mutex
	connSendQuota   int64
	streamSendQuota map[uint32]int64
	quotaSignal     chan struct{}
	connInFlow      trInFlow
	streamInFlow    map[uint32]*inFlow

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

	// pendingMsgs stores buffered MESSAGE frames from write(Last=true) to be
	// coalesced with TRAILERS in writeStatus(), reducing ring operations for
	// unary RPCs. Keyed by stream ID.
	pendingMsgs sync.Map // map[uint32]*pendingServerMsg

	// chunkBufs accumulates partial MESSAGE frame data for chunked messages
	// (frames with MessageFlagMORE set). When a final chunk arrives (no MORE
	// flag), the accumulated data is concatenated and delivered as one message.
	// Keyed by stream ID.
	chunkBufs sync.Map // map[uint32][][]byte
}

func (t *ShmServerTransport) sendGoAway(flags uint8, debugData string) {
	if t.closed.Load() || t.serverToClient == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_ = writeFrame(ctx, t.serverToClient, FrameHeader{Type: FrameTypeGOAWAY, Flags: flags}, []byte(debugData))
}

func (t *ShmServerTransport) notifyQuotaChangeLocked() {
	close(t.quotaSignal)
	t.quotaSignal = make(chan struct{})
}

func (t *ShmServerTransport) addSendQuota(streamID uint32, delta uint32) {
	if delta == 0 {
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

func (t *ShmServerTransport) acquireSendQuota(ctx context.Context, streamID uint32, n int) error {
	if n == 0 {
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

func (t *ShmServerTransport) sendWindowUpdate(streamID uint32, delta uint32) {
	if delta == 0 || t.closed.Load() {
		return
	}
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, delta)
	_ = writeFrame(context.Background(), t.serverToClient, FrameHeader{Type: FrameTypeWindowUpdate, StreamID: streamID}, buf)
}

func (t *ShmServerTransport) rejectNewStream(streamID uint32, msg string) {
	if t.closed.Load() || t.serverToClient == nil {
		return
	}
	// Best-effort send GOAWAY as a signal to stop creating new streams.
	t.sendGoAway(GoAwayFlagDRAINING, msg)

	trailers := encodeTrailers(TrailersV1{Version: 1, GRPCStatusCode: uint32(codes.Unavailable), GRPCStatusMsg: msg})
	fh := FrameHeader{Type: FrameTypeTRAILERS, StreamID: streamID, Length: uint32(len(trailers))}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_ = writeFrame(ctx, t.serverToClient, fh, trailers)
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
		ctx:             ctx,
		cancel:          cancel,
		streams:         make(map[uint32]*ServerStream),
		streamSendQuota: make(map[uint32]int64),
		streamInFlow:    make(map[uint32]*inFlow),
		errCh:           make(chan struct{}),
		quotaSignal:     make(chan struct{}),
		done:            make(chan struct{}),
		keepaliveDone:   make(chan struct{}),
	}
	// Initialize flow control windows.
	t.connSendQuota = int64(maxWindowSize)
	t.connInFlow = trInFlow{limit: uint32(maxWindowSize)}
	t.connInFlow.updateEffectiveWindowSize()
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
	t.readerWG.Add(1)
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
		log.Printf("[DEBUG] ShmServerTransport.processIncomingData: STARTED, ring=%p", t.clientToServer)
	}
	defer func() {
		shmDebugf("[DEBUG] ShmServerTransport.processIncomingData: EXITING")
		if !t.closed.Load() {
			go t.Close(errors.New("incoming data processing ended"))
		}
	}()

	for {
		if t.closed.Load() {
			shmDebugf("[DEBUG] ShmServerTransport.processIncomingData: transport closed, exiting")
			return
		}
		if shmDebugEnabled {
			log.Printf("[DEBUG] ShmServerTransport.processIncomingData: waiting for frame from client... widx=%d, ridx=%d", t.clientToServer.header().WriteIndex(), t.clientToServer.header().ReadIndex())
		}
		fh, payloadBuf, err := readFrameView(ctx, t.clientToServer)
		if err != nil {
			if shmDebugEnabled {
				log.Printf("[DEBUG] ShmServerTransport.processIncomingData: readFrameView error: %v", err)
			}
			if errors.Is(err, ErrRingClosed) || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || t.closed.Load() {
				return
			}
			continue
		}
		if shmDebugEnabled {
			log.Printf("[DEBUG] ShmServerTransport.processIncomingData: received frame type=%d, streamID=%d, length=%d", fh.Type, fh.StreamID, fh.Length)
		}

		// Update last read timestamp for keepalive tracking.
		atomic.StoreInt64(&t.lastRead, time.Now().UnixNano())

		payloadTransferred := false
		release := func() {
			if shmDebugEnabled {
				log.Printf("[DEBUG] ShmServerTransport.processIncomingData: release() called, payloadTransferred=%v, payloadBuf=%v", payloadTransferred, payloadBuf)
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
			if payloadBuf != nil {
				payloadTransferred = true
				t.handleMessageBuffer(fh.StreamID, fh.Flags, payloadBuf)
				payloadBuf = nil
			} else {
				t.handleMessage(fh.StreamID, fh.Flags, payload)
			}
		case FrameTypeTRAILERS:
			t.handleTrailers(fh.StreamID, payload)
			release()
		case FrameTypeHALFCLOSE:
			// Client signals no more messages on this stream.
			t.mu.RLock()
			s, ok := t.streams[fh.StreamID]
			t.mu.RUnlock()
			if ok {
				s.write(recvMsg{err: io.EOF})
			}
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
				delta := binary.LittleEndian.Uint32(payload[:4])
				t.addSendQuota(fh.StreamID, delta)
			}
			release()
			continue
		case FrameTypePONG:
			// No-op for now; could be used for keepalive bookkeeping.
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
	s.fc = inFlow{limit: uint32(maxWindowSize)}

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
	// Clear idle time when we have active streams.
	t.idle = time.Time{}
	// Reset ping strikes when streams become active.
	t.pingStrikes = 0
	h := t.handleFunc
	t.mu.Unlock()

	// Initialize send quota for this stream (protected by sendQuotaMu, not mu).
	t.sendQuotaMu.Lock()
	t.streamSendQuota[streamID] = int64(maxWindowSize)
	t.sendQuotaMu.Unlock()

	// Call the handler in a new goroutine.
	if h != nil {
		go h(s)
	}

	return nil
}

// handleMessage processes a MESSAGE frame.
// For client->server, the final MESSAGE is indicated by MessageFlagMORE being unset.
func (t *ShmServerTransport) handleMessage(streamID uint32, flags uint8, payload []byte) {
	t.mu.RLock()
	s, exists := t.streams[streamID]
	t.mu.RUnlock()

	if !exists {
		t.chunkBufs.Delete(streamID)
		return
	}

	sz := uint32(len(payload))
	if wu := t.connInFlow.onData(sz); wu > 0 {
		t.sendWindowUpdate(0, wu)
	}
	if err := s.fc.onData(sz); err != nil {
		t.chunkBufs.Delete(streamID)
		s.write(recvMsg{err: err})
		return
	}

	// Chunked reassembly: accumulate data when MORE flag is set.
	if flags&MessageFlagMORE != 0 {
		chunk := make([]byte, len(payload))
		copy(chunk, payload)
		if prev, ok := t.chunkBufs.Load(streamID); ok {
			t.chunkBufs.Store(streamID, append(prev.([][]byte), chunk))
		} else {
			t.chunkBufs.Store(streamID, [][]byte{chunk})
		}
		return
	}

	// Final chunk (or single-frame message). Reassemble if needed.
	var buf mem.Buffer
	if prev, ok := t.chunkBufs.LoadAndDelete(streamID); ok {
		chunks := prev.([][]byte)
		totalLen := len(payload)
		for _, c := range chunks {
			totalLen += len(c)
		}
		assembled := make([]byte, 0, totalLen)
		for _, c := range chunks {
			assembled = append(assembled, c...)
		}
		assembled = append(assembled, payload...)
		buf = mem.SliceBuffer(assembled)
	} else {
		buf = mem.Copy(payload, mem.DefaultBufferPool())
	}
	s.write(recvMsg{buffer: buf})
}

// handleMessageBuffer mirrors handleMessage but transfers ownership of the
// provided ring-backed buffer to the stream to avoid copying.
func (t *ShmServerTransport) handleMessageBuffer(streamID uint32, flags uint8, buf mem.Buffer) {
	if buf == nil {
		return
	}
	payload := buf.ReadOnlyData()
	s, exists := func() (*ServerStream, bool) {
		t.mu.RLock()
		s, ok := t.streams[streamID]
		t.mu.RUnlock()
		return s, ok
	}()
	if !exists {
		buf.Free()
		t.chunkBufs.Delete(streamID)
		return
	}

	sz := uint32(len(payload))
	if wu := t.connInFlow.onData(sz); wu > 0 {
		t.sendWindowUpdate(0, wu)
	}
	if err := s.fc.onData(sz); err != nil {
		buf.Free()
		t.chunkBufs.Delete(streamID)
		s.write(recvMsg{err: err})
		return
	}

	// Chunked reassembly: accumulate data when MORE flag is set.
	// Note: readFrameView already copies data for MORE-flagged frames,
	// so buf here is a copied buffer (not pinning ring space).
	if flags&MessageFlagMORE != 0 {
		chunk := make([]byte, len(payload))
		copy(chunk, payload)
		buf.Free()
		if prev, ok := t.chunkBufs.Load(streamID); ok {
			t.chunkBufs.Store(streamID, append(prev.([][]byte), chunk))
		} else {
			t.chunkBufs.Store(streamID, [][]byte{chunk})
		}
		return
	}

	// Final chunk (or single-frame message). Reassemble if needed.
	if prev, ok := t.chunkBufs.LoadAndDelete(streamID); ok {
		chunks := prev.([][]byte)
		totalLen := len(payload)
		for _, c := range chunks {
			totalLen += len(c)
		}
		assembled := make([]byte, 0, totalLen)
		for _, c := range chunks {
			assembled = append(assembled, c...)
		}
		assembled = append(assembled, payload...)
		buf.Free()
		s.write(recvMsg{buffer: mem.SliceBuffer(assembled)})
	} else {
		// Single frame, zero-copy fast path.
		s.write(recvMsg{buffer: buf})
	}
}

// handlePing processes a PING frame, sends PONG, and enforces keepalive policy.
func (t *ShmServerTransport) handlePing(ctx context.Context, payload []byte) {
	if t.closed.Load() {
		return
	}
	// Send PONG.
	t.writeMu.Lock()
	_ = writeFrame(ctx, t.serverToClient, FrameHeader{Type: FrameTypePONG}, payload)
	t.writeMu.Unlock()

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

	// Clean up any in-progress chunk reassembly for this stream.
	t.chunkBufs.Delete(streamID)

	// Remove stream send quota (protected by sendQuotaMu).
	t.sendQuotaMu.Lock()
	delete(t.streamSendQuota, streamID)
	t.sendQuotaMu.Unlock()

	// Remove stream from active streams and finish draining if needed.
	var shouldClose bool
	var dbg string
	t.mu.Lock()
	delete(t.streams, streamID)
	delete(t.streamInFlow, streamID)
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

	// Clean up any in-progress chunk reassembly for this stream.
	t.chunkBufs.Delete(streamID)

	// Remove stream send quota (protected by sendQuotaMu).
	t.sendQuotaMu.Lock()
	delete(t.streamSendQuota, streamID)
	t.sendQuotaMu.Unlock()

	// Remove from active streams and finish draining if needed.
	var shouldClose bool
	var dbg string
	t.mu.Lock()
	delete(t.streams, streamID)
	delete(t.streamInFlow, streamID)
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
		t.closed.Store(true)
		if err == nil {
			err = ErrConnClosing
		}

		// Signal the keepalive goroutine to exit.
		close(t.done)

		// If the underlying segment was already closed (e.g., listener teardown
		// raced this Close path), avoid touching unmapped memory in the rings.
		segClosed := t.segment != nil && t.segment.closed.Load()

		t.sendQuotaMu.Lock()
		t.notifyQuotaChangeLocked()
		t.sendQuotaMu.Unlock()

		// Best-effort notify peer that we're closing immediately with debug data.
		// Hold writeMu to avoid racing with handler goroutines that may still be writing.
		if t.serverToClient != nil && !segClosed {
			t.writeMu.Lock()
			_ = writeFrame(context.Background(), t.serverToClient, FrameHeader{Type: FrameTypeGOAWAY, Flags: GoAwayFlagIMMEDIATE}, []byte("server closing"))
			t.writeMu.Unlock()
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

		// Close the rings
		if !segClosed {
			t.serverToClient.Close()
			t.clientToServer.Close()
		}

		// Close the named events (Windows)
		if t.readEvents != nil {
			t.readEvents.Close()
		}
		if t.writeEvents != nil {
			t.writeEvents.Close()
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
		log.Printf("[DEBUG] ShmServerTransport.writeHeader: stream=%d, metadata keys=%v", s.id, len(md))
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
		log.Printf("[DEBUG] ShmServerTransport.writeHeader: Writing HEADERS frame, streamID=%d, length=%d", s.id, fh.Length)
	}

	// Write frame to server->client ring
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if err := writeFrame(context.Background(), t.serverToClient, fh, payload); err != nil {
		if shmDebugEnabled {
			log.Printf("[ERROR] ShmServerTransport.writeHeader: Failed to write frame: %v", err)
		}
		return err
	}

	shmDebugf("[DEBUG] ShmServerTransport.writeHeader: Successfully wrote HEADERS frame")
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

// write writes header and data for a stream
func (t *ShmServerTransport) write(s *ServerStream, hdr []byte, data mem.BufferSlice, opts *WriteOptions) error {
	if t.closed.Load() {
		return ErrConnClosing
	}

	// Check if we need to send HEADERS before MESSAGE. If so, prepare the
	// HEADERS payload and coalesce both frames into a single ring reservation.
	needHeaders := !s.isHeaderSent()
	var headerPayload []byte
	var headerFH FrameHeader
	if needHeaders {
		if s.updateHeaderSent() {
			// Another goroutine already sent headers; proceed with MESSAGE only.
			needHeaders = false
		} else {
			s.hdrMu.Lock()
			md := s.header.Copy()
			s.hdrMu.Unlock()

			var kvs []KV
			for k, vals := range md {
				var byteVals [][]byte
				for _, v := range vals {
					byteVals = append(byteVals, []byte(v))
				}
				kvs = append(kvs, KV{Key: k, Values: byteVals})
			}
			headersV1 := HeadersV1{
				Version:  1,
				HdrType:  1, // server-initial
				Metadata: kvs,
			}
			headerPayload = encodeHeaders(headersV1)
			headerFH = FrameHeader{
				Type:     FrameTypeHEADERS,
				StreamID: s.id,
			}
		}
	}

	payloadLen := len(hdr) + data.Len()
	if shmDebugEnabled {
		log.Printf("[DEBUG] ShmServerTransport.write: stream=%d, hdr_len=%d, data_bytes=%d, coalesce_headers=%v", s.id, len(hdr), data.Len(), needHeaders)
	}

	// Enforce outbound flow control before writing.
	if err := t.acquireSendQuota(s.ctx, s.id, payloadLen); err != nil {
		return err
	}

	// Create MESSAGE frame
	fh := FrameHeader{
		Type:     FrameTypeMESSAGE,
		StreamID: s.id,
	}

	// If this is the last message (unary response), buffer the frames so that
	// writeStatus() can coalesce MESSAGE + TRAILERS into one ring write.
	if opts != nil && opts.Last {
		msgPayload := materializePayload(hdr, data)
		var frames []coalescedFrame
		if needHeaders {
			frames = append(frames, coalescedFrame{FH: headerFH, Payload: headerPayload})
		}
		frames = append(frames, coalescedFrame{FH: fh, Payload: msgPayload})
		t.pendingMsgs.Store(s.id, &pendingServerMsg{frames: frames})
		shmDebugf("[DEBUG] ShmServerTransport.write: buffered MESSAGE for coalescing with TRAILERS")
		return nil
	}

	// Write frame(s) to server->client ring
	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	if needHeaders {
		// Coalesced path: HEADERS + MESSAGE in one reservation.
		if err := writeFrameWithPrefix(context.Background(), t.serverToClient, headerFH, headerPayload, fh, hdr, data); err != nil {
			if shmDebugEnabled {
				log.Printf("[ERROR] ShmServerTransport.write: coalesced write failed: %v", err)
			}
			return err
		}
	} else {
		if err := writeFrameBuffersChunked(context.Background(), t.serverToClient, fh, hdr, data, 0); err != nil {
			if shmDebugEnabled {
				log.Printf("[ERROR] ShmServerTransport.write: Failed to write frame: %v", err)
			}
			return err
		}
	}

	shmDebugf("[DEBUG] ShmServerTransport.write: Successfully wrote MESSAGE frame")
	return nil
}

// writeStatusFrames writes the TRAILERS frame (and any pending MESSAGE and/or
// HEADERS frames) to the ring. It handles three cases:
//  1. Pending MESSAGE from write(Last=true): coalesce [HEADERS+] MESSAGE + TRAILERS.
//  2. No pending MESSAGE, headers not yet sent: coalesce HEADERS + TRAILERS.
//  3. No pending MESSAGE, headers already sent: write TRAILERS alone.
func (t *ShmServerTransport) writeStatusFrames(s *ServerStream, trFH FrameHeader, trailersPayload []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	if v, ok := t.pendingMsgs.LoadAndDelete(s.id); ok {
		// Case 1: coalesced [HEADERS +] MESSAGE + TRAILERS in one ring op.
		pm := v.(*pendingServerMsg)
		frames := append(pm.frames, coalescedFrame{FH: trFH, Payload: trailersPayload})
		if err := writeFramesCoalesced(context.Background(), t.serverToClient, frames); err != nil {
			if shmDebugEnabled {
				log.Printf("[ERROR] ShmServerTransport.writeStatusFrames: coalesced write failed: %v", err)
			}
			return err
		}
		shmDebugf("[DEBUG] ShmServerTransport.writeStatusFrames: coalesced [HEADERS+]MESSAGE+TRAILERS succeeded")
		return nil
	}

	// No pending message. Ensure headers are sent inline (writeHeader would
	// deadlock since we already hold writeMu).
	if !s.isHeaderSent() && !s.updateHeaderSent() {
		s.hdrMu.Lock()
		md := s.header.Copy()
		s.hdrMu.Unlock()
		var hdrKVs []KV
		for k, vals := range md {
			var byteVals [][]byte
			for _, v := range vals {
				byteVals = append(byteVals, []byte(v))
			}
			hdrKVs = append(hdrKVs, KV{Key: k, Values: byteVals})
		}
		headersV1 := HeadersV1{
			Version:  1,
			HdrType:  1, // server-initial
			Metadata: hdrKVs,
		}
		hdrPayload := encodeHeaders(headersV1)
		hdrFH := FrameHeader{Type: FrameTypeHEADERS, StreamID: s.id}
		// Case 2: coalesce HEADERS + TRAILERS.
		frames := []coalescedFrame{
			{FH: hdrFH, Payload: hdrPayload},
			{FH: trFH, Payload: trailersPayload},
		}
		if err := writeFramesCoalesced(context.Background(), t.serverToClient, frames); err != nil {
			if shmDebugEnabled {
				log.Printf("[ERROR] ShmServerTransport.writeStatusFrames: HEADERS+TRAILERS write failed: %v", err)
			}
			return err
		}
		shmDebugf("[DEBUG] ShmServerTransport.writeStatusFrames: coalesced HEADERS+TRAILERS succeeded")
		return nil
	}

	// Case 3: TRAILERS only.
	if err := writeFrame(context.Background(), t.serverToClient, trFH, trailersPayload); err != nil {
		if shmDebugEnabled {
			log.Printf("[ERROR] ShmServerTransport.writeStatusFrames: Failed to write TRAILERS: %v", err)
		}
		return err
	}
	shmDebugf("[DEBUG] ShmServerTransport.writeStatusFrames: wrote TRAILERS frame")
	return nil
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

	if shmDebugEnabled {
		log.Printf("[DEBUG] ShmServerTransport.writeStatus: stream=%d, code=%v, msg=%s", s.id, st.Code(), st.Message())
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

	trailersPayload := encodeTrailers(trailers)
	trFH := FrameHeader{
		Type:     FrameTypeTRAILERS,
		StreamID: s.id,
	}

	if shmDebugEnabled {
		log.Printf("[DEBUG] ShmServerTransport.writeStatus: Writing TRAILERS frame, streamID=%d, length=%d", s.id, len(trailersPayload))
	}

	if err := t.writeStatusFrames(s, trFH, trailersPayload); err != nil {
		return err
	}

	// Clean up any in-progress chunk reassembly for this stream.
	t.chunkBufs.Delete(s.id)

	// Remove stream send quota (protected by sendQuotaMu).
	t.sendQuotaMu.Lock()
	delete(t.streamSendQuota, s.id)
	t.sendQuotaMu.Unlock()

	// Remove stream from active streams and finish draining if needed.
	var shouldClose bool
	var dbg string
	t.mu.Lock()
	delete(t.streams, s.id)
	delete(t.streamInFlow, s.id)
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

// updateWindow adjusts the inbound quota for the stream.
// Window updates will be sent out when the cumulative quota
// exceeds the corresponding threshold.
func (t *ShmServerTransport) updateWindow(s *ServerStream, n uint32) {
	if w := s.fc.onRead(n); w > 0 {
		t.sendWindowUpdate(s.id, w)
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
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return writeFrame(context.Background(), t.serverToClient, FrameHeader{Type: FrameTypePING}, data[:])
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
