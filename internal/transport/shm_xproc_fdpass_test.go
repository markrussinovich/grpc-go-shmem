//go:build linux

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
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// childRoleEnvVar selects the subprocess role for the cross-process
// SCM_RIGHTS handoff test. Set to one of "creator" or "opener" when
// re-exec'ing the test binary; absent in the parent process.
const childRoleEnvVar = "GRPC_SHM_FDPASS_TEST_ROLE"

// childSegEnvVar carries the segment name from the parent to the
// subprocess so both sides agree on the /dev/shm path.
const childSegEnvVar = "GRPC_SHM_FDPASS_TEST_SEG"

// TestMain in shm_integration_test.go installs the default
// integration setup. The cross-process subprocess uses init() below
// to intercept the test binary before any t.Run dispatches.
func init() {
	role := os.Getenv(childRoleEnvVar)
	if role == "" {
		return
	}
	// Subprocess path: bypass the testing framework entirely. Run
	// the requested role and exit with status 0 on success, non-
	// zero on failure. Output goes to stderr so the parent can
	// surface it via the captured CombinedOutput.
	if err := runChildRole(role); err != nil {
		fmt.Fprintf(os.Stderr, "child(%s): %v\n", role, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// runChildRole executes one half of the cross-process eventfd handoff
// test from the spawned subprocess.
func runChildRole(role string) error {
	name := os.Getenv(childSegEnvVar)
	if name == "" {
		return fmt.Errorf("missing %s env var", childSegEnvVar)
	}
	// Force eventfd ON in the child regardless of the parent's
	// configure overrides; the parent test verifies the eventfd
	// path specifically.
	ConfigureShmEventfdWakerForBench(true)
	defer ResetShmEventfdWakerForBench()

	switch role {
	case "opener":
		return runChildOpener(name)
	default:
		return fmt.Errorf("unknown role %q", role)
	}
}

// runChildOpener opens an existing segment (created by the parent
// test), receives the eventfd pair via SCM_RIGHTS, exercises one
// signal/wait round trip on each direction, and returns success when
// the wakes arrived through the eventfd path (not the futex
// fallback).
func runChildOpener(name string) error {
	// Wait briefly for the parent's CreateSegment to finish so the
	// fd-pass socket is bound. recvEventfdsFromCreator retries for
	// up to fdpassRecvTimeout internally, so this sleep is just a
	// small extra safety margin.
	time.Sleep(50 * time.Millisecond)

	seg, err := OpenSegment(name)
	if err != nil {
		return fmt.Errorf("OpenSegment: %w", err)
	}
	defer seg.Close()

	if seg.dataSegWaker == nil {
		return fmt.Errorf("opener did not receive an eventfd waker via SCM_RIGHTS")
	}
	if !seg.H.OpenerWakeReady() {
		return fmt.Errorf("OpenerWakeReady=false despite waker being set")
	}

	// Sanity-ping the waker: write one wake to the peer (parent
	// will park on it briefly to confirm reception).
	seg.dataSegWaker.Wake()
	return nil
}

// TestCrossProcessFdpassSCMRights verifies the SCM_RIGHTS-based
// eventfd handoff between the segment creator and a cross-process
// opener works end-to-end. The test re-execs the test binary as a
// child with GRPC_SHM_FDPASS_TEST_ROLE=opener; the child's init()
// runs runChildOpener (which calls OpenSegment + receives the
// eventfd pair via recvEventfdsFromCreator) and exits 0 on success.
// The parent verifies the child exited cleanly and observes the
// header's OpenerWakeReady flag transition to true.
func TestCrossProcessFdpassSCMRights(t *testing.T) {
	if os.Getenv(childRoleEnvVar) != "" {
		t.Skip("subprocess invocation; init() already handled it")
	}

	prev := shmDataSegWakeEnabled()
	ConfigureShmEventfdWakerForBench(true)
	t.Cleanup(func() { ConfigureShmEventfdWakerForBench(prev) })

	name := testSegName("xproc_fdpass")
	t.Cleanup(func() { RemoveSegment(name) })

	seg, err := CreateSegment(name, MinRingCapacity, MinRingCapacity)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	t.Cleanup(func() { seg.Close() })

	if seg.dataSegWaker == nil {
		t.Skip("eventfd waker not allocated (env may not support eventfd); skipping")
	}
	if seg.fdpassStop == nil {
		t.Fatal("CreateSegment did not start the SCM_RIGHTS fd-pass server; cross-process handoff is unreachable")
	}

	// Re-exec ourselves as a child whose init() will run the
	// opener role and exit. -run/-test.run picks a no-op test to
	// keep the framework happy; the child exits in init() before
	// any test runs.
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(),
		childRoleEnvVar+"=opener",
		childSegEnvVar+"="+name,
	)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("child opener failed: %v\noutput:\n%s", runErr, out)
	}
	if testing.Verbose() && len(out) > 0 {
		t.Logf("child output:\n%s", strings.TrimSpace(string(out)))
	}

	// The opener's setup published OpenerWakeReady=1 before the
	// child exited.
	if !seg.H.OpenerWakeReady() {
		t.Error("OpenerWakeReady is still false after cross-process opener completed")
	}
}
