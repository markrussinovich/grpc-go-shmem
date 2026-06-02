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

// Package serverrole runs the gRPC server child process for a single
// transport. It prints a single "READY <dialTarget>" line to stdout once the
// server is listening; all logs go to stderr.
package serverrole

import (
	"fmt"
	"os"

	"google.golang.org/grpc"

	"shmdemo/internal/bench"
	"shmdemo/internal/transportx"
	pb "shmdemo/proto/shmdemobench"
)

// Run starts a BenchmarkService server on the given transport and blocks until
// the listener is closed (e.g. when the process is killed by the parent). The
// profile selects the flow-control window applied to TCP/UDS server options;
// SHM's window/frame are applied process-wide by the caller via
// transportx.ConfigureSHM before Run is invoked.
func Run(kind transportx.Kind, endpoint string, profile transportx.Profile) error {
	lis, dialTarget, err := transportx.Listen(kind, endpoint)
	if err != nil {
		return fmt.Errorf("listen %s: %w", kind, err)
	}
	defer lis.Close()

	// Allow large payloads (up to the demo's 256 MiB option) on every
	// transport. The SHM ring stays at its 64 MiB default, so messages larger
	// than the ring stream through in frames.
	const maxMsgBytes = 512 * 1024 * 1024
	serverOpts := append([]grpc.ServerOption{
		grpc.MaxRecvMsgSize(maxMsgBytes),
		grpc.MaxSendMsgSize(maxMsgBytes),
	}, transportx.ServerOptions(kind, profile)...)
	s := grpc.NewServer(serverOpts...)
	pb.RegisterBenchmarkServiceServer(s, bench.NewServer())

	// Signal readiness to the parent with the exact dial target to use.
	fmt.Fprintf(os.Stdout, "READY %s\n", dialTarget)
	_ = os.Stdout.Sync()

	return s.Serve(lis)
}
