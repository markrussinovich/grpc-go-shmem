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
// Note what is NOT here: there is no message-typed WriteProto. The send path is
// byte-based (ClientStream.Write); marshalling an application message into the
// transport buffer (the INLINE_TX fast path) is a codec responsibility that is
// not portable across gRPC languages, so it is deliberately excluded from the
// pluggable contract and remains a first-party, monolithic-only optimization.
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
