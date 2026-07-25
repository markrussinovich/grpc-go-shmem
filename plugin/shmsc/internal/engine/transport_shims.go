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

package engine

import "errors"

// This file extracts the few small, self-contained declarations the SHM engine
// borrowed from google.golang.org/grpc/internal/transport's transport.go (which
// as a whole is the CORE transport contract and is NOT part of this
// self-contained engine). Keeping copies here lets the engine build without any
// internal/* dependency (enforced by TestNoInternalImports).

// noCopy may be added to structs which must not be copied after the first use.
//
// See https://golang.org/issues/8005#issuecomment-190753527 for details. It
// implements sync.Locker so `go vet`'s copylocks check flags accidental copies.
type noCopy struct{}

// Lock is a no-op used by the -copylocks checker.
func (*noCopy) Lock() {}

// Unlock is a no-op used by the -copylocks checker.
func (*noCopy) Unlock() {}

// errStreamDone is returned by transport operations that target a stream which
// has already finished.
var errStreamDone = errors.New("the stream is done")

// BDP-estimator tuning constants used by the SHM flow-control estimator
// (shmBDPEstimator). Extracted from the HTTP/2 bdp_estimator.go, which is not
// otherwise part of this engine (it pulls the HTTP/2 loopy-writer control
// machinery the SHM engine does not use).
const (
	alpha = 0.9
	beta  = 0.66
	gamma = 2
)
