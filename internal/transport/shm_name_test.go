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

package transport

import (
	"strings"
	"testing"
)

// TestValidateSegmentName ensures the public segment-name grammar
// accepts the documented set of friendly names and rejects path-
// traversal / oversize / reserved-suffix inputs.
func TestValidateSegmentName(t *testing.T) {
	good := []string{
		"foo",
		"Foo_Bar-1.0",
		"a",
		"0",
		"1",
		"42",
		"abcdefghijklmnopqrstuvwxyz0123456789._-",
		strings.Repeat("x", maxSegmentNameLen),
	}
	for _, n := range good {
		if err := validateSegmentName(n); err != nil {
			t.Errorf("validateSegmentName(%q) = %v, want nil", n, err)
		}
	}

	bad := map[string]string{
		"":                                       "empty",
		".":                                      "path-traversal",
		"..":                                     "path-traversal",
		"a/b":                                    "invalid character",
		"a\\b":                                   "invalid character",
		"a..b":                                   "path-traversal",
		"foo bar":                                "invalid character",
		"foo\x00bar":                             "invalid character",
		"foo\tbar":                               "invalid character",
		"foo:bar":                                "invalid character",
		"foo;bar":                                "invalid character",
		"foo$bar":                                "invalid character",
		"foo`bar":                                "invalid character",
		"foo_ctl":                                "reserved suffix",
		"foo.lock":                               "reserved suffix",
		"foo.fds.sock":                           "reserved suffix",
		strings.Repeat("x", maxSegmentNameLen+1): "too long",
	}
	for n, why := range bad {
		err := validateSegmentName(n)
		if err == nil {
			t.Errorf("validateSegmentName(%q) = nil; want error (%s)", n, why)
			continue
		}
	}
}

// TestValidateSegmentName_DialerEntry verifies that DialShm itself
// rejects an invalid segment name with a structured ShmError carrying
// ShmErrInvalidConfig. This is the user-facing contract on
// shm://<name> targets that come through the experimental/shm dialer.
func TestValidateSegmentName_DialerEntry(t *testing.T) {
	// DialShm only reaches the validation check when opts.ConnectTimeout
	// is non-zero (otherwise it would block); we pass a tiny timeout via
	// DialOptions but the validation happens first so we never enter
	// the timing path.
	_, err := DialShm(t.Context(), "../etc/passwd", DefaultDialOptions())
	if err == nil {
		t.Fatal("DialShm with traversal name returned nil error")
	}
	var sErr *ShmError
	if !errorsAs(err, &sErr) {
		t.Fatalf("DialShm error %T = %v; want *ShmError", err, err)
	}
	if sErr.Code != ShmErrInvalidConfig {
		t.Errorf("DialShm error code = %v; want ShmErrInvalidConfig", sErr.Code)
	}
}

// errorsAs is a tiny replacement for errors.As that avoids an import
// cycle in this test file.
func errorsAs(err error, target **ShmError) bool {
	for err != nil {
		if e, ok := err.(*ShmError); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
