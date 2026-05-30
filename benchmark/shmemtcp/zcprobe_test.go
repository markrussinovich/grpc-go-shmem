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

package main

import (
	"os"
	"testing"

	"google.golang.org/grpc/internal/transport"
)

// SHM_BENCH_ZC=1 enables per-iteration reporting of which write/read
// code path actually ran. Answers reviewer questions like "is 1 MiB
// actually going through zero-copy on the send side?" with hard
// numbers instead of speculation.
//
// Metrics reported per iteration (only the non-zero ones):
//
//   zc-write/op      — proto marshalled directly into ring (true ZC send)
//   vec-write/op     — vectored fast path (hdr+data into ring, no
//                      heap materialisation; second-best after ZC)
//   chunked-write/op — multi-DATA-frame path (one copy per chunk)
//   skip-quota/op    — ZC write skipped due to flow control window
//   skip-inline/op   — ZC write skipped because frameWriter was busy
//   skip-frame/op    — ZC write skipped due to shmMaxFrameSize gate
//   skip-space/op    — ZC write skipped due to ring contiguous-space
//   skip-budget/op   — ZC write skipped due to cap/3 single-frame budget
//   zc-read/op       — receiver got ring-backed buffer (true ZC recv)
//   copy-read/op     — receiver got pool-copied buffer (one memcpy)
//   acc-read/op      — receiver routed through lpmAccumulator (multi-
//                      DATA-frame or fallback)
//
// Each metric counts events across BOTH directions of the ping-pong
// (so a typical 1-message-per-iteration RPC reports values close to 2
// for the dominant path).

var zcProbeEnabled = os.Getenv("SHM_BENCH_ZC") == "1"

// startZCProbe captures a snapshot of the SHM path counters. The
// returned closure computes the delta and reports per-op rates via
// b.ReportMetric. No-op when SHM_BENCH_ZC != "1".
//
// Call AFTER b.ResetTimer() and the returned closure AFTER b.StopTimer
// to scope the snapshot to exactly the timed loop.
func startZCProbe(b *testing.B) func() {
	if !zcProbeEnabled {
		return func() {}
	}
	before := transport.LoadShmPathCounters()
	beforeDS := transport.LoadShmDataSegWakeCounters()
	return func() {
		if b.N <= 0 {
			return
		}
		delta := transport.LoadShmPathCounters().Sub(before)
		dsDelta := transport.LoadShmDataSegWakeCounters().Sub(beforeDS)
		n := float64(b.N)
		report := func(name string, v uint64) {
			if v == 0 {
				return
			}
			b.ReportMetric(float64(v)/n, name)
		}
		report("zc-write/op", delta.ZCWriteFire)
		report("vec-write/op", delta.VectoredWriteFire)
		report("chunked-write/op", delta.ChunkedWriteFire)
		report("chunked-write-vec/op", delta.ChunkedWriteVecFire)
		report("skip-quota/op", delta.ZCWriteSkipQuota)
		report("skip-inline/op", delta.ZCWriteSkipInlineBusy)
		report("skip-frame/op", delta.ZCWriteSkipMaxFrame)
		report("skip-space/op", delta.ZCWriteSkipSpace)
		report("skip-budget/op", delta.ZCWriteSkipBudget)
		report("zc-read/op", delta.ZCReadFire)
		report("copy-read/op", delta.CopyReadFire)
		report("acc-read/op", delta.AccReadFire)
		report("zc-anchor-budget/op", delta.ZCAnchorBudgetExceeded)
		report("zc-fail-wrap/op", delta.ZCFailPSecondNonzero)
		report("zc-fail-shorthdr/op", delta.ZCFailPFirstShort)
		report("zc-fail-accinprogress/op", delta.ZCFailAccInProgress)
		report("zc-fail-lpmmismatch/op", delta.ZCFailLpmMismatch)
		report("zc-fail-ineligible/op", delta.ZCFailIneligible)
		report("zc-elig-bp/op", delta.ZCEligBackPressure)
		report("zc-elig-smallpl/op", delta.ZCEligPayloadSmall)
		report("zc-elig-smallring/op", delta.ZCEligRingTooSmall)
		report("zc-elig-notcontig/op", delta.ZCEligNotContig)
		// Inline-write fast-path counters: emit single-frame whole-
		// message DATA directly from the sender goroutine, bypassing
		// the channel + writer-goroutine handoff. Bails dominate at
		// high concurrency (preserves existing batching win); fires
		// dominate at low concurrency (closes the goroutine-handoff
		// latency gap to UDS).
		report("inline-write-fire/op", delta.InlineWriteFire)
		report("inline-write-bail-locked/op", delta.InlineWriteBailLocked)
		report("inline-write-bail-closed/op", delta.InlineWriteBailClosed)
		report("inline-write-bail-streamdone/op", delta.InlineWriteBailStreamDone)
		report("inline-write-bail-ctxdone/op", delta.InlineWriteBailCtxDone)
		report("inline-write-bail-queued/op", delta.InlineWriteBailQueued)
		report("inline-write-bail-quota/op", delta.InlineWriteBailQuota)
		report("inline-write-bail-frame/op", delta.InlineWriteBailFrameSize)
		report("inline-write-bail-zero/op", delta.InlineWriteBailZeroLen)
		// inline-piggyback-drain/op: frames a tryInlineWrite holder
		// drained from w.ch (bounded ≤8) before releasing inlineMu.
		// Amortises writer-goroutine cycles. High at high concurrency
		// = piggyback working; near-zero at low concurrency = chan
		// empty (no work to amortise).
		report("inline-piggyback-drain/op", delta.InlinePiggybackDrain)
		// Per-data-segment socketpair waker diagnostics (zero on
		// non-Linux / when the eventfd waker is disabled).
		report("ds-wake/op", dsDelta.WakeCallsTotal)
		report("ds-wake-sys/op", dsDelta.WakeSyscalls)
		report("ds-wait/op", dsDelta.WaitCallsTotal)
		report("ds-wait-sys/op", dsDelta.WaitSyscalls)
		report("ds-wait-nil/op", dsDelta.WaitReturnNil)
		report("ds-wait-timeout/op", dsDelta.WaitReturnTimeout)
		report("ds-wait-closed/op", dsDelta.WaitReturnClosed)
		report("ds-wait-eof/op", dsDelta.WaitReturnEOF)
		report("ds-wait-other/op", dsDelta.WaitReturnOther)
		report("ds-rewake/op", dsDelta.RewakeLocal)
		report("ds-fanout-bail/op", dsDelta.FanOutBailout)
	}
}
