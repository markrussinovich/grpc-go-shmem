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
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/resolver"
)

// TransportType indicates the type of transport to use for a connection.
type TransportType int

const (
	// TransportTypeHTTP2 indicates standard HTTP/2 over TCP transport.
	TransportTypeHTTP2 TransportType = iota

	// TransportTypeShm indicates shared memory transport.
	TransportTypeShm
)

// String returns a string representation of the transport type.
func (t TransportType) String() string {
	switch t {
	case TransportTypeHTTP2:
		return "HTTP2"
	case TransportTypeShm:
		return "Shm"
	default:
		return fmt.Sprintf("TransportType(%d)", t)
	}
}

// TransportSelector determines which transport type to use for an address.
// This implements RFC A73 compliant transport selection at the subchannel level.
type TransportSelector struct {
	// ServiceConfig holds the shm service configuration.
	// If nil, defaults to auto policy.
	ServiceConfig *ShmServiceConfig

	// fallbackHandler handles fallback logic when shm transport fails.
	// Initialized lazily on first use.
	fallbackHandler *ShmFallbackHandler
}

// NewTransportSelector creates a new transport selector with the given config.
func NewTransportSelector(cfg *ShmServiceConfig) *TransportSelector {
	return &TransportSelector{
		ServiceConfig: cfg,
	}
}

// SelectTransport determines which transport to use for the given address.
// It checks the address attributes for ShmCapability and uses the
// service config policy to make the decision.
//
// This is the main entry point for RFC A73 compliant transport selection.
func (s *TransportSelector) SelectTransport(addr resolver.Address) TransportType {
	// Check if address has shm capability
	cap := GetShmCapability(addr)
	hasCapability := cap != nil && cap.Enabled

	// Check transport hint from LB policy (if present)
	hint := GetShmTransportHint(addr)
	if hint != nil {
		if hint.PreferShm && hasCapability {
			return TransportTypeShm
		}
		if !hint.PreferShm {
			return TransportTypeHTTP2
		}
	}

	// Use service config policy to decide
	cfg := s.ServiceConfig
	if cfg == nil {
		cfg = DefaultShmServiceConfig()
	}

	if cfg.ShouldUseShm(hasCapability) {
		return TransportTypeShm
	}

	return TransportTypeHTTP2
}

// GetSegmentName extracts the shared memory segment name from an address.
// It checks the ShmCapability first, then falls back to parsing the address.
func GetSegmentName(addr resolver.Address) string {
	// First check capability attribute
	cap := GetShmCapability(addr)
	if cap != nil && cap.SegmentName != "" {
		return cap.SegmentName
	}

	// Fall back to parsing address format "shm:segment_name"
	if strings.HasPrefix(addr.Addr, "shm:") {
		return strings.TrimPrefix(addr.Addr, "shm:")
	}

	// Use ServerName if available
	if addr.ServerName != "" {
		return addr.ServerName
	}

	return addr.Addr
}

// ShmAwareDialer wraps the standard dialer and shm dialer to provide
// RFC A73 compliant transport selection at connection time.
type ShmAwareDialer struct {
	// Selector determines which transport to use.
	Selector *TransportSelector

	// ShmDialer is used when shm transport is selected.
	ShmDialer *ShmDialer

	// OnTransportSelected is called when a transport type is selected.
	// This can be used for logging or metrics.
	OnTransportSelected func(addr resolver.Address, transportType TransportType)
}

// NewShmAwareDialer creates a new shm-aware dialer with the given options.
func NewShmAwareDialer(cfg *ShmServiceConfig, ShmOpts *DialOptions) *ShmAwareDialer {
	return &ShmAwareDialer{
		Selector:    NewTransportSelector(cfg),
		ShmDialer: NewShmDialer(ShmOpts),
	}
}

// ShouldUseShm determines if shm transport should be used for the address.
// This is a convenience method that delegates to the selector.
func (d *ShmAwareDialer) ShouldUseShm(addr resolver.Address) bool {
	return d.Selector.SelectTransport(addr) == TransportTypeShm
}

// DialShm creates a shm transport to the given address.
// Returns the transport and any error that occurred.
func (d *ShmAwareDialer) DialShm(ctx context.Context, addr resolver.Address) (ClientTransport, error) {
	segmentName := GetSegmentName(addr)
	if segmentName == "" {
		return nil, fmt.Errorf("shm: no segment name for address %s", addr.Addr)
	}

	if d.OnTransportSelected != nil {
		d.OnTransportSelected(addr, TransportTypeShm)
	}

	return DialShm(ctx, segmentName, d.ShmDialer.opts)
}

// TransportSelectionResult contains the result of transport selection.
type TransportSelectionResult struct {
	// Type is the selected transport type.
	Type TransportType

	// SegmentName is the shm segment name (only for shm transport).
	SegmentName string

	// FallbackAllowed indicates if fallback to HTTP2 is allowed.
	FallbackAllowed bool
}

// SelectTransportWithDetails returns detailed transport selection information.
// This is useful for logging, debugging, and implementing fallback logic.
func (s *TransportSelector) SelectTransportWithDetails(addr resolver.Address) TransportSelectionResult {
	result := TransportSelectionResult{
		Type:            TransportTypeHTTP2,
		FallbackAllowed: true,
	}

	cap := GetShmCapability(addr)
	hasCapability := cap != nil && cap.Enabled

	// Check transport hint
	hint := GetShmTransportHint(addr)
	if hint != nil {
		result.FallbackAllowed = hint.FallbackAllowed
	}

	// Use service config policy
	cfg := s.ServiceConfig
	if cfg == nil {
		cfg = DefaultShmServiceConfig()
	}

	if cfg.ShouldUseShm(hasCapability) {
		result.Type = TransportTypeShm
		result.SegmentName = GetSegmentName(addr)
		result.FallbackAllowed = cfg.IsFallbackEnabled()

		// Override fallback if policy is required
		if cfg.Policy == ShmPolicyRequired {
			result.FallbackAllowed = false
		}
	}

	return result
}

// CanUseShmForAddress is a quick check to see if shm is possible for an address.
// This doesn't consider the service config policy, just the address capability.
func CanUseShmForAddress(addr resolver.Address) bool {
	return IsShmEnabled(addr)
}

// MustUseShmForAddress checks if shm is required (no fallback allowed).
func MustUseShmForAddress(addr resolver.Address, cfg *ShmServiceConfig) bool {
	if cfg != nil && cfg.Policy == ShmPolicyRequired {
		return true
	}
	hint := GetShmTransportHint(addr)
	if hint != nil && hint.PreferShm && !hint.FallbackAllowed {
		return true
	}
	return false
}

// NewShmClient creates a new shared memory client transport.
// This function has a similar signature to NewHTTP2Client to allow
// transparent substitution in clientconn.go for RFC A73 compliance.
//
// Parameters:
//   - connectCtx: Context for connection establishment (with deadline)
//   - ctx: Long-lived context for the transport
//   - addr: Resolver address containing shm capability attributes
//   - opts: Connect options (currently unused for shm but included for API compatibility)
//   - onClose: Callback invoked when transport is closed
//
// Returns the ClientTransport or an error if connection fails.
func NewShmClient(connectCtx, ctx context.Context, addr resolver.Address, opts ConnectOptions, onClose func(GoAwayReason)) (ClientTransport, error) {
	segmentName := GetSegmentName(addr)
	if segmentName == "" {
		return nil, fmt.Errorf("shm: no segment name available for address %q", addr.Addr)
	}

	// Configure dial options from connect options
	dialOpts := DefaultDialOptions()
	if opts.KeepaliveParams != (keepalive.ClientParameters{}) {
		dialOpts.KeepaliveParams = opts.KeepaliveParams
	}

	// Use connect context timeout if available
	if deadline, ok := connectCtx.Deadline(); ok {
		dialOpts.ConnectTimeout = time.Until(deadline)
	}

	// Create the transport
	transport, err := DialShm(connectCtx, segmentName, dialOpts)
	if err != nil {
		return nil, err
	}

	// Set the onClose callback for ClientConn integration
	if shmTr, ok := transport.(*ShmClientTransport); ok {
		shmTr.SetOnClose(onClose)
	}

	return transport, nil
}
