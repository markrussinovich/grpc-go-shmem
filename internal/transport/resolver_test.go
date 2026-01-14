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

package transport

import (
	"fmt"
	"net/url"
	"testing"

	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
)

// TestShmResolverRegistration verifies that the shm resolver is registered.
func TestShmResolverRegistration(t *testing.T) {
	builder := resolver.Get("shm")
	if builder == nil {
		t.Fatal("shm resolver not registered")
	}

	if builder.Scheme() != "shm" {
		t.Errorf("Expected scheme 'shm', got '%s'", builder.Scheme())
	}
}

// mockClientConn implements resolver.ClientConn for testing.
type mockClientConn struct {
	state resolver.State
	err   error
}

func (m *mockClientConn) UpdateState(state resolver.State) error {
	m.state = state
	return m.err
}

func (m *mockClientConn) ReportError(err error) {
	m.err = err
}

func (m *mockClientConn) NewAddress(addresses []resolver.Address) {
	m.state = resolver.State{Addresses: addresses}
}

func (m *mockClientConn) ParseServiceConfig(_ string) *serviceconfig.ParseResult {
	return nil
}

// TestShmResolverBuild tests building a shm resolver.
func TestShmResolverBuild(t *testing.T) {
	tests := []struct {
		name        string
		target      string
		wantErr     bool
		wantAddr    string
		wantSegment string
	}{
		{
			name:        "valid segment name",
			target:      "shm://test_segment",
			wantErr:     false,
			wantAddr:    "shm:test_segment",
			wantSegment: "test_segment",
		},
		{
			name:        "segment with underscores",
			target:      "shm://my_test_segment_123",
			wantErr:     false,
			wantAddr:    "shm:my_test_segment_123",
			wantSegment: "my_test_segment_123",
		},
		{
			name:    "empty endpoint",
			target:  "shm:",
			wantErr: true,
		},
	}

	builder := resolver.Get("shm")
	if builder == nil {
		t.Fatal("shm resolver not registered")
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse the target
			target := parseTarget(t, tt.target)

			// Create mock ClientConn
			cc := &mockClientConn{}

			// Build the resolver
			r, err := builder.Build(target, cc, resolver.BuildOptions{})

			if tt.wantErr {
				if err == nil {
					t.Error("Expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			defer r.Close()

			// Check that UpdateState was called with the correct address
			if len(cc.state.Addresses) != 1 {
				t.Fatalf("Expected 1 address, got %d", len(cc.state.Addresses))
			}

			addr := cc.state.Addresses[0]
			if addr.Addr != tt.wantAddr {
				t.Errorf("Expected Addr=%s, got %s", tt.wantAddr, addr.Addr)
			}

			if addr.ServerName != tt.wantSegment {
				t.Errorf("Expected ServerName=%s, got %s", tt.wantSegment, addr.ServerName)
			}
		})
	}
}

// TestShmResolverResolveNow tests that ResolveNow is a no-op.
func TestShmResolverResolveNow(t *testing.T) {
	builder := resolver.Get("shm")
	if builder == nil {
		t.Fatal("shm resolver not registered")
	}

	target := parseTarget(t, "shm://test_segment")
	cc := &mockClientConn{}

	r, err := builder.Build(target, cc, resolver.BuildOptions{})
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	defer r.Close()

	// Save the initial state
	initialAddr := cc.state.Addresses[0].Addr

	// Call ResolveNow (should be a no-op)
	r.ResolveNow(resolver.ResolveNowOptions{})

	// State should remain unchanged
	if cc.state.Addresses[0].Addr != initialAddr {
		t.Error("ResolveNow should not change the resolved address")
	}
}

// parseTarget is a helper to parse a target string into resolver.Target.
// It uses real URL parsing to match how gRPC actually parses targets.
func parseTarget(t *testing.T, targetStr string) resolver.Target {
	t.Helper()

	// Use real URL parsing like gRPC does
	u, err := url.Parse(targetStr)
	if err != nil {
		t.Fatalf("Failed to parse target %q: %v", targetStr, err)
	}
	return resolver.Target{URL: *u}
}

// TestShmResolverIntegration tests the resolver with a more realistic scenario.
func TestShmResolverIntegration(t *testing.T) {
	builder := resolver.Get("shm")
	if builder == nil {
		t.Fatal("shm resolver not registered")
	}

	// Test with a segment name that would be used in real scenarios
	segmentName := fmt.Sprintf("grpc_test_%d", 12345)
	target := parseTarget(t, fmt.Sprintf("shm://%s", segmentName))
	cc := &mockClientConn{}

	r, err := builder.Build(target, cc, resolver.BuildOptions{})
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	defer r.Close()

	// Verify the resolved address can be used for connection
	if len(cc.state.Addresses) == 0 {
		t.Fatal("No addresses resolved")
	}

	addr := cc.state.Addresses[0]
	t.Logf("Resolved address: %+v", addr)

	// The address should be in a format our dialer can understand
	if addr.Addr != fmt.Sprintf("shm:%s", segmentName) {
		t.Errorf("Address not in expected format: %s", addr.Addr)
	}

	// The ServerName should be the segment name
	if addr.ServerName != segmentName {
		t.Errorf("ServerName mismatch: expected %s, got %s", segmentName, addr.ServerName)
	}
}
