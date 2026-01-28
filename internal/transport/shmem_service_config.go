//go:build linux || windows

/*
 *
 * Copyright 2025 gRPC authors.
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

// ShmemTransportPolicy defines the shared memory transport selection policy.
type ShmemTransportPolicy string

const (
	// ShmemPolicyDisabled means shared memory transport is never used.
	ShmemPolicyDisabled ShmemTransportPolicy = "disabled"

	// ShmemPolicyPreferred means shared memory transport is used when available,
	// with automatic fallback to network transport if shmem fails.
	ShmemPolicyPreferred ShmemTransportPolicy = "preferred"

	// ShmemPolicyRequired means shared memory transport must be used.
	// RPCs will fail if shmem is not available.
	ShmemPolicyRequired ShmemTransportPolicy = "required"

	// ShmemPolicyAuto means transport selection is automatic based on
	// endpoint locality attributes from the resolver.
	ShmemPolicyAuto ShmemTransportPolicy = "auto"
)

// ShmemServiceConfig defines the shared memory transport configuration
// that can be embedded in a gRPC service config or used standalone.
// This follows RFC A73 guidelines for control-plane signaled transport selection.
type ShmemServiceConfig struct {
	// Policy determines when shared memory transport should be used.
	// Default is "auto" which uses shmem when the endpoint is local.
	Policy ShmemTransportPolicy `json:"shmemPolicy,omitempty"`

	// SegmentSizeBytes is the size of the shared memory segment to create.
	// If 0, the default segment size is used.
	SegmentSizeBytes uint64 `json:"shmemSegmentSizeBytes,omitempty"`

	// RingBufferSizeBytes is the size of each ring buffer in the segment.
	// If 0, the default ring buffer size is used.
	RingBufferSizeBytes uint64 `json:"shmemRingBufferSizeBytes,omitempty"`

	// FallbackEnabled allows falling back to network transport if shmem
	// connection fails. Only relevant when Policy is "preferred" or "auto".
	// Default is true.
	FallbackEnabled *bool `json:"shmemFallbackEnabled,omitempty"`

	// MaxConcurrentStreams limits the number of concurrent streams per
	// shmem connection. If 0, no limit is applied.
	MaxConcurrentStreams uint32 `json:"shmemMaxConcurrentStreams,omitempty"`
}

// DefaultShmemServiceConfig returns the default shmem service config.
func DefaultShmemServiceConfig() *ShmemServiceConfig {
	fallback := true
	return &ShmemServiceConfig{
		Policy:          ShmemPolicyAuto,
		FallbackEnabled: &fallback,
	}
}

// IsFallbackEnabled returns whether fallback to network transport is enabled.
func (c *ShmemServiceConfig) IsFallbackEnabled() bool {
	if c == nil || c.FallbackEnabled == nil {
		return true // default is enabled
	}
	return *c.FallbackEnabled
}

// ShouldUseShmem determines whether shared memory transport should be
// attempted based on the policy and the address capability.
func (c *ShmemServiceConfig) ShouldUseShmem(addrHasShmemCapability bool) bool {
	if c == nil {
		c = DefaultShmemServiceConfig()
	}

	switch c.Policy {
	case ShmemPolicyDisabled:
		return false
	case ShmemPolicyRequired:
		return true // always attempt, will fail if not available
	case ShmemPolicyPreferred:
		return true // always attempt, may fallback
	case ShmemPolicyAuto:
		// Use shmem only if the address indicates capability
		return addrHasShmemCapability
	default:
		// Unknown policy, default to auto behavior
		return addrHasShmemCapability
	}
}

// ParseShmemServiceConfig parses the shmem-specific fields from a JSON
// service config string. This can be used to extract shmem config from
// a larger service config JSON.
func ParseShmemServiceConfig(js string) (*ShmemServiceConfig, error) {
	if js == "" {
		return DefaultShmemServiceConfig(), nil
	}

	var cfg ShmemServiceConfig
	if err := json.Unmarshal([]byte(js), &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse shmem service config: %w", err)
	}

	// Apply defaults for unset fields
	if cfg.Policy == "" {
		cfg.Policy = ShmemPolicyAuto
	}

	return &cfg, nil
}

// MarshalJSON implements json.Marshaler for ShmemServiceConfig.
func (c *ShmemServiceConfig) MarshalJSON() ([]byte, error) {
	type alias ShmemServiceConfig
	return json.Marshal((*alias)(c))
}

// Validate validates the shmem service config.
func (c *ShmemServiceConfig) Validate() error {
	if c == nil {
		return nil
	}

	switch c.Policy {
	case ShmemPolicyDisabled, ShmemPolicyPreferred, ShmemPolicyRequired, ShmemPolicyAuto, "":
		// Valid policies
	default:
		return fmt.Errorf("invalid shmem policy: %q", c.Policy)
	}

	return nil
}
