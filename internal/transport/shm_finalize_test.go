//go:build linux

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
)

// TestFinalizeDataSegWaker_OpenerReady verifies that finalizeDataSegWaker
// keeps the eventfd waker when the opener published OpenerWakeReady=true
// in the header (same-process via in-memory stash OR cross-process via
// SCM_RIGHTS handoff -- both yield the same observable state).
func TestFinalizeDataSegWaker_OpenerReady(t *testing.T) {
	// Force eventfd ON for this test (TestMain in shm_integration_test.go
	// disables it by default for low-level raw-ring tests; here we
	// exercise the production path explicitly).
	prev := shmDataSegWakeEnabled()
	ConfigureShmEventfdWakerForBench(true)
	t.Cleanup(func() { ConfigureShmEventfdWakerForBench(prev) })

	name := testSegName("finalize_opener_ready")
	defer RemoveSegment(name)

	seg, err := CreateSegment(name, MinRingCapacity, MinRingCapacity)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	if seg.dataSegWaker == nil {
		t.Skip("eventfd waker not allocated (env may not support eventfd); skipping")
	}

	// Simulate the post-handshake state: opener published its
	// readiness flag (whether via stash claim or SCM_RIGHTS recv).
	seg.H.SetOpenerWakeReady(true)

	seg.finalizeDataSegWaker()

	if seg.dataSegWaker == nil {
		t.Error("finalizeDataSegWaker dropped the waker when OpenerWakeReady=true")
	}
}

// TestFinalizeDataSegWaker_OpenerMissing verifies that finalizeDataSegWaker
// drops the eventfd waker when the opener failed to obtain one (i.e.,
// OpenerWakeReady=false). Without this, the creator-with-waker /
// opener-without-waker asymmetry would deadlock the opener-producer /
// creator-consumer direction (futex_wake never reaches eventfd-parked
// Read). Finalize must release the creator's waker so both peers
// converge on the futex fallback.
func TestFinalizeDataSegWaker_OpenerMissing(t *testing.T) {
	prev := shmDataSegWakeEnabled()
	ConfigureShmEventfdWakerForBench(true)
	t.Cleanup(func() { ConfigureShmEventfdWakerForBench(prev) })

	name := testSegName("finalize_opener_missing")
	defer RemoveSegment(name)

	seg, err := CreateSegment(name, MinRingCapacity, MinRingCapacity)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	if seg.dataSegWaker == nil {
		t.Skip("eventfd waker not allocated (env may not support eventfd); skipping")
	}

	// Register a ring so we can verify the waker is cleared from
	// previously-registered rings too.
	ring := NewShmRingFromSegment(seg.A, seg.Mem)
	seg.RegisterRing(ring)
	if ring.dataSegWaker == nil {
		t.Fatal("RegisterRing did not propagate the segment waker to the ring")
	}

	// Simulate the post-handshake state: opener did NOT obtain a waker
	// (no stash entry AND SCM_RIGHTS recv failed).
	seg.H.SetOpenerWakeReady(false)

	seg.finalizeDataSegWaker()

	if seg.dataSegWaker != nil {
		t.Error("finalizeDataSegWaker did not drop the segment waker when OpenerWakeReady=false")
	}
	if ring.dataSegWaker != nil {
		t.Error("finalizeDataSegWaker did not clear waker from registered ring when OpenerWakeReady=false")
	}
}

// TestFinalizeDataSegWaker_NilWaker confirms finalize is a no-op when
// the segment has no waker (the typical opener side in any scenario,
// and any side when eventfd is disabled).
func TestFinalizeDataSegWaker_NilWaker(t *testing.T) {
	name := testSegName("finalize_nil_waker")
	defer RemoveSegment(name)

	seg, err := CreateSegment(name, MinRingCapacity, MinRingCapacity)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	seg.dataSegWaker = nil // force nil regardless of env
	seg.finalizeDataSegWaker()
	if seg.dataSegWaker != nil {
		t.Error("finalizeDataSegWaker re-installed a waker; expected no-op")
	}
}
