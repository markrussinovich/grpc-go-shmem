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

// Package server defines an EXPERIMENTAL, purpose-built public contract for a
// pluggable server-side gRPC transport, the peer of
// google.golang.org/grpc/experimental/transport/client.
//
// # Stability
//
// EXPERIMENTAL. See the client package doc for the stability statement and the
// byte-based / optional-INLINE_TX design rationale; the same applies here.
//
// # Concurrency and lifetime
//
// Unless a transport documents otherwise: HandleStreams blocks until the
// transport closes and invokes onStream once per accepted stream, possibly
// concurrently; Drain and Close are safe to call concurrently with
// HandleStreams and with each other. Writes on a SINGLE stream are serialized by
// the caller; an implementation MUST NOT retain the hdr or data arguments beyond
// a Write call (data may be kept alive only via a Ref); WriteOptions is passed
// by value and is read-only. grpc-go does not mutate a BuildOptions (or its
// slices/pointers) after Build returns; the transport MAY retain it and its
// reference fields for the connection lifetime.
package server

import (
	"context"
	"net"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// WriteOptions carries per-write flags.
type WriteOptions struct {
	// Last indicates this is the final write on the stream.
	Last bool
}

// BuildOptions carries the per-connection inputs grpc-go supplies to a server
// transport Builder. Purpose-built and by value; no internal types leak.
type BuildOptions struct {
	// Credentials performs the transport-security handshake, or nil for
	// insecure.
	Credentials credentials.TransportCredentials
	// ConnectionTimeout bounds the initial handshake.
	ConnectionTimeout time.Duration
	// MaxConcurrentStreams bounds concurrent streams per connection, or 0 for
	// the transport default.
	MaxConcurrentStreams uint32
	// Keepalive configures server-side keepalive.
	Keepalive keepalive.ServerParameters
	// KeepalivePolicy enforces limits on client keepalive behavior.
	KeepalivePolicy keepalive.EnforcementPolicy
	// InitialWindowSize is the initial per-stream flow-control window, or 0 for
	// the transport default.
	InitialWindowSize uint32
	// InitialConnWindowSize is the initial connection-level flow-control window,
	// or 0 for the transport default.
	InitialConnWindowSize uint32
	// MaxHeaderListSize bounds the decoded header list size, or nil for the
	// transport default.
	MaxHeaderListSize *uint32
	// HeaderTableSize is the HPACK dynamic table size, or nil for the default.
	HeaderTableSize *uint32
	// BufferPool is the pool the transport should use for read/write buffers.
	BufferPool mem.BufferPool
}

// ServerStream is the per-RPC stream grpc-go drives on the server side. The
// mandatory contract is byte-based; the optional INLINE_TX fast path is
// ProtoWriteStream.
type ServerStream interface {
	// ReadMessageHeader and Read form the parser contract.
	ReadMessageHeader(header []byte) error
	Read(n int) (mem.BufferSlice, error)
	// RecvCompress reports the inbound message compression algorithm.
	RecvCompress() string

	// Write / WriteStatus / header plumbing are the server send path. Write MUST
	// NOT retain hdr or data beyond the call; data may be kept alive via a Ref.
	Write(hdr []byte, data mem.BufferSlice, opts WriteOptions) error
	WriteStatus(st *status.Status) error
	SendHeader(md metadata.MD) error
	SetHeader(md metadata.MD) error
	SetTrailer(md metadata.MD) error
	Header() (metadata.MD, error)
	Trailer() metadata.MD
	HeaderWireLength() int

	// Identity / compression accessors used by grpc-go's stream dispatch.
	Method() string
	Context() context.Context
	SetContext(ctx context.Context)
	SendCompress() string
	SetSendCompress(name string) error
	ContentSubtype() string
	ClientAdvertisedCompressors() []string
}

// ProtoWriteStream is the OPTIONAL INLINE_TX capability a ServerStream MAY
// additionally implement; it mirrors client.ProtoWriteStream, including the
// OWNERSHIP semantics of the (handled, err) result: handled=false is a clean
// decline (err MUST be nil; no DATA/quota/terminal-state; safe to fall back to
// the byte Write path), while handled=true means the implementation took
// ownership (err!=nil propagates without byte fallback). A fallible header flush
// that fails MUST be reported as handled=true. See the client-side doc for the
// full contract.
type ProtoWriteStream interface {
	WriteProto(msg proto.Message, size int, opts WriteOptions) (handled bool, err error)
}

// ServerTransport is a server-side gRPC transport produced by a Builder.
type ServerTransport interface {
	// HandleStreams drives inbound streams, invoking onStream once per accepted
	// stream. It blocks until the transport is closed.
	HandleStreams(ctx context.Context, onStream func(ServerStream))
	// Drain begins a graceful drain, sending a GOAWAY with debugData.
	Drain(debugData string)
	// Close tears the transport down with the given error.
	Close(err error)
	// Peer returns the connection peer.
	Peer() *peer.Peer
}

// Builder builds server transports from accepted connections. The plugin's
// listener produces conn; grpc-go selects the Builder by the accepted
// connection's transport type (see the plugin's listener contract).
type Builder interface {
	Build(conn net.Conn, opts BuildOptions) (ServerTransport, error)
}
