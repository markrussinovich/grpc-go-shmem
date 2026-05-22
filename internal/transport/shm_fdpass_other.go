//go:build !linux

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

import "errors"

// Non-Linux stub for the SCM_RIGHTS-based file-descriptor passing
// helpers. Windows uses named events (which the kernel exposes by
// name to any process that can open them), so there is no per-
// segment FD handoff to perform; macOS / others fall through to the
// futex-equivalent path.

func fdpassSocketPath(segPath string) string {
	return segPath + ".fds.sock"
}

func serveEventfdsForCreatorWaker(_ string, _ *shmDataSegWaker) (func(), error) {
	return func() {}, errors.New("fdpass: SCM_RIGHTS is only available on Linux")
}

func recvEventfdsFromCreator(_ string) ([]int, error) {
	return nil, errors.New("fdpass: SCM_RIGHTS is only available on Linux")
}

func newShmDataSegWakerFromOpenerFds(_ []int) (*shmDataSegWaker, error) {
	return nil, errors.New("fdpass: SCM_RIGHTS is only available on Linux")
}
