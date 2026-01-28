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

			// RFC A73: Verify that ShmCapability attribute is set
			if !IsShmEnabled(addr) {
				t.Error("Expected IsShmEnabled to be true for resolved address")
			}
			if !IsShmPreferred(addr) {
				t.Error("Expected IsShmPreferred to be true for shm:// scheme")
			}
			cap := GetShmCapability(addr)
			if cap == nil {
				t.Fatal("Expected ShmCapability attribute to be set")
			}
			if cap.SegmentName != tt.wantSegment {
				t.Errorf("Expected ShmCapability.SegmentName=%s, got %s", tt.wantSegment, cap.SegmentName)
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

	// RFC A73: Verify that ShmCapability is set correctly
	cap := GetShmCapability(addr)
	if cap == nil {
		t.Fatal("ShmCapability attribute not set on resolved address")
	}
	if !cap.Enabled {
		t.Error("ShmCapability.Enabled should be true")
	}
	if cap.SegmentName != segmentName {
		t.Errorf("ShmCapability.SegmentName: expected %s, got %s", segmentName, cap.SegmentName)
	}
	if !cap.Preferred {
		t.Error("ShmCapability.Preferred should be true for shm:// scheme")
	}

	// Test that the ShouldUseShm method works correctly with resolved addresses
	cfg := DefaultShmServiceConfig()
	if !cfg.ShouldUseShm(IsShmEnabled(addr)) {
		t.Error("ShouldUseShm should return true for addresses with ShmCapability")
	}
}

// TestShmResolverRFCA73Attributes tests RFC A73 compliant attribute propagation.
// This verifies that the resolver correctly sets attributes that enable the
// Load Balancer to select the appropriate transport.
func TestShmResolverRFCA73Attributes(t *testing.T) {
	builder := resolver.Get("shm")
	if builder == nil {
		t.Fatal("shm resolver not registered")
	}

	tests := []struct {
		name              string
		target            string
		wantSegment       string
		wantEnabled       bool
		wantPreferred     bool
		serviceConfigJSON string
		wantShouldUse     bool
	}{
		{
			name:              "shm scheme with auto policy",
			target:            "shm://test_segment",
			wantSegment:       "test_segment",
			wantEnabled:       true,
			wantPreferred:     true,
			serviceConfigJSON: `{"shmPolicy":"auto"}`,
			wantShouldUse:     true,
		},
		{
			name:              "shm scheme with preferred policy",
			target:            "shm://preferred_segment",
			wantSegment:       "preferred_segment",
			wantEnabled:       true,
			wantPreferred:     true,
			serviceConfigJSON: `{"shmPolicy":"preferred"}`,
			wantShouldUse:     true,
		},
		{
			name:              "shm scheme with disabled policy",
			target:            "shm://disabled_segment",
			wantSegment:       "disabled_segment",
			wantEnabled:       true,
			wantPreferred:     true,
			serviceConfigJSON: `{"shmPolicy":"disabled"}`,
			wantShouldUse:     false, // disabled policy overrides capability
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := parseTarget(t, tt.target)
			cc := &mockClientConn{}

			r, err := builder.Build(target, cc, resolver.BuildOptions{})
			if err != nil {
				t.Fatalf("Build failed: %v", err)
			}
			defer r.Close()

			if len(cc.state.Addresses) != 1 {
				t.Fatalf("Expected 1 address, got %d", len(cc.state.Addresses))
			}

			addr := cc.state.Addresses[0]

			// Verify capability attribute
			cap := GetShmCapability(addr)
			if cap == nil {
				t.Fatal("ShmCapability attribute not set")
			}
			if cap.Enabled != tt.wantEnabled {
				t.Errorf("Enabled: got %v, want %v", cap.Enabled, tt.wantEnabled)
			}
			if cap.Preferred != tt.wantPreferred {
				t.Errorf("Preferred: got %v, want %v", cap.Preferred, tt.wantPreferred)
			}
			if cap.SegmentName != tt.wantSegment {
				t.Errorf("SegmentName: got %s, want %s", cap.SegmentName, tt.wantSegment)
			}

			// Verify helper functions
			if IsShmEnabled(addr) != tt.wantEnabled {
				t.Errorf("IsShmEnabled: got %v, want %v", IsShmEnabled(addr), tt.wantEnabled)
			}
			if IsShmPreferred(addr) != (tt.wantEnabled && tt.wantPreferred) {
				t.Errorf("IsShmPreferred: got %v, want %v", IsShmPreferred(addr), tt.wantEnabled && tt.wantPreferred)
			}

			// Verify service config integration
			cfg, err := ParseShmServiceConfig(tt.serviceConfigJSON)
			if err != nil {
				t.Fatalf("Failed to parse service config: %v", err)
			}

			shouldUse := cfg.ShouldUseShm(IsShmEnabled(addr))
			if shouldUse != tt.wantShouldUse {
				t.Errorf("ShouldUseShm: got %v, want %v (policy=%s, hasCapability=%v)",
					shouldUse, tt.wantShouldUse, cfg.Policy, IsShmEnabled(addr))
			}
		})
	}
}

// TestShmResolverWithTransportHint tests that ShmTransportHint can be applied
// alongside ShmCapability on resolved addresses.
func TestShmResolverWithTransportHint(t *testing.T) {
	builder := resolver.Get("shm")
	if builder == nil {
		t.Fatal("shm resolver not registered")
	}

	target := parseTarget(t, "shm://hint_test_segment")
	cc := &mockClientConn{}

	r, err := builder.Build(target, cc, resolver.BuildOptions{})
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	defer r.Close()

	if len(cc.state.Addresses) != 1 {
		t.Fatalf("Expected 1 address, got %d", len(cc.state.Addresses))
	}

	// Get the resolved address
	addr := cc.state.Addresses[0]

	// Verify capability is set by resolver
	if !IsShmEnabled(addr) {
		t.Fatal("Expected ShmCapability to be set by resolver")
	}

	// Simulate what an LB policy would do: add a transport hint
	hint := ShmTransportHint{
		PreferShm:       true,
		FallbackAllowed: true,
	}
	addr = SetShmTransportHint(addr, hint)

	// Both attributes should coexist
	cap := GetShmCapability(addr)
	if cap == nil {
		t.Fatal("ShmCapability lost after adding hint")
	}

	gotHint := GetShmTransportHint(addr)
	if gotHint == nil {
		t.Fatal("ShmTransportHint not set")
	}

	if !gotHint.PreferShm {
		t.Error("PreferShm should be true")
	}
	if !gotHint.FallbackAllowed {
		t.Error("FallbackAllowed should be true")
	}
}
