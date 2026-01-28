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

// ShmemErrorCode represents the type of shared memory transport error.
type ShmemErrorCode int

const (
	// ShmemErrUnknown is an unknown error.
	ShmemErrUnknown ShmemErrorCode = iota
	// ShmemErrSegmentNotFound indicates the shared memory segment doesn't exist.
	ShmemErrSegmentNotFound
	// ShmemErrPermissionDenied indicates permission issues accessing the segment.
	ShmemErrPermissionDenied
	// ShmemErrConnectionRefused indicates the server is not listening on the segment.
	ShmemErrConnectionRefused
	// ShmemErrProtocolMismatch indicates incompatible protocol versions.
	ShmemErrProtocolMismatch
	// ShmemErrInvalidConfig indicates invalid configuration.
	ShmemErrInvalidConfig
	// ShmemErrTimeout indicates a timeout during connection.
	ShmemErrTimeout
	// ShmemErrResourceExhausted indicates no available resources (e.g., full buffer).
	ShmemErrResourceExhausted
)

// ShmemError represents an error from the shared memory transport layer.
type ShmemError struct {
	Code    ShmemErrorCode
	Message string
	Cause   error
}

// Error implements the error interface.
func (e *ShmemError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("shmem error [%s]: %s: %v", e.codeName(), e.Message, e.Cause)
	}
	return fmt.Sprintf("shmem error [%s]: %s", e.codeName(), e.Message)
}

// Unwrap returns the underlying error.
func (e *ShmemError) Unwrap() error {
	return e.Cause
}

// codeName returns a string representation of the error code.
func (e *ShmemError) codeName() string {
	switch e.Code {
	case ShmemErrSegmentNotFound:
		return "SEGMENT_NOT_FOUND"
	case ShmemErrPermissionDenied:
		return "PERMISSION_DENIED"
	case ShmemErrConnectionRefused:
		return "CONNECTION_REFUSED"
	case ShmemErrProtocolMismatch:
		return "PROTOCOL_MISMATCH"
	case ShmemErrInvalidConfig:
		return "INVALID_CONFIG"
	case ShmemErrTimeout:
		return "TIMEOUT"
	case ShmemErrResourceExhausted:
		return "RESOURCE_EXHAUSTED"
	default:
		return "UNKNOWN"
	}
}

// IsRetryable returns true if this error type should trigger a retry.
func (e *ShmemError) IsRetryable() bool {
	switch e.Code {
	case ShmemErrSegmentNotFound, ShmemErrPermissionDenied, ShmemErrConnectionRefused,
		ShmemErrTimeout, ShmemErrResourceExhausted, ShmemErrUnknown:
		return true
	case ShmemErrProtocolMismatch, ShmemErrInvalidConfig:
		return false
	default:
		return true
	}
}

// IsPermanent returns true if this error type is permanent and should not be retried.
func (e *ShmemError) IsPermanent() bool {
	switch e.Code {
	case ShmemErrProtocolMismatch, ShmemErrInvalidConfig:
		return true
	default:
		return false
	}
}

// NewShmemError creates a new ShmemError.
func NewShmemError(code ShmemErrorCode, message string) *ShmemError {
	return &ShmemError{
		Code:    code,
		Message: message,
	}
}

// NewShmemErrorWithCause creates a new ShmemError with an underlying cause.
func NewShmemErrorWithCause(code ShmemErrorCode, message string, cause error) *ShmemError {
	return &ShmemError{
		Code:    code,
		Message: message,
		Cause:   cause,
	}
}

// IsShmemErrorRetryable checks if an error is a retryable shmem error.
// Generic errors are considered retryable by default.
func IsShmemErrorRetryable(err error) bool {
	if err == nil {
		return false
	}

	var shmemErr *ShmemError
	if errors.As(err, &shmemErr) {
		return shmemErr.IsRetryable()
	}

	// Generic errors are retryable (might be transient)
	return true
}

// IsShmemErrorPermanent checks if an error is a permanent shmem error.
func IsShmemErrorPermanent(err error) bool {
	if err == nil {
		return false
	}

	var shmemErr *ShmemError
	if errors.As(err, &shmemErr) {
		return shmemErr.IsPermanent()
	}

	// Generic errors are not permanent
	return false
}

// FallbackResult represents the result of a fallback decision.
type FallbackResult struct {
	// ShouldFallback indicates whether to fall back to HTTP/2.
	ShouldFallback bool
	// Error is set if fallback is not allowed and shmem failed.
	Error error
	// OriginalError is the original shmem error.
	OriginalError error
}

// ShmemFallbackHandler handles fallback logic when shmem transport fails.
type ShmemFallbackHandler struct {
	fallbackCount atomic.Int64
}

// NewShmemFallbackHandler creates a new fallback handler.
func NewShmemFallbackHandler() *ShmemFallbackHandler {
	return &ShmemFallbackHandler{}
}

// HandleShmemError determines whether to fall back to HTTP/2 after a shmem error.
func (h *ShmemFallbackHandler) HandleShmemError(err error, fallbackAllowed bool) FallbackResult {
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
		Error:          fmt.Errorf("shmem transport failed and fallback not allowed: %w", err),
		OriginalError:  err,
	}
}

// FallbackCount returns the number of fallbacks that have occurred.
func (h *ShmemFallbackHandler) FallbackCount() int64 {
	return h.fallbackCount.Load()
}

// ResetMetrics resets the fallback counter.
func (h *ShmemFallbackHandler) ResetMetrics() {
	h.fallbackCount.Store(0)
}

// HandleTransportError handles transport errors and returns a fallback result.
// This is called by TransportSelector when a transport error occurs.
func (s *TransportSelector) HandleTransportError(err error, addr resolver.Address) FallbackResult {
	if s.fallbackHandler == nil {
		s.fallbackHandler = NewShmemFallbackHandler()
	}

	fallbackAllowed := IsFallbackAllowed(addr)
	return s.fallbackHandler.HandleShmemError(err, fallbackAllowed)
}

// TransportDialer is an interface for dialing transports.
type TransportDialer interface {
	Dial(ctx context.Context, addr resolver.Address) (interface{}, error)
}

// TransportCreatorResult contains the result of transport creation.
type TransportCreatorResult struct {
	Transport     interface{}
	TransportName string
	WasFallback   bool
}

// FallbackTransportCreator creates transports with fallback support.
type FallbackTransportCreator struct {
	shmemDialer   TransportDialer
	http2Dialer   TransportDialer
	selector      *TransportSelector
	fallbackHandler *ShmemFallbackHandler
}

// NewFallbackTransportCreator creates a new fallback-aware transport creator.
func NewFallbackTransportCreator(shmemDialer, http2Dialer TransportDialer) *FallbackTransportCreator {
	return &FallbackTransportCreator{
		shmemDialer:     shmemDialer,
		http2Dialer:     http2Dialer,
		selector:        NewTransportSelector(nil),
		fallbackHandler: NewShmemFallbackHandler(),
	}
}

// CreateTransport creates a transport for the given address, falling back if necessary.
func (c *FallbackTransportCreator) CreateTransport(ctx context.Context, addr resolver.Address) (*TransportCreatorResult, error) {
	// Check if shmem should be attempted
	transportType := c.selector.SelectTransport(addr)

	if transportType == TransportTypeShmem {
		// Try shmem first
		transport, err := c.shmemDialer.Dial(ctx, addr)
		if err == nil {
			return &TransportCreatorResult{
				Transport:     transport,
				TransportName: "shmem",
				WasFallback:   false,
			}, nil
		}

		// Shmem failed - check if fallback is allowed
		fallbackAllowed := IsFallbackAllowed(addr)
		result := c.fallbackHandler.HandleShmemError(err, fallbackAllowed)

		if !result.ShouldFallback {
			return nil, result.Error
		}

		// Fall back to HTTP/2
		transport, err = c.http2Dialer.Dial(ctx, addr)
		if err != nil {
			return nil, fmt.Errorf("http2 fallback also failed: %w (original shmem error: %v)",
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
