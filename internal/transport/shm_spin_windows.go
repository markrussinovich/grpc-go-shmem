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

// Windows spin-wait UPPER bound. See shm_spin_linux.go for the
// rationale: default behaviour is no spin, operators opt in via
// ConfigureShmSpinIterations. Windows WaitOnAddress costs ~40 µs
// (cgocall), so the operator-facing cap can be a bit higher than on
// Linux without the spin ever exceeding the cost of a kernel wait.
const (
	// spinIterationsLimit caps the maximum value the adaptive spin
	// cutoff can be configured to on Windows. ~280 µs at 7 ns/PAUSE.
	spinIterationsLimit = 40000
)
