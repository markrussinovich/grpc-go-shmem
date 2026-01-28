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

// TestShmFallbackErrorTypes tests that errors are correctly categorized
// as retryable or permanent.
func TestShmFallbackErrorTypes(t *testing.T) {
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
			err:         NewShmError(ShmErrSegmentNotFound, "segment /test not found"),
			isRetryable: true,
			isPermanent: false,
		},
		{
			name:        "permission denied - retryable",
			err:         NewShmError(ShmErrPermissionDenied, "access denied"),
			isRetryable: true,
			isPermanent: false,
		},
		{
			name:        "connection refused - retryable",
			err:         NewShmError(ShmErrConnectionRefused, "server not listening"),
			isRetryable: true,
			isPermanent: false,
		},
		{
			name:        "protocol mismatch - permanent",
			err:         NewShmError(ShmErrProtocolMismatch, "version mismatch"),
			isRetryable: false,
			isPermanent: true,
		},
		{
			name:        "invalid config - permanent",
			err:         NewShmError(ShmErrInvalidConfig, "bad segment size"),
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
			if got := IsShmErrorRetryable(tt.err); got != tt.isRetryable {
				t.Errorf("IsShmErrorRetryable(%v) = %v, want %v", tt.err, got, tt.isRetryable)
			}
			if got := IsShmErrorPermanent(tt.err); got != tt.isPermanent {
				t.Errorf("IsShmErrorPermanent(%v) = %v, want %v", tt.err, got, tt.isPermanent)
			}
		})
	}
}

// TestShmFallbackHandler tests the fallback handler logic.
func TestShmFallbackHandler(t *testing.T) {
	tests := []struct {
		name            string
		ShmErr        error
		fallbackAllowed bool
		expectFallback  bool
		expectError     bool
	}{
		{
			name:            "shm succeeds - no fallback needed",
			ShmErr:        nil,
			fallbackAllowed: true,
			expectFallback:  false,
			expectError:     false,
		},
		{
			name:            "shm fails, fallback allowed - should fallback",
			ShmErr:        NewShmError(ShmErrSegmentNotFound, "segment missing"),
			fallbackAllowed: true,
			expectFallback:  true,
			expectError:     false,
		},
		{
			name:            "shm fails, fallback not allowed - should error",
			ShmErr:        NewShmError(ShmErrSegmentNotFound, "segment missing"),
			fallbackAllowed: false,
			expectFallback:  false,
			expectError:     true,
		},
		{
			name:            "permanent error, fallback allowed - should fallback",
			ShmErr:        NewShmError(ShmErrProtocolMismatch, "version mismatch"),
			fallbackAllowed: true,
			expectFallback:  true,
			expectError:     false,
		},
		{
			name:            "permanent error, fallback not allowed - should error",
			ShmErr:        NewShmError(ShmErrProtocolMismatch, "version mismatch"),
			fallbackAllowed: false,
			expectFallback:  false,
			expectError:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewShmFallbackHandler()

			result := handler.HandleShmError(tt.ShmErr, tt.fallbackAllowed)

			if result.ShouldFallback != tt.expectFallback {
				t.Errorf("ShouldFallback = %v, want %v", result.ShouldFallback, tt.expectFallback)
			}
			if (result.Error != nil) != tt.expectError {
				t.Errorf("Error = %v, wantError = %v", result.Error, tt.expectError)
			}
		})
	}
}

// TestShmFallbackWithAddress tests fallback behavior based on address attributes.
func TestShmFallbackWithAddress(t *testing.T) {
	tests := []struct {
		name           string
		addr           resolver.Address
		ShmErr       error
		expectFallback bool
	}{
		{
			name: "address allows fallback - should fallback on error",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:50051"}, ShmCapability{
				Enabled:     true,
				SegmentName: "test_seg",
				Required:    false, // fallback allowed
			}),
			ShmErr:       NewShmError(ShmErrSegmentNotFound, "missing"),
			expectFallback: true,
		},
		{
			name: "address requires shm - should not fallback",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:50051"}, ShmCapability{
				Enabled:     true,
				SegmentName: "test_seg",
				Required:    true, // no fallback
			}),
			ShmErr:       NewShmError(ShmErrSegmentNotFound, "missing"),
			expectFallback: false,
		},
		{
			name:           "no shm capability - should use HTTP2 directly (not a fallback)",
			addr:           resolver.Address{Addr: "remote:50051"},
			ShmErr:       nil, // no shm attempt
			expectFallback: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewShmFallbackHandler()

			fallbackAllowed := IsFallbackAllowed(tt.addr)

			if tt.ShmErr != nil {
				result := handler.HandleShmError(tt.ShmErr, fallbackAllowed)
				if result.ShouldFallback != tt.expectFallback {
					t.Errorf("ShouldFallback = %v, want %v for addr %v",
						result.ShouldFallback, tt.expectFallback, tt.addr)
				}
			}
		})
	}
}

// TestShmFallbackMetrics tests that fallback events are tracked.
func TestShmFallbackMetrics(t *testing.T) {
	handler := NewShmFallbackHandler()

	// Initially no fallbacks
	if handler.FallbackCount() != 0 {
		t.Errorf("Initial FallbackCount = %d, want 0", handler.FallbackCount())
	}

	// Trigger fallback
	handler.HandleShmError(
		NewShmError(ShmErrSegmentNotFound, "missing"),
		true, // fallback allowed
	)

	if handler.FallbackCount() != 1 {
		t.Errorf("After fallback, FallbackCount = %d, want 1", handler.FallbackCount())
	}

	// Failed attempt (no fallback allowed) should not count
	handler.HandleShmError(
		NewShmError(ShmErrSegmentNotFound, "missing"),
		false, // fallback not allowed
	)

	if handler.FallbackCount() != 1 {
		t.Errorf("After failed attempt, FallbackCount = %d, want 1", handler.FallbackCount())
	}
}

// TestShmFallbackIntegration tests the full fallback flow in TransportSelector.
func TestShmFallbackIntegration(t *testing.T) {
	tests := []struct {
		name           string
		addr           resolver.Address
		config         *ShmServiceConfig
		simulateError  error
		expectedResult TransportType
		expectError    bool
	}{
		{
			name: "shm works - use shm",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:50051"}, ShmCapability{
				Enabled:     true,
				SegmentName: "test",
			}),
			config:         &ShmServiceConfig{Policy: ShmPolicyPreferred},
			simulateError:  nil,
			expectedResult: TransportTypeShm,
			expectError:    false,
		},
		{
			name: "shm fails, policy preferred - fallback to HTTP2",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:50051"}, ShmCapability{
				Enabled:     true,
				SegmentName: "test",
			}),
			config:         &ShmServiceConfig{Policy: ShmPolicyPreferred},
			simulateError:  NewShmError(ShmErrSegmentNotFound, "missing"),
			expectedResult: TransportTypeHTTP2,
			expectError:    false,
		},
		{
			name: "shm fails, policy required - error",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:50051"}, ShmCapability{
				Enabled:     true,
				SegmentName: "test",
				Required:    true,
			}),
			config:         &ShmServiceConfig{Policy: ShmPolicyRequired},
			simulateError:  NewShmError(ShmErrSegmentNotFound, "missing"),
			expectedResult: TransportTypeShm, // attempted shm
			expectError:    true,
		},
		{
			name:           "no shm capability - use HTTP2 directly",
			addr:           resolver.Address{Addr: "remote:50051"},
			config:         &ShmServiceConfig{Policy: ShmPolicyAuto},
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

			// If shm was selected and we're simulating an error, test fallback
			if transportType == TransportTypeShm && tt.simulateError != nil {
				fallbackAllowed := IsFallbackAllowed(tt.addr)
				result := selector.HandleTransportError(tt.simulateError, tt.addr)

				if result.ShouldFallback && !fallbackAllowed {
					t.Error("Fallback should not be allowed for required shm")
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
		ShmDialSucceeds  bool
		http2DialSucceeds  bool
		expectedTransport  string
		expectError        bool
	}{
		{
			name: "shm succeeds",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:50051"}, ShmCapability{
				Enabled:     true,
				SegmentName: "test",
			}),
			ShmDialSucceeds: true,
			http2DialSucceeds: true,
			expectedTransport: "Shm",
			expectError:       false,
		},
		{
			name: "shm fails, http2 succeeds - fallback",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:50051"}, ShmCapability{
				Enabled:     true,
				SegmentName: "test",
			}),
			ShmDialSucceeds: false,
			http2DialSucceeds: true,
			expectedTransport: "http2",
			expectError:       false,
		},
		{
			name: "shm required, shm fails - error",
			addr: SetShmCapability(resolver.Address{Addr: "localhost:50051"}, ShmCapability{
				Enabled:     true,
				SegmentName: "test",
				Required:    true,
			}),
			ShmDialSucceeds: false,
			http2DialSucceeds: true,
			expectedTransport: "",
			expectError:       true,
		},
		{
			name:               "no shm capability - http2 directly",
			addr:               resolver.Address{Addr: "remote:50051"},
			ShmDialSucceeds:  false,
			http2DialSucceeds:  true,
			expectedTransport:  "http2",
			expectError:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock dialers
			mockShmDialer := &mockDialer{succeeds: tt.ShmDialSucceeds, name: "Shm"}
			mockHTTP2Dialer := &mockDialer{succeeds: tt.http2DialSucceeds, name: "http2"}

			// Create fallback-aware transport creator
			creator := NewFallbackTransportCreator(mockShmDialer, mockHTTP2Dialer)

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
	return nil, NewShmError(ShmErrSegmentNotFound, "mock dial failed")
}

type mockTransport struct {
	name string
}
