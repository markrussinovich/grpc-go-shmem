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

package grpc

import (
	"context"
	"testing"
	"time"
)

func (s) TestWithShmTransport(t *testing.T) {
	// Test that WithShmTransport returns a valid DialOption
	opt := WithShmTransport()
	if opt == nil {
		t.Fatal("WithShmTransport() returned nil")
	}

	// Test that WithShmTransportAndOptions returns a valid DialOption
	opt = WithShmTransportAndOptions(nil)
	if opt == nil {
		t.Fatal("WithShmTransportAndOptions(nil) returned nil")
	}
}

func (s) TestShmDialerIntegration(t *testing.T) {
	// This is a basic test to ensure the dialer function works correctly
	// We can't actually dial without a server, but we can test the function structure

	opt := WithShmTransport()

	// Apply the option to a dialOptions struct
	var opts dialOptions
	for _, o := range []DialOption{opt} {
		o.apply(&opts)
	}

	// Verify that a dialer was set
	if opts.copts.Dialer == nil {
		t.Fatal("WithShmTransport() did not set a dialer")
	}

	// Test that the dialer rejects non-shm addresses
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := opts.copts.Dialer(ctx, "tcp://localhost:50051")
	if err == nil {
		t.Fatal("Expected error for non-shm address, got nil")
	}
	if err.Error() != "WithShmTransport can only dial shm:// addresses, got: tcp://localhost:50051" {
		t.Fatalf("Unexpected error message: %v", err)
	}
}
