//go:build linux && (amd64 || arm64)

package shm

import (
	"context"
	"testing"
	"time"
)

func TestServerTransportFactory(t *testing.T) {
	// Test server transport factory creation
	addr := "test_server_factory"
	factory, err := NewServerTransportFactory(addr, DefaultSegmentSize, DefaultRingASize, DefaultRingBSize)
	if err != nil {
		t.Fatalf("Failed to create server transport factory: %v", err)
	}
	defer factory.Close()

	// Verify factory properties
	if factory.Addr().Network() != "shm" {
		t.Errorf("Expected network 'shm', got %s", factory.Addr().Network())
	}

	if factory.Addr().String() != addr {
		t.Errorf("Expected address %s, got %s", addr, factory.Addr().String())
	}

	// Test that Accept would block (we can't easily test the actual accept without a client)
	// So we'll test Accept with a timeout to ensure it doesn't panic
	go func() {
		time.Sleep(10 * time.Millisecond)
		factory.Close() // This should cause Accept to return with an error
	}()

	_, err = factory.Accept()
	if err == nil {
		t.Error("Expected Accept to fail after factory close")
	}
}

func TestClientTransportFactory(t *testing.T) {
	// Test client transport factory creation
	factory := NewClientTransportFactory(nil) // Use default options
	if factory == nil {
		t.Fatal("Failed to create client transport factory")
	}

	// Test NewTransport with timeout (should fail since no server)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	transport, err := factory.NewTransport(ctx, "non_existent")
	if err == nil {
		transport.Close(nil)
		t.Error("Expected NewTransport to fail with no server")
	} else {
		t.Logf("NewTransport correctly failed: %v", err)
	}
}

func TestShmTransportOptions(t *testing.T) {
	// Test default options
	defaultOpts := DefaultShmTransportOptions()
	if defaultOpts.ServerSegmentSize != DefaultSegmentSize {
		t.Errorf("Expected server segment size %d, got %d", DefaultSegmentSize, defaultOpts.ServerSegmentSize)
	}
	if defaultOpts.ServerRingASize != DefaultRingASize {
		t.Errorf("Expected server ring A size %d, got %d", DefaultRingASize, defaultOpts.ServerRingASize)
	}
	if defaultOpts.ServerRingBSize != DefaultRingBSize {
		t.Errorf("Expected server ring B size %d, got %d", DefaultRingBSize, defaultOpts.ServerRingBSize)
	}
	if defaultOpts.ClientDialOptions == nil {
		t.Error("Expected client dial options to be set")
	}
}

func TestCreateShmServerTransportFactory(t *testing.T) {
	// Test with default options
	factory1, err := CreateShmServerTransportFactory("test_factory_1", nil)
	if err != nil {
		t.Fatalf("Failed to create server factory with default options: %v", err)
	}
	defer factory1.Close()

	// Test with custom options
	customOpts := &ShmTransportOptions{
		ServerSegmentSize: 8 * 1024 * 1024,
		ServerRingASize:   2 * 1024 * 1024,
		ServerRingBSize:   2 * 1024 * 1024,
	}
	factory2, err := CreateShmServerTransportFactory("test_factory_2", customOpts)
	if err != nil {
		t.Fatalf("Failed to create server factory with custom options: %v", err)
	}
	defer factory2.Close()

	// Verify addresses are different
	if factory1.Addr().String() == factory2.Addr().String() {
		t.Error("Expected different addresses for different factories")
	}
}

func TestCreateShmClientTransportFactory(t *testing.T) {
	// Test with default options
	factory1 := CreateShmClientTransportFactory(nil)
	if factory1 == nil {
		t.Fatal("Failed to create client factory with default options")
	}

	// Test with custom options
	customOpts := &ShmTransportOptions{
		ClientDialOptions: &DialOptions{
			SegmentSize:    8 * 1024 * 1024,
			RingASize:      2 * 1024 * 1024,
			RingBSize:      2 * 1024 * 1024,
			ConnectTimeout: 5 * time.Second,
		},
	}
	factory2 := CreateShmClientTransportFactory(customOpts)
	if factory2 == nil {
		t.Fatal("Failed to create client factory with custom options")
	}

	// Verify the custom options are applied
	if factory2.dialer.opts.SegmentSize != customOpts.ClientDialOptions.SegmentSize {
		t.Error("Custom segment size not applied")
	}
}

func TestTransportFactoryIntegration(t *testing.T) {
	// This test verifies that the factories can be created and used together
	// In a real scenario, they would be in separate processes

	addr := "test_integration"

	// Create server factory
	serverFactory, err := CreateShmServerTransportFactory(addr, nil)
	if err != nil {
		t.Fatalf("Failed to create server factory: %v", err)
	}
	defer serverFactory.Close()

	// Create client factory
	clientFactory := CreateShmClientTransportFactory(nil)
	if clientFactory == nil {
		t.Fatal("Failed to create client factory")
	}

	// Test that client factory fails to connect (no server accepting)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	clientTransport, err := clientFactory.NewTransport(ctx, addr)
	if err == nil {
		clientTransport.Close(nil)
		t.Error("Expected client transport creation to fail with no accepting server")
	} else {
		t.Logf("Client transport creation correctly failed: %v", err)
	}
}
