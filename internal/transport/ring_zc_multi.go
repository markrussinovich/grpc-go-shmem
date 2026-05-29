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
	"sync/atomic"
)

// Multi-anchor single-frame zero-copy receive.
//
// Replaces the at-most-one-ZC binary gate (zcActive) in
// IsSpeculativeZCEligible with a bounded ordered FIFO of up to
// zcAnchorBudgetCount in-flight anchors. Each anchor records the
// (start, end) absolute byte range of a single DATA-frame payload
// held by a consumer-side mem.Buffer. Consumers release the buffer
// (typically inside proto.Unmarshal's read of the bytes), which
// transitions the slot to "released"; a prefix-walk advances
// header.ReadIdx through the contiguous released prefix.
//
// Concurrency model:
//
//   - Begin (BeginMultiAnchor) is called by the SINGLE reader
//     goroutine that runs the codec parse loop. No CAS needed on
//     the tail pointer.
//   - Release (anchor.Release) is called from arbitrary consumer
//     goroutines (any goroutine that holds the mem.Buffer and is
//     about to Free it).
//   - drainReleasedAnchorPrefix is MPMC-safe via CAS on the head
//     pointer; multiple racing releasers all converge on the
//     correct final head and the published header.ReadIdx is
//     monotonic-forward via publishTarget's CAS loop.
//
// Why this lifts the single-anchor restriction:
//
//   - The single-anchor scheme freezes header.ReadIdx for the
//     entire ZC duration of ONE frame. With 1000 concurrent
//     streams sharing one ring, 999 concurrent receives fall back
//     to the single-frame copy path because zcActive is already
//     held.
//   - Multi-anchor allows up to zcAnchorBudgetCount concurrent ZC
//     frames in flight at the same time. Each anchor independently
//     tracks its byte range; head advances through the prefix as
//     anchors release in any order. Out-of-order release stalls
//     the prefix advance until the head anchor also releases —
//     wire-correct but momentarily over-conservative on ReadIdx.
//
// Why this is NOT the previously-reverted D-lite design:
//
//   - D-lite (commit 912202af4, reverted) used a sync.Mutex
//     (zcAnchorsMu) around Begin and Release. At 1000 concurrent
//     streams the mutex churn was the dominant cost — D-lite
//     regressed -26 % on N=100/size=4096 even at 99 % ZC hit rate.
//     This implementation is lock-free: head/tail are
//     atomic.Uint64, slot.state is atomic.Uint32, and no mutex is
//     held on the hot path.
//   - D-lite's BeginAnchor passed a stale commitReadIdx capture as
//     each new anchor's start. Multiple anchors got identical
//     overlapping ranges. We use zcDeferredTarget as the running
//     offset (which the BeginAnchor fix 3bc8f7da later adopted)
//     and start fresh from header.ReadIdx only when no anchors
//     are in flight.

// zcAnchorBudgetCount caps in-flight single-frame ZC anchors per ring.
// Sized for the bench's 1000-concurrent-streams profile: at 1000
// streams pipelining 1 message in flight each, a 256-slot ring lets
// any 256 streams hold ZC bytes while the other 744 fall back to
// single-frame copy. In practice released slots cycle quickly
// (proto.Unmarshal holds the bytes for ≤ a few µs) so the steady-state
// occupancy is much lower than 256.
//
// Memory cost: zcAnchorBudgetCount × sizeof(anchorSlot). With 64-byte
// padded slots, 256 slots = 16 KiB per ring. Two data rings per
// transport (TX, RX) = 32 KiB per connection. Acceptable for the
// ZC throughput gain.
const zcAnchorBudgetCount = 256

// anchorSlot is one entry in the multi-anchor FIFO. Padded to a 64-byte
// cache line to prevent false sharing between adjacent in-flight
// anchors under concurrent Release.
//
// State machine:
//
//	0 (free)      → 1 (in-flight): Begin claims, writes start/end
//	1 (in-flight) → 2 (released):  anchor.Release marks done
//	2 (released)  → 0 (free):      drainReleasedAnchorPrefix frees
//
// Memory ordering: state.Store(1) in Begin uses sequential
// consistency (Go's atomic.Uint32 default), which establishes
// happens-before with any subsequent state.Load() in drainPrefix /
// Begin. start/end are atomic.Uint64 (not plain) because
// drainReleasedAnchorPrefix's nextSlot.start read at
// [drainReleasedAnchorPrefix#newCur<tail branch] does NOT first
// check nextSlot.state — the safety hand-off there relies only on
// anchorTail.Load() > newCur (release/acquire on anchorTail).
// Under slot reuse (anchorTail wraps past budget back to the same
// slot position), a stale walker can race with a new Begin's plain
// store on the SAME slot.start/end. Making both sides atomic
// eliminates the race without changing the happens-before contract
// established by state and anchorTail.
type anchorSlot struct {
	start atomic.Uint64 // 8B  — absolute ring offset of the held bytes' first byte
	end   atomic.Uint64 // 8B  — absolute ring offset of one past the last held byte
	state atomic.Uint32 // 4B — 0=free, 1=in-flight, 2=released
	_     [44]byte     // pad to 64 B cache line (8+8+4 = 20 used)
}

// MultiAnchor was previously a heap-allocated handle (ring*, seq);
// it has been retired. Begin returns (seq uint64, ok bool) directly
// and Release is now (*ShmRing).ReleaseMultiAnchor(seq). This
// eliminates ~2 M heap allocations per 5 s under N=1000/4 K bench
// without requiring a sync.Pool — the anchor is a pure identifier
// with no state independent of the ring's anchorSlots[].

// shmZCAnchorBudgetExceeded counts BeginMultiAnchor returns due to all
// slots being in use. Reported via the zcprobe bench harness as
// "zc-anchor-budget/op". A high value vs zc-read/op indicates the
// budget needs tuning for the workload.
var shmZCAnchorBudgetExceeded uint64

// ===== Diagnostic counters for ZC fast-path rejection =====
//
// Per-rejection-point counters that explain why a ZC fast-path attempt
// fell back to copy. Sum of these (per op) + ZCReadFire (per op)
// equals the per-op DATA frame count for the workload. Exposed via
// the same `bench` machinery as the other shm counters so production
// profiles can attribute ZC drop-outs to their root cause.
var (
	shmZCFailPSecondNonzero  uint64 // body wrapped across ring boundary
	shmZCFailPFirstShort     uint64 // first slice < 5 B (LPM header doesn't fit)
	shmZCFailAccInProgress   uint64 // per-stream LPM accumulator non-empty
	shmZCFailLpmMismatch     uint64 // 5+bodyLen != payloadLen (multi-LPM in one DATA)
	shmZCFailIneligible      uint64 // IsMultiAnchorZCEligible returned false
)

// Sub-counters for IsMultiAnchorZCEligible rejection. Sum across these
// = shmZCFailIneligible. Reported via the zcprobe bench harness as
// zc-elig-* metrics. Back-pressure rejection at high concurrency is
// expected (ring fills past 75% used) — surfaces as a tunable, not a
// bug.
var (
	shmZCElig_NotContig    uint64
	shmZCElig_RingTooSmall uint64
	shmZCElig_PayloadSmall uint64
	shmZCElig_BackPressure uint64

	// shmZCFailPendingFrame: readFrameViewH2 returned via the
	// pendingFrame replay path at the TOP of its loop (frame's bytes
	// were already heap-copied in a prior iteration). This bypasses
	// the ZC fast path entirely. Population indicates a prior DATA
	// frame produced leftover bytes (slow-path multi-LPM-in-frame).
	shmZCFailPendingFrame uint64
)

// BeginMultiAnchor claims a single-frame ZC slot in the FIFO. On
// success returns (seq, true) where `seq` identifies the slot for the
// matching ReleaseMultiAnchor call; on FIFO full returns (0, false)
// and the caller falls back to the single-frame copy path.
//
// The returned `seq` is a plain uint64 — no heap allocation is
// performed per ZC frame. Callers store it in their per-frame
// release-pool struct (which is itself sync.Pool'd, see
// zc_multi_release.go), keeping the steady-state allocation count for
// the ZC anchor itself at zero.
//
// `start` is the absolute ring offset of the body's first byte —
// typically the caller's `commitPayload.bodyEndIdx-payloadLen`, the
// post-frame-header position captured by ReadSlices. `payloadLen` is
// the body byte count. The held byte range is [start, start+payloadLen);
// drainReleasedAnchorPrefix uses these bounds to advance header.ReadIdx
// when the anchor is released. Caller is responsible for the geometric
// eligibility checks (IsMultiAnchorZCEligible).
//
// Concurrency:
//   - Single producer per ring (the reader goroutine running
//     readFrameViewH2). No CAS on tail.
//   - Releases run from arbitrary consumer goroutines and race the
//     prefix-walk's head CAS (drainReleasedAnchorPrefix).
//
// Store ordering matters for correctness against the drain
// goroutine's zcActive-clear path. See drainReleasedAnchorPrefix
// for the matching protocol. The rules here:
//
//  1. atomic Store slot.start / slot.end (atomic because a stale
//     drainPrefix walker on a later wrap-cycle can race-read these;
//     see anchorSlot's type comment).
//  2. CAS-forward zcDeferredTarget to ≥ end so a drain whose head==tail
//     case publishes through to our end (no in-flight successor anchor
//     exists yet).
//  3. atomic Store slot.state = 1 (publishes start/end to drain).
//  4. atomic Store anchorTail = seq+1 (publishes slot existence to drain).
//  5. atomic Store zcActive = 1.
//
// Step 4 BEFORE step 5 is the contract drain relies on: drain may
// race Store(zcActive=0) between any two steps; its re-check of
// anchorTail after the Store(0) observes step 4 and restores
// zcActive=1, restoring the deferred-Commit invariant.
func (r *ShmRing) BeginMultiAnchor(start uint64, payloadLen int) (uint64, bool) {
	if payloadLen <= 0 {
		return 0, false
	}
	seq := r.anchorTail.Load()
	slot := &r.anchorSlots[seq%zcAnchorBudgetCount]
	if slot.state.Load() != 0 {
		// Slot still released-but-not-freed OR all slots in flight.
		// Either way the FIFO position is occupied. Caller falls back
		// to the single-frame copy path.
		atomic.AddUint64(&shmZCAnchorBudgetExceeded, 1)
		return 0, false
	}

	end := start + uint64(payloadLen)
	slot.start.Store(start)
	slot.end.Store(end)

	// CAS-forward zcDeferredTarget to ≥ end. Begin runs in the single
	// reader goroutine, but drainPrefix in consumer goroutines may
	// concurrently read zcDeferredTarget for its "head==tail publish"
	// case. CAS loop ensures monotonic forward progress without locks
	// and tolerates intervening Commit-deferred bumps.
	for {
		cur := atomic.LoadUint64(&r.zcDeferredTarget)
		if cur >= end {
			break
		}
		if atomic.CompareAndSwapUint64(&r.zcDeferredTarget, cur, end) {
			break
		}
	}

	// Publish slot state. atomic.Uint32.Store is sequentially
	// consistent in Go, so the plain stores above (slot.start, end)
	// happen-before any drain Load that observes state>=1.
	slot.state.Store(1)

	// Publish tail BEFORE zcActive so a racing drain whose
	// Store(zcActive=0) interleaves between our anchorTail.Store and
	// zcActive.Store will, on its re-load of anchorTail, see seq+1 >
	// cur and restore zcActive=1.
	r.anchorTail.Store(seq + 1)

	// Activate the Commit deferred path so intervening non-ZC commits
	// while we hold the anchor don't prematurely advance header.ReadIdx
	// past our held range.
	atomic.StoreUint32(&r.zcActive, 1)

	return seq, true
}

// ReleaseMultiAnchor marks the anchor's slot released (state 1 → 2)
// and triggers a prefix-walk. If this anchor was the oldest in
// flight, the walk advances header.ReadIdx through the contiguous
// released prefix.
//
// Safe to call from any goroutine. Multiple concurrent Release calls
// race the prefix-walk's head CAS; each loser retries with the new
// head, so all releasable bytes get published exactly once.
//
// Calling twice on the same seq is a logic bug but not a memory-safety
// bug — the second Store(2) is a no-op (already 2), the drainPrefix
// loop sees the same state.
func (r *ShmRing) ReleaseMultiAnchor(seq uint64) {
	if r == nil {
		return
	}
	slot := &r.anchorSlots[seq%zcAnchorBudgetCount]
	slot.state.Store(2)
	r.drainReleasedAnchorPrefix()
}

// drainReleasedAnchorPrefix walks the anchor FIFO from head. For each
// anchor whose state is "released" (2), the walker:
//
//  1. Determine the publish target:
//     - If newer anchors exist: publish to the next anchor's start.
//       This covers the released anchor's body PLUS any intervening
//       non-ZC bytes that were Commit-deferred into zcDeferredTarget.
//     - Else: publish to zcDeferredTarget (the running tip).
//  2. publishTarget (CAS-forward header.ReadIdx).
//  3. CAS-advance head (only one walker wins per slot).
//  4. Mark slot free (state 2 → 0) so Begin can reclaim it.
//  5. Wake space/contig waiters on the writer side.
//
// Publish BEFORE CAS-head: ensures that a racing Begin observing
// head==tail (next-iteration empty FIFO) cannot see a stale
// header.ReadIdx that lags zcDeferredTarget.
//
// zcActive clear protocol (matches BeginMultiAnchor's store order):
// when the loop top observes head==tail (FIFO empty), we
//   (a) Store(zcActive=0),
//   (b) Load(anchorTail).
// If the load observes a value > cur, a concurrent Begin slipped a
// new anchor in between (a) and (b). Restore zcActive=1 immediately;
// the next drain (triggered by that anchor's Release) will retry the
// clear. Begin's store order — Store(anchorTail) BEFORE
// Store(zcActive=1) — guarantees this protocol cannot leave
// zcActive=0 with an anchor in flight (which would let a subsequent
// Commit advance header.ReadIdx past the anchor's start and let the
// cross-process writer overwrite held bytes).
//
// MPMC-safe: head advance is a CAS loop. publishTarget is also a
// CAS loop. Concurrent walkers converge to the correct final state
// without locks.
func (r *ShmRing) drainReleasedAnchorPrefix() {
	if atomic.LoadUint32(&r.closed) != 0 {
		return
	}
	hdr := r.header()
	for {
		cur := r.anchorHead.Load()
		tail := r.anchorTail.Load()
		if cur >= tail {
			// FIFO empty. Clear zcActive, then re-load tail to catch
			// a racing Begin that incremented tail between our two
			// initial Loads.
			//
			// If a Begin slipped in, restore zcActive=1 and return
			// WITHOUT publishing: zcDeferredTarget has been CAS-forwarded
			// to the new anchor's end, so publishing through it would
			// advance header.ReadIdx past the new anchor's start and
			// allow the cross-process writer to overwrite the new
			// anchor's held bytes.
			//
			// If no Begin raced, publish zcDeferredTarget so that any
			// Commit-deferred bytes accumulated during the previous
			// anchor cycle drain through to header.ReadIdx. We
			// (zcActive=0) and Commit (reader-goroutine, same as Begin
			// so cannot race us here) converge correctly: subsequent
			// Commits run direct and advance header.ReadIdx themselves;
			// publishTarget is monotonic so the order is irrelevant.
			atomic.StoreUint32(&r.zcActive, 0)
			if r.anchorTail.Load() > cur {
				atomic.StoreUint32(&r.zcActive, 1)
				return
			}
			r.publishTarget(hdr, atomic.LoadUint64(&r.zcDeferredTarget))
			return
		}
		slot := &r.anchorSlots[cur%zcAnchorBudgetCount]
		if slot.state.Load() != 2 {
			return
		}
		// Compute publish target.
		newCur := cur + 1
		var target uint64
		if newCur < tail {
			// Next anchor exists. tail.Load() observing >= newCur+1
			// (i.e. tail > newCur) implies Begin's anchorTail.Store
			// happened-before our Load — by transitivity, Begin's
			// plain Stores to nextSlot.start / end are visible.
			nextSlot := &r.anchorSlots[newCur%zcAnchorBudgetCount]
			target = nextSlot.start.Load()
		} else {
			// We are freeing the last in-flight anchor. Publish through
			// to the running deferred-target tip (covers any commits
			// that happened-after this anchor's Begin).
			target = atomic.LoadUint64(&r.zcDeferredTarget)
		}
		// Publish BEFORE advancing head: ensures the next loop
		// iteration (or a fresh drain on the same goroutine) sees
		// header.ReadIdx ≥ target by the time head moves past this
		// slot, eliminating the stale-Read race window for any caller
		// that observes head==tail after our CAS.
		r.publishTarget(hdr, target)
		if !r.anchorHead.CompareAndSwap(cur, cur+1) {
			continue // lost race; retry from the new head
		}
		// Free the slot (Begin may now reclaim it).
		slot.state.Store(0)
		// Wake space/contig waiters on the writer side.
		if hdr.ContigWaiters() > 0 {
			hdr.IncrementContigSequence()
			r.signalContig(&hdr.contigSeq)
		}
		if hdr.SpaceWaiters() > 0 {
			hdr.IncrementSpaceSequence()
			r.signalSpace(&hdr.spaceSeq)
		}
	}
}

// IsMultiAnchorZCEligible is the geometric eligibility check for the
// multi-anchor ZC fast path. Unlike IsSpeculativeZCEligible (which
// also enforces the at-most-one-ZC gate via zcActive), this method
// has NO concurrency-gate check — the anchor budget is enforced
// authoritatively by BeginMultiAnchor returning nil when all slots
// are in use, which is the correct cap for our lock-free FIFO.
//
// Conditions retained from IsSpeculativeZCEligible:
//   - payload is contiguous (no ring wrap; the H2 codec only attempts
//     ZC when len(pSecond) == 0)
//   - ring capacity ≥ 1 MiB (avoids ZC freezing a measurable fraction
//     of a tiny ring)
//   - payload ≥ 4 KiB (below this the per-anchor bookkeeping cost
//     reliably loses to a direct mem.Copy of one page)
//   - back-pressure self-disable: ring > 75 % full deactivates ZC so
//     held bytes do not stall the writer
func (r *ShmRing) IsMultiAnchorZCEligible(payloadLength int, contiguous bool) bool {
	if !contiguous {
		atomic.AddUint64(&shmZCElig_NotContig, 1)
		return false
	}
	const minRingForZC = uint64(1) << 20 // 1 MiB
	if r.capacity < minRingForZC {
		atomic.AddUint64(&shmZCElig_RingTooSmall, 1)
		return false
	}
	if payloadLength < 4*1024 {
		atomic.AddUint64(&shmZCElig_PayloadSmall, 1)
		return false
	}
	hdr := r.header()
	used := hdr.WriteIndex() - hdr.ReadIndex()
	if used*4 > r.capacity*3 {
		atomic.AddUint64(&shmZCElig_BackPressure, 1)
		return false
	}
	return true
}
