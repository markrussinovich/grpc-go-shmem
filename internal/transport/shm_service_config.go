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
	"encoding/json"
	"fmt"
)

// ShmTransportPolicy defines the shared memory transport selection policy.
type ShmTransportPolicy string

const (
	// ShmPolicyDisabled means shared memory transport is never used.
	ShmPolicyDisabled ShmTransportPolicy = "disabled"

	// ShmPolicyPreferred means shared memory transport is used when available,
	// with automatic fallback to network transport if shm fails.
	ShmPolicyPreferred ShmTransportPolicy = "preferred"

	// ShmPolicyRequired means shared memory transport must be used.
	// RPCs will fail if shm is not available.
	ShmPolicyRequired ShmTransportPolicy = "required"

	// ShmPolicyAuto means transport selection is automatic based on
	// endpoint locality attributes from the resolver.
	ShmPolicyAuto ShmTransportPolicy = "auto"
)

// ShmServiceConfig defines the shared memory transport configuration
// that can be embedded in a gRPC service config or used standalone.
// This follows RFC A73 guidelines for control-plane signaled transport selection.
type ShmServiceConfig struct {
	// Policy determines when shared memory transport should be used.
	// Default is "auto" which uses shm when the endpoint is local.
	Policy ShmTransportPolicy `json:"ShmPolicy,omitempty"`

	// SegmentSizeBytes is the size of the shared memory segment to create.
	// If 0, the default segment size is used.
	SegmentSizeBytes uint64 `json:"ShmSegmentSizeBytes,omitempty"`

	// RingBufferSizeBytes is the size of each ring buffer in the segment.
	// If 0, the default ring buffer size is used.
	RingBufferSizeBytes uint64 `json:"ShmRingBufferSizeBytes,omitempty"`

	// FallbackEnabled allows falling back to network transport if shm
	// connection fails. Only relevant when Policy is "preferred" or "auto".
	// Default is true.
	FallbackEnabled *bool `json:"ShmFallbackEnabled,omitempty"`

	// MaxConcurrentStreams limits the number of concurrent streams per
	// shm connection. If 0, no limit is applied.
	MaxConcurrentStreams uint32 `json:"ShmMaxConcurrentStreams,omitempty"`
}

// DefaultShmServiceConfig returns the default shm service config.
func DefaultShmServiceConfig() *ShmServiceConfig {
	fallback := true
	return &ShmServiceConfig{
		Policy:          ShmPolicyAuto,
		FallbackEnabled: &fallback,
	}
}

// IsFallbackEnabled returns whether fallback to network transport is enabled.
func (c *ShmServiceConfig) IsFallbackEnabled() bool {
	if c == nil || c.FallbackEnabled == nil {
		return true // default is enabled
	}
	return *c.FallbackEnabled
}

// ShouldUseShm determines whether shared memory transport should be
// attempted based on the policy and the address capability.
func (c *ShmServiceConfig) ShouldUseShm(addrHasShmCapability bool) bool {
	if c == nil {
		c = DefaultShmServiceConfig()
	}

	switch c.Policy {
	case ShmPolicyDisabled:
		return false
	case ShmPolicyRequired:
		return true // always attempt, will fail if not available
	case ShmPolicyPreferred:
		return true // always attempt, may fallback
	case ShmPolicyAuto:
		// Use shm only if the address indicates capability
		return addrHasShmCapability
	default:
		// Unknown policy, default to auto behavior
		return addrHasShmCapability
	}
}

// ParseShmServiceConfig parses the shm-specific fields from a JSON
// service config string. This can be used to extract shm config from
// a larger service config JSON.
func ParseShmServiceConfig(js string) (*ShmServiceConfig, error) {
	if js == "" {
		return DefaultShmServiceConfig(), nil
	}

	var cfg ShmServiceConfig
	if err := json.Unmarshal([]byte(js), &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse shm service config: %w", err)
	}

	// Apply defaults for unset fields
	if cfg.Policy == "" {
		cfg.Policy = ShmPolicyAuto
	}

	return &cfg, nil
}

// MarshalJSON implements json.Marshaler for ShmServiceConfig.
func (c *ShmServiceConfig) MarshalJSON() ([]byte, error) {
	type alias ShmServiceConfig
	return json.Marshal((*alias)(c))
}

// Validate validates the shm service config.
func (c *ShmServiceConfig) Validate() error {
	if c == nil {
		return nil
	}

	switch c.Policy {
	case ShmPolicyDisabled, ShmPolicyPreferred, ShmPolicyRequired, ShmPolicyAuto, "":
		// Valid policies
	default:
		return fmt.Errorf("invalid shm policy: %q", c.Policy)
	}

	return nil
}

// String implements fmt.Stringer for debugging.
func (c *ShmServiceConfig) String() string {
	if c == nil {
		return "ShmServiceConfig{nil}"
	}
	fallback := "nil"
	if c.FallbackEnabled != nil {
		fallback = fmt.Sprintf("%v", *c.FallbackEnabled)
	}
	return fmt.Sprintf("ShmServiceConfig{Policy:%q, SegmentSize:%d, RingBufferSize:%d, Fallback:%s, MaxStreams:%d}",
		c.Policy, c.SegmentSizeBytes, c.RingBufferSizeBytes, fallback, c.MaxConcurrentStreams)
}
