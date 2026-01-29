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
	"testing"
)

func TestDefaultShmServiceConfig(t *testing.T) {
	cfg := DefaultShmServiceConfig()
	if cfg == nil {
		t.Fatal("DefaultShmServiceConfig() returned nil")
	}
	if cfg.Policy != ShmPolicyAuto {
		t.Errorf("Default Policy: got %q, want %q", cfg.Policy, ShmPolicyAuto)
	}
	if !cfg.IsFallbackEnabled() {
		t.Error("Default IsFallbackEnabled() should be true")
	}
}

func TestParseShmServiceConfig(t *testing.T) {
	tests := []struct {
		name        string
		json        string
		wantPolicy  ShmTransportPolicy
		wantErr     bool
		wantSegSize uint64
	}{
		{
			name:       "empty string",
			json:       "",
			wantPolicy: ShmPolicyAuto,
		},
		{
			name:       "policy disabled",
			json:       `{"ShmPolicy":"disabled"}`,
			wantPolicy: ShmPolicyDisabled,
		},
		{
			name:       "policy preferred",
			json:       `{"ShmPolicy":"preferred"}`,
			wantPolicy: ShmPolicyPreferred,
		},
		{
			name:       "policy required",
			json:       `{"ShmPolicy":"required"}`,
			wantPolicy: ShmPolicyRequired,
		},
		{
			name:       "policy auto",
			json:       `{"ShmPolicy":"auto"}`,
			wantPolicy: ShmPolicyAuto,
		},
		{
			name:        "with segment size",
			json:        `{"ShmPolicy":"preferred","ShmSegmentSizeBytes":1048576}`,
			wantPolicy:  ShmPolicyPreferred,
			wantSegSize: 1048576,
		},
		{
			name:    "invalid json",
			json:    `{invalid}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ParseShmServiceConfig(tt.json)
			if tt.wantErr {
				if err == nil {
					t.Error("Expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if cfg.Policy != tt.wantPolicy {
				t.Errorf("Policy: got %q, want %q", cfg.Policy, tt.wantPolicy)
			}
			if tt.wantSegSize != 0 && cfg.SegmentSizeBytes != tt.wantSegSize {
				t.Errorf("SegmentSizeBytes: got %d, want %d", cfg.SegmentSizeBytes, tt.wantSegSize)
			}
		})
	}
}

func TestShmServiceConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *ShmServiceConfig
		wantErr bool
	}{
		{
			name:    "nil config",
			cfg:     nil,
			wantErr: false,
		},
		{
			name:    "valid disabled",
			cfg:     &ShmServiceConfig{Policy: ShmPolicyDisabled},
			wantErr: false,
		},
		{
			name:    "valid preferred",
			cfg:     &ShmServiceConfig{Policy: ShmPolicyPreferred},
			wantErr: false,
		},
		{
			name:    "valid required",
			cfg:     &ShmServiceConfig{Policy: ShmPolicyRequired},
			wantErr: false,
		},
		{
			name:    "valid auto",
			cfg:     &ShmServiceConfig{Policy: ShmPolicyAuto},
			wantErr: false,
		},
		{
			name:    "empty policy (valid, defaults to auto)",
			cfg:     &ShmServiceConfig{},
			wantErr: false,
		},
		{
			name:    "invalid policy",
			cfg:     &ShmServiceConfig{Policy: "invalid_policy"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr && err == nil {
				t.Error("Expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestShmServiceConfigShouldUseShm(t *testing.T) {
	tests := []struct {
		name                  string
		cfg                   *ShmServiceConfig
		addrHasShmCapability bool
		want                  bool
	}{
		{
			name:                  "nil config auto - addr has capability",
			cfg:                   nil,
			addrHasShmCapability: true,
			want:                  true,
		},
		{
			name:                  "nil config auto - addr no capability",
			cfg:                   nil,
			addrHasShmCapability: false,
			want:                  false,
		},
		{
			name:                  "disabled - addr has capability",
			cfg:                   &ShmServiceConfig{Policy: ShmPolicyDisabled},
			addrHasShmCapability: true,
			want:                  false,
		},
		{
			name:                  "disabled - addr no capability",
			cfg:                   &ShmServiceConfig{Policy: ShmPolicyDisabled},
			addrHasShmCapability: false,
			want:                  false,
		},
		{
			name:                  "preferred - addr has capability",
			cfg:                   &ShmServiceConfig{Policy: ShmPolicyPreferred},
			addrHasShmCapability: true,
			want:                  true,
		},
		{
			name:                  "preferred - addr no capability",
			cfg:                   &ShmServiceConfig{Policy: ShmPolicyPreferred},
			addrHasShmCapability: false,
			want:                  true, // preferred always attempts
		},
		{
			name:                  "required - addr has capability",
			cfg:                   &ShmServiceConfig{Policy: ShmPolicyRequired},
			addrHasShmCapability: true,
			want:                  true,
		},
		{
			name:                  "required - addr no capability",
			cfg:                   &ShmServiceConfig{Policy: ShmPolicyRequired},
			addrHasShmCapability: false,
			want:                  true, // required always attempts (will fail if unavailable)
		},
		{
			name:                  "auto - addr has capability",
			cfg:                   &ShmServiceConfig{Policy: ShmPolicyAuto},
			addrHasShmCapability: true,
			want:                  true,
		},
		{
			name:                  "auto - addr no capability",
			cfg:                   &ShmServiceConfig{Policy: ShmPolicyAuto},
			addrHasShmCapability: false,
			want:                  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.ShouldUseShm(tt.addrHasShmCapability)
			if got != tt.want {
				t.Errorf("ShouldUseShm(%v): got %v, want %v", tt.addrHasShmCapability, got, tt.want)
			}
		})
	}
}

func TestShmServiceConfigIsFallbackEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  *ShmServiceConfig
		want bool
	}{
		{
			name: "nil config",
			cfg:  nil,
			want: true,
		},
		{
			name: "nil FallbackEnabled field",
			cfg:  &ShmServiceConfig{Policy: ShmPolicyPreferred},
			want: true,
		},
		{
			name: "FallbackEnabled true",
			cfg:  &ShmServiceConfig{FallbackEnabled: boolPtr(true)},
			want: true,
		},
		{
			name: "FallbackEnabled false",
			cfg:  &ShmServiceConfig{FallbackEnabled: boolPtr(false)},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.IsFallbackEnabled()
			if got != tt.want {
				t.Errorf("IsFallbackEnabled(): got %v, want %v", got, tt.want)
			}
		})
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func TestShmServiceConfigMarshalJSON(t *testing.T) {
	cfg := &ShmServiceConfig{
		Policy:              ShmPolicyPreferred,
		SegmentSizeBytes:    2097152,
		RingBufferSizeBytes: 524288,
		FallbackEnabled:     boolPtr(true),
		MaxConcurrentStreams: 100,
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}

	// Unmarshal and verify round-trip
	var parsed ShmServiceConfig
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if parsed.Policy != cfg.Policy {
		t.Errorf("Policy round-trip: got %q, want %q", parsed.Policy, cfg.Policy)
	}
	if parsed.SegmentSizeBytes != cfg.SegmentSizeBytes {
		t.Errorf("SegmentSizeBytes round-trip: got %d, want %d", parsed.SegmentSizeBytes, cfg.SegmentSizeBytes)
	}
	if parsed.RingBufferSizeBytes != cfg.RingBufferSizeBytes {
		t.Errorf("RingBufferSizeBytes round-trip: got %d, want %d", parsed.RingBufferSizeBytes, cfg.RingBufferSizeBytes)
	}
	if parsed.MaxConcurrentStreams != cfg.MaxConcurrentStreams {
		t.Errorf("MaxConcurrentStreams round-trip: got %d, want %d", parsed.MaxConcurrentStreams, cfg.MaxConcurrentStreams)
	}
}

func TestShmServiceConfigString(t *testing.T) {
	tests := []struct {
		name string
		cfg  *ShmServiceConfig
		want string
	}{
		{
			name: "nil config",
			cfg:  nil,
			want: "ShmServiceConfig{nil}",
		},
		{
			name: "basic config",
			cfg:  &ShmServiceConfig{Policy: ShmPolicyPreferred},
			want: `ShmServiceConfig{Policy:"preferred", SegmentSize:0, RingBufferSize:0, Fallback:nil, MaxStreams:0}`,
		},
		{
			name: "full config",
			cfg: &ShmServiceConfig{
				Policy:              ShmPolicyRequired,
				SegmentSizeBytes:    1048576,
				RingBufferSizeBytes: 65536,
				FallbackEnabled:     boolPtr(false),
				MaxConcurrentStreams: 50,
			},
			want: `ShmServiceConfig{Policy:"required", SegmentSize:1048576, RingBufferSize:65536, Fallback:false, MaxStreams:50}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.String()
			if got != tt.want {
				t.Errorf("String(): got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShmServiceConfigUnknownPolicy(t *testing.T) {
	// Test ShouldUseShm with unknown policy (should default to auto behavior)
	cfg := &ShmServiceConfig{Policy: "unknown_policy"}

	// With capability, should use shm (auto behavior)
	if !cfg.ShouldUseShm(true) {
		t.Error("Unknown policy should default to auto behavior (use shm when capable)")
	}
	// Without capability, should not use shm
	if cfg.ShouldUseShm(false) {
		t.Error("Unknown policy should default to auto behavior (don't use shm when not capable)")
	}
}
