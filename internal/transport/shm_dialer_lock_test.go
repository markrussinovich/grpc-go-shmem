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
	"testing"
	"time"
)

// TestDialShm_ControlLockReusedAfterSuccess verifies that the
// control-segment lock is released cleanly after a successful dial
// and can be re-acquired immediately by a follow-up dial. The
// failure mode this guards against is a leaked lock that would
// block all subsequent CONNECT requests.
func TestDialShm_ControlLockReusedAfterSuccess(t *testing.T) {
	name := testSegName("dial_lock_reuse")
	defer RemoveSegment(name)
	defer RemoveSegment(name + shmControlSuffix)
	defer removeControlLock(name + shmControlSuffix)

	lis, err := NewShmListener(&ShmAddr{Name: name}, DefaultSegmentSize, DefaultRingASize, DefaultRingBSize)
	if err != nil {
		t.Fatalf("NewShmListener: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	// Accept connections on a background goroutine; close each as
	// soon as the test no longer needs them.
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		tr, err := DialShm(ctx, name, &DialOptions{
			SegmentSize: DefaultSegmentSize,
			RingASize:   DefaultRingASize,
			RingBSize:   DefaultRingBSize,
		})
		cancel()
		if err != nil {
			t.Fatalf("iter %d: DialShm: %v", i, err)
		}
		tr.Close(nil)
	}

	// Confirm the control-segment lock file is releaseable (i.e.,
	// not stuck held by a leaked closure). acquireControlLock should
	// succeed promptly.
	acqCtx, acqCancel := context.WithTimeout(context.Background(), time.Second)
	defer acqCancel()
	release, err := acquireControlLock(acqCtx, name+shmControlSuffix)
	if err != nil {
		t.Fatalf("acquireControlLock after dials: %v", err)
	}
	release()

	_ = lis.Close()
	<-acceptDone
}
