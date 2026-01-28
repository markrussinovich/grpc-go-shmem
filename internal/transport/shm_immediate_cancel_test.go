//go:build linux

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
	"context"
	"fmt"
	"log"
	"testing"
	"time"
)

// TestImmediateCancellation tests cancellation that happens before any I/O
func TestImmediateCancellation(t *testing.T) {
	log.Printf("=== Starting TestImmediateCancellation ===")
	name := fmt.Sprintf("immediate-cancel-%d", time.Now().UnixNano())
	seg, err := CreateSegment(name, 65536, 65536)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	client := NewShmUnaryClient(seg)
	defer client.Close() // Ensure client is properly closed
	payload := make([]byte, 5+3)
	payload[0] = 0
	payload[1] = 3
	payload[2] = 0
	payload[3] = 0
	payload[4] = 0
	copy(payload[5:], []byte("abc"))

	// Create an already-cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately
	log.Printf("Client: Context already canceled before UnaryCall")

	start := time.Now()
	_, _, _, err = client.UnaryCall(ctx, "/svc.X/Unary", "example.com", nil, payload)
	duration := time.Since(start)

	log.Printf("Client: UnaryCall returned in %v with error: %v", duration, err)

	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}

	if duration > 100*time.Millisecond {
		t.Fatalf("UnaryCall took too long (%v), expected immediate return", duration)
	}

	log.Printf("=== TestImmediateCancellation completed ===")
}
