/*
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
 */

package shm_test

import (
	"fmt"
	"testing"

	"google.golang.org/grpc/internal/transport/shm"
)

func TestRing_NewCapacityPowerOfTwo(t *testing.T) {
	testCases := []struct {
		minCap   int
		expected int
	}{
		{1, 16},      // Below minimum
		{8, 16},      // Below minimum
		{15, 16},     // Below minimum
		{16, 16},     // Exact minimum
		{17, 32},     // Round up
		{31, 32},     // Round up
		{32, 32},     // Exact power of two
		{33, 64},     // Round up
		{63, 64},     // Round up
		{64, 64},     // Exact power of two
		{65, 128},    // Round up
		{127, 128},   // Round up
		{128, 128},   // Exact power of two
		{129, 256},   // Round up
		{1000, 1024}, // Round up to next power of two
		{1024, 1024}, // Exact power of two
		{1025, 2048}, // Round up to next power of two
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("minCap_%d", tc.minCap), func(t *testing.T) {
			ring, err := shm.NewRing(tc.minCap)
			if err != nil {
				t.Fatalf("NewRing(%d) failed: %v", tc.minCap, err)
			}
			if ring == nil {
				t.Fatalf("NewRing(%d) returned nil ring", tc.minCap)
			}

			actual := ring.Capacity()
			if actual != tc.expected {
				t.Errorf("NewRing(%d).Capacity() = %d, want %d", tc.minCap, actual, tc.expected)
			}
		})
	}
}

func TestRing_CapacityReportsCorrectly(t *testing.T) {
	testCases := []int{16, 32, 64, 128, 256, 512, 1024, 2048, 4096}

	for _, expectedCap := range testCases {
		t.Run(fmt.Sprintf("capacity_%d", expectedCap), func(t *testing.T) {
			// Request exactly the capacity (which is a power of two)
			ring, err := shm.NewRing(expectedCap)
			if err != nil {
				t.Fatalf("NewRing(%d) failed: %v", expectedCap, err)
			}

			actual := ring.Capacity()
			if actual != expectedCap {
				t.Errorf("Ring.Capacity() = %d, want %d", actual, expectedCap)
			}
		})
	}
}

func TestRing_NewRingInvalidCapacity(t *testing.T) {
	testCases := []int{0, -1, -10, -100}

	for _, invalidCap := range testCases {
		t.Run(fmt.Sprintf("invalid_%d", invalidCap), func(t *testing.T) {
			ring, err := shm.NewRing(invalidCap)
			if err == nil {
				t.Errorf("NewRing(%d) should have returned an error", invalidCap)
			}
			if ring != nil {
				t.Errorf("NewRing(%d) should have returned nil ring on error", invalidCap)
			}
		})
	}
}

func TestRing_Availability(t *testing.T) {
	ring, err := shm.NewRing(64)
	if err != nil {
		t.Fatalf("NewRing(64) failed: %v", err)
	}

	// After creating a new ring, no data has been written yet
	if availRead := ring.AvailableRead(); availRead != 0 {
		t.Errorf("AvailableRead() = %d, want 0 for new ring", availRead)
	}

	// All capacity should be available for writing
	expectedCap := ring.Capacity()
	if availWrite := ring.AvailableWrite(); availWrite != expectedCap {
		t.Errorf("AvailableWrite() = %d, want %d for new ring", availWrite, expectedCap)
	}

	// Test different ring sizes
	testCases := []int{16, 32, 128, 256, 1024}
	for _, capacity := range testCases {
		t.Run(fmt.Sprintf("capacity_%d", capacity), func(t *testing.T) {
			r, err := shm.NewRing(capacity)
			if err != nil {
				t.Fatalf("NewRing(%d) failed: %v", capacity, err)
			}

			if availRead := r.AvailableRead(); availRead != 0 {
				t.Errorf("AvailableRead() = %d, want 0 for new ring", availRead)
			}

			actualCap := r.Capacity()
			if availWrite := r.AvailableWrite(); availWrite != actualCap {
				t.Errorf("AvailableWrite() = %d, want %d for new ring", availWrite, actualCap)
			}
		})
	}
}
