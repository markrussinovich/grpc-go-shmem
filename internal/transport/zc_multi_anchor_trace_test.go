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

// TestMultiAnchorZC_ConcurrentPingPongTrace reproduces the
// "zc-fail-ineligible/op ≈ N reads/op" pattern from the Linux
// bench in a single-process Windows-friendly test so we can iterate
// on the eligibility check locally without round-tripping through
// the VM. It runs N concurrent producer/consumer pairs over a
// shared ring at the given message size, measures the post-test
// distribution of the ZC sub-counters, and reports a one-line
// summary.
//
// This is a diagnostic test, not a correctness test. It always
// passes (t.Log only).
func TestMultiAnchorZC_ConcurrentPingPongTrace(t *testing.T) {
	if testing.Short() {
		t.Skip("trace test — needs full duration")
	}
	const (
		ringSize      = 64 * 1024 * 1024
		bodyLen       = 64 * 1024
		numStreams    = 100
		framesPerStrm = 50
	)

	// Snapshot all relevant counters BEFORE the test.
	snap := func() (zcRead, copyRead, accRead, ineligible, bp, smallPL, smallRing, notContig, failWrap, failShort, failAcc, failLpm, budget, pending uint64) {
		return atomic.LoadUint64(&shmZCReadFire),
			atomic.LoadUint64(&shmCopyReadFire),
			atomic.LoadUint64(&shmAccReadFire),
			atomic.LoadUint64(&shmZCFailIneligible),
			atomic.LoadUint64(&shmZCElig_BackPressure),
			atomic.LoadUint64(&shmZCElig_PayloadSmall),
			atomic.LoadUint64(&shmZCElig_RingTooSmall),
			atomic.LoadUint64(&shmZCElig_NotContig),
			atomic.LoadUint64(&shmZCFailPSecondNonzero),
			atomic.LoadUint64(&shmZCFailPFirstShort),
			atomic.LoadUint64(&shmZCFailAccInProgress),
			atomic.LoadUint64(&shmZCFailLpmMismatch),
			atomic.LoadUint64(&shmZCAnchorBudgetExceeded),
			atomic.LoadUint64(&shmZCFailPendingFrame)
	}
	zr0, cr0, ar0, ie0, bp0, sp0, sr0, nc0, fw0, fs0, fa0, fl0, bd0, pf0 := snap()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	segName := fmt.Sprintf("zcmulti-trace-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)
	seg, err := CreateSegment(segName, ringSize, ringSize)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()
	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	// One reader goroutine drains via readFrameView (which dispatches
	// to readFrameViewH2 — the same path the production transport hits).
	totalFrames := numStreams * framesPerStrm
	var readWG sync.WaitGroup
	readWG.Add(1)
	go func() {
		defer readWG.Done()
		for read := 0; read < totalFrames; read++ {
			fh, buf, err := readFrameView(ctx, rx)
			if err != nil {
				t.Errorf("read %d: %v", read, err)
				return
			}
			if fh.Type != FrameTypeMESSAGE {
				continue
			}
			// Free immediately to mimic gRPC's proto.Unmarshal lifecycle.
			if buf != nil {
				buf.Free()
			}
		}
	}()

	// numStreams concurrent producer goroutines, each sending
	// framesPerStrm single-LPM MESSAGE frames of bodyLen bytes.
	//
	// IMPORTANT: tx.ReserveWrite is single-producer (no mutex). Multiple
	// concurrent writers corrupt the ring. We funnel through a single
	// writer goroutine over a channel — this mirrors the production
	// transport's writeLoop serialization (inlineMu+writeLoop).
	type writeJob struct {
		streamID uint32
		payload  []byte
	}
	jobs := make(chan writeJob, numStreams*4)
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		for j := range jobs {
			if err := writeFrame(ctx, tx, FrameHeader{
				Type: FrameTypeMESSAGE, StreamID: j.streamID,
			}, j.payload); err != nil {
				t.Errorf("write stream %d: %v", j.streamID, err)
				return
			}
		}
	}()

	var sendWG sync.WaitGroup
	for s := 0; s < numStreams; s++ {
		sendWG.Add(1)
		go func(streamID uint32) {
			defer sendWG.Done()
			payload := make([]byte, 5+bodyLen)
			binary.BigEndian.PutUint32(payload[1:5], uint32(bodyLen))
			for f := 0; f < framesPerStrm; f++ {
				jobs <- writeJob{streamID: streamID, payload: payload}
			}
		}(uint32(s + 1))
	}
	sendWG.Wait()
	close(jobs)
	writerWG.Wait()
	readWG.Wait()

	zr1, cr1, ar1, ie1, bp1, sp1, sr1, nc1, fw1, fs1, fa1, fl1, bd1, pf1 := snap()
	t.Logf("=== ZC trace summary (totalFrames=%d) ===", totalFrames)
	t.Logf("  zc-read           = %d (%.2f%%)", zr1-zr0, 100*float64(zr1-zr0)/float64(totalFrames))
	t.Logf("  copy-read         = %d (%.2f%%)", cr1-cr0, 100*float64(cr1-cr0)/float64(totalFrames))
	t.Logf("  acc-read          = %d (%.2f%%)", ar1-ar0, 100*float64(ar1-ar0)/float64(totalFrames))
	t.Logf("  pending-replay    = %d (%.2f%%)", pf1-pf0, 100*float64(pf1-pf0)/float64(totalFrames))
	t.Logf("  zc-anchor-budget  = %d", bd1-bd0)
	t.Logf("  fail-ineligible   = %d (%.2f%%)", ie1-ie0, 100*float64(ie1-ie0)/float64(totalFrames))
	t.Logf("    elig-bp         = %d", bp1-bp0)
	t.Logf("    elig-smallpl    = %d", sp1-sp0)
	t.Logf("    elig-smallring  = %d", sr1-sr0)
	t.Logf("    elig-notcontig  = %d", nc1-nc0)
	t.Logf("  fail-wrap         = %d", fw1-fw0)
	t.Logf("  fail-shorthdr     = %d", fs1-fs0)
	t.Logf("  fail-accinprogress= %d", fa1-fa0)
	t.Logf("  fail-lpmmismatch  = %d", fl1-fl0)
}
