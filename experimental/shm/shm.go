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

// Package shm provides experimental shared-memory transport helpers for
// gRPC. It contains the user-facing surface for dialing a gRPC client
// over a shared-memory segment and for accepting SHM connections on the
// server side. The transport implementation itself lives in
// google.golang.org/grpc/internal/transport; this package is the thin
// public façade so the root google.golang.org/grpc package does not
// need to host SHM-specific exports.
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
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/internal/transport"
)

var logger = grpclog.Component("shm")

// Config contains configuration options for the shared-memory transport.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
type Config struct {
	// DialOptions contains shm-specific dial options (segment size, ring
	// sizes, etc.).
	DialOptions *transport.DialOptions

	// FallbackEnabled allows falling back to TCP if shm connection fails.
	// Default is true.
	FallbackEnabled bool

	// TCPFallbackAddr is the TCP address to fall back to if shm fails.
	// If empty, fallback is disabled even if FallbackEnabled is true.
	// Format: "host:port".
	TCPFallbackAddr string

	// AllowMixedTransport allows the dialer to handle both shm and TCP
	// addresses. When true, non-shm addresses will use standard TCP dial.
	// Default is true for RFC A73 compliance.
	AllowMixedTransport bool

	// SingleStreamMode requests single-stream optimizations from the
	// server. When enabled, the CONNECT frame includes a flag that both
	// sides use to activate inline write paths (bypassing the writer
	// goroutine queue). Enable this for unary or single-session benchmark
	// scenarios. Default: false.
	SingleStreamMode bool
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
// use shared memory transport. Use with addresses of the form
// "shm://segment_name".
//
// When used, the dialer will:
//   - use shared memory transport for shm:// addresses,
//   - fall back to standard TCP for non-shm addresses (mixed transport),
//   - optionally fall back to TCP if shm connection fails (configurable).
//
// Example:
//
//	conn, err := grpc.NewClient("shm://my_segment",
//	    shm.WithTransport(),
//	    grpc.WithTransportCredentials(insecure.NewCredentials()))
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
// control. This is the most flexible option for configuring shared
// memory transport.
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

	fallbackHandler := transport.NewShmFallbackHandler()

	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "shm:") {
			return dialWithFallback(ctx, addr, cfg, fallbackHandler)
		}
		if cfg.AllowMixedTransport {
			// RFC A73: mixed transport — dial TCP for non-shm addresses.
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}
		return nil, fmt.Errorf("shm.WithTransport in strict mode can only dial shm:// addresses, got: %s", addr)
	}

	return grpc.WithContextDialer(dialer)
}

// dialWithFallback attempts to dial via shared memory, falling back to
// TCP if configured.
func dialWithFallback(ctx context.Context, addr string, cfg *Config, fallbackHandler *transport.ShmFallbackHandler) (net.Conn, error) {
	// Strip both URI forms: the canonical "shm://name" (which gRPC's
	// passthrough resolver may pass through verbatim) and the resolver-
	// normalised "shm:name". TrimPrefix("shm:") would otherwise leave a
	// stray "//" prefix that fails segment-name validation downstream.
	segmentName := strings.TrimPrefix(addr, "shm://")
	if segmentName == addr {
		segmentName = strings.TrimPrefix(addr, "shm:")
	}

	clientTransport, err := transport.DialShm(ctx, segmentName, cfg.DialOptions)
	if err == nil {
		shmTransport := clientTransport.(*transport.ShmClientTransport)
		return transport.NewShmConn(shmTransport, shmTransport.GetAuthInfo()), nil
	}

	if !cfg.FallbackEnabled || cfg.TCPFallbackAddr == "" {
		return nil, fmt.Errorf("failed to dial shared memory segment %q: %v", segmentName, err)
	}

	result := fallbackHandler.HandleShmError(err, true)
	if !result.ShouldFallback {
		return nil, fmt.Errorf("shm transport failed and fallback not triggered: %w", err)
	}

	logger.Infof("shm dial to %q failed, falling back to TCP %q: %v",
		segmentName, cfg.TCPFallbackAddr, err)

	tcpConn, tcpErr := (&net.Dialer{}).DialContext(ctx, "tcp", cfg.TCPFallbackAddr)
	if tcpErr != nil {
		return nil, fmt.Errorf("shm failed (%v) and TCP fallback to %q also failed: %w",
			err, cfg.TCPFallbackAddr, tcpErr)
	}
	return tcpConn, nil
}

// ListenerConfig configures the shared memory listener.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
type ListenerConfig struct {
	// SegmentSize is the total size of the shared memory segment in bytes.
	// Default: 136 MiB (covers two 64 MiB rings plus headers).
	//
	// SegmentSize is treated as a configured upper bound: NewListener
	// validates that SegmentSize >= 2*RingSize and returns an error
	// otherwise. The underlying allocator currently derives the actual
	// segment size from RingSize and headers (so a SegmentSize larger
	// than the minimum has no effect beyond passing the consistency
	// check), but this validation prevents silently-ignored
	// misconfiguration where a smaller SegmentSize gives the
	// impression of capping memory use.
	SegmentSize uint64

	// RingSize is the capacity of each ring buffer in bytes. There are
	// two rings: client->server and server->client. Default: 64 MiB.
	RingSize uint64
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
// shared memory. The segmentName identifies the shared memory segment
// and must match the name used by the client (e.g.,
// "shm://segmentName").
//
// Example:
//
//	lis, err := shm.NewListener("my_segment", nil)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	s := grpc.NewServer()
//	pb.RegisterMyServiceServer(s, &myServer{})
//	s.Serve(lis)
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func NewListener(segmentName string, cfg *ListenerConfig) (net.Listener, error) {
	if segmentName == "" {
		return nil, fmt.Errorf("segmentName must not be empty")
	}
	if cfg == nil {
		cfg = DefaultListenerConfig()
	}
	if cfg.SegmentSize == 0 {
		cfg.SegmentSize = transport.DefaultSegmentSize
	}
	if cfg.RingSize == 0 {
		cfg.RingSize = transport.DefaultRingASize
	}
	// Reject configurations where SegmentSize cannot fit two RingSize
	// rings. Without this check a too-small SegmentSize would be
	// silently ignored (the field is documented as an upper bound but
	// the underlying allocator derives the actual size from RingSize),
	// giving the impression that the user can cap segment memory below
	// the ring requirements.
	if cfg.SegmentSize < 2*cfg.RingSize {
		return nil, fmt.Errorf("shm: ListenerConfig.SegmentSize (%d) must be at least 2*RingSize (%d)", cfg.SegmentSize, 2*cfg.RingSize)
	}
	return transport.NewShmListener(
		&transport.ShmAddr{Name: segmentName},
		cfg.SegmentSize,
		cfg.RingSize,
		cfg.RingSize,
	)
}
