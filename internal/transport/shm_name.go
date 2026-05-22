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

// Segment-name validation. Segment names appear in filesystem paths on
// Linux (/dev/shm/grpc_shm_<name>) and Windows (TempDir/grpc_shm_<name>
// plus named-mutex and event names). Allowing arbitrary characters
// would let a caller escape the intended namespace via path-traversal
// metacharacters, inject NUL bytes into kernel APIs, or collide with
// the internal "_ctl" / ".lock" / ".fds.sock" suffixes used by the
// transport. validateSegmentName enforces a conservative grammar
// applied at every public entry that constructs a segment.

package transport

import (
	"fmt"
	"strings"
)

// maxSegmentNameLen caps the user-supplied portion of a segment name.
// /dev/shm path limits, Windows mutex/event names (MAX_PATH minus
// namespace prefix and our own suffixes), and POSIX shm_open's 255-byte
// limit all comfortably accommodate 200 characters; this leaves room
// for our "grpc_shm_" prefix, "_conn_<id>" connection-id tail, and
// ".fds.sock" / ".lock" siblings.
const maxSegmentNameLen = 200

// validateSegmentName rejects names that would be unsafe to embed in
// filesystem paths or kernel object names. The accepted grammar is:
//
//	name := [A-Za-z0-9._-]{1,maxSegmentNameLen}
//
// The dot is allowed for callers that want to embed a version /
// tenant tag, but ".." is rejected explicitly to prevent path
// traversal. Names ending in the internal suffixes used by the
// transport ("_ctl" for the control segment, ".lock" for the
// control-segment lock file, ".fds.sock" for the SCM_RIGHTS handoff
// socket) are also rejected so external callers cannot collide with
// or shadow those internal artifacts.
func validateSegmentName(name string) error {
	if name == "" {
		return fmt.Errorf("shm: segment name must not be empty")
	}
	if len(name) > maxSegmentNameLen {
		return fmt.Errorf("shm: segment name too long (%d > %d)", len(name), maxSegmentNameLen)
	}
	if name == "." || name == ".." || strings.Contains(name, "..") {
		return fmt.Errorf("shm: segment name %q contains path-traversal sequence", name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		valid := (c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == '-'
		if !valid {
			return fmt.Errorf("shm: segment name %q contains invalid character %q at offset %d (allowed: A-Z a-z 0-9 . _ -)", name, c, i)
		}
	}
	if strings.HasSuffix(name, shmControlSuffix) {
		return fmt.Errorf("shm: segment name %q ends in reserved suffix %q", name, shmControlSuffix)
	}
	if strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("shm: segment name %q ends in reserved suffix %q", name, ".lock")
	}
	if strings.HasSuffix(name, ".fds.sock") {
		return fmt.Errorf("shm: segment name %q ends in reserved suffix %q", name, ".fds.sock")
	}
	return nil
}
