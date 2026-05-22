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
	"strings"
	"testing"
	"time"
)

// TestNewShmListener_DupDetect verifies that NewShmListener refuses to
// start when a control segment with the same name already exists and
// has its ServerReady flag set, instead of silently unlinking the
// live segment (which would break the original listener's ability to
// accept new clients).
//
// Regression guard for the round-1 P1 fix in NewShmListener that
// probes existing segments via OpenSegment + ServerReady() before
// cleanup.
func TestNewShmListener_DupDetect(t *testing.T) {
	name := fmt.Sprintf("dup-detect-%d", time.Now().UnixNano())
	defer RemoveSegment(name + shmControlSuffix)

	addr := &ShmAddr{Name: name}
	lis1, err := NewShmListener(addr, MinRingCapacity*32, MinRingCapacity, MinRingCapacity)
	if err != nil {
		t.Fatalf("first NewShmListener: %v", err)
	}
	defer lis1.Close()

	// Second listener on the same name must fail with a clear "in use"
	// error, NOT silently steal the segment.
	_, err = NewShmListener(addr, MinRingCapacity*32, MinRingCapacity, MinRingCapacity)
	if err == nil {
		t.Fatal("second NewShmListener: expected error for duplicate, got nil")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("second NewShmListener: expected 'already in use' in error, got %q", err)
	}
}
