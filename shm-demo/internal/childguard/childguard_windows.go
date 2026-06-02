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

//go:build windows

package childguard

import (
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// prepare is a no-op on Windows; the guard is applied after Start via guard.
func prepare(_ *exec.Cmd) {}

// guard assigns the started child to a Job Object configured to kill all member
// processes when the job's last handle closes. Because the returned job handle
// is held only by this parent process, an abrupt parent termination closes the
// handle automatically, which tears down the child and any grandchildren. The
// returned release closes the job handle (killing the child if it is still
// alive, which is the desired behavior when reaping after a normal stop).
func guard(cmd *exec.Cmd) (func(), error) {
	noop := func() {}
	if cmd.Process == nil {
		return noop, nil
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return noop, err
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return noop, err
	}

	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return noop, err
	}
	defer windows.CloseHandle(h)

	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return noop, err
	}

	return func() { windows.CloseHandle(job) }, nil
}
