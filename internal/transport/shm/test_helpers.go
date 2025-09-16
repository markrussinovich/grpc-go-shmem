/*
 * Copyright 2024 gRPC authors.
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

package shm

import (
	"fmt"
	"testing"
	"time"
)

// createTestSegment creates a test segment with a unique name and proper cleanup.
// It automatically registers cleanup with t.Cleanup() to ensure the segment is
// always cleaned up even if the test fails or panics.
func createTestSegment(t *testing.T, baseName string, ringACapacity, ringBCapacity uint64) *Segment {
	t.Helper()

	// Create unique name using test name and timestamp
	segName := fmt.Sprintf("%s-%s-%d", baseName, t.Name(), time.Now().UnixNano())

	// Ensure any existing segment is removed first
	RemoveSegment(segName)

	// Create the segment
	seg, err := CreateSegment(segName, ringACapacity, ringBCapacity)
	if err != nil {
		t.Fatalf("Failed to create test segment %s: %v", segName, err)
	}

	// Register cleanup to ensure segment is always cleaned up
	t.Cleanup(func() {
		if seg != nil {
			seg.Close()
		}
		RemoveSegment(segName)
	})

	return seg
}

// createTestSegmentWithName creates a test segment with a specific name (for compatibility).
// It automatically registers cleanup to ensure proper resource cleanup.
func createTestSegmentWithName(t *testing.T, name string, ringACapacity, ringBCapacity uint64) *Segment {
	t.Helper()

	// Ensure any existing segment is removed first
	RemoveSegment(name)

	// Create the segment
	seg, err := CreateSegment(name, ringACapacity, ringBCapacity)
	if err != nil {
		t.Fatalf("Failed to create test segment %s: %v", name, err)
	}

	// Register cleanup to ensure segment is always cleaned up
	t.Cleanup(func() {
		if seg != nil {
			seg.Close()
		}
		RemoveSegment(name)
	})

	return seg
}
