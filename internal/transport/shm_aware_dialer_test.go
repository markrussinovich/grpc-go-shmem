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

package transport

import (
	"testing"

	"google.golang.org/grpc/resolver"
)

// TestTransportType tests the TransportType enum and String method.
func TestTransportType(t *testing.T) {
	tests := []struct {
		name     string
		typ      TransportType
		expected string
	}{
		{"HTTP2", TransportTypeHTTP2, "HTTP2"},
		{"Shm", TransportTypeShm, "Shm"},
		{"Unknown", TransportType(99), "TransportType(99)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.typ.String(); got != tt.expected {
				t.Errorf("TransportType.String() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// TestTransportSelectorBasic tests basic transport selection based on attributes.
func TestTransportSelectorBasic(t *testing.T) {
	tests := []struct {
		name     string
		addr     resolver.Address
		config   *ShmServiceConfig
		expected TransportType
	}{
		{
			name:     "No attributes - HTTP2",
			addr:     resolver.Address{Addr: "localhost:8080"},
			config:   nil,
			expected: TransportTypeHTTP2,
		},
		{
			name: "Shm enabled - shm selected",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:8080"}, ShmCapability{
				Enabled:     true,
				SegmentName: "test_segment",
			}),
			config:   nil,
			expected: TransportTypeShm,
		},
		{
			name: "Shm enabled but policy disabled - HTTP2",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:8080"}, ShmCapability{
				Enabled:     true,
				SegmentName: "test_segment",
			}),
			config:   &ShmServiceConfig{Policy: ShmPolicyDisabled},
			expected: TransportTypeHTTP2,
		},
		{
			// When policy is required but address has no capability,
			// we still try shm (and let it fail) rather than silently fall back to HTTP2.
			// This is intentional - "required" means we should not use HTTP2.
			name:   "Shm not enabled, policy required - still attempts shm (will fail)",
			addr:   resolver.Address{Addr: "localhost:8080"},
			config: &ShmServiceConfig{Policy: ShmPolicyRequired},
			expected: TransportTypeShm,
		},
		{
			name: "Transport hint prefers shm",
			addr: SetShmTransportHint(
				SetShmCapability(resolver.Address{Addr: "localhost:8080"}, ShmCapability{
					Enabled:     true,
					SegmentName: "test_segment",
				}),
				ShmTransportHint{PreferShm: true, FallbackAllowed: true},
			),
			config:   nil,
			expected: TransportTypeShm,
		},
		{
			name: "Transport hint prefers HTTP2",
			addr: SetShmTransportHint(
				SetShmCapability(resolver.Address{Addr: "localhost:8080"}, ShmCapability{
					Enabled:     true,
					SegmentName: "test_segment",
				}),
				ShmTransportHint{PreferShm: false, FallbackAllowed: true},
			),
			config:   nil,
			expected: TransportTypeHTTP2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selector := NewTransportSelector(tt.config)
			got := selector.SelectTransport(tt.addr)
			if got != tt.expected {
				t.Errorf("SelectTransport() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestTransportSelectorWithDetails tests detailed transport selection.
func TestTransportSelectorWithDetails(t *testing.T) {
	tests := []struct {
		name            string
		addr            resolver.Address
		config          *ShmServiceConfig
		expectedType    TransportType
		expectedFallback bool
		expectedSegment string
	}{
		{
			name:             "HTTP2 with fallback allowed",
			addr:             resolver.Address{Addr: "localhost:8080"},
			config:           nil,
			expectedType:     TransportTypeHTTP2,
			expectedFallback: true,
			expectedSegment:  "",
		},
		{
			name: "Shm with fallback allowed",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:8080"}, ShmCapability{
				Enabled:     true,
				SegmentName: "my_segment",
			}),
			config:           nil,
			expectedType:     TransportTypeShm,
			expectedFallback: true,
			expectedSegment:  "my_segment",
		},
		{
			name: "Shm required - no fallback",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:8080"}, ShmCapability{
				Enabled:     true,
				SegmentName: "required_segment",
			}),
			config:           &ShmServiceConfig{Policy: ShmPolicyRequired},
			expectedType:     TransportTypeShm,
			expectedFallback: false,
			expectedSegment:  "required_segment",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selector := NewTransportSelector(tt.config)
			result := selector.SelectTransportWithDetails(tt.addr)

			if result.Type != tt.expectedType {
				t.Errorf("Type = %v, want %v", result.Type, tt.expectedType)
			}
			if result.FallbackAllowed != tt.expectedFallback {
				t.Errorf("FallbackAllowed = %v, want %v", result.FallbackAllowed, tt.expectedFallback)
			}
			if result.SegmentName != tt.expectedSegment {
				t.Errorf("SegmentName = %q, want %q", result.SegmentName, tt.expectedSegment)
			}
		})
	}
}

// TestGetSegmentName tests segment name extraction from addresses.
func TestGetSegmentName(t *testing.T) {
	tests := []struct {
		name     string
		addr     resolver.Address
		expected string
	}{
		{
			name: "From capability attribute",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:8080"}, ShmCapability{
				Enabled:     true,
				SegmentName: "cap_segment",
			}),
			expected: "cap_segment",
		},
		{
			name:     "From shm: prefix",
			addr:     resolver.Address{Addr: "shm:prefix_segment"},
			expected: "prefix_segment",
		},
		{
			name:     "From ServerName",
			addr:     resolver.Address{Addr: "localhost:8080", ServerName: "server_segment"},
			expected: "server_segment",
		},
		{
			name:     "From Addr fallback",
			addr:     resolver.Address{Addr: "fallback_segment"},
			expected: "fallback_segment",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetSegmentName(tt.addr)
			if got != tt.expected {
				t.Errorf("GetSegmentName() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// TestIsFallbackAllowed tests the fallback logic.
func TestIsFallbackAllowed(t *testing.T) {
	tests := []struct {
		name     string
		addr     resolver.Address
		expected bool
	}{
		{
			name:     "No attributes - fallback allowed",
			addr:     resolver.Address{Addr: "localhost:8080"},
			expected: true,
		},
		{
			name: "Hint allows fallback",
			addr: SetShmTransportHint(resolver.Address{Addr: "localhost:8080"},
				ShmTransportHint{PreferShm: true, FallbackAllowed: true}),
			expected: true,
		},
		{
			name: "Hint disallows fallback",
			addr: SetShmTransportHint(resolver.Address{Addr: "localhost:8080"},
				ShmTransportHint{PreferShm: true, FallbackAllowed: false}),
			expected: false,
		},
		{
			name: "Capability required - no fallback",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:8080"},
				ShmCapability{Enabled: true, Required: true}),
			expected: false,
		},
		{
			name: "Capability not required - fallback allowed",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:8080"},
				ShmCapability{Enabled: true, Required: false}),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsFallbackAllowed(tt.addr)
			if got != tt.expected {
				t.Errorf("IsFallbackAllowed() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestCanUseShmForAddress tests the quick shm availability check.
func TestCanUseShmForAddress(t *testing.T) {
	tests := []struct {
		name     string
		addr     resolver.Address
		expected bool
	}{
		{
			name:     "No capability - false",
			addr:     resolver.Address{Addr: "localhost:8080"},
			expected: false,
		},
		{
			name: "Capability enabled - true",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:8080"},
				ShmCapability{Enabled: true}),
			expected: true,
		},
		{
			name: "Capability disabled - false",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:8080"},
				ShmCapability{Enabled: false}),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CanUseShmForAddress(tt.addr)
			if got != tt.expected {
				t.Errorf("CanUseShmForAddress() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestMustUseShmForAddress tests the required shm check.
func TestMustUseShmForAddress(t *testing.T) {
	tests := []struct {
		name     string
		addr     resolver.Address
		config   *ShmServiceConfig
		expected bool
	}{
		{
			name:     "No config, no hint - false",
			addr:     resolver.Address{Addr: "localhost:8080"},
			config:   nil,
			expected: false,
		},
		{
			name:     "Config required - true",
			addr:     resolver.Address{Addr: "localhost:8080"},
			config:   &ShmServiceConfig{Policy: ShmPolicyRequired},
			expected: true,
		},
		{
			name: "Hint requires shm - true",
			addr: SetShmTransportHint(resolver.Address{Addr: "localhost:8080"},
				ShmTransportHint{PreferShm: true, FallbackAllowed: false}),
			config:   nil,
			expected: true,
		},
		{
			name: "Hint prefers but allows fallback - false",
			addr: SetShmTransportHint(resolver.Address{Addr: "localhost:8080"},
				ShmTransportHint{PreferShm: true, FallbackAllowed: true}),
			config:   nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MustUseShmForAddress(tt.addr, tt.config)
			if got != tt.expected {
				t.Errorf("MustUseShmForAddress() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestShmAwareDialerShouldUseShm tests the dialer's transport selection.
func TestShmAwareDialerShouldUseShm(t *testing.T) {
	dialer := NewShmAwareDialer(nil, nil)

	tests := []struct {
		name     string
		addr     resolver.Address
		expected bool
	}{
		{
			name:     "No capability - false",
			addr:     resolver.Address{Addr: "localhost:8080"},
			expected: false,
		},
		{
			name: "Capability enabled - true",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:8080"},
				ShmCapability{Enabled: true, SegmentName: "test"}),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dialer.ShouldUseShm(tt.addr)
			if got != tt.expected {
				t.Errorf("ShouldUseShm() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestNewTransportSelector tests the constructor.
func TestNewTransportSelector(t *testing.T) {
	// With nil config
	s1 := NewTransportSelector(nil)
	if s1 == nil {
		t.Error("NewTransportSelector(nil) returned nil")
	}
	if s1.ServiceConfig != nil {
		t.Error("Expected nil ServiceConfig")
	}

	// With config
	cfg := &ShmServiceConfig{Policy: ShmPolicyPreferred}
	s2 := NewTransportSelector(cfg)
	if s2.ServiceConfig != cfg {
		t.Error("ServiceConfig not set correctly")
	}
}
