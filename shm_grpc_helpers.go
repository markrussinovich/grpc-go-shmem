//go:build linux

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

	"google.golang.org/grpc/internal/transport"
)

// WithShmTransport returns a DialOption that configures the client to use
// shared memory transport. This should be used with addresses of the form
// "shm://segment_name".
//
// Example:
//
//	conn, err := grpc.NewClient("shm://my_segment", grpc.WithShmTransport(), grpc.WithTransportCredentials(insecure.NewCredentials()))
func WithShmTransport() DialOption {
	return WithShmTransportAndOptions(nil)
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
	if opts == nil {
		opts = transport.DefaultDialOptions()
	}

	// Create a context dialer that understands shm:// addresses
	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		// Check if this is a shm:// address
		if strings.HasPrefix(addr, "shm:") {
			// Extract segment name from "shm:segment_name"
			segmentName := strings.TrimPrefix(addr, "shm:")

			// Use the shared memory dialer
			clientTransport, err := transport.DialShm(ctx, segmentName, opts)
			if err != nil {
				return nil, fmt.Errorf("failed to dial shared memory segment %q: %v", segmentName, err)
			}

			// Wrap the transport in a net.Conn-compatible interface
			shmTransport := clientTransport.(*transport.ShmClientTransport)
			localAddr := &transport.ShmAddr{Name: segmentName + "_client"}
			return &shmClientConn{
				transport:  shmTransport,
				localAddr:  localAddr,
				remoteAddr: shmTransport.RemoteAddr(),
			}, nil
		}

		// Not a shm:// address, return error
		return nil, fmt.Errorf("WithShmTransport can only dial shm:// addresses, got: %s", addr)
	}

	return WithContextDialer(dialer)
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
func (c *shmClientConn) Read(b []byte) (n int, err error) {
	return 0, fmt.Errorf("direct read not supported on shared memory connection")
}

// Write implements net.Conn - not used directly in gRPC transport layer
func (c *shmClientConn) Write(b []byte) (n int, err error) {
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
func (c *shmClientConn) SetDeadline(t time.Time) error {
	return nil
}

// SetReadDeadline implements net.Conn - no-op for shared memory
func (c *shmClientConn) SetReadDeadline(t time.Time) error {
	return nil
}

// SetWriteDeadline implements net.Conn - no-op for shared memory
func (c *shmClientConn) SetWriteDeadline(t time.Time) error {
	return nil
}

// GetClientTransport returns the underlying shared memory client transport.
// This is used internally by gRPC to access the transport after dialing.
func (c *shmClientConn) GetClientTransport() transport.ClientTransport {
	return c.transport
}
