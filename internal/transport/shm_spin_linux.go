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

package transport

// Linux spin-wait constants tuned for native futex which costs ~1-2µs per
// wake/wait cycle. Moderate spin counts balance latency vs CPU usage —
// too low (32) causes futex fallback on every frame, too high (2000+)
// starves the peer goroutine on shared hyperthreads.
const (
	// spinIterationsDefault: ~2.1µs of spinning (300 × 7ns PAUSE).
	// Covers typical SHM peer write latency for small/medium payloads.
	spinIterationsDefault = 300

	// spinIterationsMin: minimum adaptive floor.
	spinIterationsMin = 50

	// spinIterationsMax: cap for sustained throughput workloads.
	// ~28µs at 7ns/PAUSE — moderate because Linux futex has no
	// cgocall overhead, so falling back is cheap.
	spinIterationsMax = 4000

	// spinMoreBoost: when a MORE chunk is detected, boost spin to
	// cover the inter-chunk latency (~5-20µs). On Linux, futex
	// wake is cheap so we don't need the extreme 200K of Windows.
	// ~70µs at 7ns/PAUSE covers the typical chunk write time.
	spinMoreBoost = 10000
)
