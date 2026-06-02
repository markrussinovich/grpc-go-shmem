// Copyright 2026 gRPC SHM Demo authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package protocol defines the NDJSON event contract between a benchmark
// engine (child process) and the web shell (parent). The engine writes one
// JSON object per line to stdout; human logs go to stderr.
package protocol

// Event is a single NDJSON message emitted by an engine.
type Event struct {
	// Type is one of: progress, result, done, error.
	Type string `json:"type"`

	Lang      string `json:"lang,omitempty"`      // go | dotnet
	Transport string `json:"transport,omitempty"` // tcp | uds | shm
	Phase     string `json:"phase,omitempty"`     // connect | latency | throughput

	// Round/Rounds annotate progress events when a transport is measured
	// multiple times and the final result is the median across rounds. Round
	// is 1-based; both are omitted for single-round runs.
	Round  int `json:"round,omitempty"`
	Rounds int `json:"rounds,omitempty"`

	PayloadBytes int `json:"payloadBytes,omitempty"`

	// Result metrics (set when Type == "result"). These intentionally omit
	// omitempty so a genuine zero is reported explicitly rather than dropped.
	LatencyP50Us float64 `json:"latencyP50Us"`
	LatencyP99Us float64 `json:"latencyP99Us"`
	MsgPerSec    float64 `json:"msgPerSec"`
	MBPerSec     float64 `json:"mbPerSec"`
	CPUSecPer1M  float64 `json:"cpuSecPer1M"`

	// Error carries a human-readable message when Type == "error".
	Error string `json:"error,omitempty"`
}

// Transports is the canonical demo order.
var Transports = []string{"tcp", "uds", "shm"}
