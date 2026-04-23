//go:build linux || windows

/*
 *
 * Copyright 2025 gRPC authors.
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
	"fmt"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/internal/transport"
)

var shmLogger = grpclog.Component("shm")

// WithShmTransport returns a DialOption that configures the client to use
// shared memory transport. This should be used with addresses of the form
// "shm://segment_name".
//
// When used, the dialer will:
//   - Use shared memory transport for shm:// addresses
//   - Fall back to standard TCP for non-shm addresses (enabling mixed transport)
//   - Optionally fall back to TCP if shm connection fails (configurable)
//
// Example:
//
//	conn, err := grpc.NewClient("shm://my_segment", grpc.WithShmTransport(), grpc.WithTransportCredentials(insecure.NewCredentials()))
func WithShmTransport() DialOption {
	return WithShmTransportAndOptions(nil)
}

// ShmTransportConfig contains configuration options for shared memory transport.
type ShmTransportConfig struct {
	// DialOptions contains shm-specific dial options (segment size, ring sizes, etc.)
	DialOptions *transport.DialOptions

	// FallbackEnabled allows falling back to TCP if shm connection fails.
	// Default is true.
	FallbackEnabled bool

	// TCPFallbackAddr is the TCP address to fall back to if shm fails.
	// If empty, fallback is disabled even if FallbackEnabled is true.
	// Format: "host:port"
	TCPFallbackAddr string

	// AllowMixedTransport allows the dialer to handle both shm and TCP addresses.
	// When true, non-shm addresses will use standard TCP dial.
	// Default is true for RFC A73 compliance.
	AllowMixedTransport bool

	// SingleStreamMode requests single-stream optimizations from the server.
	// When enabled, the CONNECT frame includes a flag that both sides use to
	// activate inline write paths (bypassing the writer goroutine queue).
	// Enable this for unary or single-session benchmark scenarios.
	// Default: false.
	SingleStreamMode bool
}

// DefaultShmTransportConfig returns the default configuration.
func DefaultShmTransportConfig() *ShmTransportConfig {
	return &ShmTransportConfig{
		DialOptions:         transport.DefaultDialOptions(),
		FallbackEnabled:     true,
		AllowMixedTransport: true,
	}
}

// WithShmTransportAndOptions returns a DialOption that configures the client to use
// shared memory transport with custom options.
//
// Example:
//
//	opts := &transport.DialOptions{
//	    SegmentSize: 1024 * 1024,  // 1MB
//	    RingASize:   256 * 1024,    // 256KB
//	    RingBSize:   256 * 1024,    // 256KB
//	}
//	conn, err := grpc.NewClient("shm://my_segment", grpc.WithShmTransportAndOptions(opts), grpc.WithTransportCredentials(insecure.NewCredentials()))
func WithShmTransportAndOptions(opts *transport.DialOptions) DialOption {
	cfg := DefaultShmTransportConfig()
	if opts != nil {
		cfg.DialOptions = opts
	}
	return WithShmTransportConfig(cfg)
}

// WithShmTransportConfig returns a DialOption with full configuration control.
// This is the most flexible option for configuring shared memory transport.
//
// Example:
//
//	cfg := &grpc.ShmTransportConfig{
//	    DialOptions:         transport.DefaultDialOptions(),
//	    FallbackEnabled:     true,
//	    TCPFallbackAddr:     "localhost:50051",
//	    AllowMixedTransport: true,
//	}
//	conn, err := grpc.NewClient("shm://my_segment", grpc.WithShmTransportConfig(cfg))
func WithShmTransportConfig(cfg *ShmTransportConfig) DialOption {
	if cfg == nil {
		cfg = DefaultShmTransportConfig()
	}
	if cfg.DialOptions == nil {
		cfg.DialOptions = transport.DefaultDialOptions()
	}
	cfg.DialOptions.SingleStreamMode = cfg.SingleStreamMode

	fallbackHandler := transport.NewShmFallbackHandler()

	// Create a context dialer that understands shm:// addresses and supports fallback
	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		// Check if this is a shm:// address
		if strings.HasPrefix(addr, "shm:") {
			return dialShmWithFallback(ctx, addr, cfg, fallbackHandler)
		}

		// Not a shm:// address - handle based on configuration
		if cfg.AllowMixedTransport {
			// RFC A73: Allow mixed transport - dial TCP for non-shm addresses
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}

		// Strict shm-only mode
		return nil, fmt.Errorf("WithShmTransport in strict mode can only dial shm:// addresses, got: %s", addr)
	}

	return WithContextDialer(dialer)
}

// dialShmWithFallback attempts to dial via shared memory, falling back to TCP if configured.
func dialShmWithFallback(ctx context.Context, addr string, cfg *ShmTransportConfig, fallbackHandler *transport.ShmFallbackHandler) (net.Conn, error) {
	// Extract segment name from "shm:segment_name"
	segmentName := strings.TrimPrefix(addr, "shm:")

	// Try shared memory first
	clientTransport, err := transport.DialShm(ctx, segmentName, cfg.DialOptions)
	if err == nil {
		// Success - wrap the transport
		shmTransport := clientTransport.(*transport.ShmClientTransport)
		localAddr := &transport.ShmAddr{Name: segmentName + "_client"}
		return &shmClientConn{
			transport:  shmTransport,
			localAddr:  localAddr,
			remoteAddr: shmTransport.RemoteAddr(),
		}, nil
	}

	// Shm dial failed - check if we should fall back
	if !cfg.FallbackEnabled || cfg.TCPFallbackAddr == "" {
		return nil, fmt.Errorf("failed to dial shared memory segment %q: %v", segmentName, err)
	}

	// Use the fallback handler to decide
	result := fallbackHandler.HandleShmError(err, true)
	if !result.ShouldFallback {
		return nil, fmt.Errorf("shm transport failed and fallback not triggered: %w", err)
	}

	// Log the fallback
	shmLogger.Infof("shm dial to %q failed, falling back to TCP %q: %v",
		segmentName, cfg.TCPFallbackAddr, err)

	// Fall back to TCP
	tcpConn, tcpErr := (&net.Dialer{}).DialContext(ctx, "tcp", cfg.TCPFallbackAddr)
	if tcpErr != nil {
		return nil, fmt.Errorf("shm failed (%v) and TCP fallback to %q also failed: %w",
			err, cfg.TCPFallbackAddr, tcpErr)
	}

	return tcpConn, nil
}

// shmClientConn wraps a shared memory client transport to implement net.Conn.
// This allows gRPC to use the transport through its standard dialing mechanism.
type shmClientConn struct {
	transport  *transport.ShmClientTransport
	localAddr  net.Addr
	remoteAddr net.Addr
	closed     bool
}

// Read implements net.Conn - not used directly in gRPC transport layer
func (c *shmClientConn) Read(_ []byte) (n int, err error) {
	return 0, fmt.Errorf("direct read not supported on shared memory connection")
}

// Write implements net.Conn - not used directly in gRPC transport layer
func (c *shmClientConn) Write(_ []byte) (n int, err error) {
	return 0, fmt.Errorf("direct write not supported on shared memory connection")
}

// Close implements net.Conn
func (c *shmClientConn) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	c.transport.Close(fmt.Errorf("connection closed"))
	return nil
}

// LocalAddr implements net.Conn
func (c *shmClientConn) LocalAddr() net.Addr {
	return c.localAddr
}

// RemoteAddr implements net.Conn
func (c *shmClientConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

// SetDeadline implements net.Conn - no-op for shared memory
func (c *shmClientConn) SetDeadline(_ time.Time) error {
	return nil
}

// SetReadDeadline implements net.Conn - no-op for shared memory
func (c *shmClientConn) SetReadDeadline(_ time.Time) error {
	return nil
}

// SetWriteDeadline implements net.Conn - no-op for shared memory
func (c *shmClientConn) SetWriteDeadline(_ time.Time) error {
	return nil
}

// GetClientTransport returns the underlying shared memory client transport.
// This is used internally by gRPC to access the transport after dialing.
func (c *shmClientConn) GetClientTransport() transport.ClientTransport {
	return c.transport
}

// --- Server-side public API ---

// ShmListenerConfig configures the shared memory listener.
type ShmListenerConfig struct {
	// SegmentSize is the total size of the shared memory segment in bytes.
	// Default: 136 MiB (covers two 64 MiB rings plus headers).
	SegmentSize uint64

	// RingSize is the capacity of each ring buffer in bytes.
	// There are two rings: client→server and server→client.
	// Default: 64 MiB.
	RingSize uint64
}

// DefaultShmListenerConfig returns the default listener configuration.
func DefaultShmListenerConfig() *ShmListenerConfig {
	return &ShmListenerConfig{
		SegmentSize: transport.DefaultSegmentSize,
		RingSize:    transport.DefaultRingASize,
	}
}

// NewShmListener creates a net.Listener that accepts gRPC connections over
// shared memory. The segmentName identifies the shared memory segment and
// must match the name used by the client (e.g., "shm://segmentName").
//
// Example:
//
//	lis, err := grpc.NewShmListener("my_segment", nil)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	s := grpc.NewServer()
//	pb.RegisterMyServiceServer(s, &myServer{})
//	s.Serve(lis)
func NewShmListener(segmentName string, cfg *ShmListenerConfig) (net.Listener, error) {
	if segmentName == "" {
		return nil, fmt.Errorf("segmentName must not be empty")
	}
	if cfg == nil {
		cfg = DefaultShmListenerConfig()
	}
	if cfg.SegmentSize == 0 {
		cfg.SegmentSize = transport.DefaultSegmentSize
	}
	if cfg.RingSize == 0 {
		cfg.RingSize = transport.DefaultRingASize
	}
	return transport.NewShmListener(
		&transport.ShmAddr{Name: segmentName},
		cfg.SegmentSize,
		cfg.RingSize,
		cfg.RingSize,
	)
}
