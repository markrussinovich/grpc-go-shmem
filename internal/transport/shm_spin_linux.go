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

// Linux spin-wait UPPER bounds. The actual spin behaviour is controlled
// by ConfigureShmSpinIterations (or the dial / server option that wraps
// it) — the constants below cap how aggressive an operator can ask
// the implementation to be on Linux.
//
// The DEFAULT spin behaviour is *no spin*. Reviewer (Doug) flagged that
// busy-spinning costs CPU that UDS / TCP don't incur, and the project's
// own anti-busy-wait rule (copilot-instructions) says spinning should
// not tie up a CPU. Operators that want sub-µs latency for hot streams
// must explicitly call ConfigureShmSpinIterations(n) (or pass the
// matching dial / server option) to trade CPU for latency.
//
// Why this cap is ~225 µs (not lower): empirically the gap between
// "writer commits" and "reader sees data" in this codebase's full
// gRPC stack — even on a quiescent dedicated core — is on the order of
// 30–100 µs (covers the handler dispatch, gRPC framework overhead,
// goroutine scheduling, and ring memory ordering). A spin cap below
// that range almost never catches the data and devolves to "pay
// spin cost AND futex cost". The cap is well below scheduler quantum
// (~10 ms) so a runnable peer is never starved.
const (
	// spinIterationsLimit caps the maximum value the adaptive spin
	// cutoff can be configured to on Linux. ~225 µs at 7 ns/PAUSE.
	spinIterationsLimit = 32000
)
