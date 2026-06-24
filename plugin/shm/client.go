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
// The types in this package — the bridge* transports and streams — are a
// deliberately THIN bridge layer. They implement the exported byte-based plugin
// contract, but they do NOT reimplement SHM: they delegate every operation to the
// full, already-developed in-tree SHM engine in internal/transport. The only
// thing the bridge adds is contract conformance — it presents ONLY the byte
// interface and drops the engine stream's WriteProto (INLINE_TX) fast path — so
// the plugin strictly obeys the cross-language plugin constraint.
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
// (transport.NewShmClient), then wraps it in the thin bridge below. The bridge is
// the plugin's whole implementation: it adds no transport logic, it only enforces
// the byte-only plugin contract by hiding the engine stream's WriteProto
// (INLINE_TX) fast path, so the plugin uses the portable Write path. Decoupling
// the engine out of internal/ into a standalone implementation owned by this
// package is the planned next phase (see the package doc).
func (clientBuilder) Build(connectCtx, ctx context.Context, addr resolver.Address, opts transportclient.BuildOptions) (transport.ClientTransport, error) {
	inner, err := transport.NewShmClient(connectCtx, ctx, addr, opts.ConnectOptions, opts.OnClose)
	if err != nil {
		return nil, err
	}
	return &bridgeClientTransport{inner: inner}, nil
}

// bridgeClientTransport is the thin bridge from the exported byte-based plugin
// contract to the full in-tree SHM client transport. Every method forwards to the
// engine; NewStream additionally wraps the returned stream to drop the
// non-portable WriteProto fast path.
type bridgeClientTransport struct {
	inner transport.ClientTransport
}

var _ transport.ClientTransport = (*bridgeClientTransport)(nil)

func (p *bridgeClientTransport) NewStream(ctx context.Context, callHdr *transport.CallHdr, handler stats.Handler) (transport.ClientStreamIface, error) {
	s, err := p.inner.NewStream(ctx, callHdr, handler)
	if err != nil {
		return nil, err
	}
	return bridgeClientStream{s}, nil
}

func (p *bridgeClientTransport) Close(err error)         { p.inner.Close(err) }
func (p *bridgeClientTransport) GracefulClose()          { p.inner.GracefulClose() }
func (p *bridgeClientTransport) Error() <-chan struct{}  { return p.inner.Error() }
func (p *bridgeClientTransport) GoAway() <-chan struct{} { return p.inner.GoAway() }
func (p *bridgeClientTransport) GetGoAwayReason() (transport.GoAwayReason, string) {
	return p.inner.GetGoAwayReason()
}
func (p *bridgeClientTransport) Peer() *peer.Peer { return p.inner.Peer() }

// bridgeClientStream embeds only the byte-based stream interface. Because that
// interface does not declare WriteProto, this wrapper type does not expose it
// either, so core's optional INLINE_TX capability assertion fails and the
// standard Write path is used.
type bridgeClientStream struct {
	transport.ClientStreamIface
}
