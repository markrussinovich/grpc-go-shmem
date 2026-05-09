//go:build linux || windows

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

package grpc

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

const (
	// ShmOfferMDKey is the metadata key sent by the client to offer SHM transport.
	// The value is empty. Per the gRFC, the server ignores this key if it does
	// not support SHM — standard gRPC metadata handling drops unknown keys.
	ShmOfferMDKey = "shm-offer"

	// ShmCtlMDKey is the metadata key returned by the server in trailing
	// metadata. Its value is the name of the SHM control segment that the
	// client should connect to.
	ShmCtlMDKey = "shm-ctl"
)

// ShmDiscoveryServerInterceptors returns unary and stream server interceptors
// that implement the server side of gRFC G3 Transport Discovery.
//
// When the server receives an RPC with the "shm-offer" metadata key, it
// verifies the client is on the same host (loopback peer address) and, if so,
// returns the control segment name in the "shm-ctl" trailing metadata.
//
// The shmCtlSegment parameter is the control segment name the server has
// already created via NewShmListener. The server MUST be Serving on that
// SHM listener concurrently so the client can connect after discovery.
//
// Usage:
//
//	shmLis, _ := grpc.NewShmListener("myservice_<uuid>", nil)
//	unary, stream := grpc.ShmDiscoveryServerInterceptors("myservice_<uuid>")
//	s := grpc.NewServer(
//	    grpc.ChainUnaryInterceptor(unary),
//	    grpc.ChainStreamInterceptor(stream),
//	)
//	go s.Serve(shmLis)   // SHM listener
//	s.Serve(tcpLis)       // TCP listener
func ShmDiscoveryServerInterceptors(shmCtlSegment string) (UnaryServerInterceptor, StreamServerInterceptor) {
	unary := func(ctx context.Context, req any, _ *UnaryServerInfo, handler UnaryHandler) (any, error) {
		maybeSetShmCtl(ctx, shmCtlSegment)
		return handler(ctx, req)
	}

	stream := func(srv any, ss ServerStream, _ *StreamServerInfo, handler StreamHandler) error {
		maybeSetShmCtl(ss.Context(), shmCtlSegment)
		return handler(srv, ss)
	}

	return unary, stream
}

// maybeSetShmCtl checks if the incoming RPC carries "shm-offer" and if the
// peer is on the same host. If both conditions are met, it sets "shm-ctl"
// in the trailing metadata.
func maybeSetShmCtl(ctx context.Context, shmCtlSegment string) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return
	}
	if len(md.Get(ShmOfferMDKey)) == 0 {
		return
	}
	if !isSameHostPeer(ctx) {
		return
	}
	if err := SetTrailer(ctx, metadata.Pairs(ShmCtlMDKey, shmCtlSegment)); err != nil {
		shmLogger.Warningf("failed to set shm-ctl trailer: %v", err)
	}
}

// isSameHostPeer checks whether the peer address is a loopback address,
// indicating the client is on the same host as the server.
//
// Note: this trusts the peer address reported by the transport layer. If
// gRPC is behind a reverse proxy that rewrites peer addresses, a remote
// client could appear to be local. This is an inherent limitation.
func isSameHostPeer(ctx context.Context) bool {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return false
	}
	// Unix-domain sockets are same-host by definition.
	switch p.Addr.Network() {
	case "unix", "unixpacket":
		return true
	}
	host, _, err := net.SplitHostPort(p.Addr.String())
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsLoopback()
}

// ShmOfferContext returns a context with "shm-offer" metadata attached.
// Use this context for the first RPC on an HTTP/2 connection to discover
// whether the server supports SHM transport.
func ShmOfferContext(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, ShmOfferMDKey, "")
}

// ShmCtlFromTrailer extracts the SHM control segment name from trailing
// metadata returned by a server that supports SHM transport discovery.
// Returns empty string if the server did not return "shm-ctl".
func ShmCtlFromTrailer(trailer metadata.MD) string {
	vals := trailer.Get(ShmCtlMDKey)
	if len(vals) > 0 {
		return vals[0]
	}
	return ""
}

// ShmDiscoveryConfig configures the client-side transport discovery behavior.
type ShmDiscoveryConfig struct {
	// ShmDialOptions are passed to the SHM dialer when creating the SHM
	// connection after discovery. If nil, DefaultShmTransportConfig() is used.
	ShmDialOptions []DialOption

	// OnDiscovered is called when the server returns shm-ctl. The callback
	// receives the discovered segment name. Optional; used for logging.
	OnDiscovered func(segment string)
}

// ShmDiscoveryClientInterceptors returns unary and stream client interceptors
// that implement the client side of gRFC G3 Transport Discovery.
//
// On the first RPC, the interceptor injects "shm-offer" into the outgoing
// metadata and reads "shm-ctl" from the trailing metadata. If the server
// returns a control segment name, subsequent RPCs are transparently routed
// over a shared memory connection.
//
// The returned interceptors share state: discovery happens exactly once.
// After the first RPC completes, all subsequent RPCs are pass-through
// (whether SHM was discovered or not).
//
// Usage:
//
//	unary, stream := grpc.ShmDiscoveryClientInterceptors(nil)
//	conn, _ := grpc.NewClient("localhost:50051",
//	    grpc.WithTransportCredentials(insecure.NewCredentials()),
//	    grpc.WithChainUnaryInterceptor(unary),
//	    grpc.WithChainStreamInterceptor(stream),
//	)
//	// First RPC: automatically discovers SHM via shm-offer/shm-ctl.
//	// If server supports SHM, a background SHM connection is established.
//	// Subsequent RPCs continue on the same TCP connection (the interceptor
//	// cannot redirect to a different ClientConn in Go's gRPC model).
//	// For full transparent upgrade, use DialWithShmDiscovery instead.
func ShmDiscoveryClientInterceptors(cfg *ShmDiscoveryConfig) (UnaryClientInterceptor, StreamClientInterceptor) {
	d := &shmDiscoveryState{cfg: cfg}

	unary := func(ctx context.Context, method string, req, reply any, cc *ClientConn, invoker UnaryInvoker, opts ...CallOption) error {
		if !d.done.Load() {
			ctx = ShmOfferContext(ctx)
			var trailer metadata.MD
			opts = append(opts, Trailer(&trailer))
			err := invoker(ctx, method, req, reply, cc, opts...)
			d.tryDiscover(trailer)
			return err
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}

	// Stream interceptor: injects shm-offer but cannot extract shm-ctl
	// from trailers (trailers arrive only when the stream ends, which may
	// be much later). Discovery via streaming RPCs is best-effort — the
	// server sets shm-ctl in trailing metadata, but the client only
	// discovers it if a unary RPC runs first.
	stream := func(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, streamer Streamer, opts ...CallOption) (ClientStream, error) {
		if !d.done.Load() {
			ctx = ShmOfferContext(ctx)
		}
		return streamer(ctx, desc, cc, method, opts...)
	}

	return unary, stream
}

// shmDiscoveryState tracks whether SHM discovery has been attempted.
type shmDiscoveryState struct {
	cfg  *ShmDiscoveryConfig
	done atomic.Bool
}

func (d *shmDiscoveryState) tryDiscover(trailer metadata.MD) {
	if d.done.Swap(true) {
		return // already discovered
	}
	segment := ShmCtlFromTrailer(trailer)
	if segment == "" {
		return
	}
	if d.cfg != nil && d.cfg.OnDiscovered != nil {
		d.cfg.OnDiscovered(segment)
	}
}

// DialWithShmDiscovery dials the target via TCP, performs a single probe
// RPC to discover whether the server supports shared memory transport,
// and returns an SHM connection if available. If the server does not
// support SHM or if the SHM connection fails, the original TCP connection
// is returned instead (graceful fallback).
//
// The probeCall parameter performs the discovery RPC. It receives a context
// with "shm-offer" already injected and a CallOption that captures the
// trailer. The probe call MUST be a real RPC to a method the server
// implements (e.g., a health check or the first application RPC).
//
// Usage:
//
//	conn, err := grpc.DialWithShmDiscovery(ctx, "localhost:50051",
//	    func(cc *grpc.ClientConn, ctx context.Context, opts ...grpc.CallOption) error {
//	        client := pb.NewGreeterClient(cc)
//	        _, err := client.SayHello(ctx, &pb.HelloRequest{Name: "probe"}, opts...)
//	        return err
//	    },
//	    grpc.WithTransportCredentials(insecure.NewCredentials()),
//	)
//	// conn is SHM if server supports it, TCP otherwise
//	client := pb.NewGreeterClient(conn)
func DialWithShmDiscovery(
	ctx context.Context,
	target string,
	probeCall func(cc *ClientConn, ctx context.Context, opts ...CallOption) error,
	opts ...DialOption,
) (*ClientConn, error) {
	// Phase 1: Dial via TCP/HTTP2.
	tcpConn, err := NewClient(target, opts...)
	if err != nil {
		return nil, err
	}

	// Phase 2: Probe RPC with shm-offer.
	probeCtx := ShmOfferContext(ctx)
	var trailer metadata.MD
	probeErr := probeCall(tcpConn, probeCtx, Trailer(&trailer))
	if probeErr != nil {
		// Probe failed — return TCP connection anyway (it may still work
		// for subsequent RPCs; the probe failure is the caller's concern).
		shmLogger.Warningf("SHM discovery probe RPC failed: %v", probeErr)
		return tcpConn, nil
	}

	// Phase 3: Check for shm-ctl.
	segment := ShmCtlFromTrailer(trailer)
	if segment == "" {
		// Server does not support SHM — continue on TCP.
		return tcpConn, nil
	}
	shmLogger.Infof("SHM transport discovered: segment=%s", segment)

	// Phase 4: Dial via SHM.
	shmTarget := "shm://" + segment
	shmOpts := append([]DialOption{}, opts...)
	shmConn, shmErr := NewClient(shmTarget, shmOpts...)
	if shmErr != nil {
		// SHM dial failed — fall back to TCP.
		shmLogger.Warningf("SHM dial to %q failed, using TCP: %v", segment, shmErr)
		return tcpConn, nil
	}

	// Success — close TCP connection and return SHM connection.
	tcpConn.Close()
	return shmConn, nil
}
