//go:build windows

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

// Windows spin-wait constants tuned for WaitOnAddress which goes through
// runtime.cgocall (~40µs per wait/wake cycle). Aggressive spinning keeps
// both reader and writer in user space, avoiding the costly kernel transition.
const (
	// spinIterationsDefault: ~14µs of spinning (2000 × 7ns PAUSE).
	// Covers the typical SHM peer write latency without falling back
	// to WaitOnAddress/cgocall.
	spinIterationsDefault = 2000

	// spinIterationsMin: minimum adaptive floor.
	spinIterationsMin = 500

	// spinIterationsMax: cap for sustained throughput workloads.
	// ~350µs at 7ns/PAUSE. The adaptive algorithm only reaches this
	// during sustained high-throughput transfers where spinning
	// consistently succeeds.
	spinIterationsMax = 50000

	// spinMoreBoost: when a MORE chunk is detected, boost spin to
	// ~1.4ms (200K × 7ns). While the reader spins, DataWaiters == 0,
	// so the writer skips WakeByAddress — eliminating cgocall on
	// BOTH sides. This is the Go equivalent of C#'s fire-and-forget.
	spinMoreBoost = 200000
)
