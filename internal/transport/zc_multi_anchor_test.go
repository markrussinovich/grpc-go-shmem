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
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMultiAnchorZC_TwoInFlight verifies the multi-anchor FIFO accepts
// two concurrent single-frame ZC reservations (which the legacy
// single-anchor protocol rejected via the zcActive gate).
func TestMultiAnchorZC_TwoInFlight(t *testing.T) {
	ctx := context.Background()
	segName := fmt.Sprintf("zcmulti-2-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, 4*1024*1024, 4*1024*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	bodyLen := 200 * 1024
	makePayload := func(seed byte) []byte {
		p := make([]byte, 5+bodyLen)
		binary.BigEndian.PutUint32(p[1:5], uint32(bodyLen))
		for i := 0; i < bodyLen; i++ {
			p[5+i] = seed + byte(i)
		}
		return p
	}
	for i := 0; i < 2; i++ {
		if err := writeFrame(ctx, tx, FrameHeader{
			Type: FrameTypeMESSAGE, StreamID: 1,
		}, makePayload(byte(i+1))); err != nil {
			t.Fatalf("writeFrame %d: %v", i, err)
		}
	}

	// Read both frames without freeing the buffers — both ZC paths
	// must succeed.
	fh1, buf1, err := readFrameView(ctx, rx)
	if err != nil {
		t.Fatalf("readFrameView 1: %v", err)
	}
	if fh1.Type != FrameTypeMESSAGE {
		t.Fatalf("frame 1 type: got %v", fh1.Type)
	}
	fh2, buf2, err := readFrameView(ctx, rx)
	if err != nil {
		t.Fatalf("readFrameView 2: %v", err)
	}
	if fh2.Type != FrameTypeMESSAGE {
		t.Fatalf("frame 2 type: got %v", fh2.Type)
	}

	// Both anchors should be in flight.
	if got := rx.anchorTail.Load() - rx.anchorHead.Load(); got != 2 {
		t.Errorf("in-flight anchors: got %d want 2", got)
	}

	// Free in reverse order: buf2 (newer) first. Prefix walk must NOT
	// advance head past buf1 (the older, still-held anchor).
	buf2.Free()
	if got := rx.anchorHead.Load(); got != 0 {
		t.Errorf("anchorHead after out-of-order Free: got %d want 0 (blocked by older anchor)", got)
	}
	// Free the older anchor. Prefix walk should drain through both.
	buf1.Free()
	// Allow drain goroutines to converge (drainPrefix is synchronous in
	// the calling goroutine but we add a tiny yield to be safe).
	if got := rx.anchorHead.Load(); got != 2 {
		t.Errorf("anchorHead after both Free: got %d want 2", got)
	}
	if rx.IsZcChainActive() {
		t.Errorf("zcActive=1 after both anchors freed")
	}
}

// TestMultiAnchorZC_BudgetExceeded verifies that BeginMultiAnchor
// returns nil and increments shmZCAnchorBudgetExceeded when the FIFO
// slot is occupied.
func TestMultiAnchorZC_BudgetExceeded(t *testing.T) {
	segName := fmt.Sprintf("zcmulti-bud-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, 4*1024*1024, 4*1024*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	// Fill the FIFO by directly calling Begin (bypasses framing).
	// Use bookkeeping-only ranges that won't interfere with real reads.
	const payloadLen = 64 * 1024
	seqs := make([]uint64, 0, zcAnchorBudgetCount)
	startBefore := atomic.LoadUint64(&shmZCAnchorBudgetExceeded)
	for i := 0; i < zcAnchorBudgetCount; i++ {
		seq, ok := rx.BeginMultiAnchor(uint64(i)*payloadLen, payloadLen)
		if !ok {
			t.Fatalf("Begin %d returned !ok (FIFO should not be full)", i)
		}
		seqs = append(seqs, seq)
	}
	// One more must be rejected.
	if _, ok := rx.BeginMultiAnchor(uint64(zcAnchorBudgetCount)*payloadLen, payloadLen); ok {
		t.Errorf("Begin %d: expected !ok (budget exceeded), got ok", zcAnchorBudgetCount)
	}
	if got := atomic.LoadUint64(&shmZCAnchorBudgetExceeded) - startBefore; got != 1 {
		t.Errorf("budget-exceeded counter delta: got %d want 1", got)
	}
	// Release all to leave the ring in a clean state.
	for _, s := range seqs {
		rx.ReleaseMultiAnchor(s)
	}
}

// TestMultiAnchorZC_ConcurrentReleases stresses out-of-order concurrent
// releases against the drain prefix walk. Designed for the race
// detector — any zcActive / head / tail mis-ordering surfaces as a
// race or an inconsistent final state.
func TestMultiAnchorZC_ConcurrentReleases(t *testing.T) {
	segName := fmt.Sprintf("zcmulti-conc-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, 4*1024*1024, 4*1024*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	const n = 64
	const payloadLen = 8 * 1024
	seqs := make([]uint64, n)
	for i := 0; i < n; i++ {
		seq, ok := rx.BeginMultiAnchor(uint64(i)*payloadLen, payloadLen)
		if !ok {
			t.Fatalf("Begin %d returned !ok", i)
		}
		seqs[i] = seq
	}

	// Release all anchors concurrently. The prefix walk must converge
	// to head==tail and zcActive==0.
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rx.ReleaseMultiAnchor(seqs[idx])
		}(i)
	}
	wg.Wait()

	// Trigger one final drain to clear zcActive in case the last
	// releaser's drain raced with itself.
	rx.drainReleasedAnchorPrefix()
	if got := rx.anchorHead.Load(); got != n {
		t.Errorf("anchorHead: got %d want %d", got, n)
	}
	if got := rx.anchorTail.Load(); got != n {
		t.Errorf("anchorTail: got %d want %d", got, n)
	}
	if rx.IsZcChainActive() {
		t.Errorf("zcActive=1 after all releases")
	}
}
