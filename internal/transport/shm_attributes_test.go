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

func TestShmCapabilitySetGet(t *testing.T) {
	addr := resolver.Address{
		Addr:       "shm:test_segment",
		ServerName: "test_segment",
	}

	// Initially no capability
	if cap := GetShmCapability(addr); cap != nil {
		t.Error("Expected nil capability for address without attributes")
	}
	if IsShmEnabled(addr) {
		t.Error("Expected IsShmEnabled to be false for address without attributes")
	}

	// Set capability
	cap := ShmCapability{
		Enabled:     true,
		SegmentName: "my_segment",
		Preferred:   true,
	}
	addr = SetShmCapability(addr, cap)

	// Retrieve and verify
	got := GetShmCapability(addr)
	if got == nil {
		t.Fatal("Expected non-nil capability after SetShmCapability")
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
	if !IsShmEnabled(addr) {
		t.Error("Expected IsShmEnabled to be true")
	}
	if !IsShmPreferred(addr) {
		t.Error("Expected IsShmPreferred to be true")
	}
}

func TestShmCapabilityDisabled(t *testing.T) {
	addr := resolver.Address{Addr: "tcp:localhost:8080"}
	cap := ShmCapability{
		Enabled:     false,
		SegmentName: "",
		Preferred:   false,
	}
	addr = SetShmCapability(addr, cap)

	if IsShmEnabled(addr) {
		t.Error("Expected IsShmEnabled to be false when Enabled=false")
	}
	if IsShmPreferred(addr) {
		t.Error("Expected IsShmPreferred to be false when Preferred=false")
	}
}

func TestShmCapabilityEqual(t *testing.T) {
	tests := []struct {
		name  string
		a, b  ShmCapability
		equal bool
	}{
		{
			name:  "identical",
			a:     ShmCapability{Enabled: true, SegmentName: "seg", Preferred: true, Required: false},
			b:     ShmCapability{Enabled: true, SegmentName: "seg", Preferred: true, Required: false},
			equal: true,
		},
		{
			name:  "different enabled",
			a:     ShmCapability{Enabled: true, SegmentName: "seg"},
			b:     ShmCapability{Enabled: false, SegmentName: "seg"},
			equal: false,
		},
		{
			name:  "different segment",
			a:     ShmCapability{Enabled: true, SegmentName: "seg1"},
			b:     ShmCapability{Enabled: true, SegmentName: "seg2"},
			equal: false,
		},
		{
			name:  "different preferred",
			a:     ShmCapability{Enabled: true, Preferred: true},
			b:     ShmCapability{Enabled: true, Preferred: false},
			equal: false,
		},
		{
			name:  "different required",
			a:     ShmCapability{Enabled: true, Required: true},
			b:     ShmCapability{Enabled: true, Required: false},
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

func TestShmTransportHintSetGet(t *testing.T) {
	addr := resolver.Address{Addr: "shm:test"}

	// Initially no hint
	if hint := GetShmTransportHint(addr); hint != nil {
		t.Error("Expected nil hint for address without attributes")
	}

	// Set hint
	hint := ShmTransportHint{
		PreferShm:       true,
		FallbackAllowed: true,
	}
	addr = SetShmTransportHint(addr, hint)

	// Retrieve and verify
	got := GetShmTransportHint(addr)
	if got == nil {
		t.Fatal("Expected non-nil hint after SetShmTransportHint")
	}
	if got.PreferShm != hint.PreferShm {
		t.Errorf("PreferShm: got %v, want %v", got.PreferShm, hint.PreferShm)
	}
	if got.FallbackAllowed != hint.FallbackAllowed {
		t.Errorf("FallbackAllowed: got %v, want %v", got.FallbackAllowed, hint.FallbackAllowed)
	}
}

func TestShmTransportHintEqual(t *testing.T) {
	tests := []struct {
		name  string
		a, b  ShmTransportHint
		equal bool
	}{
		{
			name:  "identical",
			a:     ShmTransportHint{PreferShm: true, FallbackAllowed: true},
			b:     ShmTransportHint{PreferShm: true, FallbackAllowed: true},
			equal: true,
		},
		{
			name:  "different prefer",
			a:     ShmTransportHint{PreferShm: true},
			b:     ShmTransportHint{PreferShm: false},
			equal: false,
		},
		{
			name:  "different fallback",
			a:     ShmTransportHint{FallbackAllowed: true},
			b:     ShmTransportHint{FallbackAllowed: false},
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

	cap := ShmCapability{Enabled: true, SegmentName: "combined", Preferred: true}
	hint := ShmTransportHint{PreferShm: true, FallbackAllowed: true}

	addr = SetShmCapability(addr, cap)
	addr = SetShmTransportHint(addr, hint)

	// Both should be retrievable
	gotCap := GetShmCapability(addr)
	gotHint := GetShmTransportHint(addr)

	if gotCap == nil || gotHint == nil {
		t.Fatal("Both capability and hint should be present")
	}
	if gotCap.SegmentName != "combined" {
		t.Errorf("Capability segment: got %q, want %q", gotCap.SegmentName, "combined")
	}
	if !gotHint.PreferShm {
		t.Error("Hint PreferShm should be true")
	}
}

func TestShmCapabilityString(t *testing.T) {
	tests := []struct {
		name string
		cap  ShmCapability
		want string
	}{
		{
			name: "enabled and preferred",
			cap:  ShmCapability{Enabled: true, SegmentName: "my_seg", Preferred: true},
			want: `ShmCapability{Enabled:true, SegmentName:"my_seg", Preferred:true, Required:false}`,
		},
		{
			name: "disabled",
			cap:  ShmCapability{Enabled: false, SegmentName: "", Preferred: false},
			want: `ShmCapability{Enabled:false, SegmentName:"", Preferred:false, Required:false}`,
		},
		{
			name: "with required",
			cap:  ShmCapability{Enabled: true, SegmentName: "seg", Preferred: true, Required: true},
			want: `ShmCapability{Enabled:true, SegmentName:"seg", Preferred:true, Required:true}`,
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

func TestShmTransportHintString(t *testing.T) {
	hint := ShmTransportHint{PreferShm: true, FallbackAllowed: false}
	want := `ShmTransportHint{PreferShm:true, FallbackAllowed:false}`
	got := hint.String()
	if got != want {
		t.Errorf("String(): got %q, want %q", got, want)
	}
}

func TestShmCapabilityEqualWrongType(t *testing.T) {
	cap := ShmCapability{Enabled: true, SegmentName: "seg", Preferred: true}
	// Equal should return false for wrong type
	if cap.Equal("not a ShmCapability") {
		t.Error("Equal(string) should return false")
	}
	if cap.Equal(42) {
		t.Error("Equal(int) should return false")
	}
	if cap.Equal(nil) {
		t.Error("Equal(nil) should return false")
	}
}

func TestShmTransportHintEqualWrongType(t *testing.T) {
	hint := ShmTransportHint{PreferShm: true}
	if hint.Equal("wrong type") {
		t.Error("Equal(string) should return false")
	}
	if hint.Equal(ShmCapability{Enabled: true}) {
		t.Error("Equal(ShmCapability) should return false")
	}
}

func TestGetShmCapabilityWrongType(t *testing.T) {
	// Test retrieving when attribute contains wrong type
	addr := resolver.Address{Addr: "test"}
	// Manually set wrong type in attributes
	addr.Attributes = addr.Attributes.WithValue(ShmLocalityKey{}, "wrong type")

	cap := GetShmCapability(addr)
	if cap != nil {
		t.Error("Expected nil when attribute has wrong type")
	}
}

func TestGetShmTransportHintWrongType(t *testing.T) {
	addr := resolver.Address{Addr: "test"}
	addr.Attributes = addr.Attributes.WithValue(ShmTransportHintKey{}, 12345)

	hint := GetShmTransportHint(addr)
	if hint != nil {
		t.Error("Expected nil when attribute has wrong type")
	}
}
