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
	"testing"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/resolver"
)

// TestTransportPreferenceAttribute tests that transport preference can be
// set and retrieved from addresses.
func TestTransportPreferenceAttribute(t *testing.T) {
	addr := resolver.Address{Addr: "localhost:50051"}

	// Default should be TransportPreferenceDefault
	if pref := GetTransportPreference(addr); pref != TransportPreferenceDefault {
		t.Errorf("Expected default preference, got %v", pref)
	}

	// Set shmem preference
	addr = SetTransportPreference(addr, TransportPreferenceShmem)
	if pref := GetTransportPreference(addr); pref != TransportPreferenceShmem {
		t.Errorf("Expected shmem preference, got %v", pref)
	}

	// Set TCP preference
	addr = SetTransportPreference(addr, TransportPreferenceTCP)
	if pref := GetTransportPreference(addr); pref != TransportPreferenceTCP {
		t.Errorf("Expected TCP preference, got %v", pref)
	}
}

// TestEndpointTransportPreference tests that transport preference can be
// set and retrieved from endpoints.
func TestEndpointTransportPreference(t *testing.T) {
	ep := resolver.Endpoint{
		Addresses: []resolver.Address{{Addr: "localhost:50051"}},
	}

	// Default should be TransportPreferenceDefault
	if pref := GetEndpointTransportPreference(ep); pref != TransportPreferenceDefault {
		t.Errorf("Expected default preference, got %v", pref)
	}

	// Set shmem preference
	ep = SetEndpointTransportPreference(ep, TransportPreferenceShmem)
	if pref := GetEndpointTransportPreference(ep); pref != TransportPreferenceShmem {
		t.Errorf("Expected shmem preference, got %v", pref)
	}
}

// TestLocalEndpointAttribute tests that local endpoint marking works.
func TestLocalEndpointAttribute(t *testing.T) {
	ep := resolver.Endpoint{
		Addresses: []resolver.Address{{Addr: "localhost:50051"}},
	}

	// Default should be not local
	if IsLocalEndpoint(ep) {
		t.Error("Expected endpoint to not be local by default")
	}

	// Mark as local
	ep = SetLocalEndpoint(ep, true)
	if !IsLocalEndpoint(ep) {
		t.Error("Expected endpoint to be local after marking")
	}

	// Mark as not local
	ep = SetLocalEndpoint(ep, false)
	if IsLocalEndpoint(ep) {
		t.Error("Expected endpoint to not be local after unmarking")
	}
}

// TestLocalAddressAttribute tests that local address marking works.
func TestLocalAddressAttribute(t *testing.T) {
	addr := resolver.Address{Addr: "localhost:50051"}

	// Default should be not local
	if IsLocalAddress(addr) {
		t.Error("Expected address to not be local by default")
	}

	// Mark as local
	addr = SetLocalAddress(addr, true)
	if !IsLocalAddress(addr) {
		t.Error("Expected address to be local after marking")
	}
}

// TestSortAddressesByPreference tests that addresses are sorted correctly.
func TestSortAddressesByPreference(t *testing.T) {
	// Create mixed addresses
	tcpAddr1 := resolver.Address{Addr: "remote1:50051"}
	tcpAddr2 := resolver.Address{Addr: "remote2:50051"}
	shmemAddr := SetTransportPreference(
		resolver.Address{Addr: "local:50051"},
		TransportPreferenceShmem,
	)
	localAddr := SetLocalAddress(
		resolver.Address{Addr: "localhost:50051"},
		true,
	)

	addrs := []resolver.Address{tcpAddr1, shmemAddr, tcpAddr2, localAddr}
	sorted := sortAddressesByPreference(addrs)

	// First addresses should be shmem/local
	if len(sorted) != 4 {
		t.Fatalf("Expected 4 addresses, got %d", len(sorted))
	}

	// First two should be shmem-preferred (shmemAddr and localAddr)
	// Last two should be TCP
	firstPref := GetTransportPreference(sorted[0])
	firstLocal := IsLocalAddress(sorted[0])
	secondPref := GetTransportPreference(sorted[1])
	secondLocal := IsLocalAddress(sorted[1])

	if firstPref != TransportPreferenceShmem && !firstLocal {
		t.Errorf("First address should be shmem or local, got pref=%v local=%v", firstPref, firstLocal)
	}
	if secondPref != TransportPreferenceShmem && !secondLocal {
		t.Errorf("Second address should be shmem or local, got pref=%v local=%v", secondPref, secondLocal)
	}

	// Last two should be TCP (no shmem preference, not local)
	thirdPref := GetTransportPreference(sorted[2])
	thirdLocal := IsLocalAddress(sorted[2])
	if thirdPref == TransportPreferenceShmem || thirdLocal {
		t.Errorf("Third address should be TCP, got pref=%v local=%v", thirdPref, thirdLocal)
	}
}

// TestBuilderRegistration tests that the balancer is registered.
func TestBuilderRegistration(t *testing.T) {
	b := balancer.Get(Name)
	if b == nil {
		t.Fatalf("Balancer %q not registered", Name)
	}
	if b.Name() != Name {
		t.Errorf("Expected name %q, got %q", Name, b.Name())
	}
}

// TestParseConfig tests config parsing.
func TestParseConfig(t *testing.T) {
	b := balancer.Get(Name)
	if b == nil {
		t.Fatalf("Balancer %q not registered", Name)
	}

	cfg, err := b.(balancer.ConfigParser).ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}
	if cfg == nil {
		t.Fatal("ParseConfig returned nil config")
	}
}
