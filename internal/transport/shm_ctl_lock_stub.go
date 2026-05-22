//go:build !linux && !windows

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

// Control-segment cross-process serialization lock (stub for platforms
// without flock or named mutex). The lock is process-local only; SHM
// is not supported cross-process on these platforms (no /dev/shm, no
// mmap of named POSIX shared memory).

package transport

import (
	"context"
	"sync"
)

var (
	stubLocksMu sync.Mutex
	stubLocks   = map[string]*sync.Mutex{}
)

func acquireControlLock(ctx context.Context, ctlName string) (func(), error) {
	stubLocksMu.Lock()
	m, ok := stubLocks[ctlName]
	if !ok {
		m = &sync.Mutex{}
		stubLocks[ctlName] = m
	}
	stubLocksMu.Unlock()

	// Honour ctx via a channel; sync.Mutex doesn't support cancellation
	// directly so we lock in a goroutine and select.
	locked := make(chan struct{})
	go func() {
		m.Lock()
		close(locked)
	}()
	select {
	case <-locked:
		var released bool
		return func() {
			if released {
				return
			}
			released = true
			m.Unlock()
		}, nil
	case <-ctx.Done():
		// We can't cancel the goroutine cleanly; wait for it to finish
		// in the background and unlock. This is rare on these
		// platforms (no cross-process SHM means little contention).
		go func() {
			<-locked
			m.Unlock()
		}()
		return nil, ctx.Err()
	}
}

func removeControlLock(_ string) {}
