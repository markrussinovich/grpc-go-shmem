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

package shm

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc/internal/transport"
)

func TestWithTransport(t *testing.T) {
	if opt := WithTransport(); opt == nil {
		t.Fatal("WithTransport() returned nil")
	}
	if opt := WithTransportAndOptions(nil); opt == nil {
		t.Fatal("WithTransportAndOptions(nil) returned nil")
	}
	if opt := WithTransportConfig(DefaultConfig()); opt == nil {
		t.Fatal("WithTransportConfig() returned nil")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

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

func TestDefaultListenerConfig(t *testing.T) {
	cfg := DefaultListenerConfig()
	if cfg.SegmentSize == 0 {
		t.Error("SegmentSize should be non-zero by default")
	}
	if cfg.RingSize == 0 {
		t.Error("RingSize should be non-zero by default")
	}
}

// TestDialWithFallback exercises the package-private dialWithFallback
// closure directly so we can validate fallback behaviour without
// spinning up a real gRPC ClientConn.
func TestDialWithFallback(t *testing.T) {
	// Start a TCP listener for fallback.
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
			time.Sleep(50 * time.Millisecond)
			conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fallbackHandler := transport.NewShmFallbackHandler()

	t.Run("Fallback to TCP when shm fails", func(t *testing.T) {
		cfg := &Config{
			DialOptions:         transport.DefaultDialOptions(),
			FallbackEnabled:     true,
			TCPFallbackAddr:     tcpLis.Addr().String(),
			AllowMixedTransport: true,
		}
		addr := fmt.Sprintf("shm:nonexistent_%d", os.Getpid())
		conn, err := dialWithFallback(ctx, addr, cfg, fallbackHandler)
		if err != nil {
			t.Errorf("Expected fallback to succeed, got: %v", err)
		}
		if conn != nil {
			conn.Close()
		}
	})

	t.Run("No fallback when disabled", func(t *testing.T) {
		cfg := &Config{
			DialOptions:         transport.DefaultDialOptions(),
			FallbackEnabled:     false,
			TCPFallbackAddr:     tcpLis.Addr().String(),
			AllowMixedTransport: true,
		}
		addr := fmt.Sprintf("shm:nonexistent2_%d", os.Getpid())
		if _, err := dialWithFallback(ctx, addr, cfg, fallbackHandler); err == nil {
			t.Error("Expected error when fallback is disabled")
		}
	})

	t.Run("No fallback without TCP address", func(t *testing.T) {
		cfg := &Config{
			DialOptions:         transport.DefaultDialOptions(),
			FallbackEnabled:     true,
			TCPFallbackAddr:     "",
			AllowMixedTransport: true,
		}
		addr := fmt.Sprintf("shm:nonexistent3_%d", os.Getpid())
		if _, err := dialWithFallback(ctx, addr, cfg, fallbackHandler); err == nil {
			t.Error("Expected error without fallback address")
		}
	})
}

func TestNewListenerValidation(t *testing.T) {
	if _, err := NewListener("", nil); err == nil {
		t.Error("NewListener with empty name should return an error")
	}
}
