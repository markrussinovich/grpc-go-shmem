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

// Command demo is a single binary with three roles:
//
//	(default)        run the web shell UI
//	--role engine    run the Go benchmark and emit NDJSON to stdout
//	--role server    run a BenchmarkService server for one transport
//
// The shell spawns engine children, and each engine spawns server children, so
// the SHM transport is always exercised across real process boundaries.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"shmdemo/internal/engine"
	"shmdemo/internal/serverrole"
	"shmdemo/internal/shell"
	"shmdemo/internal/transportx"
)

func main() {
	var (
		role      = flag.String("role", "shell", "shell | engine | server")
		transport = flag.String("transport", "tcp", "tcp | uds | shm")
		endpoint  = flag.String("endpoint", "", "transport endpoint (empty = auto)")
		payload   = flag.Int("payload", 4096, "payload size in bytes")
		profile   = flag.String("profile", "max", "fair | max (flow-control profile)")
		warmupMs  = flag.Int("warmup-ms", 1000, "warmup window per phase (ms)")
		measureMs = flag.Int("measure-ms", 5000, "measurement window per phase (ms)")
		reps      = flag.Int("reps", 1, "measurement rounds per transport; median wins")
		port      = flag.Int("port", 0, "shell HTTP port (0 = auto)")
	)
	flag.Parse()

	prof := transportx.Profile(*profile)

	switch *role {
	case "server":
		// SHM window/frame are process-global knobs captured at listener
		// construction, so the profile must be applied before Run listens.
		transportx.ConfigureSHM(prof)
		if err := serverrole.Run(transportx.Kind(*transport), *endpoint, prof); err != nil {
			fmt.Fprintln(os.Stderr, "server error:", err)
			os.Exit(1)
		}
	case "engine":
		// Apply the SHM profile before any client dials a segment.
		transportx.ConfigureSHM(prof)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		opts := engine.Options{
			Payload: *payload,
			Profile: prof,
			Warmup:  time.Duration(*warmupMs) * time.Millisecond,
			Measure: time.Duration(*measureMs) * time.Millisecond,
			Reps:    *reps,
		}
		if *transport != "" && *transport != "all" {
			opts.Transports = []string{*transport}
		}
		if err := engine.Run(ctx, opts); err != nil {
			fmt.Fprintln(os.Stderr, "engine error:", err)
			os.Exit(1)
		}
	default: // shell
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		if err := shell.Run(ctx, *port); err != nil {
			fmt.Fprintln(os.Stderr, "shell error:", err)
			os.Exit(1)
		}
	}
}
