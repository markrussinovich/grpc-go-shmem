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
	"strings"

	"golang.org/x/net/http2"
	"google.golang.org/grpc/grpclog"
)

// logger is the engine's grpclog component, mirroring the internal transport
// package's logger so ported bootstrap code that logs stays self-contained.
var logger = grpclog.Component("shmsc")

// GoAwayReason contains the reason for the GoAway frame received.
type GoAwayReason uint8

const (
	// GoAwayInvalid indicates that no GoAway frame is received.
	GoAwayInvalid GoAwayReason = 0
	// GoAwayNoReason is the default value when GoAway frame is received.
	GoAwayNoReason GoAwayReason = 1
	// GoAwayTooManyPings indicates that a GoAway frame with
	// ErrCodeEnhanceYourCalm was received and that the debug data said
	// "too_many_pings".
	GoAwayTooManyPings GoAwayReason = 2
)

// GoAwayInfo contains metadata about why a connection was closed.
type GoAwayInfo struct {
	// Reason is the parsed reason for an HTTP/2 GOAWAY frame.
	Reason GoAwayReason
	// GoAwayCode is the raw HTTP/2 error code received in a GOAWAY frame.
	GoAwayCode http2.ErrCode
	// Err is the underlying error that caused the connection to close, if it was
	// closed due to a socket error or context cancellation without a GOAWAY.
	Err error
}

// bdpPingData is the fixed 8-byte payload of the BDP-estimation PING, matching
// grpc-go's controlbuf bdpPing.
var bdpPingData = [8]byte{2, 4, 16, 16, 9, 14, 7, 7}

// ShmAddr is a net.Addr identifying a shared-memory segment by name.
type ShmAddr struct {
	Name string // Segment name/identifier
}

// Network returns the network type.
func (a *ShmAddr) Network() string { return "shm" }

// String returns the string representation of the address.
func (a *ShmAddr) String() string { return a.Name }

// baseContentType is the base gRPC content type.
const baseContentType = "application/grpc"

// grpcContentType builds a full gRPC content type with the given sub-type. It
// vendors google.golang.org/grpc/internal/grpcutil.ContentType so the engine
// stays free of internal/* dependencies. contentSubtype is assumed lowercase.
func grpcContentType(contentSubtype string) string {
	if contentSubtype == "" {
		return baseContentType
	}
	return baseContentType + "+" + contentSubtype
}

// grpcContentSubtype extracts the content-subtype from a gRPC content type. It
// vendors google.golang.org/grpc/internal/grpcutil.ContentSubtype. contentType
// is assumed lowercase.
func grpcContentSubtype(contentType string) (string, bool) {
	if contentType == baseContentType {
		return "", true
	}
	if !strings.HasPrefix(contentType, baseContentType) {
		return "", false
	}
	switch contentType[len(baseContentType)] {
	case '+', ';':
		return contentType[len(baseContentType)+1:], true
	default:
		return "", false
	}
}
