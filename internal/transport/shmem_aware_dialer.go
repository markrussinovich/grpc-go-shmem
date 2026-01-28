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

	// TransportTypeShmem indicates shared memory transport.
	TransportTypeShmem
)

// String returns a string representation of the transport type.
func (t TransportType) String() string {
	switch t {
	case TransportTypeHTTP2:
		return "HTTP2"
	case TransportTypeShmem:
		return "Shmem"
	default:
		return fmt.Sprintf("TransportType(%d)", t)
	}
}

// TransportSelector determines which transport type to use for an address.
// This implements RFC A73 compliant transport selection at the subchannel level.
type TransportSelector struct {
	// ServiceConfig holds the shmem service configuration.
	// If nil, defaults to auto policy.
	ServiceConfig *ShmemServiceConfig

	// fallbackHandler handles fallback logic when shmem transport fails.
	// Initialized lazily on first use.
	fallbackHandler *ShmemFallbackHandler
}

// NewTransportSelector creates a new transport selector with the given config.
func NewTransportSelector(cfg *ShmemServiceConfig) *TransportSelector {
	return &TransportSelector{
		ServiceConfig: cfg,
	}
}

// SelectTransport determines which transport to use for the given address.
// It checks the address attributes for ShmemCapability and uses the
// service config policy to make the decision.
//
// This is the main entry point for RFC A73 compliant transport selection.
func (s *TransportSelector) SelectTransport(addr resolver.Address) TransportType {
	// Check if address has shmem capability
	cap := GetShmemCapability(addr)
	hasCapability := cap != nil && cap.Enabled

	// Check transport hint from LB policy (if present)
	hint := GetShmemTransportHint(addr)
	if hint != nil {
		if hint.PreferShmem && hasCapability {
			return TransportTypeShmem
		}
		if !hint.PreferShmem {
			return TransportTypeHTTP2
		}
	}

	// Use service config policy to decide
	cfg := s.ServiceConfig
	if cfg == nil {
		cfg = DefaultShmemServiceConfig()
	}

	if cfg.ShouldUseShmem(hasCapability) {
		return TransportTypeShmem
	}

	return TransportTypeHTTP2
}

// GetSegmentName extracts the shared memory segment name from an address.
// It checks the ShmemCapability first, then falls back to parsing the address.
func GetSegmentName(addr resolver.Address) string {
	// First check capability attribute
	cap := GetShmemCapability(addr)
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

// ShmemAwareDialer wraps the standard dialer and shmem dialer to provide
// RFC A73 compliant transport selection at connection time.
type ShmemAwareDialer struct {
	// Selector determines which transport to use.
	Selector *TransportSelector

	// ShmemDialer is used when shmem transport is selected.
	ShmemDialer *ShmDialer

	// OnTransportSelected is called when a transport type is selected.
	// This can be used for logging or metrics.
	OnTransportSelected func(addr resolver.Address, transportType TransportType)
}

// NewShmemAwareDialer creates a new shmem-aware dialer with the given options.
func NewShmemAwareDialer(cfg *ShmemServiceConfig, shmemOpts *DialOptions) *ShmemAwareDialer {
	return &ShmemAwareDialer{
		Selector:    NewTransportSelector(cfg),
		ShmemDialer: NewShmDialer(shmemOpts),
	}
}

// ShouldUseShmem determines if shmem transport should be used for the address.
// This is a convenience method that delegates to the selector.
func (d *ShmemAwareDialer) ShouldUseShmem(addr resolver.Address) bool {
	return d.Selector.SelectTransport(addr) == TransportTypeShmem
}

// DialShmem creates a shmem transport to the given address.
// Returns the transport and any error that occurred.
func (d *ShmemAwareDialer) DialShmem(ctx context.Context, addr resolver.Address) (ClientTransport, error) {
	segmentName := GetSegmentName(addr)
	if segmentName == "" {
		return nil, fmt.Errorf("shmem: no segment name for address %s", addr.Addr)
	}

	if d.OnTransportSelected != nil {
		d.OnTransportSelected(addr, TransportTypeShmem)
	}

	return DialShm(ctx, segmentName, d.ShmemDialer.opts)
}

// TransportSelectionResult contains the result of transport selection.
type TransportSelectionResult struct {
	// Type is the selected transport type.
	Type TransportType

	// SegmentName is the shmem segment name (only for shmem transport).
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

	cap := GetShmemCapability(addr)
	hasCapability := cap != nil && cap.Enabled

	// Check transport hint
	hint := GetShmemTransportHint(addr)
	if hint != nil {
		result.FallbackAllowed = hint.FallbackAllowed
	}

	// Use service config policy
	cfg := s.ServiceConfig
	if cfg == nil {
		cfg = DefaultShmemServiceConfig()
	}

	if cfg.ShouldUseShmem(hasCapability) {
		result.Type = TransportTypeShmem
		result.SegmentName = GetSegmentName(addr)
		result.FallbackAllowed = cfg.IsFallbackEnabled()

		// Override fallback if policy is required
		if cfg.Policy == ShmemPolicyRequired {
			result.FallbackAllowed = false
		}
	}

	return result
}

// CanUseShmemForAddress is a quick check to see if shmem is possible for an address.
// This doesn't consider the service config policy, just the address capability.
func CanUseShmemForAddress(addr resolver.Address) bool {
	return IsShmemEnabled(addr)
}

// MustUseShmemForAddress checks if shmem is required (no fallback allowed).
func MustUseShmemForAddress(addr resolver.Address, cfg *ShmemServiceConfig) bool {
	if cfg != nil && cfg.Policy == ShmemPolicyRequired {
		return true
	}
	hint := GetShmemTransportHint(addr)
	if hint != nil && hint.PreferShmem && !hint.FallbackAllowed {
		return true
	}
	return false
}

// NewShmemClient creates a new shared memory client transport.
// This function has a similar signature to NewHTTP2Client to allow
// transparent substitution in clientconn.go for RFC A73 compliance.
//
// Parameters:
//   - connectCtx: Context for connection establishment (with deadline)
//   - ctx: Long-lived context for the transport
//   - addr: Resolver address containing shmem capability attributes
//   - opts: Connect options (currently unused for shmem but included for API compatibility)
//   - onClose: Callback invoked when transport is closed
//
// Returns the ClientTransport or an error if connection fails.
func NewShmemClient(connectCtx, ctx context.Context, addr resolver.Address, opts ConnectOptions, onClose func(GoAwayReason)) (ClientTransport, error) {
	segmentName := GetSegmentName(addr)
	if segmentName == "" {
		return nil, fmt.Errorf("shmem: no segment name available for address %q", addr.Addr)
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
