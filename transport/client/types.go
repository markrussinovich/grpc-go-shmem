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

package client

import "google.golang.org/grpc/internal/transport"

// The following aliases re-export the in-tree transport contract so that an
// out-of-tree transport plugin can name and implement it WITHOUT importing
// google.golang.org/grpc/internal/*. They are the exported, byte-based,
// cross-language-shaped pluggable client transport interface.
//
// POC SCAFFOLD — NOT A FINAL PUBLIC API. These aliases bind directly to internal
// types: ConnectOptions and CallHdr are internal kitchen-sink structs that also
// carry credentials, keepalive, window sizes, stats handlers, channelz, buffer
// pools, and more. Aliasing them makes every field de-facto public and freezes
// internal layout the moment a plugin compiles against it. This is intentional
// only to PROVE method-set sufficiency (a transport can be selected, driven, and
// capability-restricted purely through this surface). A real public API must
// replace these with purpose-built, minimal option structs that copy across only
// what a transport legitimately consumes — see plugin/DESIGN.md.
//
// Note what is NOT in the MANDATORY interface: there is no message-typed
// WriteProto. The required send path is byte-based (ClientStream.Write).
// Marshalling an application message directly into transport-owned memory (the
// INLINE_TX fast path) is instead an OPTIONAL capability, ProtoWriteStream
// below: a transport MAY additionally implement it to recover marshal-into-ring,
// and grpc-go transparently falls back to Write when it is absent or declines.
// Keeping it optional rather than mandatory is what preserves portability — a
// stack or transport without a compatible serialization destination simply omits
// it, while a plugin over a capable engine (e.g. the SHM bridge) forwards it.
type (
	// ClientTransport is a client-side gRPC transport. A plugin's Builder
	// returns one of these.
	ClientTransport = transport.ClientTransport

	// ClientStream is the per-RPC stream a ClientTransport produces. It is
	// purely byte-based: Write sends pre-framed bytes; ReadMessageHeader + Read
	// receive bytes (Read returns a ref-counted mem.BufferSlice that may be
	// backed by transport memory, which is how read-side zero-copy survives a
	// clean transport boundary).
	ClientStream = transport.ClientStreamIface

	// CallHdr carries the per-RPC header information passed to NewStream.
	CallHdr = transport.CallHdr

	// WriteOptions carries per-write flags (e.g. Last).
	WriteOptions = transport.WriteOptions

	// ConnectOptions carries the per-connection options grpc-go passes to a
	// transport at dial time (credentials, keepalive, window sizes, stats, ...).
	ConnectOptions = transport.ConnectOptions

	// OnCloseFunc is invoked when a transport terminates (GOAWAY / drain).
	OnCloseFunc = transport.OnCloseFunc

	// GoAwayReason describes why a transport received a drain signal.
	GoAwayReason = transport.GoAwayReason
)

// ProtoWriteStream is the OPTIONAL INLINE_TX capability a ClientStream MAY
// additionally implement to let grpc-go marshal a protobuf message DIRECTLY into
// transport-owned memory (e.g. an SHM ring), skipping the intermediate marshal
// buffer and payload copy that the mandatory byte Write path incurs.
//
// It is OPTIONAL and detected by interface assertion: a transport that does not
// implement it — or that returns handled=false for a particular message —
// causes grpc-go to transparently use the byte Write path. Contract:
//
//   - handled=false means the message was NOT written by this fast path;
//     grpc-go transparently falls back to the byte Write path. On this path the
//     implementation MUST NOT consume message flow-control quota, write message
//     DATA, or transition terminal write state — though prerequisite metadata
//     such as HEADERS MAY already have been emitted (Write will not re-emit
//     them). It SHOULD return err=nil; grpc-go IGNORES any error returned
//     alongside handled=false, and the byte Write path re-surfaces a genuine
//     terminal error (e.g. a closed transport).
//   - handled=true means the message was accepted and fully serialized; err
//     reports the outcome. The implementation MUST finish reading msg BEFORE
//     returning and MUST NOT retain or access msg afterward, so the caller may
//     reuse the message exactly as it can after the byte Write path.
//
// grpc-go only attempts this path for an uncompressed protobuf message using the
// built-in codec and within the configured max send size; all other cases use
// Write. This mirrors the first-party (monolithic) transport, which implements
// the same capability on its concrete streams.
type ProtoWriteStream interface {
	WriteProto(msg any, opts *WriteOptions) (handled bool, err error)
}
