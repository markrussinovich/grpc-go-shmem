//go:build windows

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

package engine

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"unsafe"
)

// WaitForClient waits for the client to mark itself as ready.
// On Windows, uses named events because WaitOnAddress only works within
// the same virtual address mapping.
func (s *Segment) WaitForClient(ctx context.Context) error {
	addr := (*uint32)(unsafe.Pointer(&s.H.header().clientReady))
	// Fast path - check if already ready
	if atomic.LoadUint32(addr) != 0 {
		return nil
	}

	// Extract segment name from path
	segmentName := extractSegmentNameFromPath(s.Path)

	for {
		if err := WaitClientReady(ctx, segmentName); err != nil {
			return err
		}

		// Require the shared flag to be set. A stale named-event signal
		// (from a previous accept that timed out and was cleaned up, or
		// from a peer that crashed after signalling but before storing)
		// must NOT count as success: the listener relies on this gate to
		// know the client actually mapped the segment. The event is
		// auto-reset, so consuming a stale signal and retrying waits for
		// the next real transition.
		if atomic.LoadUint32(addr) != 0 {
			return nil
		}
	}
}

// WaitForServer waits for the server to mark itself as ready.
// On Windows, uses named events because WaitOnAddress only works within
// the same virtual address mapping.
func (s *Segment) WaitForServer(ctx context.Context) error {
	addr := (*uint32)(unsafe.Pointer(&s.H.header().serverReady))
	// Fast path - check if already ready
	if atomic.LoadUint32(addr) != 0 {
		return nil
	}

	// Extract segment name from path
	segmentName := extractSegmentNameFromPath(s.Path)

	for {
		if err := WaitServerReady(ctx, segmentName); err != nil {
			return err
		}

		// Require the shared flag to be set. See WaitForClient for
		// rationale on consuming stale-event signals and retrying.
		if atomic.LoadUint32(addr) != 0 {
			return nil
		}
	}
}

// extractSegmentNameFromPath extracts the segment name from the file path.
// Path format: C:\Users\...\Temp\grpc_shm_<segmentName>
func extractSegmentNameFromPath(path string) string {
	base := filepath.Base(path)
	const prefix = "grpc_shm_"
	if len(base) > len(prefix) && base[:len(prefix)] == prefix {
		return base[len(prefix):]
	}
	return base
}
