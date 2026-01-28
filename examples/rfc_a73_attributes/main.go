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
the ShmCapability, ShmServiceConfig, and TransportSelector APIs.

RFC A73 Compliance:

This example shows how:
1. The Name Resolver signals shared memory capability via attributes
2. The TransportSelector chooses transport type based on attributes and policy
3. The clientconn uses NewShmClient() when appropriate

Key Concepts:

1. ShmCapability Attribute:
   When a resolver returns addresses, it can annotate them with ShmCapability:

       cap := transport.ShmCapability{
           Enabled:     true,
           SegmentName: "my_segment",
           Preferred:   true,
           Required:    false,
       }
       addr = transport.SetShmCapability(addr, cap)

2. TransportSelector (Phase 2):
   The TransportSelector determines transport type based on attributes:

       selector := transport.NewTransportSelector(cfg)
       transportType := selector.SelectTransport(addr)
       // Returns TransportTypeHTTP2 or TransportTypeShm

3. Fallback Logic:
   IsFallbackAllowed() determines if HTTP/2 fallback is allowed:

       if transport.IsFallbackAllowed(addr) {
           // Can fall back to HTTP/2 if shm fails
       }

4. NewShmClient():
   Creates shm transport with same signature as NewHTTP2Client for
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
	fmt.Println("â•”â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•—")
	fmt.Println("â•‘    RFC A73 Compliant Transport Selection Demo             â•‘")
	fmt.Println("â•‘    Phases 1-3: Attributes, Selection, Fallback            â•‘")
	fmt.Println("â•šâ•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•â•")
	fmt.Println()

	// Demonstrate ShmCapability attribute usage
	demonstrateCapabilityAttributes()

	// Demonstrate ShmServiceConfig usage
	demonstrateServiceConfig()

	// Demonstrate TransportSelector (Phase 2)
	demonstrateTransportSelector()

	// Demonstrate the full decision flow
	demonstrateTransportSelection()

	// Demonstrate fallback error handling (Phase 3)
	demonstrateFallbackErrorHandling()
}

func demonstrateCapabilityAttributes() {
	fmt.Println("1. ShmCapability Attribute Demo")
	fmt.Println("   â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€")

	// Create an address without shm capability
	addr := resolver.Address{
		Addr:       "localhost:50051",
		ServerName: "my-service",
	}

	fmt.Printf("   Initial address: %s\n", addr.Addr)
	fmt.Printf("   IsShmEnabled: %v\n", transport.IsShmEnabled(addr))
	fmt.Printf("   IsFallbackAllowed: %v\n", transport.IsFallbackAllowed(addr))
	fmt.Println()

	// Add shm capability (as a resolver would do)
	cap := transport.ShmCapability{
		Enabled:     true,
		SegmentName: "my_service_segment",
		Preferred:   true,
		Required:    false, // Phase 2: fallback allowed
	}
	addr = transport.SetShmCapability(addr, cap)

	fmt.Println("   After resolver adds ShmCapability:")
	fmt.Printf("   IsShmEnabled: %v\n", transport.IsShmEnabled(addr))
	fmt.Printf("   IsShmPreferred: %v\n", transport.IsShmPreferred(addr))
	fmt.Printf("   IsFallbackAllowed: %v\n", transport.IsFallbackAllowed(addr))

	retrieved := transport.GetShmCapability(addr)
	if retrieved != nil {
		fmt.Printf("   SegmentName: %s\n", retrieved.SegmentName)
		fmt.Printf("   Required: %v\n", retrieved.Required)
	}
	fmt.Println()
}

func demonstrateServiceConfig() {
	fmt.Println("2. ShmServiceConfig Demo")
	fmt.Println("   â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€")

	// Parse from JSON (as would come from service config)
	jsonCfg := `{
		"ShmPolicy": "preferred",
		"ShmFallbackEnabled": true,
		"ShmSegmentSizeBytes": 1048576
	}`

	cfg, err := transport.ParseShmServiceConfig(jsonCfg)
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
	policies := []transport.ShmTransportPolicy{
		transport.ShmPolicyDisabled,
		transport.ShmPolicyPreferred,
		transport.ShmPolicyRequired,
		transport.ShmPolicyAuto,
	}

	for _, policy := range policies {
		testCfg := &transport.ShmServiceConfig{Policy: policy}
		fmt.Printf("   %s: ShouldUseShm(capable=true)=%v, ShouldUseShm(capable=false)=%v\n",
			policy,
			testCfg.ShouldUseShm(true),
			testCfg.ShouldUseShm(false))
	}
	fmt.Println()
}

func demonstrateTransportSelector() {
	fmt.Println("3. TransportSelector Demo (Phase 2)")
	fmt.Println("   â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€")

	// Create addresses with different capabilities
	localAddr := resolver.Address{Addr: "localhost:50051"}
	localAddr = transport.SetShmCapability(localAddr, transport.ShmCapability{
		Enabled:     true,
		SegmentName: "local_segment",
	})

	remoteAddr := resolver.Address{Addr: "remote-host:50051"}

	// Create selector with default config
	selector := transport.NewTransportSelector(nil)

	fmt.Println("   Transport selection results:")
	fmt.Printf("   Local address (%s):\n", localAddr.Addr)
	fmt.Printf("     CanUseShm: %v\n", transport.CanUseShmForAddress(localAddr))
	fmt.Printf("     SelectedTransport: %s\n", selector.SelectTransport(localAddr))

	fmt.Printf("   Remote address (%s):\n", remoteAddr.Addr)
	fmt.Printf("     CanUseShm: %v\n", transport.CanUseShmForAddress(remoteAddr))
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
	requiredCfg := &transport.ShmServiceConfig{Policy: transport.ShmPolicyRequired}
	requiredSelector := transport.NewTransportSelector(requiredCfg)
	result = requiredSelector.SelectTransportWithDetails(localAddr)
	fmt.Println("   With Policy=Required:")
	fmt.Printf("     Type: %s\n", result.Type)
	fmt.Printf("     FallbackAllowed: %v (no fallback when required)\n", result.FallbackAllowed)
	fmt.Println()
}

func demonstrateTransportSelection() {
	fmt.Println("4. Full Transport Selection Decision Flow")
	fmt.Println("   â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€")

	// Simulate addresses from resolver - some with shm capability, some without
	addresses := []resolver.Address{
		{Addr: "localhost:50051", ServerName: "local-svc"},
		{Addr: "remote-host:50051", ServerName: "remote-svc"},
	}

	// Local address gets shm capability
	addresses[0] = transport.SetShmCapability(addresses[0], transport.ShmCapability{
		Enabled:     true,
		SegmentName: "local_svc_segment",
		Preferred:   true,
	})

	// Create selector with auto policy
	cfg := transport.DefaultShmServiceConfig()
	selector := transport.NewTransportSelector(cfg)

	fmt.Printf("   Service Config Policy: %s\n", cfg.Policy)
	fmt.Println()

	for _, addr := range addresses {
		transportType := selector.SelectTransport(addr)
		fallback := transport.IsFallbackAllowed(addr)

		fmt.Printf("   Address: %s\n", addr.Addr)
		fmt.Printf("     CanUseShm: %v\n", transport.CanUseShmForAddress(addr))
		fmt.Printf("     SelectedTransport: %s\n", transportType)
		fmt.Printf("     FallbackAllowed: %v\n", fallback)

		if transportType == transport.TransportTypeShm {
			cap := transport.GetShmCapability(addr)
			if cap != nil {
				fmt.Printf("     â†’ clientconn calls NewShmClient (segment: %s)\n", cap.SegmentName)
			}
		} else {
			fmt.Printf("     â†’ clientconn calls NewHTTP2Client\n")
		}
		fmt.Println()
	}

	fmt.Println("   â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€")
	fmt.Println("   RFC A73 Transport Selection Flow:")
	fmt.Println("   1. Resolver annotates addresses with ShmCapability")
	fmt.Println("   2. Service config defines policy (auto/preferred/required)")
	fmt.Println("   3. TransportSelector.SelectTransport() chooses type")
	fmt.Println("   4. clientconn.createTransport() uses appropriate client:")
	fmt.Println("      - NewShmClient() for shm addresses")
	fmt.Println("      - NewHTTP2Client() for network addresses")
	fmt.Println("      - Falls back to HTTP/2 if shm fails (when allowed)")
	fmt.Println()
}

func demonstrateFallbackErrorHandling() {
	fmt.Println("5. Fallback Error Handling Demo (Phase 3)")
	fmt.Println("   â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€")

	// Demonstrate ShmError types
	fmt.Println("   ShmError Types:")
	errorCodes := []transport.ShmErrorCode{
		transport.ShmErrSegmentNotFound,
		transport.ShmErrPermissionDenied,
		transport.ShmErrConnectionRefused,
		transport.ShmErrTimeout,
		transport.ShmErrProtocolMismatch,
		transport.ShmErrUnknown,
	}

	for _, code := range errorCodes {
		err := transport.NewShmError(code, "example error")
		fmt.Printf("     %d: Retryable=%v, Permanent=%v\n",
			code, transport.IsShmErrorRetryable(err), transport.IsShmErrorPermanent(err))
	}
	fmt.Println()

	// Demonstrate fallback handler
	fmt.Println("   ShmFallbackHandler Demo:")
	handler := transport.NewShmFallbackHandler()

	// Simulate different error scenarios
	scenarios := []struct {
		name            string
		err             error
		fallbackAllowed bool
	}{
		{
			name:            "Segment not found (fallback allowed)",
			err:             transport.NewShmError(transport.ShmErrSegmentNotFound, "segment missing"),
			fallbackAllowed: true,
		},
		{
			name:            "Permission denied (fallback NOT allowed)",
			err:             transport.NewShmError(transport.ShmErrPermissionDenied, "access denied"),
			fallbackAllowed: false,
		},
		{
			name:            "Connection refused (fallback allowed)",
			err:             transport.NewShmError(transport.ShmErrConnectionRefused, "server not ready"),
			fallbackAllowed: true,
		},
	}

	for _, s := range scenarios {
		result := handler.HandleShmError(s.err, s.fallbackAllowed)
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
	addr1 := transport.SetShmCapability(resolver.Address{Addr: "localhost:50051"},
		transport.ShmCapability{Enabled: true, SegmentName: "test1", Required: false})
	addr2 := transport.SetShmCapability(resolver.Address{Addr: "localhost:50052"},
		transport.ShmCapability{Enabled: true, SegmentName: "test2", Required: true})

	fmt.Printf("   Preferred address: IsFallbackAllowed=%v\n", transport.IsFallbackAllowed(addr1))
	fmt.Printf("   Required address:  IsFallbackAllowed=%v\n", transport.IsFallbackAllowed(addr2))
	fmt.Println()

	fmt.Println("   â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€")
	fmt.Println("   RFC A73 Phase 3 Fallback Flow:")
	fmt.Println("   1. clientconn.createTransport() attempts shm (if selected)")
	fmt.Println("   2. If shm fails, error is classified as ShmError")
	fmt.Println("   3. IsShmErrorRetryable() determines retry behavior")
	fmt.Println("   4. If IsFallbackAllowed() and not Required policy:")
	fmt.Println("      â†’ Falls back to HTTP/2 transparently")
	fmt.Println("   5. If Required policy and shm fails:")
	fmt.Println("      â†’ Returns error (no fallback)")
	fmt.Println()
}
