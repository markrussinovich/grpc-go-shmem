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

import (
	"fmt"
	"net/url"
	"strconv"
)

// NewURLListener creates a shared-memory listener from a URL of the form
//
//	shm://name?cap=262144
//
// where cap is the per-ring capacity in bytes (power of two). Both rings use
// the same capacity in this helper.
func NewURLListener(raw string) (*ShmListener, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse shm url: %w", err)
	}
	if u.Scheme != "shm" {
		return nil, fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}
	name := u.Host
	if name == "" {
		// Allow shm://name (name in path) as well
		if u.Path != "" {
			name = u.Path
			if len(name) > 0 && name[0] == '/' {
				name = name[1:]
			}
		}
	}
	if name == "" {
		return nil, fmt.Errorf("missing shm name")
	}
	capStr := u.Query().Get("cap")
	capVal := int(DefaultRingASize)
	if capStr != "" {
		v64, err := strconv.ParseUint(capStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid cap: %w", err)
		}
		capVal = int(v64)
	}
	return NewShmListener(&ShmAddr{Name: name}, DefaultSegmentSize, uint64(capVal), uint64(capVal))
}
