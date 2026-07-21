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

package shm

import (
	"context"
	"net"

	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/peer"
	transportserver "google.golang.org/grpc/transport/server"
)

func init() {
	transportserver.Register(Name, serverBuilder{})
}

type serverBuilder struct{}

// Build wraps the full in-tree SHM server transport (transport.NewServerTransport)
// in the thin bridge below. Each stream handed to grpc.Server is the engine's
// stream directly, which already implements the exported contract (the byte
// interface + the optional WriteProto/INLINE_TX capability), symmetric to the
// client side.
func (serverBuilder) Build(conn net.Conn, opts transportserver.BuildOptions) (transport.ServerTransport, error) {
	inner, err := transport.NewServerTransport(conn, opts.Config)
	if err != nil {
		return nil, err
	}
	return &bridgeServerTransport{inner: inner}, nil
}

// bridgeServerTransport is the thin bridge from the exported byte-based plugin
// contract to the full in-tree SHM server transport. HandleStreams passes each
// accepted engine stream directly to the grpc.Server handler (the engine stream
// already implements the exported contract, including the optional WriteProto
// capability).
type bridgeServerTransport struct {
	inner transport.ServerTransport
}

var _ transport.ServerTransport = (*bridgeServerTransport)(nil)

func (p *bridgeServerTransport) HandleStreams(ctx context.Context, handle func(transport.ServerStreamIface)) {
	p.inner.HandleStreams(ctx, handle)
}

func (p *bridgeServerTransport) Close(err error)        { p.inner.Close(err) }
func (p *bridgeServerTransport) Peer() *peer.Peer       { return p.inner.Peer() }
func (p *bridgeServerTransport) Drain(debugData string) { p.inner.Drain(debugData) }

// NewListener returns a net.Listener that serves the SHM transport over the
// named segment. Pass it to grpc.Server.Serve:
//
//	lis, _ := shm.NewListener("my_segment")
//	s := grpc.NewServer()
//	s.Serve(lis)
//
// Accepted connections are tagged with the "shm" transport type so grpc-go
// selects the registered server builder above — symmetric to client-side
// selection by resolver.Address.TransportType. The SHM engine's data path is
// reused unchanged.
func NewListener(segmentName string) (net.Listener, error) {
	return NewListenerWithSizes(
		segmentName,
		transport.DefaultSegmentSize,
		transport.DefaultRingASize,
		transport.DefaultRingASize,
	)
}

// NewListenerWithSizes is NewListener with explicit segment and per-direction
// ring sizes. It exists mainly so benchmarks can match the segment/ring sizing
// the in-tree SHM benchmark uses for an apples-to-apples comparison.
func NewListenerWithSizes(segmentName string, segSize, ringASize, ringBSize uint64) (net.Listener, error) {
	inner, err := transport.NewShmListener(
		&transport.ShmAddr{Name: segmentName},
		segSize,
		ringASize,
		ringBSize,
	)
	if err != nil {
		return nil, err
	}
	return &taggedListener{Listener: inner}, nil
}

// taggedListener wraps the SHM listener so each accepted connection advertises
// its transport type to grpc-go's server-side selection.
type taggedListener struct{ net.Listener }

func (l *taggedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return taggedConn{Conn: c}, nil
}

// taggedConn adds TransportType to the accepted connection while forwarding the
// internal ServerTransportProvider seam that carries the prebuilt transport.
type taggedConn struct{ net.Conn }

func (taggedConn) TransportType() string { return Name }

func (c taggedConn) GetServerTransport() transport.ServerTransport {
	return c.Conn.(transport.ServerTransportProvider).GetServerTransport()
}
