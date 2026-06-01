//go:build linux || windows

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

package shm

import "google.golang.org/grpc/internal/transport"

// ConfigureFlowControlForBench overrides the shared-memory transport's
// initial flow-control window and maximum HTTP/2 DATA-frame size
// process-wide. It exists so out-of-tree benchmark and demo programs
// (which cannot import google.golang.org/grpc/internal/transport) can
// drive the SHM transport into a fair-comparison profile that matches
// the HTTP/2 spec defaults used by TCP and Unix-socket transports
// (65535-byte window, 16384-byte frame).
//
// initialWindow sets the per-stream flow-control window the SHM
// transport starts from (production default: 32 MiB). maxFrame bounds
// the body of a single DATA frame the producer emits (production
// default: the RFC 7540 ceiling of 16 MiB-1); it is clamped to the
// RFC range [2^14, 2^24-1].
//
// MUST be called BEFORE any SHM client or server transport is dialed
// or listened — the values are captured once at transport
// construction. NOT safe to call from the data plane. Use
// ResetFlowControlForBench to restore the production defaults.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func ConfigureFlowControlForBench(initialWindow, maxFrame int) {
	transport.ConfigureShmFlowControlForBench(initialWindow)
	transport.ConfigureShmMaxFrameSizeForBench(maxFrame)
}

// ResetFlowControlForBench restores the SHM flow-control knobs to their
// production defaults (32 MiB window, RFC-ceiling frame size). It is
// the inverse of ConfigureFlowControlForBench.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func ResetFlowControlForBench() {
	transport.ResetShmFlowControlForBench()
}
