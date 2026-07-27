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

package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestDuplicateListenerRefused verifies the RFC "MUST refuse to bind an in-use
// name" requirement: a second listener on a name whose control segment reports
// ServerReady==true must be refused rather than silently unlinking the live
// segment (which would hijack the running server's clients).
func TestDuplicateListenerRefused(t *testing.T) {
	name := fmt.Sprintf("shmsc_dup_%d", time.Now().UnixNano())
	l1, err := NewShmListener(&ShmAddr{Name: name}, DefaultSegmentSize, DefaultRingASize, DefaultRingBSize)
	if err != nil {
		t.Fatalf("first listener: %v", err)
	}
	defer l1.Close()

	l2, err := NewShmListener(&ShmAddr{Name: name}, DefaultSegmentSize, DefaultRingASize, DefaultRingBSize)
	if err == nil {
		l2.Close()
		t.Fatalf("second listener on in-use name %q must be refused, but it succeeded", name)
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Errorf("duplicate-bind refusal error = %v, want it to mention 'already in use'", err)
	}
}

// TestSegmentFilePermissions verifies the RFC "backing file SHOULD be 0600"
// requirement: the control segment's backing file is owner-only.
func TestSegmentFilePermissions(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("file-mode check is Linux-specific (Windows uses named mappings, not a mode-bearing file)")
	}
	name := fmt.Sprintf("shmsc_perms_%d", time.Now().UnixNano())
	l, err := NewShmListener(&ShmAddr{Name: name}, DefaultSegmentSize, DefaultRingASize, DefaultRingBSize)
	if err != nil {
		t.Fatalf("listener: %v", err)
	}
	defer l.Close()

	// The control segment backing file is grpc_shm_<name><suffix> in /dev/shm
	// (preferred) or the temp dir (fallback).
	backing := "grpc_shm_" + name + shmControlSuffix
	candidates := []string{
		filepath.Join("/dev/shm", backing),
		filepath.Join(os.TempDir(), backing),
	}
	found := false
	for _, p := range candidates {
		fi, serr := os.Stat(p)
		if serr != nil {
			continue
		}
		found = true
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("control segment backing file %q mode = %#o, want 0600", p, perm)
		}
	}
	if !found {
		t.Skipf("could not locate control segment backing file (%v)", candidates)
	}
}
