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
// does NOT expose a message-typed fast path such as WriteProto(msg any):
// marshalling an application message is a codec responsibility (user-pluggable
// in every gRPC language) and is not portable across C++/Java/Python/Go, so
// "marshal directly into the transport buffer" (INLINE_TX) stays a first-party,
// monolithic-only optimization rather than part of the pluggable contract. Core
// still uses that optimization opportunistically via an optional capability
// assertion (see writeProtoCapable in the grpc package) when the concrete stream
// offers it; a plugin that implements only this interface simply does not, and
// falls back to Write.
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
// Like ClientStreamIface it is byte-based and excludes WriteProto for the same
// portability reasons; the send-side zero-copy a plugin retains is the single
// contiguous Write into transport memory, not marshal-into-buffer.
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
// detects it by assertion (see writeProtoCapable in the grpc package), so the
// first-party monolithic transport keeps INLINE_TX while a byte-only plugin does
// not.
var (
	_ ClientStreamIface = (*ClientStream)(nil)
	_ ServerStreamIface = (*ServerStream)(nil)
)
