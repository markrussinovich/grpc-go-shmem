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

// Package shm plugs the shared-memory (SHM) gRPC transport into grpc-go through
// the exported pluggable-transport registries (transport/client, transport/server).
// Import it for its side effects:
//
//	import _ "google.golang.org/grpc/plugin/shm"
//
// and dial an address whose resolver.Address.TransportType == shm.Name.
//
// # Implementation status: a thin bridge over the existing engine
//
// The types in this package — the bridge* transports — are a deliberately THIN
// bridge layer. They implement the exported byte-based plugin contract, but they
// do NOT reimplement SHM: they delegate every operation to the full,
// already-developed in-tree SHM engine in internal/transport. The bridge returns
// the engine's streams DIRECTLY: those streams already implement the mandatory
// byte interface AND the OPTIONAL INLINE_TX capability (WriteProto, see
// transport/client.ProtoWriteStream), so the plugin marshals protobuf directly
// into the SHM ring exactly as the first-party monolithic transport does. Core
// detects WriteProto by assertion and, for any transport whose stream lacks it,
// transparently uses the byte Write path — which is what keeps the contract
// portable. No per-stream wrapper is used, so there is no per-RPC wrapping cost.
//
// This is intentional for the POC: it shows a working, contract-conformant plugin
// today, on top of the full engine, without rewriting the ~40k-line engine. The
// planned next phase decouples that engine out of internal/ into a standalone
// implementation owned by this package; see plugin/README.md and plugin/DESIGN.md.
package shm

import (
	"context"

	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/stats"
	transportclient "google.golang.org/grpc/transport/client"
)

// Name is the resolver.Address.TransportType under which the SHM transport is
// registered.
const Name = "shm"

func init() {
	transportclient.Register(Name, clientBuilder{})
}

type clientBuilder struct{}

// Build dials an SHM client transport using the full in-tree engine
// (transport.NewShmClient), then wraps it in the thin bridge below. The bridge
// adds no transport logic: it forwards the transport lifecycle and returns the
// engine's streams directly, which already implement the exported contract (the
// byte interface + the optional WriteProto/INLINE_TX capability). Decoupling the
// engine out of internal/ into a standalone implementation owned by this package
// is the planned next phase (see the package doc).
func (clientBuilder) Build(connectCtx, ctx context.Context, addr resolver.Address, opts transportclient.BuildOptions) (transport.ClientTransport, error) {
	inner, err := transport.NewShmClient(connectCtx, ctx, addr, opts.ConnectOptions, opts.OnClose)
	if err != nil {
		return nil, err
	}
	return &bridgeClientTransport{inner: inner}, nil
}

// bridgeClientTransport is the thin bridge from the exported byte-based plugin
// contract to the full in-tree SHM client transport. Every method forwards to the
// engine; NewStream returns the engine stream directly (it already implements the
// exported contract, including the optional WriteProto capability).
type bridgeClientTransport struct {
	inner transport.ClientTransport
}

var _ transport.ClientTransport = (*bridgeClientTransport)(nil)

func (p *bridgeClientTransport) NewStream(ctx context.Context, callHdr *transport.CallHdr, handler stats.Handler) (transport.ClientStreamIface, error) {
	// Return the engine stream directly. It already implements the full
	// exported contract — the mandatory byte interface AND the optional
	// WriteProto (INLINE_TX) capability (transportclient.ProtoWriteStream) —
	// so no per-stream wrapper is needed. Core detects WriteProto by
	// assertion and uses marshal-into-ring exactly like the monolithic
	// transport. Avoiding the wrapper also removes a per-RPC allocation
	// (measurable on unary; amortised on streaming).
	return p.inner.NewStream(ctx, callHdr, handler)
}

func (p *bridgeClientTransport) Close(err error)         { p.inner.Close(err) }
func (p *bridgeClientTransport) GracefulClose()          { p.inner.GracefulClose() }
func (p *bridgeClientTransport) Error() <-chan struct{}  { return p.inner.Error() }
func (p *bridgeClientTransport) GoAway() <-chan struct{} { return p.inner.GoAway() }
func (p *bridgeClientTransport) GetGoAwayReason() (transport.GoAwayReason, string) {
	return p.inner.GetGoAwayReason()
}
func (p *bridgeClientTransport) Peer() *peer.Peer { return p.inner.Peer() }
