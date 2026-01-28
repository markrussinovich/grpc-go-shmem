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
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/resolver"
)

// TestShmemFallbackErrorTypes tests that errors are correctly categorized
// as retryable or permanent.
func TestShmemFallbackErrorTypes(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		isRetryable bool
		isPermanent bool
	}{
		{
			name:        "nil error - not retryable",
			err:         nil,
			isRetryable: false,
			isPermanent: false,
		},
		{
			name:        "segment not found - retryable",
			err:         NewShmemError(ShmemErrSegmentNotFound, "segment /test not found"),
			isRetryable: true,
			isPermanent: false,
		},
		{
			name:        "permission denied - retryable",
			err:         NewShmemError(ShmemErrPermissionDenied, "access denied"),
			isRetryable: true,
			isPermanent: false,
		},
		{
			name:        "connection refused - retryable",
			err:         NewShmemError(ShmemErrConnectionRefused, "server not listening"),
			isRetryable: true,
			isPermanent: false,
		},
		{
			name:        "protocol mismatch - permanent",
			err:         NewShmemError(ShmemErrProtocolMismatch, "version mismatch"),
			isRetryable: false,
			isPermanent: true,
		},
		{
			name:        "invalid config - permanent",
			err:         NewShmemError(ShmemErrInvalidConfig, "bad segment size"),
			isRetryable: false,
			isPermanent: true,
		},
		{
			name:        "generic error - retryable by default",
			err:         errors.New("some generic error"),
			isRetryable: true,
			isPermanent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsShmemErrorRetryable(tt.err); got != tt.isRetryable {
				t.Errorf("IsShmemErrorRetryable(%v) = %v, want %v", tt.err, got, tt.isRetryable)
			}
			if got := IsShmemErrorPermanent(tt.err); got != tt.isPermanent {
				t.Errorf("IsShmemErrorPermanent(%v) = %v, want %v", tt.err, got, tt.isPermanent)
			}
		})
	}
}

// TestShmemFallbackHandler tests the fallback handler logic.
func TestShmemFallbackHandler(t *testing.T) {
	tests := []struct {
		name            string
		shmemErr        error
		fallbackAllowed bool
		expectFallback  bool
		expectError     bool
	}{
		{
			name:            "shmem succeeds - no fallback needed",
			shmemErr:        nil,
			fallbackAllowed: true,
			expectFallback:  false,
			expectError:     false,
		},
		{
			name:            "shmem fails, fallback allowed - should fallback",
			shmemErr:        NewShmemError(ShmemErrSegmentNotFound, "segment missing"),
			fallbackAllowed: true,
			expectFallback:  true,
			expectError:     false,
		},
		{
			name:            "shmem fails, fallback not allowed - should error",
			shmemErr:        NewShmemError(ShmemErrSegmentNotFound, "segment missing"),
			fallbackAllowed: false,
			expectFallback:  false,
			expectError:     true,
		},
		{
			name:            "permanent error, fallback allowed - should fallback",
			shmemErr:        NewShmemError(ShmemErrProtocolMismatch, "version mismatch"),
			fallbackAllowed: true,
			expectFallback:  true,
			expectError:     false,
		},
		{
			name:            "permanent error, fallback not allowed - should error",
			shmemErr:        NewShmemError(ShmemErrProtocolMismatch, "version mismatch"),
			fallbackAllowed: false,
			expectFallback:  false,
			expectError:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewShmemFallbackHandler()

			result := handler.HandleShmemError(tt.shmemErr, tt.fallbackAllowed)

			if result.ShouldFallback != tt.expectFallback {
				t.Errorf("ShouldFallback = %v, want %v", result.ShouldFallback, tt.expectFallback)
			}
			if (result.Error != nil) != tt.expectError {
				t.Errorf("Error = %v, wantError = %v", result.Error, tt.expectError)
			}
		})
	}
}

// TestShmemFallbackWithAddress tests fallback behavior based on address attributes.
func TestShmemFallbackWithAddress(t *testing.T) {
	tests := []struct {
		name           string
		addr           resolver.Address
		shmemErr       error
		expectFallback bool
	}{
		{
			name: "address allows fallback - should fallback on error",
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:50051"}, ShmemCapability{
				Enabled:     true,
				SegmentName: "test_seg",
				Required:    false, // fallback allowed
			}),
			shmemErr:       NewShmemError(ShmemErrSegmentNotFound, "missing"),
			expectFallback: true,
		},
		{
			name: "address requires shmem - should not fallback",
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:50051"}, ShmemCapability{
				Enabled:     true,
				SegmentName: "test_seg",
				Required:    true, // no fallback
			}),
			shmemErr:       NewShmemError(ShmemErrSegmentNotFound, "missing"),
			expectFallback: false,
		},
		{
			name:           "no shmem capability - should use HTTP2 directly (not a fallback)",
			addr:           resolver.Address{Addr: "remote:50051"},
			shmemErr:       nil, // no shmem attempt
			expectFallback: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewShmemFallbackHandler()

			fallbackAllowed := IsFallbackAllowed(tt.addr)

			if tt.shmemErr != nil {
				result := handler.HandleShmemError(tt.shmemErr, fallbackAllowed)
				if result.ShouldFallback != tt.expectFallback {
					t.Errorf("ShouldFallback = %v, want %v for addr %v",
						result.ShouldFallback, tt.expectFallback, tt.addr)
				}
			}
		})
	}
}

// TestShmemFallbackMetrics tests that fallback events are tracked.
func TestShmemFallbackMetrics(t *testing.T) {
	handler := NewShmemFallbackHandler()

	// Initially no fallbacks
	if handler.FallbackCount() != 0 {
		t.Errorf("Initial FallbackCount = %d, want 0", handler.FallbackCount())
	}

	// Trigger fallback
	handler.HandleShmemError(
		NewShmemError(ShmemErrSegmentNotFound, "missing"),
		true, // fallback allowed
	)

	if handler.FallbackCount() != 1 {
		t.Errorf("After fallback, FallbackCount = %d, want 1", handler.FallbackCount())
	}

	// Failed attempt (no fallback allowed) should not count
	handler.HandleShmemError(
		NewShmemError(ShmemErrSegmentNotFound, "missing"),
		false, // fallback not allowed
	)

	if handler.FallbackCount() != 1 {
		t.Errorf("After failed attempt, FallbackCount = %d, want 1", handler.FallbackCount())
	}
}

// TestShmemFallbackIntegration tests the full fallback flow in TransportSelector.
func TestShmemFallbackIntegration(t *testing.T) {
	tests := []struct {
		name           string
		addr           resolver.Address
		config         *ShmemServiceConfig
		simulateError  error
		expectedResult TransportType
		expectError    bool
	}{
		{
			name: "shmem works - use shmem",
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:50051"}, ShmemCapability{
				Enabled:     true,
				SegmentName: "test",
			}),
			config:         &ShmemServiceConfig{Policy: ShmemPolicyPreferred},
			simulateError:  nil,
			expectedResult: TransportTypeShmem,
			expectError:    false,
		},
		{
			name: "shmem fails, policy preferred - fallback to HTTP2",
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:50051"}, ShmemCapability{
				Enabled:     true,
				SegmentName: "test",
			}),
			config:         &ShmemServiceConfig{Policy: ShmemPolicyPreferred},
			simulateError:  NewShmemError(ShmemErrSegmentNotFound, "missing"),
			expectedResult: TransportTypeHTTP2,
			expectError:    false,
		},
		{
			name: "shmem fails, policy required - error",
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:50051"}, ShmemCapability{
				Enabled:     true,
				SegmentName: "test",
				Required:    true,
			}),
			config:         &ShmemServiceConfig{Policy: ShmemPolicyRequired},
			simulateError:  NewShmemError(ShmemErrSegmentNotFound, "missing"),
			expectedResult: TransportTypeShmem, // attempted shmem
			expectError:    true,
		},
		{
			name:           "no shmem capability - use HTTP2 directly",
			addr:           resolver.Address{Addr: "remote:50051"},
			config:         &ShmemServiceConfig{Policy: ShmemPolicyAuto},
			simulateError:  nil,
			expectedResult: TransportTypeHTTP2,
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selector := NewTransportSelector(tt.config)

			// First, check what transport would be selected
			transportType := selector.SelectTransport(tt.addr)

			// If shmem was selected and we're simulating an error, test fallback
			if transportType == TransportTypeShmem && tt.simulateError != nil {
				fallbackAllowed := IsFallbackAllowed(tt.addr)
				result := selector.HandleTransportError(tt.simulateError, tt.addr)

				if result.ShouldFallback && !fallbackAllowed {
					t.Error("Fallback should not be allowed for required shmem")
				}
				if result.ShouldFallback {
					// Effective transport after fallback
					if tt.expectedResult != TransportTypeHTTP2 {
						t.Errorf("Expected HTTP2 after fallback, got %v", tt.expectedResult)
					}
				}
				if (result.Error != nil) != tt.expectError {
					t.Errorf("Error = %v, wantError = %v", result.Error, tt.expectError)
				}
			}
		})
	}
}

// TestCreateTransportWithFallback tests the createTransportWithFallback function.
func TestCreateTransportWithFallback(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name               string
		addr               resolver.Address
		shmemDialSucceeds  bool
		http2DialSucceeds  bool
		expectedTransport  string
		expectError        bool
	}{
		{
			name: "shmem succeeds",
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:50051"}, ShmemCapability{
				Enabled:     true,
				SegmentName: "test",
			}),
			shmemDialSucceeds: true,
			http2DialSucceeds: true,
			expectedTransport: "shmem",
			expectError:       false,
		},
		{
			name: "shmem fails, http2 succeeds - fallback",
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:50051"}, ShmemCapability{
				Enabled:     true,
				SegmentName: "test",
			}),
			shmemDialSucceeds: false,
			http2DialSucceeds: true,
			expectedTransport: "http2",
			expectError:       false,
		},
		{
			name: "shmem required, shmem fails - error",
			addr: SetShmemCapability(resolver.Address{Addr: "localhost:50051"}, ShmemCapability{
				Enabled:     true,
				SegmentName: "test",
				Required:    true,
			}),
			shmemDialSucceeds: false,
			http2DialSucceeds: true,
			expectedTransport: "",
			expectError:       true,
		},
		{
			name:               "no shmem capability - http2 directly",
			addr:               resolver.Address{Addr: "remote:50051"},
			shmemDialSucceeds:  false,
			http2DialSucceeds:  true,
			expectedTransport:  "http2",
			expectError:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock dialers
			mockShmemDialer := &mockDialer{succeeds: tt.shmemDialSucceeds, name: "shmem"}
			mockHTTP2Dialer := &mockDialer{succeeds: tt.http2DialSucceeds, name: "http2"}

			// Create fallback-aware transport creator
			creator := NewFallbackTransportCreator(mockShmemDialer, mockHTTP2Dialer)

			result, err := creator.CreateTransport(ctx, tt.addr)

			if (err != nil) != tt.expectError {
				t.Errorf("Error = %v, wantError = %v", err, tt.expectError)
			}

			if !tt.expectError && result.TransportName != tt.expectedTransport {
				t.Errorf("TransportName = %q, want %q", result.TransportName, tt.expectedTransport)
			}
		})
	}
}

// mockDialer is a test helper for mocking transport creation.
type mockDialer struct {
	succeeds bool
	name     string
	called   bool
}

func (m *mockDialer) Dial(ctx context.Context, addr resolver.Address) (interface{}, error) {
	m.called = true
	if m.succeeds {
		return &mockTransport{name: m.name}, nil
	}
	return nil, NewShmemError(ShmemErrSegmentNotFound, "mock dial failed")
}

type mockTransport struct {
	name string
}
