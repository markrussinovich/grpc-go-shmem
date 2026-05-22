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

// Control-segment cross-process serialization lock (Linux / POSIX).
//
// The gRFC requires clients to serialize CONNECT writes / ACCEPT reads on
// the shared control segment using an OS-level mutual-exclusion primitive
// keyed off the control-segment name: an flock(LOCK_EX) on a sibling lock
// file. This is necessary because the control segment's Ring A is shared
// among all clients connecting to the same server -- the SPSC ring
// assumption only holds for the duration of one client's exchange.
//
// The lock file lives next to the control segment as "<segmentPath>.lock";
// it is created on first use with mode 0600 so only the segment owner can
// participate. The lock is released by closing the fd (Linux's POSIX flock
// semantics).

package transport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// acquireControlLock acquires an exclusive flock on a sibling ".lock"
// file next to the named control segment. It blocks until the lock is
// granted or ctx is cancelled. The returned release function MUST be
// invoked once the CONNECT/ACCEPT exchange completes; releasing it
// before then leaves the control ring open for other clients to race
// on. The release closure is idempotent.
func acquireControlLock(ctx context.Context, ctlName string) (func(), error) {
	lockPath := generateSegmentPath(ctlName) + ".lock"

	// Open (creating if absent) with restrictive 0600 mode. The file
	// has no payload; flock state lives in the kernel and is keyed by
	// the underlying inode.
	fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("shm: open control lock %q: %w", lockPath, err)
	}

	// Tighten permissions defensively in case a prior process created
	// the file with a looser umask before this fix shipped.
	_ = unix.Fchmod(fd, 0o600)

	// Acquire LOCK_EX. We use non-blocking flock in a polling loop so
	// ctx cancellation is honoured promptly (a plain blocking flock
	// would leave the goroutine parked in a syscall, ignoring ctx).
	// Polling interval is short (~5ms) but bounded; contention here
	// only occurs during connection establishment which is already
	// not on the data-plane hot path.
	const pollInterval = 5 * time.Millisecond
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			unix.Close(fd)
			return nil, fmt.Errorf("shm: flock control lock %q: %w", lockPath, err)
		}
		// Lock held by another client; wait and retry.
		select {
		case <-ctx.Done():
			unix.Close(fd)
			return nil, fmt.Errorf("shm: control lock %q: %w", lockPath, ctx.Err())
		case <-time.After(pollInterval):
		}
	}

	var released atomic.Bool
	return func() {
		if released.Swap(true) {
			return
		}
		// LOCK_UN is implicit on close, but explicit unlock first makes
		// the wake of any waiting peer prompt (the close path can be
		// deferred by the runtime).
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
	}, nil
}

// removeControlLock best-effort unlinks the lock file. It is called by
// the listener at shutdown after the control segment itself has been
// removed; failure (e.g., file already absent) is silently ignored.
func removeControlLock(ctlName string) {
	_ = os.Remove(generateSegmentPath(ctlName) + ".lock")
}
