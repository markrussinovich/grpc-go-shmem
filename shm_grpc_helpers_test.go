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
	"os"
	"testing"
	"time"

	"google.golang.org/grpc/internal/transport"
)

func (s) TestWithShmTransport(t *testing.T) {
	// Test that WithShmTransport returns a valid DialOption
	opt := WithShmTransport()
	if opt == nil {
		t.Fatal("WithShmTransport() returned nil")
	}

	// Test that WithShmTransportAndOptions returns a valid DialOption
	opt = WithShmTransportAndOptions(nil)
	if opt == nil {
		t.Fatal("WithShmTransportAndOptions(nil) returned nil")
	}

	// Test that WithShmTransportConfig returns a valid DialOption
	cfg := DefaultShmTransportConfig()
	opt = WithShmTransportConfig(cfg)
	if opt == nil {
		t.Fatal("WithShmTransportConfig() returned nil")
	}
}

func (s) TestShmDialerIntegration(t *testing.T) {
	// This is a basic test to ensure the dialer function works correctly

	opt := WithShmTransport()

	// Apply the option to a dialOptions struct
	var opts dialOptions
	for _, o := range []DialOption{opt} {
		o.apply(&opts)
	}

	// Verify that a dialer was set
	if opts.copts.Dialer == nil {
		t.Fatal("WithShmTransport() did not set a dialer")
	}
}

func (s) TestShmTransportMixedDialing(t *testing.T) {
	// Test that mixed transport allows TCP dialing for non-shm addresses

	// Start a TCP listener for testing
	tcpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create TCP listener: %v", err)
	}
	defer tcpLis.Close()

	// Accept connections in background
	go func() {
		for {
			conn, err := tcpLis.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	t.Run("TCP dial with default config (mixed enabled)", func(t *testing.T) {
		opt := WithShmTransport() // Default has AllowMixedTransport=true

		var opts dialOptions
		opt.apply(&opts)

		// Should successfully dial TCP address
		conn, err := opts.copts.Dialer(ctx, tcpLis.Addr().String())
		if err != nil {
			t.Errorf("Expected TCP dial to succeed with mixed transport, got: %v", err)
		}
		if conn != nil {
			conn.Close()
		}
	})

	t.Run("TCP dial with strict shm mode", func(t *testing.T) {
		cfg := &ShmTransportConfig{
			DialOptions:         transport.DefaultDialOptions(),
			FallbackEnabled:     false,
			AllowMixedTransport: false, // Strict mode
		}
		opt := WithShmTransportConfig(cfg)

		var opts dialOptions
		opt.apply(&opts)

		// Should fail for TCP address in strict mode
		_, err := opts.copts.Dialer(ctx, tcpLis.Addr().String())
		if err == nil {
			t.Error("Expected error for TCP address in strict shm mode")
		}
	})
}

func (s) TestShmTransportFallback(t *testing.T) {
	// Start a TCP listener for fallback
	tcpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create TCP listener: %v", err)
	}
	defer tcpLis.Close()

	// Accept connections in background
	go func() {
		for {
			conn, err := tcpLis.Accept()
			if err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
			conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fallbackHandler := transport.NewShmFallbackHandler()

	t.Run("Fallback to TCP when shm fails", func(t *testing.T) {
		cfg := &ShmTransportConfig{
			DialOptions:         transport.DefaultDialOptions(),
			FallbackEnabled:     true,
			TCPFallbackAddr:     tcpLis.Addr().String(),
			AllowMixedTransport: true,
		}

		// Use a non-existent segment name
		segmentName := fmt.Sprintf("nonexistent_%d", os.Getpid())
		addr := "shm:" + segmentName

		conn, err := dialShmWithFallback(ctx, addr, cfg, fallbackHandler)
		if err != nil {
			t.Errorf("Expected fallback to succeed, got: %v", err)
		}
		if conn != nil {
			conn.Close()
		}
	})

	t.Run("No fallback when disabled", func(t *testing.T) {
		cfg := &ShmTransportConfig{
			DialOptions:         transport.DefaultDialOptions(),
			FallbackEnabled:     false, // Disabled
			TCPFallbackAddr:     tcpLis.Addr().String(),
			AllowMixedTransport: true,
		}

		segmentName := fmt.Sprintf("nonexistent2_%d", os.Getpid())
		addr := "shm:" + segmentName

		_, err := dialShmWithFallback(ctx, addr, cfg, fallbackHandler)
		if err == nil {
			t.Error("Expected error when fallback is disabled")
		}
	})

	t.Run("No fallback without TCP address", func(t *testing.T) {
		cfg := &ShmTransportConfig{
			DialOptions:         transport.DefaultDialOptions(),
			FallbackEnabled:     true,
			TCPFallbackAddr:     "", // No address
			AllowMixedTransport: true,
		}

		segmentName := fmt.Sprintf("nonexistent3_%d", os.Getpid())
		addr := "shm:" + segmentName

		_, err := dialShmWithFallback(ctx, addr, cfg, fallbackHandler)
		if err == nil {
			t.Error("Expected error without fallback address")
		}
	})
}

func (s) TestShmTransportConfigDefaults(t *testing.T) {
	cfg := DefaultShmTransportConfig()

	if cfg.DialOptions == nil {
		t.Error("DialOptions should not be nil")
	}
	if !cfg.FallbackEnabled {
		t.Error("FallbackEnabled should be true by default")
	}
	if !cfg.AllowMixedTransport {
		t.Error("AllowMixedTransport should be true by default (RFC A73)")
	}
	if cfg.TCPFallbackAddr != "" {
		t.Errorf("TCPFallbackAddr should be empty by default, got: %s", cfg.TCPFallbackAddr)
	}
}

func (s) TestFallbackCountTracking(t *testing.T) {
	// Start a TCP listener for fallback
	tcpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create TCP listener: %v", err)
	}
	defer tcpLis.Close()

	go func() {
		for {
			conn, err := tcpLis.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	handler := transport.NewShmFallbackHandler()
	initialCount := handler.FallbackCount()

	cfg := &ShmTransportConfig{
		DialOptions:         transport.DefaultDialOptions(),
		FallbackEnabled:     true,
		TCPFallbackAddr:     tcpLis.Addr().String(),
		AllowMixedTransport: true,
	}

	// Trigger a fallback
	segmentName := fmt.Sprintf("count_test_%d", os.Getpid())
	conn, err := dialShmWithFallback(ctx, "shm:"+segmentName, cfg, handler)
	if err != nil {
		t.Errorf("Fallback should succeed: %v", err)
	}
	if conn != nil {
		conn.Close()
	}

	if handler.FallbackCount() <= initialCount {
		t.Error("Fallback count should have increased")
	}
}
