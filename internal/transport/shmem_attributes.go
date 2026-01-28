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

// ShmemLocalityKey is the attribute key used to indicate that an endpoint
// supports shared memory transport and is co-located on the same host.
// This enables the Name Resolver and Load Balancer to participate in
// transport selection as per RFC A73.
type ShmemLocalityKey struct{}

// ShmemCapability describes the shared memory transport capability of an endpoint.
// When present in resolver.Address.Attributes, it signals that the endpoint
// can be reached via shared memory transport.
type ShmemCapability struct {
	// Enabled indicates whether shared memory transport is available for this endpoint.
	Enabled bool

	// SegmentName is the name/path of the shared memory segment to use for
	// connecting to this endpoint. If empty and Enabled is true, the segment
	// name will be derived from the address.
	SegmentName string

	// Preferred indicates whether shared memory should be preferred over
	// network transport when both are available. When false, shmem is used
	// only if explicitly requested or if network transport fails.
	Preferred bool
}

// Equal implements the Equal method for use in attributes comparison.
func (c ShmemCapability) Equal(o any) bool {
	oc, ok := o.(ShmemCapability)
	if !ok {
		return false
	}
	return c.Enabled == oc.Enabled &&
		c.SegmentName == oc.SegmentName &&
		c.Preferred == oc.Preferred
}

// String implements fmt.Stringer for debugging.
func (c ShmemCapability) String() string {
	return fmt.Sprintf("ShmemCapability{Enabled:%v, SegmentName:%q, Preferred:%v}",
		c.Enabled, c.SegmentName, c.Preferred)
}

// GetShmemCapability extracts the ShmemCapability from the address attributes.
// Returns nil if no shmem capability is set.
func GetShmemCapability(addr resolver.Address) *ShmemCapability {
	if addr.Attributes == nil {
		return nil
	}
	v := addr.Attributes.Value(ShmemLocalityKey{})
	if v == nil {
		return nil
	}
	cap, ok := v.(ShmemCapability)
	if !ok {
		return nil
	}
	return &cap
}

// SetShmemCapability returns a new address with the ShmemCapability set in its attributes.
func SetShmemCapability(addr resolver.Address, cap ShmemCapability) resolver.Address {
	addr.Attributes = addr.Attributes.WithValue(ShmemLocalityKey{}, cap)
	return addr
}

// IsShmemEnabled is a convenience function that checks if an address has
// shared memory transport enabled.
func IsShmemEnabled(addr resolver.Address) bool {
	cap := GetShmemCapability(addr)
	return cap != nil && cap.Enabled
}

// IsShmemPreferred is a convenience function that checks if shared memory
// transport is both enabled and preferred for an address.
func IsShmemPreferred(addr resolver.Address) bool {
	cap := GetShmemCapability(addr)
	return cap != nil && cap.Enabled && cap.Preferred
}

// ShmemTransportHintKey is the attribute key used to signal transport
// preference at the subchannel level. This allows the LB policy to
// communicate its transport decision to the transport layer.
type ShmemTransportHintKey struct{}

// ShmemTransportHint describes the transport hint for subchannel creation.
type ShmemTransportHint struct {
	// PreferShmem indicates the LB policy's preference for shared memory.
	PreferShmem bool

	// FallbackAllowed indicates whether falling back to network transport
	// is permitted if shmem connection fails.
	FallbackAllowed bool
}

// Equal implements the Equal method for use in attributes comparison.
func (h ShmemTransportHint) Equal(o any) bool {
	oh, ok := o.(ShmemTransportHint)
	if !ok {
		return false
	}
	return h.PreferShmem == oh.PreferShmem && h.FallbackAllowed == oh.FallbackAllowed
}

// String implements fmt.Stringer for debugging.
func (h ShmemTransportHint) String() string {
	return fmt.Sprintf("ShmemTransportHint{PreferShmem:%v, FallbackAllowed:%v}",
		h.PreferShmem, h.FallbackAllowed)
}

// GetShmemTransportHint extracts the ShmemTransportHint from the address attributes.
func GetShmemTransportHint(addr resolver.Address) *ShmemTransportHint {
	if addr.Attributes == nil {
		return nil
	}
	v := addr.Attributes.Value(ShmemTransportHintKey{})
	if v == nil {
		return nil
	}
	hint, ok := v.(ShmemTransportHint)
	if !ok {
		return nil
	}
	return &hint
}

// SetShmemTransportHint returns a new address with the ShmemTransportHint set.
func SetShmemTransportHint(addr resolver.Address, hint ShmemTransportHint) resolver.Address {
	addr.Attributes = addr.Attributes.WithValue(ShmemTransportHintKey{}, hint)
	return addr
}
