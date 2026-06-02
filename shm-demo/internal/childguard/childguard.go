// Copyright 2026 gRPC SHM Demo authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package childguard ties a spawned child process's lifetime to its parent so
// the child (and its own descendants) is killed if the parent dies — even if
// the parent is terminated abruptly with an uncatchable signal (Windows
// TerminateProcess / SIGKILL), in which case the parent's deferred cleanup
// never runs.
//
// This matters for the demo's three-level process tree (shell -> engine ->
// grpc server): when the shell force-kills a stalled engine, the engine's
// deferred server teardown does not execute, so without this guard the grpc
// server would be orphaned and keep its shared-memory ring mapped.
//
// Usage:
//
//	childguard.Prepare(cmd)         // before cmd.Start()
//	cmd.Start()
//	release, _ := childguard.Guard(cmd) // after cmd.Start()
//	defer release()                 // when the child is reaped normally
package childguard

import "os/exec"

// Prepare configures cmd, before it is started, so the child is killed when
// this parent process dies. It must be called before cmd.Start(). On platforms
// where the guard is applied after start (Windows), this is a no-op.
func Prepare(cmd *exec.Cmd) { prepare(cmd) }

// Guard finalizes the parent-death guard after cmd has been started and returns
// a release func that frees any OS resources held for the guard. The release
// func is safe to call exactly once (e.g. via defer) when the child has been
// reaped; calling it does not, on its own, signal the child. On platforms where
// the guard is applied before start (Unix), this returns a no-op release.
func Guard(cmd *exec.Cmd) (release func(), err error) { return guard(cmd) }
