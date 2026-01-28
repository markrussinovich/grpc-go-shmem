//go:build linux || windows

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
	"log"
	"testing"
	"time"
)

// TestContextCancellation tests if basic context cancellation works
func TestContextCancellation(t *testing.T) {
	log.Printf("=== Testing basic context cancellation ===")

	ctx, cancel := context.WithCancel(context.Background())

	// Test that context works
	done := make(chan struct{})
	go func() {
		log.Printf("Goroutine: Waiting for ctx.Done()...")
		<-ctx.Done()
		log.Printf("Goroutine: ctx.Done() fired! err=%v", ctx.Err())
		close(done)
	}()

	time.Sleep(10 * time.Millisecond)
	log.Printf("Main: About to call cancel()")
	cancel()
	log.Printf("Main: cancel() returned")

	select {
	case <-done:
		log.Printf("Main: Goroutine signaled completion")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Context cancellation didn't work!")
	}

	log.Printf("=== Basic context cancellation works ===")
}
