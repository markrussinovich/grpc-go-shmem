//go:build linux || windows

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

package transport

import (
	"testing"
)

// TestTrInFlow_MaybeAdjust_NoOpWhenWithinWindow asserts that
// maybeAdjust returns 0 when the requested message fits within the
// peer's currently-estimated conn quota, and leaves all internal
// counters unchanged. This is the common case for small messages on
// a healthy connection.
func TestTrInFlow_MaybeAdjust_NoOpWhenWithinWindow(t *testing.T) {
	var f trInFlow
	f.limit = 65535
	f.updateEffectiveWindowSize()

	got := f.maybeAdjust(1024)
	if got != 0 {
		t.Errorf("maybeAdjust(1024) with empty 65535 window = %d, want 0", got)
	}
	if f.delta != 0 {
		t.Errorf("delta = %d after no-op maybeAdjust, want 0", f.delta)
	}
	if f.unacked != 0 {
		t.Errorf("unacked = %d after no-op maybeAdjust, want 0", f.unacked)
	}
}

// TestTrInFlow_MaybeAdjust_PrecreditDebtTracked asserts that
// maybeAdjust(n) when n > available conn window:
//   - returns the exact shortfall increment
//   - records the increment in `delta`
//   - updates the effective window so future maybeAdjust calls see it
func TestTrInFlow_MaybeAdjust_PrecreditDebtTracked(t *testing.T) {
	var f trInFlow
	f.limit = 65535
	f.updateEffectiveWindowSize()

	const messageSize uint32 = 1 << 20 // 1 MiB
	got := f.maybeAdjust(messageSize)
	want := messageSize - 65535
	if got != want {
		t.Errorf("maybeAdjust(1 MiB) on 65535 window = %d, want %d", got, want)
	}
	if f.delta != want {
		t.Errorf("delta = %d after precredit, want %d", f.delta, want)
	}
	// A second maybeAdjust for the SAME message should now be a no-op
	// because the peer's quota was already inflated.
	got2 := f.maybeAdjust(messageSize)
	if got2 != 0 {
		t.Errorf("maybeAdjust repeated on inflated window = %d, want 0", got2)
	}
}

// TestTrInFlow_MaybeAdjust_CappedAtMaxWindowSize asserts that
// maybeAdjust never inflates the peer's view of the conn window
// past the HTTP/2 31-bit max-window-size ceiling. The HTTP/2 spec
// requires sender's window to fit in a signed 32-bit integer; if
// SHM ever pre-credited past that point the peer would have to
// reject the WINDOW_UPDATE with FLOW_CONTROL_ERROR.
func TestTrInFlow_MaybeAdjust_CappedAtMaxWindowSize(t *testing.T) {
	var f trInFlow
	f.limit = maxWindowSize - 1024
	f.updateEffectiveWindowSize()

	// Request a message huge enough to want past the cap.
	got := f.maybeAdjust(maxWindowSize)
	if got > 1024 {
		t.Errorf("maybeAdjust returned %d, want <=1024 (room to cap)", got)
	}
	// Estimated peer quota must equal exactly maxWindowSize now.
	est := uint64(f.limit) + uint64(f.delta) - uint64(f.unacked)
	if est != uint64(maxWindowSize) {
		t.Errorf("estimated peer quota=%d after cap, want %d", est, maxWindowSize)
	}
	// Another huge request should now return 0 (no room left).
	got2 := f.maybeAdjust(maxWindowSize)
	if got2 != 0 {
		t.Errorf("maybeAdjust at cap = %d, want 0", got2)
	}
}

// TestTrInFlow_OnData_DebtFirstThenUnacked asserts that bytes
// received after a pre-credit are first applied to repay the
// `delta` debt, and only the EXCESS counts towards the limit/4
// drip-credit threshold. This avoids the double-credit failure
// mode where the same 1 MiB message would trigger pre-credit AND
// 64x drip WUs (~2 MiB total credit emitted for 1 MiB consumed,
// silently inflating the peer's conn window each ping-pong).
//
// The exact behaviour for 1 MiB inbound on a 65535 baseline:
//
//   pre-credit (maybeAdjust)      = 1 MiB - 65535     = 983041 bytes
//   delta absorbs first 983041 bytes of onData
//   remaining 65535 bytes flow through ordinary unacked accounting
//     emitting drip WUs every limit/4 = 16383 bytes
//   total emitted WU = 983041 + ~65535 = ~1 MiB == bytes consumed
//
// Net: total credit returned to peer over the message lifetime equals
// the bytes consumed (so the peer's conn window settles back to the
// baseline 65535 after the message completes, exactly preserving
// HTTP/2 conn-FC semantics).
func TestTrInFlow_OnData_DebtFirstThenUnacked(t *testing.T) {
	var f trInFlow
	f.limit = 65535
	f.updateEffectiveWindowSize()

	const lpmSize uint32 = 1 << 20 // 1 MiB
	pre := f.maybeAdjust(lpmSize)
	wantPre := lpmSize - 65535
	if pre != wantPre {
		t.Fatalf("maybeAdjust(1 MiB) on 65535 window = %d, want %d", pre, wantPre)
	}

	// Simulate the 1 MiB DATA arriving in 16 KiB chunks.
	const chunk uint32 = 16384
	chunks := lpmSize / chunk
	if r := lpmSize - chunks*chunk; r != 0 {
		t.Fatalf("test invariant: lpmSize=%d chunk=%d, remainder=%d", lpmSize, chunk, r)
	}
	var totalReturned uint32
	for i := uint32(0); i < chunks; i++ {
		w := f.onData(chunk)
		totalReturned += w
	}

	// Total credit returned to peer = pre-credit + drip WUs == bytes
	// consumed. That preserves the HTTP/2 invariant that the peer's
	// conn window settles back to baseline after a complete message.
	totalCredit := pre + totalReturned
	if totalCredit != lpmSize {
		t.Errorf("total credit returned to peer = %d (pre=%d + drip=%d), want %d (== bytes consumed)",
			totalCredit, pre, totalReturned, lpmSize)
	}
	if f.delta != 0 {
		t.Errorf("delta = %d after full message consumed, want 0", f.delta)
	}
	// unacked should be < limit/4 (any residual after last reset).
	if f.unacked >= f.limit/4 {
		t.Errorf("unacked = %d should be below limit/4=%d after final reset", f.unacked, f.limit/4)
	}
}

// TestTrInFlow_OnData_NoDripDuringPureDebtConsumption asserts that
// when a message body is ENTIRELY absorbed by debt (i.e. the message
// would have fit in baseline + debt without exceeding either), no
// drip WUs fire mid-message. This is the small-message-with-pre-credit
// case where we DON'T want to slap a drip WU on top of a tiny
// overage.
func TestTrInFlow_OnData_NoDripDuringPureDebtConsumption(t *testing.T) {
	var f trInFlow
	f.limit = 65535
	f.updateEffectiveWindowSize()

	// Artificially set a large debt to simulate an in-flight large
	// message whose pre-credit hasn't been fully consumed yet. With
	// limit=65535 maybeAdjust caps at maxWindowSize-65535 worth of
	// debt for a single call; for the test we just write directly.
	f.delta = 100_000

	// Send 50 KiB. Debt should absorb everything; unacked stays 0; no WU.
	if w := f.onData(50000); w != 0 {
		t.Errorf("onData(50K) under 100K debt returned %d, want 0", w)
	}
	if f.delta != 50000 {
		t.Errorf("delta = %d after 50K consumed from 100K debt, want 50000", f.delta)
	}
	if f.unacked != 0 {
		t.Errorf("unacked = %d during pure-debt consumption, want 0", f.unacked)
	}
}

// TestTrInFlow_OnData_PartialDebtRepay asserts the boundary case
// where the incoming DATA exceeds the outstanding debt: the
// remainder of the data must cross into ordinary unacked
// accounting, and a drip WU must still be eligible to fire.
func TestTrInFlow_OnData_PartialDebtRepay(t *testing.T) {
	var f trInFlow
	f.limit = 65535
	// Synthesise a small debt of 8 KiB.
	f.delta = 8192
	f.updateEffectiveWindowSize()

	// Send 24 KiB of data: 8 KiB repays delta, 16 KiB counts as
	// unacked. 16384 == limit/4 (rounded down 65535/4 = 16383)? In
	// fact limit/4 = 16383, so 16384 unacked WILL cross the
	// threshold and emit a 16384 WU; let's assert it.
	w := f.onData(24576)
	if w == 0 {
		t.Errorf("onData(24K) with 8K debt: expected drip WU after 16K excess, got 0")
	}
	if w != 16384 {
		t.Errorf("onData drip WU = %d, want exactly 16384 (the excess past 8K debt)", w)
	}
	if f.delta != 0 {
		t.Errorf("delta = %d after onData, want 0", f.delta)
	}
	if f.unacked != 0 {
		t.Errorf("unacked = %d after reset, want 0", f.unacked)
	}
}

// TestTrInFlow_OnData_BelowThresholdAccumulates asserts ordinary
// limit/4 drip-credit batching still works when there is NO
// outstanding debt — the unified path must not break the baseline
// HTTP/2 conn FC behaviour for the no-pre-credit case (which is
// the common case for small messages).
func TestTrInFlow_OnData_BelowThresholdAccumulates(t *testing.T) {
	var f trInFlow
	f.limit = 65535
	f.updateEffectiveWindowSize()

	// 8 KiB << limit/4 = 16383, no WU expected.
	if w := f.onData(8192); w != 0 {
		t.Errorf("onData(8K) below threshold returned %d, want 0", w)
	}
	if f.unacked != 8192 {
		t.Errorf("unacked = %d after 8K, want 8192", f.unacked)
	}
	// Another 8K crosses the threshold.
	if w := f.onData(8192); w != 16384 {
		t.Errorf("onData second 8K (crossing threshold) returned %d, want 16384", w)
	}
	if f.unacked != 0 {
		t.Errorf("unacked = %d after drip emission, want 0", f.unacked)
	}
}

// TestTrInFlow_NewLimit_PreservesDebt asserts that a BDP-driven
// window growth (newLimit on the conn-level inFlow, called from
// updateFlowControl) does NOT silently drop or duplicate an
// outstanding pre-credit debt. The peer's effective conn window
// after both events should be limit_new + delta - unacked, not
// just limit_new - unacked.
func TestTrInFlow_NewLimit_PreservesDebt(t *testing.T) {
	var f trInFlow
	f.limit = 65535
	f.updateEffectiveWindowSize()

	const lpmSize uint32 = 1 << 19 // 512 KiB
	pre := f.maybeAdjust(lpmSize)
	if pre == 0 {
		t.Fatalf("maybeAdjust returned 0, expected precredit")
	}
	debtBefore := f.delta
	if debtBefore == 0 {
		t.Fatalf("delta=0 after maybeAdjust, want non-zero")
	}

	// BDP grows the conn window to 4 MiB. The returned WU delta
	// is the baseline-window growth; debt is unaffected.
	wu := f.newLimit(4 * 1024 * 1024)
	if wu == 0 {
		t.Errorf("newLimit returned 0, want non-zero WU delta")
	}
	if f.delta != debtBefore {
		t.Errorf("delta = %d after newLimit, want %d (preserved)", f.delta, debtBefore)
	}
	// Effective window must reflect new limit + preserved debt.
	want := uint64(4*1024*1024) + uint64(debtBefore)
	if want > uint64(maxWindowSize) {
		want = uint64(maxWindowSize)
	}
	if got := uint64(f.getSize()); got != want {
		t.Errorf("effectiveWindowSize = %d after newLimit + debt, want %d", got, want)
	}
}

// TestTrInFlow_UpdateEffectiveWindowSize_NoUnderflow guards against
// the underflow trap when limit shrinks below unacked (which can
// happen transiently during BDP shrink — rare but defensible).
// With saturating uint64 math we should observe 0, never wrap.
func TestTrInFlow_UpdateEffectiveWindowSize_NoUnderflow(t *testing.T) {
	var f trInFlow
	f.limit = 1024
	f.unacked = 10240 // unacked > limit
	f.updateEffectiveWindowSize()
	if got := f.getSize(); got != 0 {
		t.Errorf("effectiveWindowSize with unacked > limit = %d, want 0 (saturating)", got)
	}
}
