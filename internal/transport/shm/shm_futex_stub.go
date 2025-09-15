//go:build !linux || !(amd64 || arm64)

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

package shm

import (
	"errors"
)

var ErrUnsupported = errors.New("futex operations not supported on this platform")

// futexWait is not supported on this platform
func futexWait(addr *uint32, val uint32) error {
	return ErrUnsupported
}

// futexWake is not supported on this platform
func futexWake(addr *uint32, n int) (int, error) {
	return 0, ErrUnsupported
}
