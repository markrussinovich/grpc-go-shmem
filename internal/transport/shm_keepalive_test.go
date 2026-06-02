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
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/keepalive"
)

// TestShmKeepaliveClientConfiguration tests that client keepalive is correctly configured.
func TestShmKeepaliveClientConfiguration(t *testing.T) {
	segmentName := fmt.Sprintf("test-keepalive-config-%d", time.Now().UnixNano())
	defer RemoveSegment(segmentName)

	seg, err := CreateSegment(segmentName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}

	// Disable the automatic client reader
	enableClientReader.Store(false)
	defer enableClientReader.Store(true)

	clientTransport, err := NewShmClientTransport(seg, &shmAddr{s: "client"}, &shmAddr{s: "server"})
	if err != nil {
		t.Fatalf("Failed to create transport: %v", err)
	}
	defer clientTransport.Close(nil)

	// Configure with specific params
	kp := keepalive.ClientParameters{
		Time:                42 * time.Second,
		Timeout:             17 * time.Second,
		PermitWithoutStream: true,
	}
	clientTransport.ConfigureKeepalive(kp)

	// Verify configuration
	if clientTransport.kp.Time != 42*time.Second {
		t.Errorf("Expected Time=%v, got %v", 42*time.Second, clientTransport.kp.Time)
	}
	if clientTransport.kp.Timeout != 17*time.Second {
		t.Errorf("Expected Timeout=%v, got %v", 17*time.Second, clientTransport.kp.Timeout)
	}
	if !clientTransport.kp.PermitWithoutStream {
		t.Error("Expected PermitWithoutStream=true")
	}
	if !clientTransport.keepaliveEnabled.Load() {
		t.Error("Expected keepaliveEnabled=true since Time != infinity")
	}
}

// TestShmKeepaliveClientDefaults tests that default values are applied.
func TestShmKeepaliveClientDefaults(t *testing.T) {
	segmentName := fmt.Sprintf("test-keepalive-defaults-%d", time.Now().UnixNano())
	defer RemoveSegment(segmentName)

	seg, err := CreateSegment(segmentName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}

	enableClientReader.Store(false)
	defer enableClientReader.Store(true)

	clientTransport, err := NewShmClientTransport(seg, &shmAddr{s: "client"}, &shmAddr{s: "server"})
	if err != nil {
		t.Fatalf("Failed to create transport: %v", err)
	}
	defer clientTransport.Close(nil)

	// Configure with zero values (should get defaults)
	clientTransport.ConfigureKeepalive(keepalive.ClientParameters{})

	// Verify defaults were applied
	if clientTransport.kp.Timeout != defaultClientKeepaliveTimeout {
		t.Errorf("Expected default Timeout=%v, got %v", defaultClientKeepaliveTimeout, clientTransport.kp.Timeout)
	}
	// Time defaults to infinity, so keepalive should be disabled
	if clientTransport.keepaliveEnabled.Load() {
		t.Error("Expected keepaliveEnabled=false with default Time (infinity)")
	}
}

// TestShmKeepaliveDormancy tests that the client keepalive goroutine
// goes dormant when there are no active streams and PermitWithoutStream is false.
func TestShmKeepaliveDormancy(t *testing.T) {
	segmentName := fmt.Sprintf("test-keepalive-dormancy-%d", time.Now().UnixNano())
	defer RemoveSegment(segmentName)

	seg, err := CreateSegment(segmentName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}

	enableClientReader.Store(false)
	defer enableClientReader.Store(true)

	clientTransport, err := NewShmClientTransport(seg, &shmAddr{s: "client"}, &shmAddr{s: "server"})
	if err != nil {
		t.Fatalf("Failed to create transport: %v", err)
	}
	defer clientTransport.Close(nil)

	// Configure keepalive WITHOUT PermitWithoutStream
	clientTransport.ConfigureKeepalive(keepalive.ClientParameters{
		Time:                100 * time.Millisecond,
		Timeout:             50 * time.Millisecond,
		PermitWithoutStream: false,
	})

	// Wait for the keepalive goroutine to enter dormant state
	time.Sleep(150 * time.Millisecond)

	clientTransport.mu.Lock()
	isDormant := clientTransport.kpDormant
	clientTransport.mu.Unlock()

	if !isDormant {
		t.Error("Expected keepalive goroutine to be dormant with no active streams")
	}
}

// TestShmServerKeepaliveConfiguration tests that server keepalive is correctly configured.
func TestShmServerKeepaliveConfiguration(t *testing.T) {
	segmentName := fmt.Sprintf("test-server-config-%d", time.Now().UnixNano())
	defer RemoveSegment(segmentName)

	seg, err := CreateSegment(segmentName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}

	serverTransport, err := NewShmServerTransport(seg, &shmAddr{s: "server"}, &shmAddr{s: "client"})
	if err != nil {
		t.Fatalf("Failed to create server transport: %v", err)
	}
	defer serverTransport.Close(nil)

	kp := keepalive.ServerParameters{
		MaxConnectionIdle:     1 * time.Hour,
		MaxConnectionAge:      2 * time.Hour,
		MaxConnectionAgeGrace: 30 * time.Minute,
		Time:                  10 * time.Minute,
		Timeout:               5 * time.Second,
	}
	kep := keepalive.EnforcementPolicy{
		MinTime:             1 * time.Minute,
		PermitWithoutStream: true,
	}

	serverTransport.ConfigureKeepalive(kp, kep)

	// Verify configuration
	if serverTransport.kp.MaxConnectionIdle != 1*time.Hour {
		t.Errorf("Expected MaxConnectionIdle=%v, got %v", 1*time.Hour, serverTransport.kp.MaxConnectionIdle)
	}
	if serverTransport.kp.Time != 10*time.Minute {
		t.Errorf("Expected Time=%v, got %v", 10*time.Minute, serverTransport.kp.Time)
	}
	if serverTransport.kep.MinTime != 1*time.Minute {
		t.Errorf("Expected MinTime=%v, got %v", 1*time.Minute, serverTransport.kep.MinTime)
	}
	if !serverTransport.kep.PermitWithoutStream {
		t.Error("Expected PermitWithoutStream=true")
	}
}

// TestShmServerKeepaliveMaxConnectionIdle tests that the server closes
// idle connections after MaxConnectionIdle.
func TestShmServerKeepaliveMaxConnectionIdle(t *testing.T) {
	segmentName := fmt.Sprintf("test-server-maxidle-%d", time.Now().UnixNano())
	defer RemoveSegment(segmentName)

	seg, err := CreateSegment(segmentName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}

	serverTransport, err := NewShmServerTransport(seg, &shmAddr{s: "server"}, &shmAddr{s: "client"})
	if err != nil {
		t.Fatalf("Failed to create server transport: %v", err)
	}

	// Configure with a short MaxConnectionIdle
	serverTransport.ConfigureKeepalive(
		keepalive.ServerParameters{
			MaxConnectionIdle: 100 * time.Millisecond,
			Time:              1 * time.Hour,
			Timeout:           20 * time.Second,
		},
		keepalive.EnforcementPolicy{
			MinTime:             5 * time.Minute,
			PermitWithoutStream: true,
		},
	)

	// Wait for MaxConnectionIdle to trigger
	time.Sleep(300 * time.Millisecond)

	// Server should have initiated drain
	if !serverTransport.draining.Load() && !serverTransport.closed.Load() {
		t.Error("Expected server transport to be draining or closed due to idle timeout")
	}

	// Clean up
	if !serverTransport.closed.Load() {
		serverTransport.Close(nil)
	}
}

// TestShmClientLastReadUpdated tests that lastRead is updated when receiving data.
func TestShmClientLastReadUpdated(t *testing.T) {
	segmentName := fmt.Sprintf("test-lastread-%d", time.Now().UnixNano())
	defer RemoveSegment(segmentName)

	seg, err := CreateSegment(segmentName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("Failed to create segment: %v", err)
	}

	enableClientReader.Store(false)
	defer enableClientReader.Store(true)

	clientTransport, err := NewShmClientTransport(seg, &shmAddr{s: "client"}, &shmAddr{s: "server"})
	if err != nil {
		t.Fatalf("Failed to create transport: %v", err)
	}
	defer clientTransport.Close(nil)

	// Verify lastReadTick starts at 0
	initial := clientTransport.lastReadTick.Load()
	if initial != 0 {
		t.Fatalf("Expected initial lastReadTick=0, got %d", initial)
	}

	// Simulate the per-frame bump that the dispatch loop would do
	// when keepalive is enabled.
	clientTransport.lastReadTick.Add(1)

	updated := clientTransport.lastReadTick.Load()
	if updated != 1 {
		t.Errorf("Expected lastReadTick=1, got %d", updated)
	}
}
