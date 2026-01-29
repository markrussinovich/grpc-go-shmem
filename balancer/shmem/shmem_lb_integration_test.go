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

package shmem

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/internal/testutils"
	"google.golang.org/grpc/resolver"
)

// exitIdler is a local interface for testing ExitIdle functionality.
// Using a local interface avoids using the deprecated balancer.ExitIdler.
type exitIdler interface {
	ExitIdle()
}

// TestPickerPrefersShmem tests that the picker selects shmem connections before TCP.
func TestPickerPrefersShmem(t *testing.T) {
	cc := testutils.NewBalancerClientConn(t)
	b := balancer.Get(Name).Build(cc, balancer.BuildOptions{})
	defer b.Close()

	// Create resolver state with mixed endpoints (1 local shmem + 1 remote TCP)
	shmemAddr := SetLocalAddress(
		SetTransportPreference(
			resolver.Address{Addr: "shm://local-segment"},
			TransportPreferenceShmem,
		),
		true,
	)
	tcpAddr := resolver.Address{Addr: "remote-server:50051"}

	state := balancer.ClientConnState{
		ResolverState: resolver.State{
			Addresses: []resolver.Address{tcpAddr, shmemAddr},
		},
	}

	if err := b.UpdateClientConnState(state); err != nil {
		t.Fatalf("UpdateClientConnState failed: %v", err)
	}

	// Wait for SubConns to be created (2 addresses)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var tcpSC, shmemSC *testutils.TestSubConn
	for i := 0; i < 2; i++ {
		select {
		case sc := <-cc.NewSubConnCh:
			if len(sc.Addresses) > 0 {
				if sc.Addresses[0].Addr == "shm://local-segment" {
					shmemSC = sc
				} else {
					tcpSC = sc
				}
			}
		case <-ctx.Done():
			t.Fatalf("Timeout waiting for SubConns")
		}
	}

	if shmemSC == nil || tcpSC == nil {
		t.Fatalf("SubConns not created: shmem=%v, tcp=%v", shmemSC, tcpSC)
	}

	// Mark both SubConns as Ready
	shmemSC.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Ready})
	tcpSC.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Ready})

	// Wait for picker to be updated
	select {
	case picker := <-cc.NewPickerCh:
		// Pick multiple times and verify shmem is always selected
		for i := 0; i < 10; i++ {
			result, err := picker.Pick(balancer.PickInfo{Ctx: ctx})
			if err != nil {
				t.Fatalf("Pick failed: %v", err)
			}
			if result.SubConn != shmemSC {
				t.Errorf("Pick %d: expected shmem SubConn, got different SubConn", i)
			}
		}
	case <-ctx.Done():
		t.Fatal("Timeout waiting for picker")
	}
}

// TestPickerFallsBackToTCP tests that picker falls back to TCP when no shmem is available.
func TestPickerFallsBackToTCP(t *testing.T) {
	cc := testutils.NewBalancerClientConn(t)
	b := balancer.Get(Name).Build(cc, balancer.BuildOptions{})
	defer b.Close()

	// Create resolver state with only TCP endpoints
	tcpAddr1 := resolver.Address{Addr: "server1:50051"}
	tcpAddr2 := resolver.Address{Addr: "server2:50051"}

	state := balancer.ClientConnState{
		ResolverState: resolver.State{
			Addresses: []resolver.Address{tcpAddr1, tcpAddr2},
		},
	}

	if err := b.UpdateClientConnState(state); err != nil {
		t.Fatalf("UpdateClientConnState failed: %v", err)
	}

	// Wait for SubConns to be created
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var scs []*testutils.TestSubConn
	for i := 0; i < 2; i++ {
		select {
		case sc := <-cc.NewSubConnCh:
			scs = append(scs, sc)
		case <-ctx.Done():
			t.Fatalf("Timeout waiting for SubConns")
		}
	}

	// Mark both SubConns as Ready
	for _, sc := range scs {
		sc.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Ready})
	}

	// Wait for picker to be updated
	select {
	case picker := <-cc.NewPickerCh:
		// Pick multiple times - should work with TCP endpoints
		pickedSCs := make(map[balancer.SubConn]int)
		for i := 0; i < 10; i++ {
			result, err := picker.Pick(balancer.PickInfo{Ctx: ctx})
			if err != nil {
				t.Fatalf("Pick failed: %v", err)
			}
			pickedSCs[result.SubConn]++
		}

		// Both servers should be picked (round-robin)
		if len(pickedSCs) != 2 {
			t.Errorf("Expected 2 different SubConns to be picked, got %d", len(pickedSCs))
		}
	case <-ctx.Done():
		t.Fatal("Timeout waiting for picker")
	}
}

// TestMixedEndpointsIntegration tests a realistic scenario with mixed endpoints.
func TestMixedEndpointsIntegration(t *testing.T) {
	cc := testutils.NewBalancerClientConn(t)
	b := balancer.Get(Name).Build(cc, balancer.BuildOptions{})
	defer b.Close()

	// Simulate resolver returning endpoints
	// 1 local endpoint (shmem-capable)
	// 1 remote endpoint (TCP only)
	localEndpoint := resolver.Endpoint{
		Addresses: []resolver.Address{
			{Addr: "shm://test-segment"},
		},
	}
	localEndpoint = SetEndpointTransportPreference(localEndpoint, TransportPreferenceShmem)
	localEndpoint = SetLocalEndpoint(localEndpoint, true)

	remoteEndpoint := resolver.Endpoint{
		Addresses: []resolver.Address{
			{Addr: "192.168.1.100:50051"},
		},
	}

	state := balancer.ClientConnState{
		ResolverState: resolver.State{
			Endpoints: []resolver.Endpoint{remoteEndpoint, localEndpoint},
		},
	}

	if err := b.UpdateClientConnState(state); err != nil {
		t.Fatalf("UpdateClientConnState failed: %v", err)
	}

	// Wait for SubConns to be created
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var tcpSC, shmemSC *testutils.TestSubConn
	for i := 0; i < 2; i++ {
		select {
		case sc := <-cc.NewSubConnCh:
			if len(sc.Addresses) > 0 {
				if sc.Addresses[0].Addr == "shm://test-segment" {
					shmemSC = sc
				} else {
					tcpSC = sc
				}
			}
		case <-ctx.Done():
			t.Fatalf("Timeout waiting for SubConns")
		}
	}

	if shmemSC == nil {
		t.Fatal("Shmem SubConn not created")
	}
	if tcpSC == nil {
		t.Fatal("TCP SubConn not created")
	}

	// Make both ready
	shmemSC.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Ready})
	tcpSC.UpdateState(balancer.SubConnState{ConnectivityState: connectivity.Ready})

	// Wait for picker and verify shmem is preferred
	select {
	case picker := <-cc.NewPickerCh:
		result, err := picker.Pick(balancer.PickInfo{Ctx: ctx})
		if err != nil {
			t.Fatalf("Pick failed: %v", err)
		}
		if result.SubConn != shmemSC {
			t.Error("Expected shmem SubConn to be picked for local endpoint")
		}
	case <-ctx.Done():
		t.Fatal("Timeout waiting for picker")
	}
}

// TestExitIdle tests that ExitIdle triggers connections on idle SubConns.
func TestExitIdle(t *testing.T) {
	cc := testutils.NewBalancerClientConn(t)
	b := balancer.Get(Name).Build(cc, balancer.BuildOptions{})
	defer b.Close()

	// Create resolver state with an address
	addr := resolver.Address{Addr: "localhost:50051"}
	state := balancer.ClientConnState{
		ResolverState: resolver.State{
			Addresses: []resolver.Address{addr},
		},
	}

	if err := b.UpdateClientConnState(state); err != nil {
		t.Fatalf("UpdateClientConnState failed: %v", err)
	}

	// Wait for SubConn to be created
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var sc *testutils.TestSubConn
	select {
	case sc = <-cc.NewSubConnCh:
	case <-ctx.Done():
		t.Fatal("Timeout waiting for SubConn")
	}

	if sc == nil {
		t.Fatal("SubConn not created")
	}

	// Wait for Connect to be called
	select {
	case <-sc.ConnectCh:
		t.Log("Connect was called on SubConn")
	case <-ctx.Done():
		t.Fatal("Connect was not called on SubConn")
	}

	// Call ExitIdle - this should try to reconnect idle SubConns
	b.(exitIdler).ExitIdle()

	// This just verifies ExitIdle doesn't panic
	t.Log("ExitIdle completed without error")
}

// BenchmarkShmemPicker benchmarks the picker selection.
func BenchmarkShmemPicker(b *testing.B) {
	// Create test SubConns
	shmemSC := testutils.NewTestSubConn("shmem-sc")
	tcpSC := testutils.NewTestSubConn("tcp-sc")

	picker := &shmemPreferPicker{
		shmemSCs: []balancer.SubConn{shmemSC},
		tcpSCs:   []balancer.SubConn{tcpSC},
		allSCs:   []balancer.SubConn{shmemSC, tcpSC},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	info := balancer.PickInfo{Ctx: ctx}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = picker.Pick(info)
	}
}

// BenchmarkShmemPickerParallel benchmarks parallel picker selection.
func BenchmarkShmemPickerParallel(b *testing.B) {
	shmemSC := testutils.NewTestSubConn("shmem-sc")
	tcpSC := testutils.NewTestSubConn("tcp-sc")

	picker := &shmemPreferPicker{
		shmemSCs: []balancer.SubConn{shmemSC},
		tcpSCs:   []balancer.SubConn{tcpSC},
		allSCs:   []balancer.SubConn{shmemSC, tcpSC},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	info := balancer.PickInfo{Ctx: ctx}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = picker.Pick(info)
		}
	})
}
