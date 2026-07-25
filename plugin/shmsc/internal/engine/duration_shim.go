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

import (
	"strconv"
	"time"
)

// This file vendors the single tiny grpc-timeout encoding helper the SHM engine
// borrowed from google.golang.org/grpc/internal/grpcutil, so this module carries
// NO internal/* dependency (enforced by TestNoInternalImports). Kept
// byte-for-byte equivalent to the upstream implementation.

const grpcMaxTimeoutValue int64 = 100000000 - 1

// grpcTimeoutDiv does integer division and rounds the result up.
func grpcTimeoutDiv(d, r time.Duration) int64 {
	if d%r > 0 {
		return int64(d/r + 1)
	}
	return int64(d / r)
}

// encodeDuration encodes a duration into the grpc-timeout header format.
func encodeDuration(t time.Duration) string {
	if t <= 0 {
		return "0n"
	}
	if d := grpcTimeoutDiv(t, time.Nanosecond); d <= grpcMaxTimeoutValue {
		return strconv.FormatInt(d, 10) + "n"
	}
	if d := grpcTimeoutDiv(t, time.Microsecond); d <= grpcMaxTimeoutValue {
		return strconv.FormatInt(d, 10) + "u"
	}
	if d := grpcTimeoutDiv(t, time.Millisecond); d <= grpcMaxTimeoutValue {
		return strconv.FormatInt(d, 10) + "m"
	}
	if d := grpcTimeoutDiv(t, time.Second); d <= grpcMaxTimeoutValue {
		return strconv.FormatInt(d, 10) + "S"
	}
	if d := grpcTimeoutDiv(t, time.Minute); d <= grpcMaxTimeoutValue {
		return strconv.FormatInt(d, 10) + "M"
	}
	return strconv.FormatInt(grpcTimeoutDiv(t, time.Hour), 10) + "H"
}
