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
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	server "google.golang.org/grpc/experimental/transport/server"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// serverTransport is the narrow, engine-internal contract the server stream
// uses to drive its owning transport. It is the self-contained analogue of
// grpc-go's internal internalServerTransport interface, specialized to
// *shmServerStream and dropping both the embedded public ServerTransport (the
// stream never calls those) and the stats-only incrMsgRecv. writeProto threads
// the validated proto size so the transport need not recompute proto.Size.
type serverTransport interface {
	writeHeader(s *shmServerStream, md metadata.MD) error
	write(s *shmServerStream, hdr []byte, data mem.BufferSlice, opts *WriteOptions) error
	writeStatus(s *shmServerStream, st *status.Status) error
	writeProto(s *shmServerStream, msg proto.Message, size int, opts *WriteOptions) (bool, error)
	adjustWindow(s *shmServerStream, n uint32)
	updateWindow(s *shmServerStream, n uint32)
}

// shmServerStream implements the D1 server.ServerStream (and the optional
// server.ProtoWriteStream INLINE_TX fast path) for the SHM transport. It embeds
// streamBase for the common transport-layer state and reads/writes.
type shmServerStream struct {
	streamBase // Embed for common stream functionality.

	st      serverTransport
	ctxDone <-chan struct{} // closed at the end of stream. Cache of ctx.Done() (for performance)
	// cancel is invoked at the end of stream to cancel ctx. It also stops the
	// timer for monitoring the rpc deadline if configured.
	cancel func()

	// Holds compressor names passed in grpc-accept-encoding metadata from the
	// client.
	clientAdvertisedCompressors string

	// hdrMu protects outgoing header and trailer metadata.
	hdrMu      sync.Mutex
	header     metadata.MD // the outgoing header metadata. Updated by WriteHeader.
	headerSent atomic.Bool // atomically set when the headers are sent out.

	headerWireLength int
}

var _ server.ServerStream = (*shmServerStream)(nil)
var _ server.ProtoWriteStream = (*shmServerStream)(nil)

// Read reads an n byte message from the input stream.
func (s *shmServerStream) Read(n int) (mem.BufferSlice, error) {
	return s.streamBase.read(n)
}

// SendHeader sends the header metadata for the given stream.
func (s *shmServerStream) SendHeader(md metadata.MD) error {
	return s.st.writeHeader(s, md)
}

// Write writes the hdr and data bytes to the output stream.
func (s *shmServerStream) Write(hdr []byte, data mem.BufferSlice, opts server.WriteOptions) error {
	return s.st.write(s, hdr, data, &WriteOptions{Last: opts.Last})
}

// WriteProto attempts zero-copy serialization of msg directly into the
// transport's ring buffer. size is the validated proto.Size(msg) and is
// forwarded without recomputation. Returns (true, err) if handled, (false, nil)
// to fall back to the byte Write path.
func (s *shmServerStream) WriteProto(msg proto.Message, size int, opts server.WriteOptions) (bool, error) {
	return s.st.writeProto(s, msg, size, &WriteOptions{Last: opts.Last})
}

// WriteStatus sends the status of a stream to the client. WriteStatus is the
// final call made on a stream and always occurs.
func (s *shmServerStream) WriteStatus(st *status.Status) error {
	return s.st.writeStatus(s, st)
}

// isHeaderSent indicates whether headers have been sent.
func (s *shmServerStream) isHeaderSent() bool {
	return s.headerSent.Load()
}

// updateHeaderSent updates headerSent and returns true if it was already set.
func (s *shmServerStream) updateHeaderSent() bool {
	return s.headerSent.Swap(true)
}

// RecvCompress returns the compression algorithm applied to the inbound
// message. It is empty string if there is no compression applied.
func (s *shmServerStream) RecvCompress() string {
	return s.recvCompress
}

// SendCompress returns the send compressor name.
func (s *shmServerStream) SendCompress() string {
	return s.sendCompress
}

// ContentSubtype returns the content-subtype for a request. For example, a
// content-subtype of "proto" will result in a content-type of
// "application/grpc+proto". This will always be lowercase.
func (s *shmServerStream) ContentSubtype() string {
	return s.contentSubtype
}

// SetSendCompress sets the compression algorithm to the stream.
func (s *shmServerStream) SetSendCompress(name string) error {
	if s.isHeaderSent() || s.getState() == streamDone {
		return errors.New("transport: set send compressor called after headers sent or stream done")
	}

	s.sendCompress = name
	return nil
}

// SetContext sets the context of the stream. This will be deleted once the
// stats handler callouts all move to the gRPC layer.
func (s *shmServerStream) SetContext(ctx context.Context) {
	s.ctx = ctx
}

// ClientAdvertisedCompressors returns the compressor names advertised by the
// client via grpc-accept-encoding header.
func (s *shmServerStream) ClientAdvertisedCompressors() []string {
	values := strings.Split(s.clientAdvertisedCompressors, ",")
	for i, v := range values {
		values[i] = strings.TrimSpace(v)
	}
	return values
}

// Header returns the header metadata of the stream. It returns the out header
// after WriteHeader is called. It does not block and must not be called until
// after WriteHeader.
func (s *shmServerStream) Header() (metadata.MD, error) {
	return s.header.Copy(), nil
}

// HeaderWireLength returns the size of the headers of the stream as received
// from the wire.
func (s *shmServerStream) HeaderWireLength() int {
	return s.headerWireLength
}

// SetHeader sets the header metadata. This can be called multiple times. This
// should not be called in parallel to other data writes.
func (s *shmServerStream) SetHeader(md metadata.MD) error {
	if md.Len() == 0 {
		return nil
	}
	if s.isHeaderSent() || s.getState() == streamDone {
		return ErrIllegalHeaderWrite
	}
	s.hdrMu.Lock()
	s.header = metadata.Join(s.header, md)
	s.hdrMu.Unlock()
	return nil
}

// SetTrailer sets the trailer metadata which will be sent with the RPC status
// by the server. This can be called multiple times. This should not be called
// in parallel to other data writes.
func (s *shmServerStream) SetTrailer(md metadata.MD) error {
	if md.Len() == 0 {
		return nil
	}
	if s.getState() == streamDone {
		return ErrIllegalHeaderWrite
	}
	s.hdrMu.Lock()
	s.trailer = metadata.Join(s.trailer, md)
	s.hdrMu.Unlock()
	return nil
}

func (s *shmServerStream) requestRead(n int) {
	s.st.adjustWindow(s, uint32(n))
}

func (s *shmServerStream) updateWindow(n int) {
	s.st.updateWindow(s, uint32(n))
}
