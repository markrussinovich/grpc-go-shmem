//go:build !linux && !windows

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

// Stub for the SHM credential-compatibility gate on platforms that do
// not build the SHM transport (anything other than linux / windows).
// The real implementation lives in shm_aware_dialer.go alongside the
// transport selection logic; on unsupported platforms there is no SHM
// transport at all so the gate is unconditionally a no-op. The
// platform-neutral http2_client.go calls this helper from the
// ClientTransportProvider escape hatch so non-SHM provider types
// remain unaffected.

package transport

// assertShmCompatibleCredentials is a no-op on platforms without the
// SHM transport: there is no SHM provider available so the rejection
// rules do not apply. Any custom ClientTransportProvider here is not
// the SHM transport and is the caller's responsibility to vet.
func assertShmCompatibleCredentials(_ ConnectOptions) error {
	return nil
}
