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

package engine

import (
	"fmt"
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
