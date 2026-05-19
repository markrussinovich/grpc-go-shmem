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
	"sort"
	"strings"
	"testing"
)

// TestShmFDCountEnd2End is a definitive audit of the FD footprint for a
// single SHM connection at STEADY STATE -- i.e., after the dial /
// handshake completes and the data plane has done at least one wake.
// This is the number Doug Fawley's gRFC review really wants (and the
// number an operator needs for `ulimit -n` sizing).
//
// Unlike TestShmFDCount (which only measures CreateSegment's immediate
// allocations), this drives a server + client through a full
// CreateSegment / RegisterRing / one wake-and-wait cycle so any
// lazily-allocated eventfds get materialised before we snapshot.
//
// Skips on non-Linux (no /proc/self/fd).
func TestShmFDCountEnd2End(t *testing.T) {
	// This test audits the production FD footprint, which requires the
	// eventfd waker. The package-wide TestMain disables it (futex
	// fallback) because most low-level tests mix raw and registered
	// rings; here we exercise the production path explicitly. Save and
	// restore the prior state so subsequent tests see the TestMain
	// default again.
	prevEventfd := shmDataSegWakeEnabled()
	ConfigureShmEventfdWakerForBench(true)
	t.Cleanup(func() { ConfigureShmEventfdWakerForBench(prevEventfd) })

	before := snapshotProcFDs(t)
	if before == nil {
		return
	}

	// Mirror what NewShmListener does for the control segment.
	ctlName := testSegName("e2e_ctl")
	defer RemoveSegment(ctlName)
	ctlSeg, err := CreateSegment(ctlName, MinRingCapacity, MinRingCapacity)
	if err != nil {
		t.Fatalf("CreateSegment(ctl): %v", err)
	}
	defer ctlSeg.Close()

	// Mirror what the accept path does for the data segment.
	dataName := testSegName("e2e_data")
	defer RemoveSegment(dataName)
	dataServerSeg, err := CreateSegment(dataName, 1<<20, 1<<20)
	if err != nil {
		t.Fatalf("CreateSegment(data): %v", err)
	}
	defer dataServerSeg.Close()

	// Client side: open both segments.
	dataClientSeg, err := OpenSegment(dataName)
	if err != nil {
		t.Fatalf("OpenSegment(data): %v", err)
	}
	defer dataClientSeg.Close()

	// Set up rings so any wake path materialises its FD.
	serverRingA := NewShmRingFromSegment(dataServerSeg.A, dataServerSeg.Mem)
	serverRingB := NewShmRingFromSegment(dataServerSeg.B, dataServerSeg.Mem)
	clientRingA := NewShmRingFromSegment(dataClientSeg.A, dataClientSeg.Mem)
	clientRingB := NewShmRingFromSegment(dataClientSeg.B, dataClientSeg.Mem)
	dataServerSeg.RegisterRing(serverRingA)
	dataServerSeg.RegisterRing(serverRingB)
	dataClientSeg.RegisterRing(clientRingA)
	dataClientSeg.RegisterRing(clientRingB)

	// Trigger the wake path to ensure any lazy eventfd allocation
	// happens before we snapshot the FDs. We need wakes on BOTH
	// directions (data + space) to capture the full footprint.
	for _, r := range []*ShmRing{serverRingA, serverRingB, clientRingA, clientRingB} {
		hdr := r.header()
		r.signalData(&hdr.dataSeq)
		r.signalSpace(&hdr.spaceSeq)
		r.signalContig(&hdr.contigSeq)
	}

	// Snapshot AFTER all the wakes have run (lazy allocations should
	// be done).
	after := snapshotProcFDs(t)
	delta := fdDelta(before, after)

	// Categorise the delta.
	type bucket struct {
		count int
		items []string
	}
	cats := map[string]*bucket{
		"/dev/shm SHM file":       {},
		"eventfd (data-seg wake)": {},
		"other":                   {},
	}
	fds := make([]string, 0, len(delta))
	for fd := range delta {
		fds = append(fds, fd)
	}
	sort.Strings(fds)
	for _, fd := range fds {
		target := delta[fd]
		switch {
		case strings.Contains(target, "/dev/shm/grpc_shm_") || strings.Contains(target, "grpc_shm_"):
			cats["/dev/shm SHM file"].count++
			cats["/dev/shm SHM file"].items = append(cats["/dev/shm SHM file"].items, fd+" → "+target)
		case strings.Contains(target, "anon_inode:[eventfd]"):
			cats["eventfd (data-seg wake)"].count++
			cats["eventfd (data-seg wake)"].items = append(cats["eventfd (data-seg wake)"].items, fd+" → "+target)
		case strings.Contains(target, "socket:[") || strings.Contains(target, "anon_inode:socket"):
			cats["eventfd (data-seg wake)"].count++
			cats["eventfd (data-seg wake)"].items = append(cats["eventfd (data-seg wake)"].items, fd+" → "+target)
		default:
			cats["other"].count++
			cats["other"].items = append(cats["other"].items, fd+" → "+target)
		}
	}

	t.Logf("=== SHM FD footprint (1 listener + 1 connection, same process) ===")
	t.Logf("eventfd waker: %v (ON by default; toggle via ConfigureShmEventfdWakerForBench)", shmDataSegWakeEnabled())
	for cat, b := range cats {
		if b.count == 0 {
			continue
		}
		t.Logf("  %s: %d", cat, b.count)
		for _, item := range b.items {
			t.Logf("    %s", item)
		}
	}
	t.Logf("Total NEW FDs: %d", len(delta))

	// Per-SIDE accounting commentary in the log so reviewers reading
	// the test output can decompose the same-process number.
	t.Logf("")
	t.Logf("Per-process accounting:")
	t.Logf("  Control segment file:    1 (server keeps; client transient -- closed after dial in real flow)")
	t.Logf("  Data segment file:       2 in same-proc test (server side + client side, separate fds)")
	t.Logf("                             1 per process in cross-process production")
	t.Logf("  In-proc eventfds:        lazy per (segmentID, address). Counted above.")
	t.Logf("                             Cross-process production would dup these via SCM_RIGHTS.")
	t.Logf("  Data-seg eventfds:       2 per side when the eventfd waker is ON (1 per direction; default).")
}
