//go:build linux || windows

/*
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
 */

/*
Package main demonstrates RFC A73 compliant transport selection using
the ShmemCapability and ShmemServiceConfig APIs.

RFC A73 Compliance:

This example shows how the Name Resolver can signal shared memory transport
capability via resolver.Address.Attributes. The Load Balancer can then use
this information to select the appropriate transport.

Key Concepts:

1. ShmemCapability Attribute:
   When a resolver returns addresses, it can annotate them with ShmemCapability
   to indicate whether the endpoint supports shared memory transport:

       cap := transport.ShmemCapability{
           Enabled:     true,
           SegmentName: "my_segment",
           Preferred:   true,
       }
       addr = transport.SetShmemCapability(addr, cap)

2. Checking Shmem Support:
   Load balancers and transport layers can check if an address supports shmem:

       if transport.IsShmemEnabled(addr) {
           // Use shared memory transport
       }
       if transport.IsShmemPreferred(addr) {
           // Shmem is available AND preferred
       }

3. ShmemServiceConfig:
   Applications can configure transport selection policy via service config:

       cfg := &transport.ShmemServiceConfig{
           Policy: transport.ShmemPolicyPreferred, // or "disabled", "required", "auto"
           FallbackEnabled: &fallback,
       }

4. Transport Selection Decision:
   The ShouldUseShmem method combines policy and capability:

       if cfg.ShouldUseShmem(transport.IsShmemEnabled(addr)) {
           // Proceed with shared memory transport
       }

Running This Example:

This example demonstrates the attribute flow. For full gRPC integration,
see the shm_fullgrpc_test.go tests in the repository root.

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
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Demonstrate ShmemCapability attribute usage
	demonstrateCapabilityAttributes()

	// Demonstrate ShmemServiceConfig usage
	demonstrateServiceConfig()

	// Demonstrate the full decision flow
	demonstrateTransportSelection()
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
	fmt.Printf("   IsShmemPreferred: %v\n", transport.IsShmemPreferred(addr))
	fmt.Println()

	// Add shmem capability (as a resolver would do)
	cap := transport.ShmemCapability{
		Enabled:     true,
		SegmentName: "my_service_segment",
		Preferred:   true,
	}
	addr = transport.SetShmemCapability(addr, cap)

	fmt.Println("   After resolver adds ShmemCapability:")
	fmt.Printf("   IsShmemEnabled: %v\n", transport.IsShmemEnabled(addr))
	fmt.Printf("   IsShmemPreferred: %v\n", transport.IsShmemPreferred(addr))

	retrieved := transport.GetShmemCapability(addr)
	if retrieved != nil {
		fmt.Printf("   SegmentName: %s\n", retrieved.SegmentName)
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
		fmt.Printf("   %s: ShouldUseShmem(hasCapability=true)=%v, ShouldUseShmem(hasCapability=false)=%v\n",
			policy,
			testCfg.ShouldUseShmem(true),
			testCfg.ShouldUseShmem(false))
	}
	fmt.Println()
}

func demonstrateTransportSelection() {
	fmt.Println("3. Transport Selection Decision Flow")
	fmt.Println("   ─────────────────────────────────────────────────────────")

	// Simulate addresses from resolver - some with shmem capability, some without
	addresses := []resolver.Address{
		{Addr: "localhost:50051", ServerName: "local-svc"},     // Will get shmem capability
		{Addr: "remote-host:50051", ServerName: "remote-svc"},  // No shmem capability
	}

	// Local address gets shmem capability
	addresses[0] = transport.SetShmemCapability(addresses[0], transport.ShmemCapability{
		Enabled:     true,
		SegmentName: "local_svc_segment",
		Preferred:   true,
	})

	// Service config with "auto" policy
	cfg := transport.DefaultShmemServiceConfig() // Policy: auto

	fmt.Printf("   Service Config Policy: %s\n", cfg.Policy)
	fmt.Println()

	for _, addr := range addresses {
		hasCap := transport.IsShmemEnabled(addr)
		shouldUse := cfg.ShouldUseShmem(hasCap)

		fmt.Printf("   Address: %s\n", addr.Addr)
		fmt.Printf("     HasShmemCapability: %v\n", hasCap)
		fmt.Printf("     ShouldUseShmem: %v\n", shouldUse)

		if shouldUse {
			cap := transport.GetShmemCapability(addr)
			if cap != nil {
				fmt.Printf("     → Use SharedMemory transport (segment: %s)\n", cap.SegmentName)
			}
		} else {
			fmt.Printf("     → Use Network transport\n")
		}
		fmt.Println()
	}

	fmt.Println("   ─────────────────────────────────────────────────────────")
	fmt.Println("   This demonstrates RFC A73 compliant transport selection:")
	fmt.Println("   • Resolver annotates addresses with locality/capability")
	fmt.Println("   • Service config defines policy (auto/preferred/required/disabled)")
	fmt.Println("   • LB policy uses ShouldUseShmem() for transport selection")
	fmt.Println()
}
