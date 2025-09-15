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

package shm

import (
	"testing"
	"unsafe"
)

func TestSegmentHeaderSize(t *testing.T) {
	// Verify SegmentHeader is exactly 128 bytes as specified
	size := unsafe.Sizeof(SegmentHeader{})
	if size != SegmentHeaderSize {
		t.Errorf("SegmentHeader size = %d, want %d", size, SegmentHeaderSize)
	}
}

func TestRingHeaderSize(t *testing.T) {
	// Verify RingHeader is exactly 64 bytes as specified
	size := unsafe.Sizeof(RingHeader{})
	if size != RingHeaderSize {
		t.Errorf("RingHeader size = %d, want %d", size, RingHeaderSize)
	}
}

func TestSegmentHeaderFieldOffsets(t *testing.T) {
	h := &SegmentHeader{}

	// Test field offsets match specification
	tests := []struct {
		name   string
		offset uintptr
		want   uintptr
	}{
		{"magic", unsafe.Offsetof(h.magic), 0x00},
		{"version", unsafe.Offsetof(h.version), 0x08},
		{"flags", unsafe.Offsetof(h.flags), 0x0C},
		{"totalSize", unsafe.Offsetof(h.totalSize), 0x10},
		{"ringAOff", unsafe.Offsetof(h.ringAOff), 0x18},
		{"ringACap", unsafe.Offsetof(h.ringACap), 0x20},
		{"ringBOff", unsafe.Offsetof(h.ringBOff), 0x28},
		{"ringBCap", unsafe.Offsetof(h.ringBCap), 0x30},
		{"serverPID", unsafe.Offsetof(h.serverPID), 0x38},
		{"clientPID", unsafe.Offsetof(h.clientPID), 0x3C},
		{"serverReady", unsafe.Offsetof(h.serverReady), 0x40},
		{"clientReady", unsafe.Offsetof(h.clientReady), 0x44},
		{"closed", unsafe.Offsetof(h.closed), 0x48},
		{"pad", unsafe.Offsetof(h.pad), 0x4C},
		{"reserved", unsafe.Offsetof(h.reserved), 0x50},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.offset != tt.want {
				t.Errorf("offset of %s = 0x%02X, want 0x%02X", tt.name, uint64(tt.offset), uint64(tt.want))
			}
		})
	}
}

func TestRingHeaderFieldOffsets(t *testing.T) {
	r := &RingHeader{}

	// Test field offsets match specification
	tests := []struct {
		name   string
		offset uintptr
		want   uintptr
	}{
		{"capacity", unsafe.Offsetof(r.capacity), 0x00},
		{"widx", unsafe.Offsetof(r.widx), 0x08},
		{"ridx", unsafe.Offsetof(r.ridx), 0x10},
		{"dataSeq", unsafe.Offsetof(r.dataSeq), 0x18},
		{"spaceSeq", unsafe.Offsetof(r.spaceSeq), 0x1C},
		{"closed", unsafe.Offsetof(r.closed), 0x20},
		{"pad", unsafe.Offsetof(r.pad), 0x24},
		{"reserved", unsafe.Offsetof(r.reserved), 0x28},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.offset != tt.want {
				t.Errorf("offset of %s = 0x%02X, want 0x%02X", tt.name, uint64(tt.offset), uint64(tt.want))
			}
		})
	}
}

func TestIsPowerOfTwo(t *testing.T) {
	tests := []struct {
		n    uint64
		want bool
	}{
		{0, false},
		{1, true},
		{2, true},
		{3, false},
		{4, true},
		{5, false},
		{8, true},
		{16, true},
		{1024, true},
		{1023, false},
		{4096, true},
		{65536, true},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			if got := IsPowerOfTwo(tt.n); got != tt.want {
				t.Errorf("IsPowerOfTwo(%d) = %v, want %v", tt.n, got, tt.want)
			}
		})
	}
}

func TestNextPowerOfTwo(t *testing.T) {
	tests := []struct {
		n    uint64
		want uint64
	}{
		{0, 1},
		{1, 1},
		{2, 2},
		{3, 4},
		{4, 4},
		{5, 8},
		{8, 8},
		{9, 16},
		{1023, 1024},
		{1024, 1024},
		{1025, 2048},
		{4095, 4096},
		{4096, 4096},
		{65535, 65536},
		{65536, 65536},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			if got := NextPowerOfTwo(tt.n); got != tt.want {
				t.Errorf("NextPowerOfTwo(%d) = %d, want %d", tt.n, got, tt.want)
			}
		})
	}
}

func TestCalculateSegmentLayout(t *testing.T) {
	tests := []struct {
		name          string
		ringACapacity uint64
		ringBCapacity uint64
		wantErr       bool
		wantTotalSize uint64
		wantRingAOff  uint64
		wantRingBOff  uint64
	}{
		{
			name:          "default capacities",
			ringACapacity: DefaultRingCapacity,
			ringBCapacity: DefaultRingCapacity,
			wantErr:       false,
			wantTotalSize: 131328, // Calculated: 128 + 64 + 65536 + 64 + 65536 = 131328
			wantRingAOff:  128,    // Aligned segment header
			wantRingBOff:  65728,  // Aligned after ring A: 128 + 64 + 65536 = 65728
		},
		{
			name:          "minimum capacities",
			ringACapacity: MinRingCapacity,
			ringBCapacity: MinRingCapacity,
			wantErr:       false,
		},
		{
			name:          "non-power-of-two ring A",
			ringACapacity: 1000,
			ringBCapacity: 4096,
			wantErr:       true,
		},
		{
			name:          "non-power-of-two ring B",
			ringACapacity: 4096,
			ringBCapacity: 1000,
			wantErr:       true,
		},
		{
			name:          "below minimum ring A",
			ringACapacity: 1024,
			ringBCapacity: 4096,
			wantErr:       true,
		},
		{
			name:          "below minimum ring B",
			ringACapacity: 4096,
			ringBCapacity: 1024,
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			totalSize, ringAOff, ringBOff, err := CalculateSegmentLayout(tt.ringACapacity, tt.ringBCapacity)

			if (err != nil) != tt.wantErr {
				t.Errorf("CalculateSegmentLayout() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if tt.wantTotalSize != 0 && totalSize != tt.wantTotalSize {
					t.Errorf("CalculateSegmentLayout() totalSize = %d, want %d", totalSize, tt.wantTotalSize)
				}
				if tt.wantRingAOff != 0 && ringAOff != tt.wantRingAOff {
					t.Errorf("CalculateSegmentLayout() ringAOff = %d, want %d", ringAOff, tt.wantRingAOff)
				}
				if tt.wantRingBOff != 0 && ringBOff != tt.wantRingBOff {
					t.Errorf("CalculateSegmentLayout() ringBOff = %d, want %d", ringBOff, tt.wantRingBOff)
				}
			}
		})
	}
}

func TestRingInvariants(t *testing.T) {
	r := &RingHeader{}
	r.SetCapacity(4096)
	r.SetWriteIndex(0)
	r.SetReadIndex(0)

	// Test empty ring
	if !r.IsEmpty() {
		t.Error("IsEmpty() = false, want true for new ring")
	}
	if r.IsFull() {
		t.Error("IsFull() = true, want false for new ring")
	}
	if r.Used() != 0 {
		t.Errorf("Used() = %d, want 0 for new ring", r.Used())
	}
	if r.Available() != 4096 {
		t.Errorf("Available() = %d, want 4096 for new ring", r.Available())
	}

	// Test after writing some data
	r.SetWriteIndex(100)
	if r.IsEmpty() {
		t.Error("IsEmpty() = true, want false after writing")
	}
	if r.Used() != 100 {
		t.Errorf("Used() = %d, want 100 after writing 100 bytes", r.Used())
	}
	if r.Available() != 3996 {
		t.Errorf("Available() = %d, want 3996 after writing 100 bytes", r.Available())
	}

	// Test offset calculation
	offset := r.Offset(4200) // > capacity
	if offset != 104 {       // 4200 & (4096-1) = 104
		t.Errorf("Offset(4200) = %d, want 104", offset)
	}
}

func TestSegmentHeaderAtomicAccess(t *testing.T) {
	h := &SegmentHeader{}

	// Test magic bytes
	magic := [8]byte{'G', 'R', 'P', 'C', 'S', 'H', 'M', 0}
	h.SetMagic(magic)
	if h.Magic() != magic {
		t.Errorf("Magic() = %v, want %v", h.Magic(), magic)
	}

	// Test version
	h.SetVersion(SegmentVersion)
	if h.Version() != SegmentVersion {
		t.Errorf("Version() = %d, want %d", h.Version(), SegmentVersion)
	}

	// Test flags
	h.SetServerReady(true)
	if !h.ServerReady() {
		t.Error("ServerReady() = false, want true")
	}

	h.SetClientReady(true)
	if !h.ClientReady() {
		t.Error("ClientReady() = false, want true")
	}

	h.SetClosed(true)
	if !h.Closed() {
		t.Error("Closed() = false, want true")
	}
}
