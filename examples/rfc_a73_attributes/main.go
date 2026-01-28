//go:build linux || windows

/*
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
 */

/*
Package main demonstrates RFC A73 compliant transport selection using
the ShmemCapability, ShmemServiceConfig, and TransportSelector APIs.

RFC A73 Compliance:

This example shows how:
1. The Name Resolver signals shared memory capability via attributes
2. The TransportSelector chooses transport type based on attributes and policy
3. The clientconn uses NewShmemClient() when appropriate

Key Concepts:

1. ShmemCapability Attribute:
   When a resolver returns addresses, it can annotate them with ShmemCapability:

       cap := transport.ShmemCapability{
           Enabled:     true,
           SegmentName: "my_segment",
           Preferred:   true,
           Required:    false,
       }
       addr = transport.SetShmemCapability(addr, cap)

2. TransportSelector (Phase 2):
   The TransportSelector determines transport type based on attributes:

       selector := transport.NewTransportSelector(cfg)
       transportType := selector.SelectTransport(addr)
       // Returns TransportTypeHTTP2 or TransportTypeShmem

3. Fallback Logic:
   IsFallbackAllowed() determines if HTTP/2 fallback is allowed:

       if transport.IsFallbackAllowed(addr) {
           // Can fall back to HTTP/2 if shmem fails
       }

4. NewShmemClient():
   Creates shmem transport with same signature as NewHTTP2Client for
   seamless integration in clientconn.go.

Running This Example:

    go run examples/rfc_a73_attributes/main.go
*/
package main

import (
	"fmt"

	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/resolver"
)

func main() {
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║    RFC A73 Compliant Transport Selection Demo             ║")
	fmt.Println("║    Phases 1-3: Attributes, Selection, Fallback            ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Demonstrate ShmemCapability attribute usage
	demonstrateCapabilityAttributes()

	// Demonstrate ShmemServiceConfig usage
	demonstrateServiceConfig()

	// Demonstrate TransportSelector (Phase 2)
	demonstrateTransportSelector()

	// Demonstrate the full decision flow
	demonstrateTransportSelection()

	// Demonstrate fallback error handling (Phase 3)
	demonstrateFallbackErrorHandling()
}

func demonstrateCapabilityAttributes() {
	fmt.Println("1. ShmemCapability Attribute Demo")
	fmt.Println("   ─────────────────────────────────────────────────────────")

	// Create an address without shmem capability
	addr := resolver.Address{
		Addr:       "localhost:50051",
		ServerName: "my-service",
	}

	fmt.Printf("   Initial address: %s\n", addr.Addr)
	fmt.Printf("   IsShmemEnabled: %v\n", transport.IsShmemEnabled(addr))
	fmt.Printf("   IsFallbackAllowed: %v\n", transport.IsFallbackAllowed(addr))
	fmt.Println()

	// Add shmem capability (as a resolver would do)
	cap := transport.ShmemCapability{
		Enabled:     true,
		SegmentName: "my_service_segment",
		Preferred:   true,
		Required:    false, // Phase 2: fallback allowed
	}
	addr = transport.SetShmemCapability(addr, cap)

	fmt.Println("   After resolver adds ShmemCapability:")
	fmt.Printf("   IsShmemEnabled: %v\n", transport.IsShmemEnabled(addr))
	fmt.Printf("   IsShmemPreferred: %v\n", transport.IsShmemPreferred(addr))
	fmt.Printf("   IsFallbackAllowed: %v\n", transport.IsFallbackAllowed(addr))

	retrieved := transport.GetShmemCapability(addr)
	if retrieved != nil {
		fmt.Printf("   SegmentName: %s\n", retrieved.SegmentName)
		fmt.Printf("   Required: %v\n", retrieved.Required)
	}
	fmt.Println()
}

func demonstrateServiceConfig() {
	fmt.Println("2. ShmemServiceConfig Demo")
	fmt.Println("   ─────────────────────────────────────────────────────────")

	// Parse from JSON (as would come from service config)
	jsonCfg := `{
		"shmemPolicy": "preferred",
		"shmemFallbackEnabled": true,
		"shmemSegmentSizeBytes": 1048576
	}`

	cfg, err := transport.ParseShmemServiceConfig(jsonCfg)
	if err != nil {
		fmt.Printf("   Error parsing config: %v\n", err)
		return
	}

	fmt.Printf("   Parsed Policy: %s\n", cfg.Policy)
	fmt.Printf("   FallbackEnabled: %v\n", cfg.IsFallbackEnabled())
	fmt.Printf("   SegmentSizeBytes: %d\n", cfg.SegmentSizeBytes)
	fmt.Println()

	// Show policy behaviors
	fmt.Println("   Policy behaviors:")
	policies := []transport.ShmemTransportPolicy{
		transport.ShmemPolicyDisabled,
		transport.ShmemPolicyPreferred,
		transport.ShmemPolicyRequired,
		transport.ShmemPolicyAuto,
	}

	for _, policy := range policies {
		testCfg := &transport.ShmemServiceConfig{Policy: policy}
		fmt.Printf("   %s: ShouldUseShmem(capable=true)=%v, ShouldUseShmem(capable=false)=%v\n",
			policy,
			testCfg.ShouldUseShmem(true),
			testCfg.ShouldUseShmem(false))
	}
	fmt.Println()
}

func demonstrateTransportSelector() {
	fmt.Println("3. TransportSelector Demo (Phase 2)")
	fmt.Println("   ─────────────────────────────────────────────────────────")

	// Create addresses with different capabilities
	localAddr := resolver.Address{Addr: "localhost:50051"}
	localAddr = transport.SetShmemCapability(localAddr, transport.ShmemCapability{
		Enabled:     true,
		SegmentName: "local_segment",
	})

	remoteAddr := resolver.Address{Addr: "remote-host:50051"}

	// Create selector with default config
	selector := transport.NewTransportSelector(nil)

	fmt.Println("   Transport selection results:")
	fmt.Printf("   Local address (%s):\n", localAddr.Addr)
	fmt.Printf("     CanUseShmem: %v\n", transport.CanUseShmemForAddress(localAddr))
	fmt.Printf("     SelectedTransport: %s\n", selector.SelectTransport(localAddr))

	fmt.Printf("   Remote address (%s):\n", remoteAddr.Addr)
	fmt.Printf("     CanUseShmem: %v\n", transport.CanUseShmemForAddress(remoteAddr))
	fmt.Printf("     SelectedTransport: %s\n", selector.SelectTransport(remoteAddr))
	fmt.Println()

	// Show detailed selection
	fmt.Println("   Detailed selection for local address:")
	result := selector.SelectTransportWithDetails(localAddr)
	fmt.Printf("     Type: %s\n", result.Type)
	fmt.Printf("     SegmentName: %s\n", result.SegmentName)
	fmt.Printf("     FallbackAllowed: %v\n", result.FallbackAllowed)
	fmt.Println()

	// Show with Required policy
	requiredCfg := &transport.ShmemServiceConfig{Policy: transport.ShmemPolicyRequired}
	requiredSelector := transport.NewTransportSelector(requiredCfg)
	result = requiredSelector.SelectTransportWithDetails(localAddr)
	fmt.Println("   With Policy=Required:")
	fmt.Printf("     Type: %s\n", result.Type)
	fmt.Printf("     FallbackAllowed: %v (no fallback when required)\n", result.FallbackAllowed)
	fmt.Println()
}

func demonstrateTransportSelection() {
	fmt.Println("4. Full Transport Selection Decision Flow")
	fmt.Println("   ─────────────────────────────────────────────────────────")

	// Simulate addresses from resolver - some with shmem capability, some without
	addresses := []resolver.Address{
		{Addr: "localhost:50051", ServerName: "local-svc"},
		{Addr: "remote-host:50051", ServerName: "remote-svc"},
	}

	// Local address gets shmem capability
	addresses[0] = transport.SetShmemCapability(addresses[0], transport.ShmemCapability{
		Enabled:     true,
		SegmentName: "local_svc_segment",
		Preferred:   true,
	})

	// Create selector with auto policy
	cfg := transport.DefaultShmemServiceConfig()
	selector := transport.NewTransportSelector(cfg)

	fmt.Printf("   Service Config Policy: %s\n", cfg.Policy)
	fmt.Println()

	for _, addr := range addresses {
		transportType := selector.SelectTransport(addr)
		fallback := transport.IsFallbackAllowed(addr)

		fmt.Printf("   Address: %s\n", addr.Addr)
		fmt.Printf("     CanUseShmem: %v\n", transport.CanUseShmemForAddress(addr))
		fmt.Printf("     SelectedTransport: %s\n", transportType)
		fmt.Printf("     FallbackAllowed: %v\n", fallback)

		if transportType == transport.TransportTypeShmem {
			cap := transport.GetShmemCapability(addr)
			if cap != nil {
				fmt.Printf("     → clientconn calls NewShmemClient (segment: %s)\n", cap.SegmentName)
			}
		} else {
			fmt.Printf("     → clientconn calls NewHTTP2Client\n")
		}
		fmt.Println()
	}

	fmt.Println("   ─────────────────────────────────────────────────────────")
	fmt.Println("   RFC A73 Transport Selection Flow:")
	fmt.Println("   1. Resolver annotates addresses with ShmemCapability")
	fmt.Println("   2. Service config defines policy (auto/preferred/required)")
	fmt.Println("   3. TransportSelector.SelectTransport() chooses type")
	fmt.Println("   4. clientconn.createTransport() uses appropriate client:")
	fmt.Println("      - NewShmemClient() for shmem addresses")
	fmt.Println("      - NewHTTP2Client() for network addresses")
	fmt.Println("      - Falls back to HTTP/2 if shmem fails (when allowed)")
	fmt.Println()
}

func demonstrateFallbackErrorHandling() {
	fmt.Println("5. Fallback Error Handling Demo (Phase 3)")
	fmt.Println("   ─────────────────────────────────────────────────────────")

	// Demonstrate ShmemError types
	fmt.Println("   ShmemError Types:")
	errorCodes := []transport.ShmemErrorCode{
		transport.ShmemErrSegmentNotFound,
		transport.ShmemErrPermissionDenied,
		transport.ShmemErrConnectionRefused,
		transport.ShmemErrTimeout,
		transport.ShmemErrProtocolMismatch,
		transport.ShmemErrUnknown,
	}

	for _, code := range errorCodes {
		err := transport.NewShmemError(code, "example error")
		fmt.Printf("     %d: Retryable=%v, Permanent=%v\n",
			code, transport.IsShmemErrorRetryable(err), transport.IsShmemErrorPermanent(err))
	}
	fmt.Println()

	// Demonstrate fallback handler
	fmt.Println("   ShmemFallbackHandler Demo:")
	handler := transport.NewShmemFallbackHandler()

	// Simulate different error scenarios
	scenarios := []struct {
		name            string
		err             error
		fallbackAllowed bool
	}{
		{
			name:            "Segment not found (fallback allowed)",
			err:             transport.NewShmemError(transport.ShmemErrSegmentNotFound, "segment missing"),
			fallbackAllowed: true,
		},
		{
			name:            "Permission denied (fallback NOT allowed)",
			err:             transport.NewShmemError(transport.ShmemErrPermissionDenied, "access denied"),
			fallbackAllowed: false,
		},
		{
			name:            "Connection refused (fallback allowed)",
			err:             transport.NewShmemError(transport.ShmemErrConnectionRefused, "server not ready"),
			fallbackAllowed: true,
		},
	}

	for _, s := range scenarios {
		result := handler.HandleShmemError(s.err, s.fallbackAllowed)
		fmt.Printf("     %s:\n", s.name)
		fmt.Printf("       ShouldFallback: %v\n", result.ShouldFallback)
		if result.Error != nil {
			fmt.Printf("       Error: %v\n", result.Error)
		}
	}
	fmt.Println()

	// Show fallback count
	fmt.Printf("   Total fallbacks: %d\n", handler.FallbackCount())
	fmt.Println()

	// Demonstrate with address-based fallback decision
	fmt.Println("   Address-based Fallback Demo:")
	addr1 := transport.SetShmemCapability(resolver.Address{Addr: "localhost:50051"},
		transport.ShmemCapability{Enabled: true, SegmentName: "test1", Required: false})
	addr2 := transport.SetShmemCapability(resolver.Address{Addr: "localhost:50052"},
		transport.ShmemCapability{Enabled: true, SegmentName: "test2", Required: true})

	fmt.Printf("   Preferred address: IsFallbackAllowed=%v\n", transport.IsFallbackAllowed(addr1))
	fmt.Printf("   Required address:  IsFallbackAllowed=%v\n", transport.IsFallbackAllowed(addr2))
	fmt.Println()

	fmt.Println("   ─────────────────────────────────────────────────────────")
	fmt.Println("   RFC A73 Phase 3 Fallback Flow:")
	fmt.Println("   1. clientconn.createTransport() attempts shmem (if selected)")
	fmt.Println("   2. If shmem fails, error is classified as ShmemError")
	fmt.Println("   3. IsShmemErrorRetryable() determines retry behavior")
	fmt.Println("   4. If IsFallbackAllowed() and not Required policy:")
	fmt.Println("      → Falls back to HTTP/2 transparently")
	fmt.Println("   5. If Required policy and shmem fails:")
	fmt.Println("      → Returns error (no fallback)")
	fmt.Println()
}
