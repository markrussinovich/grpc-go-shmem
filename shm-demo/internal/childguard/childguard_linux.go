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

//go:build linux

package childguard

import (
	"os/exec"
	"syscall"
)

// prepare requests that the kernel send SIGKILL to the child when this parent
// process dies. Pdeathsig must be set before the child is started, so this is
// applied in Prepare rather than after Start.
func prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}

// guard is a no-op on Linux; the guard is established in prepare.
func guard(_ *exec.Cmd) (func(), error) { return func() {}, nil }
