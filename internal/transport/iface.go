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

package transport

import (
	"context"

	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ClientStreamIface is the contract grpc-go core uses to drive a client-side
// transport stream. It is the in-tree backing type for the exported,
// cross-language-shaped pluggable-transport interface
// (google.golang.org/grpc/transport/client.ClientStream).
//
// Design: the contract is purely BYTE-BASED. The send path takes already-framed
// bytes (Write); the receive path hands ring/buffer-backed bytes upward
// (ReadMessageHeader + Read returning a ref-counted mem.BufferSlice, which is
// how read-side zero-copy survives a clean transport boundary). It deliberately
// does NOT include a message-typed fast path such as WriteProto(msg any) in the
// MANDATORY method set: the required send path is byte-based. Marshalling an
// application message directly into transport memory (INLINE_TX) is instead an
// OPTIONAL capability that a stream MAY additionally implement; core detects it
// by assertion (see writeProtoCapable in the grpc package, exported as
// ProtoWriteStream in transport/client and transport/server) and transparently
// falls back to Write when it is absent or declines. Keeping it optional rather
// than mandatory preserves portability — a plugin whose stack has no compatible
// serialization destination simply omits it — while a plugin over a capable
// engine (e.g. the SHM bridge) forwards it to recover marshal-into-ring.
type ClientStreamIface interface {
	// Write writes the pre-framed hdr and data bytes to the output stream.
	Write(hdr []byte, data mem.BufferSlice, opts *WriteOptions) error

	// ReadMessageHeader and Read form the parser contract; Read returns a
	// ref-counted buffer slice that may be backed by transport memory.
	ReadMessageHeader(header []byte) error
	Read(n int) (mem.BufferSlice, error)
	// RecvCompress reports the inbound message compression algorithm.
	RecvCompress() string

	// Header and Trailer expose the received header/trailer metadata; Status is
	// the RPC status received from the server.
	Header() (metadata.MD, error)
	Trailer() metadata.MD
	Status() *status.Status

	// Context, Done and the retry predicates are what stream.go's finish/retry
	// logic calls.
	Context() context.Context
	Done() <-chan struct{}
	Unprocessed() bool
	TrailersOnly() bool
	BytesReceived() bool
	Close(err error)
}

// ServerStreamIface is the contract grpc-go core uses to drive a server-side
// transport stream. It is the in-tree backing type for the exported pluggable
// interface (google.golang.org/grpc/transport/server.ServerStream).
//
// Like ClientStreamIface it keeps WriteProto out of the MANDATORY method set for
// the same portability reasons; it remains available as the optional
// ProtoWriteStream capability that a capable plugin MAY forward (see the
// ClientStreamIface note above).
type ServerStreamIface interface {
	// Read / ReadMessageHeader form the parser contract.
	ReadMessageHeader(header []byte) error
	Read(n int) (mem.BufferSlice, error)
	RecvCompress() string

	// Write / WriteStatus / header plumbing are the server send path.
	Write(hdr []byte, data mem.BufferSlice, opts *WriteOptions) error
	WriteStatus(st *status.Status) error
	SendHeader(md metadata.MD) error
	SetHeader(md metadata.MD) error
	SetTrailer(md metadata.MD) error
	Header() (metadata.MD, error)
	Trailer() metadata.MD
	HeaderWireLength() int

	// Identity / compression accessors used by server.go handleStream.
	Method() string
	Context() context.Context
	SetContext(ctx context.Context)
	SendCompress() string
	SetSendCompress(name string) error
	ContentSubtype() string
	ClientAdvertisedCompressors() []string
}

// Compile-time checks: the concrete in-tree streams satisfy the byte-based
// interfaces. They additionally implement the optional WriteProto (INLINE_TX)
// fast path, which is deliberately NOT part of the byte interfaces above: core
// detects it by assertion (see writeProtoCapable in the grpc package). Both the
// first-party monolithic transport and a plugin that forwards the optional
// ProtoWriteStream capability (e.g. the SHM bridge) keep INLINE_TX; a plugin
// whose stream implements only the mandatory byte interface falls back to Write.
var (
	_ ClientStreamIface = (*ClientStream)(nil)
	_ ServerStreamIface = (*ServerStream)(nil)
)
