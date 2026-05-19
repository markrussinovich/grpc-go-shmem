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
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestDataSegWaker_PingPong verifies basic two-direction routing:
// A wakes B, B wakes A. Each side has its own eventfd-for-reading;
// peer wakes by writing to it. No fan-out, no spurious wakes.
func TestDataSegWaker_PingPong(t *testing.T) {
	a, b, err := newShmDataSegWakerPair()
	if err != nil {
		t.Fatalf("newShmDataSegWakerPair: %v", err)
	}
	defer a.Close()
	defer b.Close()

	// A wakes B, B's Wait returns.
	done := make(chan error, 1)
	go func() { _, err := b.Wait(2 * time.Second); done <- err }()
	time.Sleep(10 * time.Millisecond)
	a.Wake()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("B.Wait after A.Wake returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B.Wait did not return after A.Wake")
	}

	// B wakes A.
	done2 := make(chan error, 1)
	go func() { _, err := a.Wait(2 * time.Second); done2 <- err }()
	time.Sleep(10 * time.Millisecond)
	b.Wake()
	select {
	case err := <-done2:
		if err != nil {
			t.Fatalf("A.Wait after B.Wake returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("A.Wait did not return after B.Wake")
	}
}

// TestDataSegWaker_WakesIsolatedByDirection verifies that A.Wake
// goes ONLY to B (does not wake A itself), and vice versa.
//
// This is the critical property that makes per-direction eventfds
// route correctly: a wake from one side is only delivered to the
// other side, never echoes back to the writer.
func TestDataSegWaker_WakesIsolatedByDirection(t *testing.T) {
	a, b, err := newShmDataSegWakerPair()
	if err != nil {
		t.Fatalf("newShmDataSegWakerPair: %v", err)
	}
	defer a.Close()
	defer b.Close()

	// A.Wake should NOT wake a goroutine parked on A.Wait. It should
	// only wake B. Park a goroutine on A.Wait with a short timeout
	// and verify it times out (A.Wake doesn't affect A).
	done := make(chan error, 1)
	go func() { _, err := a.Wait(200 * time.Millisecond); done <- err }()
	time.Sleep(10 * time.Millisecond)

	a.Wake() // Should go to B's counter, not A's.

	// Drain B's counter so the test cleanup doesn't leak.
	defer func() {
		_, _ = b.Wait(time.Second)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrFutexTimeout) {
			t.Fatalf("A.Wait after A.Wake should have timed out (A→A is not a route); got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("A.Wait did not return within timeout window")
	}
}

// TestDataSegWaker_WaitForChange_HappyPath verifies the cascade
// helper returns nil when the condition is met after one wake.
func TestDataSegWaker_WaitForChange_HappyPath(t *testing.T) {
	a, b, err := newShmDataSegWakerPair()
	if err != nil {
		t.Fatalf("newShmDataSegWakerPair: %v", err)
	}
	defer a.Close()
	defer b.Close()

	var addr uint32 = 42

	done := make(chan error, 1)
	go func() { done <- b.WaitForChange(&addr, 42, 2*time.Second) }()
	time.Sleep(10 * time.Millisecond)

	// Producer changes the addr and wakes.
	atomic.StoreUint32(&addr, 43)
	a.Wake()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForChange returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForChange did not return after Wake")
	}
}

// TestDataSegWaker_WaitForChange_SpuriousFanOut verifies the cascade
// behaviour: two goroutines park on the SAME side, each watching a
// DIFFERENT addr. A single Wake delivers the wake to ONE of them via
// kernel netpoll; the wrong one (whose addr didn't change) re-issues
// a local wake to give the other one a chance.
//
// Without the RewakeLocal cascade, the wrong waiter would consume
// the wake, see its addr unchanged, re-park, and the correct waiter
// would block indefinitely (until producer signals again). With the
// cascade, the wake is propagated until the correct waiter consumes
// it.
func TestDataSegWaker_WaitForChange_SpuriousFanOut(t *testing.T) {
	a, b, err := newShmDataSegWakerPair()
	if err != nil {
		t.Fatalf("newShmDataSegWakerPair: %v", err)
	}
	defer a.Close()
	defer b.Close()

	// Two B-side waiters, watching two distinct addrs.
	var addrData uint32 = 100  // would-be ring.dataSeq
	var addrSpace uint32 = 200 // would-be ring.spaceSeq

	// Goroutine 1: waits for addrData to change (it WON'T -- producer
	// will change addrSpace instead).
	dataReturned := make(chan error, 1)
	go func() {
		// Use a 1-second cap so a buggy cascade-loss can't hang the
		// test forever; the cap is generous enough that a working
		// cascade returns long before it.
		dataReturned <- b.WaitForChange(&addrData, 100, time.Second)
	}()

	// Goroutine 2: waits for addrSpace to change.
	spaceReturned := make(chan error, 1)
	go func() {
		spaceReturned <- b.WaitForChange(&addrSpace, 200, time.Second)
	}()

	// Give both goroutines time to park.
	time.Sleep(20 * time.Millisecond)

	// Producer changes ONLY addrSpace and wakes.
	atomic.StoreUint32(&addrSpace, 201)
	a.Wake()

	// The cascade should route the wake to G2 (the addrSpace waiter)
	// within a few RewakeLocal hops. G1 should still be parked
	// (addrData unchanged).
	select {
	case err := <-spaceReturned:
		if err != nil {
			t.Fatalf("space waiter returned %v after Wake; expected nil (condition met)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("space waiter never returned -- cascade lost the wake?")
	}

	// G1 should still be parked. Verify by waiting for its timeout.
	select {
	case err := <-dataReturned:
		// G1 may bail out from WaitForChange after maxFanOutRetries
		// with nil err and addrData unchanged. That's an acceptable
		// non-deadlock outcome -- the caller's outer ring loop would
		// re-park.
		t.Logf("data waiter returned early with %v (cascade exhaustion is OK)", err)
		// addrData should still be unchanged.
		if got := atomic.LoadUint32(&addrData); got != 100 {
			t.Errorf("addrData unexpectedly changed: %d", got)
		}
	case <-time.After(1100 * time.Millisecond):
		// G1's 1-second timeout fired. That's the canonical "wake
		// was for someone else" outcome.
		t.Log("data waiter timed out as expected (the Wake was not for it)")
	}
}

// TestDataSegWaker_WaitForChange_SoloParkerNoSelfSpin verifies that
// a solo parker (parkers == 1) which receives a spurious wake (peer
// wakes without changing addr) does NOT cascade RewakeLocal -- if
// it did, the caller's outer-retry loop would read the self-written
// counter and self-spin indefinitely. The parker-count gate inside
// WaitForChange ensures the wrong-parker cascade only fires when
// another parker is actually present.
func TestDataSegWaker_WaitForChange_SoloParkerNoSelfSpin(t *testing.T) {
	a, b, err := newShmDataSegWakerPair()
	if err != nil {
		t.Fatalf("newShmDataSegWakerPair: %v", err)
	}
	defer a.Close()
	defer b.Close()

	var addr uint32 = 7

	done := make(chan error, 1)
	go func() {
		// Use 2 s timeout so a buggy self-spin would be caught
		// (we expect immediate return after a single Wait+condition
		// check, with no RewakeLocal because parkers == 1).
		done <- b.WaitForChange(&addr, 7, 2*time.Second)
	}()
	time.Sleep(10 * time.Millisecond)

	before := LoadShmDataSegWakeCounters()

	// Peer wakes WITHOUT changing addr (the "addr-unchanged spurious"
	// case). With the parker-count gate, B's WaitForChange should
	// return immediately (one Wait, no RewakeLocal, bail counter
	// incremented).
	a.Wake()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		after := LoadShmDataSegWakeCounters()
		delta := after.Sub(before)
		t.Logf("solo wrong-parker handled: rewake=%d bailout=%d",
			delta.RewakeLocal, delta.FanOutBailout)
		if delta.RewakeLocal != 0 {
			t.Errorf("solo parker should NOT RewakeLocal (would self-spin caller); got %d",
				delta.RewakeLocal)
		}
		if delta.FanOutBailout != 1 {
			t.Errorf("expected exactly 1 FanOutBailout; got %d", delta.FanOutBailout)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitForChange never returned -- self-spin?")
	}
}

// TestDataSegWaker_RewakeLocal_WakesOwnSide verifies the local
// re-wake primitive: writing to OUR own fd wakes a goroutine parked
// on OUR Wait, not the peer's. This is what makes the fan-out
// cascade work.
func TestDataSegWaker_RewakeLocal_WakesOwnSide(t *testing.T) {
	a, b, err := newShmDataSegWakerPair()
	if err != nil {
		t.Fatalf("newShmDataSegWakerPair: %v", err)
	}
	defer a.Close()
	defer b.Close()

	done := make(chan error, 1)
	go func() { _, err := a.Wait(2 * time.Second); done <- err }()
	time.Sleep(10 * time.Millisecond)

	a.RewakeLocal() // Wakes A's parked Wait, NOT B's.

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("A.Wait returned %v after A.RewakeLocal, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("A.Wait did not return after A.RewakeLocal")
	}

	// Verify B is NOT affected: park B with a short timeout, expect
	// it to time out (no wake should have reached B).
	bdone := make(chan error, 1)
	go func() { _, err := b.Wait(150 * time.Millisecond); bdone <- err }()
	select {
	case err := <-bdone:
		if !errors.Is(err, ErrFutexTimeout) {
			t.Fatalf("B.Wait should time out (RewakeLocal is local-only); got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("B.Wait never returned")
	}
}

// TestDataSegWaker_ConcurrentStress mirrors the race-detector
// stress test: two goroutines, one Waking, one Waiting, duration-
// bounded, just verify no panic / no data race / no hang.
func TestDataSegWaker_ConcurrentStress(t *testing.T) {
	a, b, err := newShmDataSegWakerPair()
	if err != nil {
		t.Fatalf("newShmDataSegWakerPair: %v", err)
	}
	defer a.Close()
	defer b.Close()

	const duration = 500 * time.Millisecond
	deadline := time.Now().Add(duration)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			_, _ = b.Wait(10 * time.Millisecond)
		}
	}()
	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			a.Wake()
			time.Sleep(20 * time.Microsecond)
		}
	}()
	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(duration + 2*time.Second):
		t.Fatal("concurrent stress did not finish")
	}
}

// TestDataSegWaker_TwoParkers_SimultaneousWakeStealing covers the
// realistic worst-case production scenario:
//
//   - Side A has TWO parkers: a reader R waiting on rxDataSeq, a
//     writer W waiting on txSpaceSeq under back-pressure. Both park
//     on the SAME direction's eventfd (per-segment shared waker).
//   - Peer Side B signals BOTH conditions in quick succession:
//     signalData(rxDataSeq) then signalSpace(txSpaceSeq). Each
//     Wake = +1 to A's counter, so counter accumulates to 2.
//   - Kernel netpoll fires the edge ONCE (counter 0 -> non-zero).
//     Only one of {R, W} wakes; that one's Read drains the counter
//     to 0 in a single syscall.
//
// WITHOUT the counter-redistribution fix, the second parker would
// be stuck forever (no more edge, ring.go uses timeout=0 in the hot
// path). WITH the fix, Wait returns n=2 to WaitForChange, which
// writes n-1=1 wake back to OUR OWN counter via RewakeLocal. That
// write causes a 0->1 edge fire, waking the second parker. Both
// parkers complete promptly.
//
// This is the scenario the user pointed out and is the ONLY
// realistic multi-parker case in production (gRPC's HTTP/2-style
// transport has at most 1 reader + 1 writer goroutine per
// connection; stream-level parallelism is sequentialised through
// these two goroutines).
//
// timeout=0 deliberately: matches ring.go's hot-path call shape
// (waitForData(..., 0)). If this test ever fails, production WILL
// hang under bidirectional back-pressure.
func TestDataSegWaker_TwoParkers_SimultaneousWakeStealing(t *testing.T) {
	a, b, err := newShmDataSegWakerPair()
	if err != nil {
		t.Fatalf("newShmDataSegWakerPair: %v", err)
	}
	defer a.Close()
	defer b.Close()

	// Two distinct conditions; both parkers wait on B's read fd.
	var addrData, addrSpace uint32

	rDone := make(chan error, 1)
	wDone := make(chan error, 1)

	// Park R (reader equivalent) and W (writer equivalent) BOTH on
	// b's read fd with timeout=0 (production hot path).
	go func() {
		rDone <- b.WaitForChange(&addrData, 0, 0)
	}()
	go func() {
		wDone <- b.WaitForChange(&addrSpace, 0, 0)
	}()

	// Let both parkers reach Wait.
	time.Sleep(20 * time.Millisecond)

	before := LoadShmDataSegWakeCounters()

	// Peer signals both conditions in quick succession, mimicking
	// the bidirectional back-pressure pattern. Store-then-Wake for
	// each (matches signalData / signalSpace in ring.go).
	atomic.StoreUint32(&addrData, 1)
	a.Wake() // counter: 0 -> 1, edge fires
	atomic.StoreUint32(&addrSpace, 1)
	a.Wake() // counter: 1 -> 2, NO edge (already readable)

	// Both parkers MUST complete -- no outer-loop timeout safety
	// net (timeout=0 in WaitForChange call above).
	for i, ch := range []chan error{rDone, wDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("parker[%d] returned err=%v, want nil", i, err)
			}
		case <-time.After(2 * time.Second):
			after := LoadShmDataSegWakeCounters()
			delta := after.Sub(before)
			t.Fatalf("parker[%d] STUCK -- simultaneous-wake-stealing bug "+
				"(addrData=%d addrSpace=%d, wakes=%d waits=%d nil=%d rewake=%d bailout=%d)",
				i,
				atomic.LoadUint32(&addrData), atomic.LoadUint32(&addrSpace),
				delta.WakeCallsTotal, delta.WaitCallsTotal,
				delta.WaitReturnNil, delta.RewakeLocal, delta.FanOutBailout)
		}
	}

	after := LoadShmDataSegWakeCounters()
	delta := after.Sub(before)
	t.Logf("simultaneous wake handled: wakes=%d waits=%d nil=%d rewake=%d bailout=%d",
		delta.WakeCallsTotal, delta.WaitCallsTotal,
		delta.WaitReturnNil, delta.RewakeLocal, delta.FanOutBailout)

	// Sanity: rewake should be at least 1 (the redistribute from
	// whichever parker drained counter=2). Without the fix, this
	// would be 0 AND the test would have already failed.
	if delta.RewakeLocal < 1 {
		t.Errorf("expected at least 1 RewakeLocal (counter=2 drain "+
			"redistribute); got %d", delta.RewakeLocal)
	}
}
