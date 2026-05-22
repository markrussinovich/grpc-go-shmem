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

// Package transport implements network transport for gRPC.
package transport

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"google.golang.org/grpc/resolver"
)

// ShmErrorCode represents the type of shared memory transport error.
type ShmErrorCode int

const (
	// ShmErrUnknown is an unknown error.
	ShmErrUnknown ShmErrorCode = iota
	// ShmErrSegmentNotFound indicates the shared memory segment doesn't exist.
	ShmErrSegmentNotFound
	// ShmErrPermissionDenied indicates permission issues accessing the segment.
	ShmErrPermissionDenied
	// ShmErrConnectionRefused indicates the server is not listening on the segment.
	ShmErrConnectionRefused
	// ShmErrProtocolMismatch indicates incompatible protocol versions.
	ShmErrProtocolMismatch
	// ShmErrInvalidConfig indicates invalid configuration.
	ShmErrInvalidConfig
	// ShmErrTimeout indicates a timeout during connection.
	ShmErrTimeout
	// ShmErrResourceExhausted indicates no available resources (e.g., full buffer).
	ShmErrResourceExhausted
)

// ShmError represents an error from the shared memory transport layer.
type ShmError struct {
	Code    ShmErrorCode
	Message string
	Cause   error
}

// Error implements the error interface.
func (e *ShmError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("shm error [%s]: %s: %v", e.codeName(), e.Message, e.Cause)
	}
	return fmt.Sprintf("shm error [%s]: %s", e.codeName(), e.Message)
}

// Unwrap returns the underlying error.
func (e *ShmError) Unwrap() error {
	return e.Cause
}

// codeName returns a string representation of the error code.
func (e *ShmError) codeName() string {
	switch e.Code {
	case ShmErrSegmentNotFound:
		return "SEGMENT_NOT_FOUND"
	case ShmErrPermissionDenied:
		return "PERMISSION_DENIED"
	case ShmErrConnectionRefused:
		return "CONNECTION_REFUSED"
	case ShmErrProtocolMismatch:
		return "PROTOCOL_MISMATCH"
	case ShmErrInvalidConfig:
		return "INVALID_CONFIG"
	case ShmErrTimeout:
		return "TIMEOUT"
	case ShmErrResourceExhausted:
		return "RESOURCE_EXHAUSTED"
	default:
		return "UNKNOWN"
	}
}

// IsRetryable returns true if this error type should trigger a retry.
func (e *ShmError) IsRetryable() bool {
	switch e.Code {
	case ShmErrSegmentNotFound, ShmErrPermissionDenied, ShmErrConnectionRefused,
		ShmErrTimeout, ShmErrResourceExhausted, ShmErrUnknown:
		return true
	case ShmErrProtocolMismatch, ShmErrInvalidConfig:
		return false
	default:
		return true
	}
}

// IsPermanent returns true if this error type is permanent and should not be retried.
func (e *ShmError) IsPermanent() bool {
	switch e.Code {
	case ShmErrProtocolMismatch, ShmErrInvalidConfig:
		return true
	default:
		return false
	}
}

// NewShmError creates a new ShmError.
func NewShmError(code ShmErrorCode, message string) *ShmError {
	return &ShmError{
		Code:    code,
		Message: message,
	}
}

// NewShmErrorWithCause creates a new ShmError with an underlying cause.
func NewShmErrorWithCause(code ShmErrorCode, message string, cause error) *ShmError {
	return &ShmError{
		Code:    code,
		Message: message,
		Cause:   cause,
	}
}

// IsShmErrorRetryable checks if an error is a retryable shm error.
// Generic errors are considered retryable by default.
func IsShmErrorRetryable(err error) bool {
	if err == nil {
		return false
	}

	var ShmErr *ShmError
	if errors.As(err, &ShmErr) {
		return ShmErr.IsRetryable()
	}

	// Generic errors are retryable (might be transient)
	return true
}

// IsShmErrorPermanent checks if an error is a permanent shm error.
func IsShmErrorPermanent(err error) bool {
	if err == nil {
		return false
	}

	var ShmErr *ShmError
	if errors.As(err, &ShmErr) {
		return ShmErr.IsPermanent()
	}

	// Generic errors are not permanent
	return false
}

// FallbackResult represents the result of a fallback decision.
type FallbackResult struct {
	// ShouldFallback indicates whether to fall back to HTTP/2.
	ShouldFallback bool
	// Error is set if fallback is not allowed and shm failed.
	Error error
	// OriginalError is the original shm error.
	OriginalError error
}

// ShmFallbackHandler handles fallback logic when shm transport fails.
type ShmFallbackHandler struct {
	fallbackCount atomic.Int64
}

// NewShmFallbackHandler creates a new fallback handler.
func NewShmFallbackHandler() *ShmFallbackHandler {
	return &ShmFallbackHandler{}
}

// HandleShmError determines whether to fall back to HTTP/2 after a shm error.
func (h *ShmFallbackHandler) HandleShmError(err error, fallbackAllowed bool) FallbackResult {
	if err == nil {
		return FallbackResult{
			ShouldFallback: false,
			Error:          nil,
		}
	}

	if fallbackAllowed {
		h.fallbackCount.Add(1)
		return FallbackResult{
			ShouldFallback: true,
			Error:          nil,
			OriginalError:  err,
		}
	}

	// Fallback not allowed - return the error
	return FallbackResult{
		ShouldFallback: false,
		Error:          fmt.Errorf("shm transport failed and fallback not allowed: %w", err),
		OriginalError:  err,
	}
}

// FallbackCount returns the number of fallbacks that have occurred.
func (h *ShmFallbackHandler) FallbackCount() int64 {
	return h.fallbackCount.Load()
}

// ResetMetrics resets the fallback counter.
func (h *ShmFallbackHandler) ResetMetrics() {
	h.fallbackCount.Store(0)
}

// HandleTransportError handles transport errors and returns a fallback result.
// This is called by TransportSelector when a transport error occurs.
func (s *TransportSelector) HandleTransportError(err error, addr resolver.Address) FallbackResult {
	if s.fallbackHandler == nil {
		s.fallbackHandler = NewShmFallbackHandler()
	}

	fallbackAllowed := IsFallbackAllowed(addr)
	return s.fallbackHandler.HandleShmError(err, fallbackAllowed)
}

// TransportDialer is an interface for dialing transports.
//
//revive:disable-next-line:exported stuttering is acceptable for this exported type in internal package
type TransportDialer interface {
	Dial(ctx context.Context, addr resolver.Address) (any, error)
}

// TransportCreatorResult contains the result of transport creation.
//
//revive:disable-next-line:exported stuttering is acceptable for this exported type in internal package
type TransportCreatorResult struct {
	Transport     any
	TransportName string
	WasFallback   bool
}

// FallbackTransportCreator creates transports with fallback support.
type FallbackTransportCreator struct {
	ShmDialer       TransportDialer
	http2Dialer     TransportDialer
	selector        *TransportSelector
	fallbackHandler *ShmFallbackHandler
}

// NewFallbackTransportCreator creates a new fallback-aware transport creator.
func NewFallbackTransportCreator(ShmDialer, http2Dialer TransportDialer) *FallbackTransportCreator {
	return &FallbackTransportCreator{
		ShmDialer:       ShmDialer,
		http2Dialer:     http2Dialer,
		selector:        NewTransportSelector(nil),
		fallbackHandler: NewShmFallbackHandler(),
	}
}

// CreateTransport creates a transport for the given address, falling back if necessary.
func (c *FallbackTransportCreator) CreateTransport(ctx context.Context, addr resolver.Address) (*TransportCreatorResult, error) {
	// Check if shm should be attempted
	transportType := c.selector.SelectTransport(addr)

	if transportType == TransportTypeShm {
		// Try shm first
		transport, err := c.ShmDialer.Dial(ctx, addr)
		if err == nil {
			return &TransportCreatorResult{
				Transport:     transport,
				TransportName: "Shm",
				WasFallback:   false,
			}, nil
		}

		// shm failed - check if fallback is allowed
		fallbackAllowed := IsFallbackAllowed(addr)
		result := c.fallbackHandler.HandleShmError(err, fallbackAllowed)

		if !result.ShouldFallback {
			return nil, result.Error
		}

		// Fall back to HTTP/2
		transport, err = c.http2Dialer.Dial(ctx, addr)
		if err != nil {
			return nil, fmt.Errorf("http2 fallback also failed: %w (original shm error: %v)",
				err, result.OriginalError)
		}

		return &TransportCreatorResult{
			Transport:     transport,
			TransportName: "http2",
			WasFallback:   true,
		}, nil
	}

	// Use HTTP/2 directly
	transport, err := c.http2Dialer.Dial(ctx, addr)
	if err != nil {
		return nil, err
	}

	return &TransportCreatorResult{
		Transport:     transport,
		TransportName: "http2",
		WasFallback:   false,
	}, nil
}

// FallbackCount returns the number of fallbacks that have occurred.
func (c *FallbackTransportCreator) FallbackCount() int64 {
	return c.fallbackHandler.FallbackCount()
}
