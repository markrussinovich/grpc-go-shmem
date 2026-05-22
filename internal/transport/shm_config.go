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

package transport

import "os"

// This file is the single source of truth for runtime tunables that the
// shared-memory transport reads from process environment variables.
// Centralising the reads here makes the configuration surface auditable
// for security and gRFC review, and gives downstream readers one place
// to look when they ask "what knobs does this transport expose at run
// time?".
//
// Design conventions
//
//   - Every env var read by SHM production code that influences
//     on-ring or on-wire behaviour MUST be declared and read in this
//     file. Pure diagnostic / debug-log knobs (GRPC_SHM_DEBUG,
//     GRPC_SHM_FUTEX_DEBUG) are read at the source where they gate
//     logging because they have no semantic effect on the transport;
//     they are listed in the Knobs section below for discoverability.
//     Test / benchmark scaffolding knobs (BENCH_PROFILE,
//     BENCH_DIRTY_DEFAULT_POOL, SHM_SPIN_ITERS, SHM_BENCH_CPU,
//     SHM_BENCH_ZC) are local to bench harness files and not part of
//     the production runtime API.
//
//   - Programmatic toggles preferred. Deployment-time mode switches
//     (no-WINDOW_UPDATE, eventfd waker, spin-then-block tuning) are
//     exposed as exported Configure* functions in the transport
//     package rather than env vars. This is the same pattern as
//     ConfigureShmSpinIterations / ConfigureShmFlowControlForBench.
//     Env vars are reserved for things that genuinely cannot be
//     expressed as in-process API: cross-process child identity
//     (set by the parent), and per-process diagnostic logging.
//
//   - Defaults are production-safe. A fresh process with none of these
//     set behaves like the v3.4 production transport: eventfd waker
//     ON, no-WU flow control ON (Linux only; non-Linux uses the
//     futex / Windows-event fallback layer).
//
// Knobs (alphabetical)
//
//   GRPC_CROSS_PROCESS_CHILD
//       Set to a non-empty value when the current process is the child
//       half of a cross-process SHM connection that was spawned via
//       experimental SCM_RIGHTS handoff. Disables eventfd-based wake
//       primitives that today still assume same-process file
//       descriptors. Used as a guard until cross-process FD passing
//       lands.
//
//   GRPC_SHM_DEBUG  (diagnostic only; read at ring.go init)
//       Non-empty enables verbose ring-buffer debug logging on
//       stderr. Has no effect on on-wire behaviour.
//
//   GRPC_SHM_FUTEX_DEBUG  (diagnostic only; read at
//                          shm_futex_{linux,windows}.go init)
//       Non-empty enables verbose futex syscall logging. Has no
//       effect on on-wire behaviour.
//
// Removed knobs (do not reintroduce without strong reason)
//
//   SHM_DATASEG_WAKE   — was the eventfd-waker opt-in. eventfd is now
//                        the default wake primitive on Linux (toggle
//                        for tests / bench: ConfigureShmEventfdWakerForBench).
//   SHM_NO_WU          — was the no-WINDOW_UPDATE opt-in. no-WU is now
//                        the default v3.4 flow control mode (toggle
//                        for tests / bench: ConfigureShmNoWindowUpdate).
//   SHM_INPROC_WAKE    — was a same-process bench-only wake registry;
//                        removed for being unrepresentative of real
//                        deployments.

// shmEnv is the package-private bag of env-derived booleans. All
// fields are evaluated exactly once at init time.
type shmEnv struct {
	crossProcessChild bool
}

var shmEnvFlags = readShmEnv()

func readShmEnv() shmEnv {
	return shmEnv{
		crossProcessChild: os.Getenv("GRPC_CROSS_PROCESS_CHILD") != "",
	}
}
