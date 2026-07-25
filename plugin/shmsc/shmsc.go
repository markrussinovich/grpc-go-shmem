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

// Package shmsc is a SELF-CONTAINED shared-memory (SHM) gRPC transport plugin.
//
// This module owns its ENTIRE SHM engine and depends on grpc-go ONLY through the
// exported, experimental pluggable-transport API (google.golang.org/grpc/
// experimental/transport/{client,server}) plus public packages — it imports NO
// google.golang.org/grpc/internal/* package. That independence is enforced by
// the no-internal-import guard test in this package, and it makes the plugin a
// working proof that the exported API is sufficient for a non-trivial transport
// implemented outside the grpc-go core.
//
// It coexists with the in-tree full-featured SHM transport (which is untouched):
// this plugin registers under the distinct transport type Name ("shmsc").
//
// # Platform scope
//
// Like shared memory itself, this transport targets Linux and Windows only
// (same-host IPC). The engine's ring/segment/wake primitives are built only for
// those OSes; the module is not expected to build for other platforms (e.g.
// darwin), matching the in-tree SHM transport.
//
// STATUS: functional. A gRPC client and server can select this transport end to
// end through the exported D1 registries (client via
// resolver.Address.TransportType == Name, server via the tagged Listener), and
// exchange unary + streaming RPCs — including metadata, trailers, rich status
// (status.WithDetails), flow control, deadlines/cancellation, GOAWAY/graceful
// close, keepalive, and per-RPC credentials — with the module importing NO
// google.golang.org/grpc/internal/* package (enforced by the guard test).
//
// # Known limitations
//
// This is a first, experimental, insecure-focused transport. The following are
// deliberate, documented gaps rather than bugs:
//
//   - Transport security: only an insecure channel is supported. A non-insecure
//     TransportCredentials is REJECTED fail-closed by the builder (it is never
//     silently downgraded). Per-RPC credentials are applied, and one that
//     requires transport security is rejected on the insecure channel. The
//     built-in SHM nonce handshake is a separate, symmetric opt-in.
//   - credentials.RequestInfo is not injected into the context before a per-RPC
//     credential's GetRequestMetadata (the stock transport uses
//     internal/credentials for this; the D1 API exposes no self-contained way).
//     Credentials that read RequestInfoFromContext instead of the audience
//     argument will not observe the method/AuthInfo.
//   - Transport-level stats events (OutHeader/InHeader/InTrailer) are not
//     forwarded; grpc-go's payload and lifecycle stats still fire.
//   - These D1 BuildOptions are not honored: UserAgent, Dialer, BufferPool,
//     MaxHeaderListSize, server HeaderTableSize (the SHM framing does not use
//     HPACK), server ConnectionTimeout, and server Keepalive/KeepalivePolicy
//     from BuildOptions (server keepalive uses the listener configuration).
//   - Malformed inbound frames surface a stream error but do not force
//     connection-level termination; this transport assumes a trusted, same-host
//     peer (both endpoints share the segment).
//   - The engine uses one //go:linkname against the Go runtime
//     (runtime.procyield), on the spin path which is disabled by default.
//   - go.mod carries a replace directive pointing grpc-go at this repository, as
//     the other nested modules here do. Publishing this module independently
//     requires a released grpc-go that exports experimental/transport.
package shmsc

// Name is the resolver.Address.TransportType (and server-side accepted-conn
// transport type) under which this self-contained SHM transport registers. It is
// deliberately distinct from the in-tree "shm" transport so both can coexist.
const Name = "shmsc"
