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
	"testing"

	"google.golang.org/grpc/resolver"
)

func TestShmemCapabilitySetGet(t *testing.T) {
	addr := resolver.Address{
		Addr:       "shm:test_segment",
		ServerName: "test_segment",
	}

	// Initially no capability
	if cap := GetShmemCapability(addr); cap != nil {
		t.Error("Expected nil capability for address without attributes")
	}
	if IsShmemEnabled(addr) {
		t.Error("Expected IsShmemEnabled to be false for address without attributes")
	}

	// Set capability
	cap := ShmemCapability{
		Enabled:     true,
		SegmentName: "my_segment",
		Preferred:   true,
	}
	addr = SetShmemCapability(addr, cap)

	// Retrieve and verify
	got := GetShmemCapability(addr)
	if got == nil {
		t.Fatal("Expected non-nil capability after SetShmemCapability")
	}
	if got.Enabled != cap.Enabled {
		t.Errorf("Enabled: got %v, want %v", got.Enabled, cap.Enabled)
	}
	if got.SegmentName != cap.SegmentName {
		t.Errorf("SegmentName: got %q, want %q", got.SegmentName, cap.SegmentName)
	}
	if got.Preferred != cap.Preferred {
		t.Errorf("Preferred: got %v, want %v", got.Preferred, cap.Preferred)
	}

	// Helper functions
	if !IsShmemEnabled(addr) {
		t.Error("Expected IsShmemEnabled to be true")
	}
	if !IsShmemPreferred(addr) {
		t.Error("Expected IsShmemPreferred to be true")
	}
}

func TestShmemCapabilityDisabled(t *testing.T) {
	addr := resolver.Address{Addr: "tcp:localhost:8080"}
	cap := ShmemCapability{
		Enabled:     false,
		SegmentName: "",
		Preferred:   false,
	}
	addr = SetShmemCapability(addr, cap)

	if IsShmemEnabled(addr) {
		t.Error("Expected IsShmemEnabled to be false when Enabled=false")
	}
	if IsShmemPreferred(addr) {
		t.Error("Expected IsShmemPreferred to be false when Preferred=false")
	}
}

func TestShmemCapabilityEqual(t *testing.T) {
	tests := []struct {
		name  string
		a, b  ShmemCapability
		equal bool
	}{
		{
			name:  "identical",
			a:     ShmemCapability{Enabled: true, SegmentName: "seg", Preferred: true, Required: false},
			b:     ShmemCapability{Enabled: true, SegmentName: "seg", Preferred: true, Required: false},
			equal: true,
		},
		{
			name:  "different enabled",
			a:     ShmemCapability{Enabled: true, SegmentName: "seg"},
			b:     ShmemCapability{Enabled: false, SegmentName: "seg"},
			equal: false,
		},
		{
			name:  "different segment",
			a:     ShmemCapability{Enabled: true, SegmentName: "seg1"},
			b:     ShmemCapability{Enabled: true, SegmentName: "seg2"},
			equal: false,
		},
		{
			name:  "different preferred",
			a:     ShmemCapability{Enabled: true, Preferred: true},
			b:     ShmemCapability{Enabled: true, Preferred: false},
			equal: false,
		},
		{
			name:  "different required",
			a:     ShmemCapability{Enabled: true, Required: true},
			b:     ShmemCapability{Enabled: true, Required: false},
			equal: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Equal(tt.b); got != tt.equal {
				t.Errorf("Equal(): got %v, want %v", got, tt.equal)
			}
		})
	}
}

func TestShmemTransportHintSetGet(t *testing.T) {
	addr := resolver.Address{Addr: "shm:test"}

	// Initially no hint
	if hint := GetShmemTransportHint(addr); hint != nil {
		t.Error("Expected nil hint for address without attributes")
	}

	// Set hint
	hint := ShmemTransportHint{
		PreferShmem:     true,
		FallbackAllowed: true,
	}
	addr = SetShmemTransportHint(addr, hint)

	// Retrieve and verify
	got := GetShmemTransportHint(addr)
	if got == nil {
		t.Fatal("Expected non-nil hint after SetShmemTransportHint")
	}
	if got.PreferShmem != hint.PreferShmem {
		t.Errorf("PreferShmem: got %v, want %v", got.PreferShmem, hint.PreferShmem)
	}
	if got.FallbackAllowed != hint.FallbackAllowed {
		t.Errorf("FallbackAllowed: got %v, want %v", got.FallbackAllowed, hint.FallbackAllowed)
	}
}

func TestShmemTransportHintEqual(t *testing.T) {
	tests := []struct {
		name  string
		a, b  ShmemTransportHint
		equal bool
	}{
		{
			name:  "identical",
			a:     ShmemTransportHint{PreferShmem: true, FallbackAllowed: true},
			b:     ShmemTransportHint{PreferShmem: true, FallbackAllowed: true},
			equal: true,
		},
		{
			name:  "different prefer",
			a:     ShmemTransportHint{PreferShmem: true},
			b:     ShmemTransportHint{PreferShmem: false},
			equal: false,
		},
		{
			name:  "different fallback",
			a:     ShmemTransportHint{FallbackAllowed: true},
			b:     ShmemTransportHint{FallbackAllowed: false},
			equal: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Equal(tt.b); got != tt.equal {
				t.Errorf("Equal(): got %v, want %v", got, tt.equal)
			}
		})
	}
}

func TestCombinedAttributes(t *testing.T) {
	// Test that both capability and hint can coexist on the same address
	addr := resolver.Address{Addr: "shm:combined_test"}

	cap := ShmemCapability{Enabled: true, SegmentName: "combined", Preferred: true}
	hint := ShmemTransportHint{PreferShmem: true, FallbackAllowed: true}

	addr = SetShmemCapability(addr, cap)
	addr = SetShmemTransportHint(addr, hint)

	// Both should be retrievable
	gotCap := GetShmemCapability(addr)
	gotHint := GetShmemTransportHint(addr)

	if gotCap == nil || gotHint == nil {
		t.Fatal("Both capability and hint should be present")
	}
	if gotCap.SegmentName != "combined" {
		t.Errorf("Capability segment: got %q, want %q", gotCap.SegmentName, "combined")
	}
	if !gotHint.PreferShmem {
		t.Error("Hint PreferShmem should be true")
	}
}

func TestShmemCapabilityString(t *testing.T) {
	tests := []struct {
		name string
		cap  ShmemCapability
		want string
	}{
		{
			name: "enabled and preferred",
			cap:  ShmemCapability{Enabled: true, SegmentName: "my_seg", Preferred: true},
			want: `ShmemCapability{Enabled:true, SegmentName:"my_seg", Preferred:true, Required:false}`,
		},
		{
			name: "disabled",
			cap:  ShmemCapability{Enabled: false, SegmentName: "", Preferred: false},
			want: `ShmemCapability{Enabled:false, SegmentName:"", Preferred:false, Required:false}`,
		},
		{
			name: "with required",
			cap:  ShmemCapability{Enabled: true, SegmentName: "seg", Preferred: true, Required: true},
			want: `ShmemCapability{Enabled:true, SegmentName:"seg", Preferred:true, Required:true}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cap.String()
			if got != tt.want {
				t.Errorf("String(): got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShmemTransportHintString(t *testing.T) {
	hint := ShmemTransportHint{PreferShmem: true, FallbackAllowed: false}
	want := `ShmemTransportHint{PreferShmem:true, FallbackAllowed:false}`
	got := hint.String()
	if got != want {
		t.Errorf("String(): got %q, want %q", got, want)
	}
}

func TestShmemCapabilityEqualWrongType(t *testing.T) {
	cap := ShmemCapability{Enabled: true, SegmentName: "seg", Preferred: true}
	// Equal should return false for wrong type
	if cap.Equal("not a ShmemCapability") {
		t.Error("Equal(string) should return false")
	}
	if cap.Equal(42) {
		t.Error("Equal(int) should return false")
	}
	if cap.Equal(nil) {
		t.Error("Equal(nil) should return false")
	}
}

func TestShmemTransportHintEqualWrongType(t *testing.T) {
	hint := ShmemTransportHint{PreferShmem: true}
	if hint.Equal("wrong type") {
		t.Error("Equal(string) should return false")
	}
	if hint.Equal(ShmemCapability{Enabled: true}) {
		t.Error("Equal(ShmemCapability) should return false")
	}
}

func TestGetShmemCapabilityWrongType(t *testing.T) {
	// Test retrieving when attribute contains wrong type
	addr := resolver.Address{Addr: "test"}
	// Manually set wrong type in attributes
	addr.Attributes = addr.Attributes.WithValue(ShmemLocalityKey{}, "wrong type")

	cap := GetShmemCapability(addr)
	if cap != nil {
		t.Error("Expected nil when attribute has wrong type")
	}
}

func TestGetShmemTransportHintWrongType(t *testing.T) {
	addr := resolver.Address{Addr: "test"}
	addr.Attributes = addr.Attributes.WithValue(ShmemTransportHintKey{}, 12345)

	hint := GetShmemTransportHint(addr)
	if hint != nil {
		t.Error("Expected nil when attribute has wrong type")
	}
}
