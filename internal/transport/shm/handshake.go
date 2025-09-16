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

//go:build linux && (amd64 || arm64)

package shm

import (
    "context"
    "errors"
    "sync/atomic"
    "time"
    "unsafe"
)

var (
	ErrFutexNotSupported = errors.New("futex operations not supported on this platform")
)

// WaitForClient waits for the client to mark itself as ready.
// The server calls this after creating the segment to wait for a client connection.
func (s *Segment) WaitForClient(ctx context.Context) error {
    addr := (*uint32)(unsafe.Pointer(&s.H.header().clientReady))
    // Fast path
    if atomic.LoadUint32(addr) != 0 {
        return nil
    }
    // Respect deadline if present
    for {
        if atomic.LoadUint32(addr) != 0 {
            return nil
        }
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
        }
        // Compute optional timeout chunk from context
        var timeoutNs int64
        if dl, ok := ctx.Deadline(); ok {
            remaining := time.Until(dl)
            if remaining <= 0 {
                return context.DeadlineExceeded
            }
            timeoutNs = remaining.Nanoseconds()
        }
        if timeoutNs > 0 {
            if err := futexWaitTimeout(addr, 0, timeoutNs); err != nil && err.Error() != "futex wait timed out" {
                return err
            }
        } else {
            if err := futexWait(addr, 0); err != nil {
                return err
            }
        }
    }
}

// WaitForServer waits for the server to mark itself as ready.
// The client calls this after opening a segment to wait for server confirmation.
func (s *Segment) WaitForServer(ctx context.Context) error {
    addr := (*uint32)(unsafe.Pointer(&s.H.header().serverReady))
    if atomic.LoadUint32(addr) != 0 {
        return nil
    }
    for {
        if atomic.LoadUint32(addr) != 0 {
            return nil
        }
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
        }
        var timeoutNs int64
        if dl, ok := ctx.Deadline(); ok {
            remaining := time.Until(dl)
            if remaining <= 0 {
                return context.DeadlineExceeded
            }
            timeoutNs = remaining.Nanoseconds()
        }
        if timeoutNs > 0 {
            if err := futexWaitTimeout(addr, 0, timeoutNs); err != nil && err.Error() != "futex wait timed out" {
                return err
            }
        } else {
            if err := futexWait(addr, 0); err != nil {
                return err
            }
        }
    }
}
