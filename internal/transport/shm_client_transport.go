//go:build linux

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
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/internal/grpcutil"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ShmClientTransport implements the gRPC ClientTransport interface
// for shared memory communication.
type ShmClientTransport struct {
	// Core state
	segment        *Segment // The shared memory segment
	clientToServer *ShmRing // Ring for client->server data
	serverToClient *ShmRing // Ring for server->client data
	segmentName    string   // Segment identifier for cleanup

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
	mu       sync.RWMutex

	// Stream management
	streams         map[uint32]*ClientStream
	streamTransport map[*ClientStream]*ShmClientTransport // Track transport for each stream
	streamID        uint32                                // next stream ID to assign

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
}

func (t *ShmClientTransport) setGoAwayReason(flags uint8, debug string) {
	t.goAwayOnce.Do(func() {
		// Shmem GOAWAY frames do not carry an HTTP/2 error code or debug data.
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

func (t *ShmClientTransport) notifyQuotaChangeLocked() {
	close(t.quotaSignal)
	t.quotaSignal = make(chan struct{})
}

func (t *ShmClientTransport) addSendQuota(streamID uint32, delta uint32) {
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

func (t *ShmClientTransport) acquireSendQuota(ctx context.Context, streamID uint32, n int) error {
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

func (t *ShmClientTransport) sendWindowUpdate(streamID uint32, delta uint32) {
	if delta == 0 || t.closed.Load() {
		return
	}
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, delta)
	_ = writeFrame(context.Background(), t.clientToServer, FrameHeader{Type: FrameTypeWindowUpdate, StreamID: streamID}, buf)
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

	// Create rings for bidirectional communication
	// Ring A: client->server, Ring B: server->client
	clientToServer := NewShmRingFromSegment(segment.A, segment.Mem)
	serverToClient := NewShmRingFromSegment(segment.B, segment.Mem)

	ctx, cancel := context.WithCancel(context.Background())

	segName := ""
	if addr, ok := remoteAddr.(*ShmAddr); ok {
		segName = addr.Name
	}

	t := &ShmClientTransport{
		segment:               segment,
		clientToServer:        clientToServer,
		serverToClient:        serverToClient,
		segmentName:           segName,
		localAddr:             localAddr,
		remoteAddr:            remoteAddr,
		ctx:                   ctx,
		cancel:                cancel,
		streams:               make(map[uint32]*ClientStream),
		streamTransport:       make(map[*ClientStream]*ShmClientTransport),
		streamSendQuota:       make(map[uint32]int64),
		streamInFlow:          make(map[uint32]*inFlow),
		errCh:                 make(chan struct{}),
		goAwayCh:              make(chan struct{}),
		quotaSignal:           make(chan struct{}),
		streamsQuotaAvailable: make(chan struct{}, 1),
		keepaliveDone:         make(chan struct{}),
	}
	// Initialize dormancy condition variable.
	t.kpDormancyCond = sync.NewCond(&t.mu)
	// Initialize connection-level flow control windows to the HTTP/2 maximum.
	t.connSendQuota = int64(maxWindowSize)
	t.connInFlow = trInFlow{limit: uint32(maxWindowSize)}
	t.connInFlow.updateEffectiveWindowSize()
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

// processIncomingData reads data from the server->client ring and processes gRPC frames
func (t *ShmClientTransport) processIncomingData(ctx context.Context) {
	shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: STARTED")
	defer func() {
		shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: EXITING")
		if !t.closed.Load() {
			go t.Close(errors.New("incoming data processing ended"))
		}
	}()

	for {
		if t.closed.Load() {
			shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: transport closed, exiting")
			return
		}
		shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: waiting for frame from server...")
		// Event-driven: block on next frame from rx ring using zero-copy payload views.
		fh, payloadBuf, err := readFrameView(ctx, t.serverToClient)
		if err != nil {
			shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: readFrame error: %v", err)
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
		shmDebugf("[DEBUG] ShmClientTransport.processIncomingData: received frame type=%d, streamID=%d, length=%d", fh.Type, fh.StreamID, fh.Length)

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
			if len(payload) >= 4 {
				delta := binary.LittleEndian.Uint32(payload[:4])
				t.addSendQuota(fh.StreamID, delta)
			}
			release()
			continue
		}

		// Dispatch frame to appropriate stream
		t.mu.RLock()
		stream, ok := t.streams[fh.StreamID]
		t.mu.RUnlock()

		if !ok {
			release()
			// Stream not found - might have been closed
			continue
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
			shmDebugf("[DEBUG] ShmClientTransport: MESSAGE handler entered for stream %d, payload size=%d", fh.StreamID, len(payload))
			sz := uint32(len(payload))
			if wu := t.connInFlow.onData(sz); wu > 0 {
				t.sendWindowUpdate(0, wu)
			}
			if stream.fc == nil {
				stream.fc = &inFlow{limit: uint32(maxWindowSize)}
			}
			if err := stream.fc.onData(sz); err != nil {
				shmDebugf("[DEBUG] ShmClientTransport: MESSAGE flow control error: %v", err)
				release()
				t.closeStream(stream, err, true, http2.ErrCodeFlowControl, status.New(codes.Internal, err.Error()), nil, false)
				continue
			}

			// Transfer ownership of the ring-backed buffer to the stream for zero-copy delivery.
			if payloadBuf != nil {
				shmDebugf("[DEBUG] ShmClientTransport: MESSAGE delivering payloadBuf (len=%d) to stream %d", payloadBuf.Len(), fh.StreamID)
				payloadTransferred = true
				stream.write(recvMsg{buffer: payloadBuf})
				payloadBuf = nil
			} else {
				shmDebugf("[DEBUG] ShmClientTransport: MESSAGE delivering copied payload (len=%d) to stream %d", len(payload), fh.StreamID)
				buf := mem.Copy(payload, mem.DefaultBufferPool())
				stream.write(recvMsg{buffer: buf})
			}
			shmDebugf("[DEBUG] ShmClientTransport: MESSAGE delivered to stream %d", fh.StreamID)

		case FrameTypePING:
			// Respond with PONG carrying the same opaque data.
			_ = writeFrame(context.Background(), t.clientToServer, FrameHeader{Type: FrameTypePONG}, payload)
			release()

		case FrameTypePONG:
			// No-op for now; keepalive callbacks not yet wired.
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

		// Best-effort GOAWAY before tearing down rings so the peer observes the
		// shutdown intent (mirrors http2 immediate close behavior).
		if t.clientToServer != nil && !segClosed {
			_ = writeFrame(context.Background(), t.clientToServer, FrameHeader{Type: FrameTypeGOAWAY, Flags: GoAwayFlagIMMEDIATE}, []byte("client closing"))
		}

		// Cancel context to stop background reader goroutine and keepalive.
		t.cancel()

		// Wake up the keepalive goroutine if it's dormant, so it can exit.
		t.mu.Lock()
		if t.kpDormant {
			t.kpDormancyCond.Signal()
		}
		t.mu.Unlock()

		// Wait for keepalive goroutine to exit before unmapping the segment.
		if t.keepaliveEnabled && t.keepaliveDone != nil {
			<-t.keepaliveDone
		}

		// Terminate all active streams before closing/unmapping the segment.
		// This prevents concurrent stream Close paths from touching unmapped ring
		// memory.
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

		// Close the rings and wait for the background reader to exit before
		// unmapping.
		if !segClosed {
			if t.clientToServer != nil {
				_ = t.clientToServer.Close()
			}
			if t.serverToClient != nil {
				_ = t.serverToClient.Close()
			}
		}
		t.readerWG.Wait()

		// Close the segment last and unlink the backing file.
		if t.segment != nil {
			_ = t.segment.Close()
		}
		if t.segmentName != "" {
			_ = RemoveSegment(t.segmentName)
		}

		// Signal closure
		close(t.errCh)
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
		_ = writeFrame(context.Background(), t.clientToServer, FrameHeader{Type: FrameTypeGOAWAY, Flags: GoAwayFlagDRAINING}, []byte("draining"))
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
func (t *ShmClientTransport) NewStream(ctx context.Context, callHdr *CallHdr) (*ClientStream, error) {
	if t.closed.Load() || t.draining.Load() {
		return nil, &NewStreamError{Err: ErrConnClosing, AllowTransparentRetry: true}
	}

	firstTry := true
	var ch chan struct{}
	var s *ClientStream
	var streamID uint32
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

		// Create the client stream
		s = &ClientStream{
			Stream: &Stream{
				id:             streamID,
				ctx:            ctx,
				method:         callHdr.Method,
				sendCompress:   callHdr.SendCompress,
				buf:            newRecvBuffer(),
				fc:             &inFlow{limit: uint32(maxWindowSize)},
				contentSubtype: callHdr.ContentSubtype,
			},
			ct:         t, // Set the client transport (now an interface, no unsafe needed)
			done:       make(chan struct{}),
			headerChan: make(chan struct{}),
			doneFunc:   callHdr.DoneFunc,
		}

		// Set up transport reader for this stream
		s.trReader = &transportReader{
			reader: &recvBufferReader{
				ctx:     s.ctx,
				ctxDone: s.ctx.Done(),
				recv:    s.buf,
				closeStream: func(err error) {
					s.Close(err)
				},
			},
			windowHandler: func(_ int) {
				// Flow control: for shm transport, we don't need traditional flow control
				// as the ring buffer already provides backpressure
			},
		}

		// Set requestRead callback (required by Stream.ReadMessageHeader)
		// For shared memory transport, tie window updates to application consumption.
		s.requestRead = func(n int) {
			if n <= 0 {
				return
			}
			if wu := s.fc.onRead(uint32(n)); wu > 0 {
				t.sendWindowUpdate(streamID, wu)
			}
		}

		// Register the stream
		t.streams[streamID] = s
		t.streamTransport[s] = t
		t.streamSendQuota[streamID] = int64(maxWindowSize)
		t.streamInFlow[streamID] = s.fc
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

	if err := writeFrame(ctx, t.clientToServer, fh, payload); err != nil {
		t.mu.Lock()
		delete(t.streams, streamID)
		delete(t.streamTransport, s)
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

// incrMsgRecv increments the message received counter.
// This is called by ClientStream.Read() when a message is successfully read.
func (t *ShmClientTransport) incrMsgRecv() {
	// For shm transport, we don't track channelz metrics yet
	// This is a no-op for now, but maintains compatibility with ClientStream
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
	delete(t.streamTransport, s)
	t.sendQuotaMu.Lock()
	delete(t.streamSendQuota, s.id)
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
		_ = writeFrame(context.Background(), t.clientToServer, fh, nil)
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

// write writes data to the stream via the shared memory transport.
// This is called by ClientStream.Write() to send data.
func (t *ShmClientTransport) write(s *ClientStream, hdr []byte, data mem.BufferSlice, opts *WriteOptions) error {
	shmDebugf("[DEBUG] ShmClientTransport.write: stream=%d, hdr_len=%d, data_bytes=%d, ring=%p", s.id, len(hdr), data.Len(), t.clientToServer)
	// Check if transport is closed
	if t.closed.Load() {
		shmDebugf("[DEBUG] ShmClientTransport.write: transport closed")
		return ErrConnClosing
	}

	// Check stream state
	if opts.Last {
		// Last message - transition to write done state
		if !s.compareAndSwapState(streamActive, streamWriteDone) {
			shmDebugf("[DEBUG] ShmClientTransport.write: stream done (Last=true)")
			return errStreamDone
		}
	} else if s.getState() != streamActive {
		shmDebugf("[DEBUG] ShmClientTransport.write: stream not active")
		return errStreamDone
	}

	payloadLen := len(hdr) + data.Len()

	// Enforce outbound flow control: wait for available send window.
	shmDebugf("[DEBUG] ShmClientTransport.write: acquiring send quota for %d bytes", payloadLen)
	if err := t.acquireSendQuota(s.ctx, s.id, payloadLen); err != nil {
		shmDebugf("[DEBUG] ShmClientTransport.write: acquireSendQuota failed: %v", err)
		return err
	}
	shmDebugf("[DEBUG] ShmClientTransport.write: send quota acquired")

	// Write MESSAGE frame. MessageFlagMORE indicates more data will follow.
	fh := FrameHeader{
		StreamID: s.id,
		Type:     FrameTypeMESSAGE,
		Flags:    0,
	}
	if opts != nil && !opts.Last {
		fh.Flags = MessageFlagMORE
	}

	shmDebugf("[DEBUG] ShmClientTransport.write: writing frame to ring, widx before=%d", t.clientToServer.header().WriteIndex())
	if err := writeFrameBuffersChunked(t.clientToServer, fh, hdr, data, 0, s.ctx); err != nil {
		shmDebugf("[ERROR] ShmClientTransport.write: writeFrameBuffersChunked failed: %v", err)
		return err
	}
	shmDebugf("[DEBUG] ShmClientTransport.write: frame written successfully, widx after=%d", t.clientToServer.header().WriteIndex())

	return nil
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
	return writeFrame(t.ctx, t.clientToServer, FrameHeader{Type: FrameTypePING}, data[:])
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
				if err := t.sendPing(); err != nil {
					// Failed to send ping; connection may be broken.
					err = connectionErrorf(true, err, "keepalive failed to send ping")
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
