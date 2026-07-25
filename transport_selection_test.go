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

package grpc

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
	"google.golang.org/grpc/status"
)

// TestUnregisteredTransportTypeFailsClosed is the dispatch-level proof of the
// fail-closed selection rule: an address naming a transport type that no Builder
// is registered for must NOT silently fall back to HTTP/2. A silent fallback
// would let an explicit transport selector change the protocol on the wire,
// which is both a correctness and a security problem.
//
// The test points the address at a real listening TCP socket that speaks HTTP/2
// perfectly well, so if selection ever fell back the RPC would succeed and this
// test would fail.
func (s) TestUnregisteredTransportTypeFailsClosed(t *testing.T) {
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer lis.Close()
	srv := NewServer()
	go srv.Serve(lis)
	defer srv.Stop()

	r := manual.NewBuilderWithScheme("failclosed")
	r.InitialState(resolver.State{Addresses: []resolver.Address{
		{Addr: lis.Addr().String(), TransportType: "no-such-transport-registered"},
	}})

	cc, err := NewClient("failclosed:///whatever",
		WithResolvers(r),
		WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	err = cc.Invoke(ctx, "/some.Service/SomeMethod", &struct{}{}, &struct{}{})
	if err == nil {
		t.Fatalf("Invoke over an unregistered transport type succeeded; want failure (selection must not fall back to HTTP/2)")
	}
	if got := status.Code(err); got != codes.DeadlineExceeded && got != codes.Unavailable {
		t.Fatalf("Invoke error code = %v (%v); want DeadlineExceeded or Unavailable", got, err)
	}
	t.Logf("unregistered transport type correctly failed closed: %v", err)
}

// TestEmptyTransportTypeUsesDefault is the companion regression test: an address
// with NO transport type must keep taking the default path. A mistake in the
// fail-closed branch that also caught the empty type would break every ordinary
// connection.
func (s) TestEmptyTransportTypeUsesDefault(t *testing.T) {
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer lis.Close()
	srv := NewServer()
	go srv.Serve(lis)
	defer srv.Stop()

	r := manual.NewBuilderWithScheme("emptytype")
	r.InitialState(resolver.State{Addresses: []resolver.Address{
		{Addr: lis.Addr().String()}, // no TransportType
	}})

	cc, err := NewClient("emptytype:///whatever",
		WithResolvers(r),
		WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// Reaching READY proves selection took the default HTTP/2 path and actually
	// connected, rather than being refused by the fail-closed branch.
	cc.Connect()
	for state := cc.GetState(); state != connectivity.Ready; state = cc.GetState() {
		if !cc.WaitForStateChange(ctx, state) {
			t.Fatalf("ClientConn with an empty transport type never became READY (stuck in %v); the default transport path is broken", state)
		}
	}
}
