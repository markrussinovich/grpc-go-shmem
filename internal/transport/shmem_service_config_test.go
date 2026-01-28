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
	"testing"
)

func TestDefaultShmemServiceConfig(t *testing.T) {
	cfg := DefaultShmemServiceConfig()
	if cfg == nil {
		t.Fatal("DefaultShmemServiceConfig() returned nil")
	}
	if cfg.Policy != ShmemPolicyAuto {
		t.Errorf("Default Policy: got %q, want %q", cfg.Policy, ShmemPolicyAuto)
	}
	if !cfg.IsFallbackEnabled() {
		t.Error("Default IsFallbackEnabled() should be true")
	}
}

func TestParseShmemServiceConfig(t *testing.T) {
	tests := []struct {
		name        string
		json        string
		wantPolicy  ShmemTransportPolicy
		wantErr     bool
		wantSegSize uint64
	}{
		{
			name:       "empty string",
			json:       "",
			wantPolicy: ShmemPolicyAuto,
		},
		{
			name:       "policy disabled",
			json:       `{"shmemPolicy":"disabled"}`,
			wantPolicy: ShmemPolicyDisabled,
		},
		{
			name:       "policy preferred",
			json:       `{"shmemPolicy":"preferred"}`,
			wantPolicy: ShmemPolicyPreferred,
		},
		{
			name:       "policy required",
			json:       `{"shmemPolicy":"required"}`,
			wantPolicy: ShmemPolicyRequired,
		},
		{
			name:       "policy auto",
			json:       `{"shmemPolicy":"auto"}`,
			wantPolicy: ShmemPolicyAuto,
		},
		{
			name:        "with segment size",
			json:        `{"shmemPolicy":"preferred","shmemSegmentSizeBytes":1048576}`,
			wantPolicy:  ShmemPolicyPreferred,
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
			cfg, err := ParseShmemServiceConfig(tt.json)
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

func TestShmemServiceConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *ShmemServiceConfig
		wantErr bool
	}{
		{
			name:    "nil config",
			cfg:     nil,
			wantErr: false,
		},
		{
			name:    "valid disabled",
			cfg:     &ShmemServiceConfig{Policy: ShmemPolicyDisabled},
			wantErr: false,
		},
		{
			name:    "valid preferred",
			cfg:     &ShmemServiceConfig{Policy: ShmemPolicyPreferred},
			wantErr: false,
		},
		{
			name:    "valid required",
			cfg:     &ShmemServiceConfig{Policy: ShmemPolicyRequired},
			wantErr: false,
		},
		{
			name:    "valid auto",
			cfg:     &ShmemServiceConfig{Policy: ShmemPolicyAuto},
			wantErr: false,
		},
		{
			name:    "empty policy (valid, defaults to auto)",
			cfg:     &ShmemServiceConfig{},
			wantErr: false,
		},
		{
			name:    "invalid policy",
			cfg:     &ShmemServiceConfig{Policy: "invalid_policy"},
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

func TestShmemServiceConfigShouldUseShmem(t *testing.T) {
	tests := []struct {
		name                  string
		cfg                   *ShmemServiceConfig
		addrHasShmemCapability bool
		want                  bool
	}{
		{
			name:                  "nil config auto - addr has capability",
			cfg:                   nil,
			addrHasShmemCapability: true,
			want:                  true,
		},
		{
			name:                  "nil config auto - addr no capability",
			cfg:                   nil,
			addrHasShmemCapability: false,
			want:                  false,
		},
		{
			name:                  "disabled - addr has capability",
			cfg:                   &ShmemServiceConfig{Policy: ShmemPolicyDisabled},
			addrHasShmemCapability: true,
			want:                  false,
		},
		{
			name:                  "disabled - addr no capability",
			cfg:                   &ShmemServiceConfig{Policy: ShmemPolicyDisabled},
			addrHasShmemCapability: false,
			want:                  false,
		},
		{
			name:                  "preferred - addr has capability",
			cfg:                   &ShmemServiceConfig{Policy: ShmemPolicyPreferred},
			addrHasShmemCapability: true,
			want:                  true,
		},
		{
			name:                  "preferred - addr no capability",
			cfg:                   &ShmemServiceConfig{Policy: ShmemPolicyPreferred},
			addrHasShmemCapability: false,
			want:                  true, // preferred always attempts
		},
		{
			name:                  "required - addr has capability",
			cfg:                   &ShmemServiceConfig{Policy: ShmemPolicyRequired},
			addrHasShmemCapability: true,
			want:                  true,
		},
		{
			name:                  "required - addr no capability",
			cfg:                   &ShmemServiceConfig{Policy: ShmemPolicyRequired},
			addrHasShmemCapability: false,
			want:                  true, // required always attempts (will fail if unavailable)
		},
		{
			name:                  "auto - addr has capability",
			cfg:                   &ShmemServiceConfig{Policy: ShmemPolicyAuto},
			addrHasShmemCapability: true,
			want:                  true,
		},
		{
			name:                  "auto - addr no capability",
			cfg:                   &ShmemServiceConfig{Policy: ShmemPolicyAuto},
			addrHasShmemCapability: false,
			want:                  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.ShouldUseShmem(tt.addrHasShmemCapability)
			if got != tt.want {
				t.Errorf("ShouldUseShmem(%v): got %v, want %v", tt.addrHasShmemCapability, got, tt.want)
			}
		})
	}
}

func TestShmemServiceConfigIsFallbackEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  *ShmemServiceConfig
		want bool
	}{
		{
			name: "nil config",
			cfg:  nil,
			want: true,
		},
		{
			name: "nil FallbackEnabled field",
			cfg:  &ShmemServiceConfig{Policy: ShmemPolicyPreferred},
			want: true,
		},
		{
			name: "FallbackEnabled true",
			cfg:  &ShmemServiceConfig{FallbackEnabled: boolPtr(true)},
			want: true,
		},
		{
			name: "FallbackEnabled false",
			cfg:  &ShmemServiceConfig{FallbackEnabled: boolPtr(false)},
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

func TestShmemServiceConfigMarshalJSON(t *testing.T) {
	cfg := &ShmemServiceConfig{
		Policy:              ShmemPolicyPreferred,
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
	var parsed ShmemServiceConfig
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
