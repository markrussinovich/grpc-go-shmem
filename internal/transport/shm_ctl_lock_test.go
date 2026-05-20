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
	"sync"
	"testing"
	"time"
)

// TestAcquireControlLock_Exclusive verifies that a second caller
// blocks while the first holds the lock, and acquires it once the
// first releases.
func TestAcquireControlLock_Exclusive(t *testing.T) {
	name := testSegName("ctl_lock_excl")
	t.Cleanup(func() { removeControlLock(name) })

	ctx := t.Context()
	release1, err := acquireControlLock(ctx, name)
	if err != nil {
		t.Fatalf("first acquireControlLock: %v", err)
	}

	var release2 func()
	got := make(chan error, 1)
	go func() {
		r, err := acquireControlLock(ctx, name)
		release2 = r
		got <- err
	}()

	select {
	case err := <-got:
		t.Fatalf("second acquireControlLock returned before first released: %v", err)
	case <-time.After(50 * time.Millisecond):
		// expected: second caller is still blocked.
	}

	release1()

	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("second acquireControlLock after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second acquireControlLock did not unblock within 2s")
	}
	release2()
}

// TestAcquireControlLock_ContextCancel verifies a caller blocked on
// the lock returns promptly when its context is cancelled.
func TestAcquireControlLock_ContextCancel(t *testing.T) {
	name := testSegName("ctl_lock_cancel")
	t.Cleanup(func() { removeControlLock(name) })

	release1, err := acquireControlLock(t.Context(), name)
	if err != nil {
		t.Fatalf("first acquireControlLock: %v", err)
	}
	t.Cleanup(release1)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := acquireControlLock(ctx, name)
		done <- err
	}()

	// Let the goroutine start blocking, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("acquireControlLock did not return an error after ctx cancel")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("acquireControlLock did not respect ctx cancellation within 1s")
	}
}

// TestAcquireControlLock_Reentrant verifies that the lock can be
// released and re-acquired from the same goroutine without deadlock
// (a sanity check that the close path actually releases the kernel
// state).
func TestAcquireControlLock_Reentrant(t *testing.T) {
	name := testSegName("ctl_lock_reent")
	t.Cleanup(func() { removeControlLock(name) })

	for i := 0; i < 3; i++ {
		release, err := acquireControlLock(t.Context(), name)
		if err != nil {
			t.Fatalf("iter %d: acquireControlLock: %v", i, err)
		}
		release()
	}
}

// TestAcquireControlLock_Concurrent stresses concurrent acquirers in
// the same process. The lock serialises them; the test asserts that no
// two goroutines hold the lock simultaneously.
func TestAcquireControlLock_Concurrent(t *testing.T) {
	name := testSegName("ctl_lock_conc")
	t.Cleanup(func() { removeControlLock(name) })

	const N = 8
	var (
		mu       sync.Mutex
		holding  int
		maxHeld  int
		acquired int
		wg       sync.WaitGroup
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			release, err := acquireControlLock(t.Context(), name)
			if err != nil {
				t.Errorf("acquireControlLock: %v", err)
				return
			}
			mu.Lock()
			holding++
			if holding > maxHeld {
				maxHeld = holding
			}
			acquired++
			mu.Unlock()
			// Hold for a brief moment so any concurrent acquirer
			// would have a chance to race in and bump maxHeld if
			// the lock were broken.
			time.Sleep(2 * time.Millisecond)
			mu.Lock()
			holding--
			mu.Unlock()
			// Release BEFORE the goroutine exits, otherwise the
			// kernel object would stay owned by a defunct goroutine
			// scheduling state and the next waiter would block
			// forever.
			release()
		}()
	}
	wg.Wait()
	if maxHeld != 1 {
		t.Errorf("max concurrent holders = %d; want 1 (lock is not exclusive)", maxHeld)
	}
	if acquired != N {
		t.Errorf("acquired = %d; want %d", acquired, N)
	}
}
