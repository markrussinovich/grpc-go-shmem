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
	"sync/atomic"

	"golang.org/x/net/http2"
	client "google.golang.org/grpc/experimental/transport/client"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// clientTransport is the narrow, engine-internal contract the client stream
// uses to drive its owning transport. It is the self-contained analogue of
// grpc-go's internal clientTransport interface, specialized to *shmClientStream
// and dropping the stats-only incrMsgRecv (the SHM transport does not track
// channelz message counters). writeProto additionally threads the validated
// proto size so the transport need not recompute proto.Size.
type clientTransport interface {
	closeStream(s *shmClientStream, err error, rst bool, rstCode http2.ErrCode, st *status.Status, mdata map[string][]string, eosReceived bool)
	write(s *shmClientStream, hdr []byte, data mem.BufferSlice, opts *WriteOptions) error
	writeProto(s *shmClientStream, msg proto.Message, size int, opts *WriteOptions) (bool, error)
	adjustWindow(s *shmClientStream, n uint32)
	updateWindow(s *shmClientStream, n uint32)
}

// shmClientStream implements the D1 client.ClientStream (and the optional
// client.ProtoWriteStream INLINE_TX fast path) for the SHM transport. It embeds
// streamBase for the common transport-layer state and reads/writes.
type shmClientStream struct {
	streamBase // Embed for common stream functionality.

	ct       clientTransport
	done     chan struct{} // closed at the end of stream to unblock writers.
	doneFunc func()        // invoked at the end of stream.

	headerChan chan struct{} // closed to indicate the end of header metadata.
	header     metadata.MD   // the received header metadata

	status *status.Status // the status error received from the server

	// Non-pointer fields are at the end to optimize GC allocations.

	// headerValid indicates whether a valid header was received. Only
	// meaningful after headerChan is closed (always call waitOnHeader() before
	// reading its value).
	headerValid      bool
	noHeaders        bool        // set if the client never received headers (set only after the stream is done).
	headerChanClosed uint32      // set when headerChan is closed. Used to avoid closing headerChan multiple times.
	bytesReceived    atomic.Bool // indicates whether any bytes have been received on this stream
	unprocessed      atomic.Bool // set if the server sends a refused stream or GOAWAY including this stream
}

var _ client.ClientStream = (*shmClientStream)(nil)
var _ client.ProtoWriteStream = (*shmClientStream)(nil)

// Read reads an n byte message from the input stream.
func (s *shmClientStream) Read(n int) (mem.BufferSlice, error) {
	return s.streamBase.read(n)
}

// Close closes the stream and propagates err to any readers.
func (s *shmClientStream) Close(err error) {
	var (
		rst     bool
		rstCode http2.ErrCode
	)
	if err != nil {
		rst = true
		rstCode = http2.ErrCodeCancel
	}
	s.ct.closeStream(s, err, rst, rstCode, status.Convert(err), nil, false)
}

// Write writes the hdr and data bytes to the output stream.
func (s *shmClientStream) Write(hdr []byte, data mem.BufferSlice, opts client.WriteOptions) error {
	return s.ct.write(s, hdr, data, &WriteOptions{Last: opts.Last})
}

// WriteProto attempts zero-copy serialization of msg directly into the
// transport's ring buffer. size is the validated proto.Size(msg) and is
// forwarded without recomputation. Returns (true, err) if handled, (false, nil)
// to fall back to the byte Write path.
func (s *shmClientStream) WriteProto(msg proto.Message, size int, opts client.WriteOptions) (bool, error) {
	return s.ct.writeProto(s, msg, size, &WriteOptions{Last: opts.Last})
}

// BytesReceived indicates whether any bytes have been received on this stream.
func (s *shmClientStream) BytesReceived() bool {
	return s.bytesReceived.Load()
}

// Unprocessed indicates whether the server did not process this stream --
// i.e. it sent a refused stream or GOAWAY including this stream ID.
func (s *shmClientStream) Unprocessed() bool {
	return s.unprocessed.Load()
}

func (s *shmClientStream) waitOnHeader() {
	select {
	case <-s.ctx.Done():
		// Close the stream to prevent headers/trailers from changing after
		// this function returns.
		s.Close(ContextErr(s.ctx.Err()))
		// headerChan could possibly not be closed yet if closeStream raced
		// with operateHeaders; wait until it is closed explicitly here.
		<-s.headerChan
	case <-s.headerChan:
	}
}

// RecvCompress returns the compression algorithm applied to the inbound
// message. It is empty string if there is no compression applied.
func (s *shmClientStream) RecvCompress() string {
	s.waitOnHeader()
	return s.recvCompress
}

// Done returns a channel which is closed when it receives the final status
// from the server.
func (s *shmClientStream) Done() <-chan struct{} {
	return s.done
}

// Header returns the header metadata of the stream. It blocks until i) the
// metadata is ready or ii) there is no header metadata or iii) the stream is
// canceled/expired.
func (s *shmClientStream) Header() (metadata.MD, error) {
	s.waitOnHeader()

	if !s.headerValid || s.noHeaders {
		return nil, s.status.Err()
	}

	return s.header.Copy(), nil
}

// TrailersOnly blocks until a header or trailers-only frame is received and
// then returns true if the stream was trailers-only.
func (s *shmClientStream) TrailersOnly() bool {
	s.waitOnHeader()
	return s.noHeaders
}

// Status returns the status received from the server. It can be read safely
// only after the stream has ended, that is, after Done() is closed.
func (s *shmClientStream) Status() *status.Status {
	return s.status
}

func (s *shmClientStream) requestRead(n int) {
	s.ct.adjustWindow(s, uint32(n))
}

func (s *shmClientStream) updateWindow(n int) {
	s.ct.updateWindow(s, uint32(n))
}
