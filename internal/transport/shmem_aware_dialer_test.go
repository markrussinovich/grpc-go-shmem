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
		{"Shmem", TransportTypeShmem, "Shmem"},
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
		config   *ShmemServiceConfig
		expected TransportType
	}{
		{
			name:     "No attributes - HTTP2",
			addr:     resolver.Address{Addr: "localhost:8080"},
			config:   nil,
			expected: TransportTypeHTTP2,
		},
		{
			name: "Shmem enabled - Shmem selected",
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:8080"}, ShmemCapability{
				Enabled:     true,
				SegmentName: "test_segment",
			}),
			config:   nil,
			expected: TransportTypeShmem,
		},
		{
			name: "Shmem enabled but policy disabled - HTTP2",
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:8080"}, ShmemCapability{
				Enabled:     true,
				SegmentName: "test_segment",
			}),
			config:   &ShmemServiceConfig{Policy: ShmemPolicyDisabled},
			expected: TransportTypeHTTP2,
		},
		{
			// When policy is required but address has no capability,
			// we still try shmem (and let it fail) rather than silently fall back to HTTP2.
			// This is intentional - "required" means we should not use HTTP2.
			name:   "Shmem not enabled, policy required - still attempts Shmem (will fail)",
			addr:   resolver.Address{Addr: "localhost:8080"},
			config: &ShmemServiceConfig{Policy: ShmemPolicyRequired},
			expected: TransportTypeShmem,
		},
		{
			name: "Transport hint prefers shmem",
			addr: SetShmemTransportHint(
				SetShmemCapability(resolver.Address{Addr: "localhost:8080"}, ShmemCapability{
					Enabled:     true,
					SegmentName: "test_segment",
				}),
				ShmemTransportHint{PreferShmem: true, FallbackAllowed: true},
			),
			config:   nil,
			expected: TransportTypeShmem,
		},
		{
			name: "Transport hint prefers HTTP2",
			addr: SetShmemTransportHint(
				SetShmemCapability(resolver.Address{Addr: "localhost:8080"}, ShmemCapability{
					Enabled:     true,
					SegmentName: "test_segment",
				}),
				ShmemTransportHint{PreferShmem: false, FallbackAllowed: true},
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
		config          *ShmemServiceConfig
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
			name: "Shmem with fallback allowed",
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:8080"}, ShmemCapability{
				Enabled:     true,
				SegmentName: "my_segment",
			}),
			config:           nil,
			expectedType:     TransportTypeShmem,
			expectedFallback: true,
			expectedSegment:  "my_segment",
		},
		{
			name: "Shmem required - no fallback",
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:8080"}, ShmemCapability{
				Enabled:     true,
				SegmentName: "required_segment",
			}),
			config:           &ShmemServiceConfig{Policy: ShmemPolicyRequired},
			expectedType:     TransportTypeShmem,
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
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:8080"}, ShmemCapability{
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
			addr: SetShmemTransportHint(resolver.Address{Addr: "localhost:8080"},
				ShmemTransportHint{PreferShmem: true, FallbackAllowed: true}),
			expected: true,
		},
		{
			name: "Hint disallows fallback",
			addr: SetShmemTransportHint(resolver.Address{Addr: "localhost:8080"},
				ShmemTransportHint{PreferShmem: true, FallbackAllowed: false}),
			expected: false,
		},
		{
			name: "Capability required - no fallback",
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:8080"},
				ShmemCapability{Enabled: true, Required: true}),
			expected: false,
		},
		{
			name: "Capability not required - fallback allowed",
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:8080"},
				ShmemCapability{Enabled: true, Required: false}),
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

// TestCanUseShmemForAddress tests the quick shmem availability check.
func TestCanUseShmemForAddress(t *testing.T) {
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
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:8080"},
				ShmemCapability{Enabled: true}),
			expected: true,
		},
		{
			name: "Capability disabled - false",
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:8080"},
				ShmemCapability{Enabled: false}),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CanUseShmemForAddress(tt.addr)
			if got != tt.expected {
				t.Errorf("CanUseShmemForAddress() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestMustUseShmemForAddress tests the required shmem check.
func TestMustUseShmemForAddress(t *testing.T) {
	tests := []struct {
		name     string
		addr     resolver.Address
		config   *ShmemServiceConfig
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
			config:   &ShmemServiceConfig{Policy: ShmemPolicyRequired},
			expected: true,
		},
		{
			name: "Hint requires shmem - true",
			addr: SetShmemTransportHint(resolver.Address{Addr: "localhost:8080"},
				ShmemTransportHint{PreferShmem: true, FallbackAllowed: false}),
			config:   nil,
			expected: true,
		},
		{
			name: "Hint prefers but allows fallback - false",
			addr: SetShmemTransportHint(resolver.Address{Addr: "localhost:8080"},
				ShmemTransportHint{PreferShmem: true, FallbackAllowed: true}),
			config:   nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MustUseShmemForAddress(tt.addr, tt.config)
			if got != tt.expected {
				t.Errorf("MustUseShmemForAddress() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestShmemAwareDialerShouldUseShmem tests the dialer's transport selection.
func TestShmemAwareDialerShouldUseShmem(t *testing.T) {
	dialer := NewShmemAwareDialer(nil, nil)

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
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:8080"},
				ShmemCapability{Enabled: true, SegmentName: "test"}),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dialer.ShouldUseShmem(tt.addr)
			if got != tt.expected {
				t.Errorf("ShouldUseShmem() = %v, want %v", got, tt.expected)
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
	cfg := &ShmemServiceConfig{Policy: ShmemPolicyPreferred}
	s2 := NewTransportSelector(cfg)
	if s2.ServiceConfig != cfg {
		t.Error("ServiceConfig not set correctly")
	}
}
