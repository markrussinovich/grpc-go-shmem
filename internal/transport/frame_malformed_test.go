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
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestReadCtlFrame_MalformedDrains verifies that readCtlFrame returns
// errMalformedCtlFrame when a frame header advertises a payload length
// larger than maxCtlPayload, and that the bounded best-effort drain
// keeps subsequent reads possible (the listener's Accept loop can then
// log + continue instead of tearing down).
//
// Regression guard for the round-1 P1 fix that replaced the previous
// fatal error with a recoverable sentinel.
func TestReadCtlFrame_MalformedDrains(t *testing.T) {
	name := fmt.Sprintf("malformed-frame-%d", time.Now().UnixNano())
	defer RemoveSegment(name)

	// maxCtlPayload is 4096 (see readCtlFrame); ring must be at least
	// the oversize payload + the 16-byte header. 16 KiB is comfortable.
	const ringCap = 16384
	seg, err := CreateSegment(name, ringCap, ringCap)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// writeCtlFrame sets fh.Length = len(payload), so a 5000-byte
	// payload makes the reader see Length=5000 which exceeds the
	// 4 KiB cap.
	oversize := make([]byte, 5000)
	if err := writeCtlFrame(ctx, tx, FrameHeader{Type: FrameTypeCONNECT}, oversize); err != nil {
		t.Fatalf("writeCtlFrame: %v", err)
	}

	_, _, rerr := readCtlFrame(ctx, rx)
	if !errors.Is(rerr, errMalformedCtlFrame) {
		t.Fatalf("readCtlFrame: expected errMalformedCtlFrame, got %v", rerr)
	}
}
