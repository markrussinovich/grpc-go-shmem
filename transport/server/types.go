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

package server

import "google.golang.org/grpc/internal/transport"

// The following aliases re-export the in-tree transport contract so that an
// out-of-tree transport plugin can name and implement it WITHOUT importing
// google.golang.org/grpc/internal/*. They are the exported, byte-based,
// cross-language-shaped pluggable server transport interface.
//
// POC SCAFFOLD — NOT A FINAL PUBLIC API. ServerConfig is an internal
// kitchen-sink struct; aliasing it makes its fields de-facto public and freezes
// internal layout. This is intentional only to prove method-set sufficiency; a
// real public API must replace it with a purpose-built minimal option struct
// (see plugin/DESIGN.md).
//
// As on the client side, there is no message-typed WriteProto in the MANDATORY
// interface: the required send path is byte-based (ServerStream.Write).
// Marshal-into-transport-memory (INLINE_TX) is instead the OPTIONAL
// ProtoWriteStream capability below, which a capable transport MAY implement and
// grpc-go transparently falls back from to Write when it is absent or declines.
type (
	// ServerTransport is a server-side gRPC transport. It is driven by grpc-go's
	// push model: grpc.Server calls HandleStreams and the transport invokes the
	// handler once per accepted stream.
	ServerTransport = transport.ServerTransport

	// ServerStream is the per-RPC stream handed to the HandleStreams handler. It
	// is byte-based, mirroring ClientStream.
	ServerStream = transport.ServerStreamIface

	// WriteOptions carries per-write flags (e.g. Last).
	WriteOptions = transport.WriteOptions

	// ServerConfig carries the server-side transport configuration.
	ServerConfig = transport.ServerConfig
)

// ProtoWriteStream is the OPTIONAL INLINE_TX capability a ServerStream MAY
// additionally implement, mirroring transport/client.ProtoWriteStream. It lets
// grpc-go marshal a protobuf response message directly into transport-owned
// memory (e.g. an SHM ring) instead of into an intermediate buffer that Write
// then copies. It is detected by assertion and auto-falls-back to Write; see the
// client-side ProtoWriteStream doc for the full contract.
type ProtoWriteStream interface {
	WriteProto(msg any, opts *WriteOptions) (handled bool, err error)
}
