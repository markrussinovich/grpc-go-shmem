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
	"fmt"

	"google.golang.org/grpc/resolver"
)

// ShmLocalityKey is the attribute key used to indicate that an endpoint
// supports shared memory transport and is co-located on the same host.
// This enables the Name Resolver and Load Balancer to participate in
// transport selection as per RFC A73.
type ShmLocalityKey struct{}

// ShmCapability describes the shared memory transport capability of an endpoint.
// When present in resolver.Address.Attributes, it signals that the endpoint
// can be reached via shared memory transport.
type ShmCapability struct {
	// Enabled indicates whether shared memory transport is available for this endpoint.
	Enabled bool

	// SegmentName is the name/path of the shared memory segment to use for
	// connecting to this endpoint. If empty and Enabled is true, the segment
	// name will be derived from the address.
	SegmentName string

	// Preferred indicates whether shared memory should be preferred over
	// network transport when both are available. When false, shm is used
	// only if explicitly requested or if network transport fails.
	Preferred bool

	// Required indicates that shm MUST be used - no fallback to HTTP/2.
	// RFC A73: When true, connection will fail if shm cannot be established.
	Required bool
}

// Equal implements the Equal method for use in attributes comparison.
func (c ShmCapability) Equal(o any) bool {
	oc, ok := o.(ShmCapability)
	if !ok {
		return false
	}
	return c.Enabled == oc.Enabled &&
		c.SegmentName == oc.SegmentName &&
		c.Preferred == oc.Preferred &&
		c.Required == oc.Required
}

// String implements fmt.Stringer for debugging.
func (c ShmCapability) String() string {
	return fmt.Sprintf("ShmCapability{Enabled:%v, SegmentName:%q, Preferred:%v, Required:%v}",
		c.Enabled, c.SegmentName, c.Preferred, c.Required)
}

// GetShmCapability extracts the ShmCapability from the address attributes.
// Returns nil if no shm capability is set.
func GetShmCapability(addr resolver.Address) *ShmCapability {
	if addr.Attributes == nil {
		return nil
	}
	v := addr.Attributes.Value(ShmLocalityKey{})
	if v == nil {
		return nil
	}
	cap, ok := v.(ShmCapability)
	if !ok {
		return nil
	}
	return &cap
}

// SetShmCapability returns a new address with the ShmCapability set in its attributes.
func SetShmCapability(addr resolver.Address, cap ShmCapability) resolver.Address {
	addr.Attributes = addr.Attributes.WithValue(ShmLocalityKey{}, cap)
	return addr
}

// IsShmEnabled is a convenience function that checks if an address has
// shared memory transport enabled.
func IsShmEnabled(addr resolver.Address) bool {
	cap := GetShmCapability(addr)
	return cap != nil && cap.Enabled
}

// IsShmPreferred is a convenience function that checks if shared memory
// transport is both enabled and preferred for an address.
func IsShmPreferred(addr resolver.Address) bool {
	cap := GetShmCapability(addr)
	return cap != nil && cap.Enabled && cap.Preferred
}

// ShmTransportHintKey is the attribute key used to signal transport
// preference at the subchannel level. This allows the LB policy to
// communicate its transport decision to the transport layer.
type ShmTransportHintKey struct{}

// ShmTransportHint describes the transport hint for subchannel creation.
type ShmTransportHint struct {
	// PreferShm indicates the LB policy's preference for shared memory.
	PreferShm bool

	// FallbackAllowed indicates whether falling back to network transport
	// is permitted if shm connection fails.
	FallbackAllowed bool
}

// Equal implements the Equal method for use in attributes comparison.
func (h ShmTransportHint) Equal(o any) bool {
	oh, ok := o.(ShmTransportHint)
	if !ok {
		return false
	}
	return h.PreferShm == oh.PreferShm && h.FallbackAllowed == oh.FallbackAllowed
}

// String implements fmt.Stringer for debugging.
func (h ShmTransportHint) String() string {
	return fmt.Sprintf("ShmTransportHint{PreferShm:%v, FallbackAllowed:%v}",
		h.PreferShm, h.FallbackAllowed)
}

// GetShmTransportHint extracts the ShmTransportHint from the address attributes.
func GetShmTransportHint(addr resolver.Address) *ShmTransportHint {
	if addr.Attributes == nil {
		return nil
	}
	v := addr.Attributes.Value(ShmTransportHintKey{})
	if v == nil {
		return nil
	}
	hint, ok := v.(ShmTransportHint)
	if !ok {
		return nil
	}
	return &hint
}

// SetShmTransportHint returns a new address with the ShmTransportHint set.
func SetShmTransportHint(addr resolver.Address, hint ShmTransportHint) resolver.Address {
	addr.Attributes = addr.Attributes.WithValue(ShmTransportHintKey{}, hint)
	return addr
}

// IsFallbackAllowed checks if fallback from shm to HTTP/2 is allowed for the address.
// RFC A73: This is used during transport selection to determine if HTTP/2 fallback
// should be attempted when shm connection fails.
func IsFallbackAllowed(addr resolver.Address) bool {
	// Check transport hint first
	hint := GetShmTransportHint(addr)
	if hint != nil {
		return hint.FallbackAllowed
	}

	// Check capability
	cap := GetShmCapability(addr)
	if cap != nil && cap.Required {
		// If shm is marked as required, no fallback
		return false
	}

	// Default: allow fallback
	return true
}
