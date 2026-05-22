//go:build !linux && !windows

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

// Package shm provides experimental shared-memory transport helpers for
// gRPC. On platforms without SHM transport support, the exported API is
// still available so portable programs can compile; SHM-specific entry
// points return clear unsupported-platform errors.
//
// Notice: This package is EXPERIMENTAL and may be changed or removed in
// a later release.
package shm

import (
	"context"
	"fmt"
	"net"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/metadata"
)

const (
	// OfferMDKey is the metadata key sent by the client to offer SHM
	// transport.
	//
	// Notice: This API is EXPERIMENTAL and may be changed or removed in
	// a later release.
	OfferMDKey = "shm-offer"

	// CtlMDKey is the metadata key returned by the server in trailing
	// metadata when SHM transport is available.
	//
	// Notice: This API is EXPERIMENTAL and may be changed or removed in
	// a later release.
	CtlMDKey = "shm-ctl"
)

// Config contains configuration options for the shared-memory transport.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
type Config struct {
	DialOptions         *transport.DialOptions
	FallbackEnabled     bool
	TCPFallbackAddr     string
	AllowMixedTransport bool
	SingleStreamMode    bool
}

// DefaultConfig returns the default Config.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func DefaultConfig() *Config {
	return &Config{
		DialOptions:         transport.DefaultDialOptions(),
		FallbackEnabled:     true,
		AllowMixedTransport: true,
	}
}

// WithTransport returns a grpc.DialOption that configures the client to
// use shared memory transport.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func WithTransport() grpc.DialOption {
	return WithTransportAndOptions(nil)
}

// WithTransportAndOptions returns a grpc.DialOption that configures the
// client to use shared memory transport with custom transport-level
// options.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func WithTransportAndOptions(opts *transport.DialOptions) grpc.DialOption {
	cfg := DefaultConfig()
	if opts != nil {
		cfg.DialOptions = opts
	}
	return WithTransportConfig(cfg)
}

// WithTransportConfig returns a grpc.DialOption with full configuration
// control.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func WithTransportConfig(cfg *Config) grpc.DialOption {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if cfg.DialOptions == nil {
		cfg.DialOptions = transport.DefaultDialOptions()
	}
	cfg.DialOptions.SingleStreamMode = cfg.SingleStreamMode

	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "shm:") {
			if cfg.FallbackEnabled && cfg.TCPFallbackAddr != "" {
				return (&net.Dialer{}).DialContext(ctx, "tcp", cfg.TCPFallbackAddr)
			}
			return nil, fmt.Errorf("shm transport is not supported on this platform")
		}
		if cfg.AllowMixedTransport {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}
		return nil, fmt.Errorf("shm.WithTransport in strict mode can only dial shm:// addresses, got: %s", addr)
	}

	return grpc.WithContextDialer(dialer)
}

// ListenerConfig configures the shared memory listener.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
type ListenerConfig struct {
	SegmentSize uint64
	RingSize    uint64
}

// DefaultListenerConfig returns the default ListenerConfig.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func DefaultListenerConfig() *ListenerConfig {
	return &ListenerConfig{
		SegmentSize: transport.DefaultSegmentSize,
		RingSize:    transport.DefaultRingASize,
	}
}

// NewListener creates a net.Listener that accepts gRPC connections over
// shared memory.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func NewListener(string, *ListenerConfig) (net.Listener, error) {
	return nil, fmt.Errorf("shm listener is not supported on this platform")
}

// DiscoveryServerInterceptors returns pass-through interceptors on
// platforms without SHM support.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func DiscoveryServerInterceptors(string) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor) {
	unary := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return handler(ctx, req)
	}
	stream := func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(srv, ss)
	}
	return unary, stream
}

// OfferContext returns a context with "shm-offer" metadata attached.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func OfferContext(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, OfferMDKey, "")
}

// CtlFromTrailer extracts the SHM control segment name from trailing
// metadata.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func CtlFromTrailer(trailer metadata.MD) string {
	vals := trailer.Get(CtlMDKey)
	if len(vals) > 0 {
		return vals[0]
	}
	return ""
}

// DiscoveryConfig configures the client-side transport discovery
// behaviour.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
type DiscoveryConfig struct {
	OnDiscovered func(segment string)
}

// DiscoveryClientInterceptors returns pass-through interceptors on
// platforms without SHM support.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func DiscoveryClientInterceptors(*DiscoveryConfig) (grpc.UnaryClientInterceptor, grpc.StreamClientInterceptor) {
	unary := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	stream := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(ctx, desc, cc, method, opts...)
	}
	return unary, stream
}

// DialWithDiscovery dials the target using the standard transport on
// platforms without SHM support.
//
// On unsupported platforms the SHM discovery handshake is a no-op: the
// probeCall callback is NOT invoked, no shm-ctl trailer is examined,
// and the returned ClientConn is always the plain TCP connection to
// target. Callers that need cross-platform parity in the offer/probe
// flow should perform their own probe RPC after this call. The
// probeCall parameter is accepted (rather than rejecting the call) so
// the same call site compiles and runs on every platform.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func DialWithDiscovery(
	ctx context.Context,
	target string,
	_ func(cc *grpc.ClientConn, ctx context.Context, opts ...grpc.CallOption) error,
	opts ...grpc.DialOption,
) (*grpc.ClientConn, error) {
	return grpc.NewClient(target, opts...)
}
