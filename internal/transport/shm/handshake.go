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
	clientReadyAddr := (*uint32)(unsafe.Pointer(&s.H.header().clientReady))
	
	ticker := time.NewTicker(1 * time.Millisecond)
	defer ticker.Stop()
	
	for {
		// Check if client is already ready
		if atomic.LoadUint32(clientReadyAddr) != 0 {
			return nil
		}
		
		// Check context cancellation with small delay for efficiency
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Continue to next iteration
		}
	}
}

// WaitForServer waits for the server to mark itself as ready.
// The client calls this after opening a segment to wait for server confirmation.
func (s *Segment) WaitForServer(ctx context.Context) error {
	serverReadyAddr := (*uint32)(unsafe.Pointer(&s.H.header().serverReady))
	
	ticker := time.NewTicker(1 * time.Millisecond)
	defer ticker.Stop()
	
	for {
		// Check if server is already ready
		if atomic.LoadUint32(serverReadyAddr) != 0 {
			return nil
		}
		
		// Check context cancellation with small delay for efficiency  
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Continue to next iteration
		}
	}
}
