//go:build linux || windows

/*
 *
 * Copyright 2026 gRPC authors.
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

package engine

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"

	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
)

// http2MaxFrameLen is the maximum length of a DATA payload the engine emits per
// frame. Larger messages are split into multiple frames.
const http2MaxFrameLen = 16384 // 16KB frame

// WriteOptions is the engine-internal write option set. It mirrors the single
// public D1 WriteOptions field; the stream shells translate to/from the D1
// boundary type. Passed by pointer on the internal transport interfaces to
// match the ported frame-writer call sites.
type WriteOptions struct {
	// Last indicates this is the final write on the stream (half-close).
	Last bool
}

// streamState tracks the lifecycle of a stream. The value is stored in
// streamBase.state and mutated atomically.
type streamState uint32

const (
	streamActive    streamState = iota
	streamWriteDone             // EndStream sent
	streamReadDone              // EndStream received
	streamDone                  // the entire stream is finished.
)

// readRequester is used to state application's intentions to read data. This
// is used to adjust flow control, if needed.
type readRequester interface {
	requestRead(int)
}

// streamBase carries the transport-layer state shared by the SHM client and
// server streams. It is the self-contained engine analogue of grpc-go's
// internal transport Stream, MINUS the HTTP/2-only writeQuota and the unused
// connWaiterElem list hook (neither is referenced on the SHM data path), and
// PLUS an embedded noCopy so vet flags accidental value copies.
//
// The SHM-specific atomic fields (pendingWU/pendingWUDirty/shmDeferred/
// protoInFlight/statusSent/shmDataDropped/sendQuota) are driven by the frame
// writer's lockless flow-control and whole-message dispatch paths; see
// shm_frame_writer.go for their invariants.
type streamBase struct {
	_ noCopy

	ctx          context.Context // the associated context of the stream
	method       string          // the associated RPC method of the stream
	recvCompress string
	sendCompress string

	readRequester readRequester

	// contentSubtype is the content-subtype for requests.
	// this must be lowercase or the behavior is undefined.
	contentSubtype string

	trailer metadata.MD // the key-value map of trailer metadata.

	// Non-pointer fields are at the end to optimize GC performance.
	state    streamState
	id       uint32
	buf      recvBuffer
	trReader transportReader
	fc       inFlow

	// pendingWU accumulates inbound flow-control credit (in bytes) that the
	// receiver owes the peer as a WINDOW_UPDATE on this stream. Atomic so
	// producers can update it without a transport-wide mutex; the SHM
	// transport's lockless WU path drains it.
	pendingWU atomic.Uint32

	// pendingWUDirty is the dirty-list membership flag for the SHM transport's
	// drainPendingWUForWriter restore path. The producer that wins the 0->1
	// CAS enqueues this stream into the transport's wuDirty slice; the writer
	// goroutine clears it 1->0 BEFORE Swap'ing pendingWU (clear-first is the
	// lost-WU prevention invariant).
	pendingWUDirty atomic.Bool

	// shmDeferred is the inline-allocated state for an in-flight whole-message
	// DATA emit. gRPC's one-SendMsg-at-a-time-per-direction contract means a
	// single slot per stream suffices. Embedding by value avoids per-message
	// sync.Pool/heap churn; terminal paths null out cur.data/cur.lpmHdr after
	// Free so the GC can reclaim the buffers.
	shmDeferred deferredMessage

	// protoInFlight counts ZC proto entries (writeProto's async path) currently
	// pending for this stream. Used by writeProto's inline TryLock path to
	// enforce per-stream message order: a positive count means an async entry
	// is already ahead in the pipeline, so the sender must also enqueue async.
	protoInFlight atomic.Int32

	// statusSent is the idempotence guard for the server writeStatus path,
	// decoupled from the streamDone state transition by the trailer-sentinel
	// design so in-flight async DATA is not dropped when the status is queued.
	statusSent atomic.Bool

	// shmDataDropped is a sticky writer tombstone: set when the frame writer
	// drops at least one outbound DATA entry for this stream. It means "no
	// further outbound DATA or successful OK TRAILERS", WITHOUT claiming the
	// stream-teardown ownership that streamDone carries (which is reserved for
	// a real closer that closes the client stream's done channel).
	shmDataDropped atomic.Bool

	// sendQuota is the per-stream outbound flow-control window (bytes),
	// reserved via a lockless CAS loop in acquireSendQuota paired with the
	// transport-level connSendQuota. Initialized at NewStream to the initial
	// stream window; grown by inbound stream WINDOW_UPDATE; rolled back on a
	// racing two-resource CAS.
	sendQuota atomic.Int64
}

func (s *streamBase) swapState(st streamState) streamState {
	return streamState(atomic.SwapUint32((*uint32)(&s.state), uint32(st)))
}

func (s *streamBase) compareAndSwapState(oldState, newState streamState) bool {
	return atomic.CompareAndSwapUint32((*uint32)(&s.state), uint32(oldState), uint32(newState))
}

func (s *streamBase) getState() streamState {
	return streamState(atomic.LoadUint32((*uint32)(&s.state)))
}

// Trailer returns the cached trailer metadata. It can be safely read only after
// the stream has ended (read or write returned io.EOF); otherwise it may return
// an empty MD.
func (s *streamBase) Trailer() metadata.MD {
	return s.trailer.Copy()
}

// Context returns the context of the stream.
func (s *streamBase) Context() context.Context {
	return s.ctx
}

// Method returns the method for the stream.
func (s *streamBase) Method() string {
	return s.method
}

func (s *streamBase) write(m recvMsg) {
	s.buf.put(m)
}

// drainRecvBuffer frees any queued message buffers to release underlying ring
// reservations when a stream is being torn down without consuming all data.
func (s *streamBase) drainRecvBuffer() {
	if s == nil {
		return
	}
	s.buf.drainAndFree()
}

// ReadMessageHeader reads data into the provided header slice from the stream.
// It first returns any error from a previous read. If an io.EOF is encountered
// after partial data, it is converted to io.ErrUnexpectedEOF.
func (s *streamBase) ReadMessageHeader(header []byte) (err error) {
	// Don't request a read if there was an error earlier
	if er := s.trReader.er; er != nil {
		return er
	}
	s.readRequester.requestRead(len(header))
	for len(header) != 0 {
		n, err := s.trReader.ReadMessageHeader(header)
		header = header[n:]
		if len(header) == 0 {
			err = nil
		}
		if err != nil {
			if n > 0 && err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
	}
	return nil
}

// ceil returns the ceil after dividing the numerator and denominator while
// avoiding integer overflows.
func ceil(numerator, denominator int) int {
	if numerator == 0 {
		return 0
	}
	return (numerator-1)/denominator + 1
}

// read reads n bytes from the wire for this stream.
func (s *streamBase) read(n int) (data mem.BufferSlice, err error) {
	// Don't request a read if there was an error earlier
	if er := s.trReader.er; er != nil {
		return nil, er
	}
	// gRPC Go accepts data frames with a maximum length of 16KB. Larger
	// messages must be split into multiple frames. We pre-allocate the
	// buffer to avoid resizing during the read loop, but cap the initial
	// capacity to 128 frames (2MB) to prevent over-allocation or panics
	// when reading extremely large streams.
	allocCap := min(ceil(n, http2MaxFrameLen), 128)
	data = make(mem.BufferSlice, 0, allocCap)
	s.readRequester.requestRead(n)
	for n != 0 {
		buf, err := s.trReader.Read(n)
		var bufLen int
		if buf != nil {
			bufLen = buf.Len()
		}
		n -= bufLen
		if n == 0 {
			err = nil
		}
		if err != nil {
			if bufLen > 0 && err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			data.Free()
			return nil, err
		}
		data = append(data, buf)
	}
	return data, nil
}

// GoString is implemented so that printing %#v of a stream won't race.
func (s *streamBase) GoString() string {
	return fmt.Sprintf("<stream: %p, %v>", s, s.method)
}
