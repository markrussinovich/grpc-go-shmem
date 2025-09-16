//go:build linux && (amd64 || arm64)

package shm

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestShmListener(t *testing.T) {
	// Create a listener
	addr := &ShmAddr{Name: "test_listener"}
	listener, err := NewShmListener(addr, DefaultSegmentSize, DefaultRingASize, DefaultRingBSize)
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer listener.Close()

	// Verify listener properties
	if listener.Addr().Network() != "shm" {
		t.Errorf("Expected network 'shm', got %s", listener.Addr().Network())
	}

	if listener.Addr().String() != "test_listener" {
		t.Errorf("Expected address 'test_listener', got %s", listener.Addr().String())
	}

	// Test that we can close the listener
	if err := listener.Close(); err != nil {
		t.Errorf("Failed to close listener: %v", err)
	}

	// Test that Accept returns an error after close
	_, err = listener.Accept()
	if err == nil {
		t.Error("Expected error when accepting on closed listener")
	}
}

func TestShmDialer(t *testing.T) {
	dialer := NewShmDialer(&DialOptions{
		SegmentSize:    DefaultSegmentSize,
		RingASize:      DefaultRingASize,
		RingBSize:      DefaultRingBSize,
		ConnectTimeout: 100 * time.Millisecond, // Short timeout
	})

	// Test dial to non-existent address should timeout quickly
	ctx := context.Background()

	start := time.Now()
	conn, err := dialer.Dial(ctx, "non_existent")
	elapsed := time.Since(start)

	t.Logf("Dial result: conn=%v, err=%v, elapsed=%v", conn != nil, err, elapsed)

	if err == nil {
		if conn != nil {
			conn.Close()
		}
		// This is actually unexpected - let's understand why it succeeded
		t.Error("Expected error when dialing non-existent address, but succeeded - this suggests the segment creation and handshake completed incorrectly")
	} else {
		t.Logf("Dial correctly failed with: %v (took %v)", err, elapsed)

		// Verify it timed out reasonably quickly
		if elapsed > 200*time.Millisecond {
			t.Errorf("Dial took too long: %v, expected around 100ms", elapsed)
		}
	}
}

func TestShmConnectionEstablishment(t *testing.T) {
	// This test demonstrates the connection establishment flow
	// In a real scenario, client and server would be in separate processes
	testAddr := fmt.Sprintf("test_conn_%d", time.Now().UnixNano())

	// Test the individual components separately since full integration
	// requires actual file system interaction

	// 1. Test listener creation
	addr := &ShmAddr{Name: testAddr}
	listener, err := NewShmListener(addr, DefaultSegmentSize, DefaultRingASize, DefaultRingBSize)
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer listener.Close()

	// 2. Test that listener is ready
	if listener.Addr().String() != testAddr {
		t.Errorf("Listener address mismatch: expected %s, got %s", testAddr, listener.Addr().String())
	}

	// 3. Test dialer creation with timeout (should fail since no server)
	dialer := NewShmDialer(&DialOptions{
		SegmentSize:    DefaultSegmentSize,
		RingASize:      DefaultRingASize,
		RingBSize:      DefaultRingBSize,
		ConnectTimeout: 50 * time.Millisecond, // Short timeout for test
	})

	ctx := context.Background()
	conn, err := dialer.Dial(ctx, testAddr)
	if err == nil {
		conn.Close()
		t.Error("Expected dial to fail since no server is accepting connections")
	} else {
		t.Logf("Dial correctly failed: %v", err)
	}
}

func TestShmConnectionProperties(t *testing.T) {
	testAddr := fmt.Sprintf("test_props_%d", time.Now().UnixNano())

	dialer := NewShmDialer(DefaultDialOptions())
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	conn, err := dialer.Dial(ctx, testAddr)
	if err != nil {
		// Expected to fail since no server, but let's test what we can
		t.Logf("Dial failed as expected: %v", err)
		return
	}
	defer conn.Close()

	// Test connection properties
	if conn.LocalAddr().Network() != "shm" {
		t.Errorf("Expected network 'shm', got %s", conn.LocalAddr().Network())
	}

	if conn.RemoteAddr().Network() != "shm" {
		t.Errorf("Expected network 'shm', got %s", conn.RemoteAddr().Network())
	}

	// Test that direct read/write fail as expected
	buf := make([]byte, 10)
	_, err = conn.Read(buf)
	if err == nil {
		t.Error("Expected error on direct read")
	}

	_, err = conn.Write(buf)
	if err == nil {
		t.Error("Expected error on direct write")
	}

	// Test that deadlines are no-op
	err = conn.SetDeadline(time.Now().Add(time.Second))
	if err != nil {
		t.Errorf("SetDeadline should be no-op, got error: %v", err)
	}

	err = conn.SetReadDeadline(time.Now().Add(time.Second))
	if err != nil {
		t.Errorf("SetReadDeadline should be no-op, got error: %v", err)
	}

	err = conn.SetWriteDeadline(time.Now().Add(time.Second))
	if err != nil {
		t.Errorf("SetWriteDeadline should be no-op, got error: %v", err)
	}
}

func TestShmDialOptions(t *testing.T) {
	// Test default options
	defaultOpts := DefaultDialOptions()
	if defaultOpts.SegmentSize != DefaultSegmentSize {
		t.Errorf("Expected segment size %d, got %d", DefaultSegmentSize, defaultOpts.SegmentSize)
	}
	if defaultOpts.RingASize != DefaultRingASize {
		t.Errorf("Expected ring A size %d, got %d", DefaultRingASize, defaultOpts.RingASize)
	}
	if defaultOpts.RingBSize != DefaultRingBSize {
		t.Errorf("Expected ring B size %d, got %d", DefaultRingBSize, defaultOpts.RingBSize)
	}
	if defaultOpts.ConnectTimeout != 30*time.Second {
		t.Errorf("Expected timeout 30s, got %v", defaultOpts.ConnectTimeout)
	}

	// Test custom options
	customOpts := &DialOptions{
		SegmentSize:    8 * 1024 * 1024,
		RingASize:      2 * 1024 * 1024,
		RingBSize:      2 * 1024 * 1024,
		ConnectTimeout: 5 * time.Second,
	}

	dialer := NewShmDialer(customOpts)
	if dialer.opts.SegmentSize != customOpts.SegmentSize {
		t.Errorf("Custom options not preserved")
	}
}

func TestShmAddr(t *testing.T) {
	addr := &ShmAddr{Name: "test_address"}

	if addr.Network() != "shm" {
		t.Errorf("Expected network 'shm', got %s", addr.Network())
	}

	if addr.String() != "test_address" {
		t.Errorf("Expected string 'test_address', got %s", addr.String())
	}
}
