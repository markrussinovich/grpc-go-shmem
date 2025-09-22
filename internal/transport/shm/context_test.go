//go:build linux

package shm

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
