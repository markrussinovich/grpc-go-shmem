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

// Package transportx isolates the ONLY code that differs between the three
// transports. The gRPC service, server handlers, and client call code are
// byte-for-byte identical across TCP, UDS, and SHM; the demo's whole point is
// that switching transport is just a different listener (server) and a
// different target + one dial option (client).
package transportx

import (
	"fmt"
	"net"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/experimental/shm"
)

// Kind identifies a transport.
type Kind string

const (
	TCP Kind = "tcp"
	UDS Kind = "uds"
	SHM Kind = "shm"
)

// Profile selects the flow-control / framing tuning applied to every
// transport. It lets the demo contrast a strict apples-to-apples run
// against a run tuned for local IPC.
type Profile string

const (
	// Fair is the apples-to-apples baseline. Every transport is pinned to the
	// HTTP/2 spec defaults: a 65535-byte flow-control window and a 16384-byte
	// DATA frame. TCP and UDS carry the window via per-connection dial/server
	// options; the SHM transport reads its window and frame from the
	// process-global knobs set by ConfigureSHM. Every transport therefore
	// throttles to the same small window and emits the same small frames per
	// message, isolating raw transport cost.
	Fair Profile = "fair"

	// Max is "highest performance": the flow-control window is opened wide
	// (effectively disabling per-message WINDOW_UPDATE round-trips) and the
	// SHM frame size is left at the large local-IPC default. Where a knob
	// is not settable (TCP/UDS frame size is fixed at 16 KiB by gRPC), it
	// is simply left alone.
	Max Profile = "max"
)

const (
	// fairWindow / fairFrame are the HTTP/2 spec defaults.
	fairWindow int32 = 65535
	fairFrame  int   = 16384

	// maxWindow opens the flow-control window wide for the highest-
	// performance profile (64 MiB matches the SHM ring size).
	maxWindow int32 = 64 * 1024 * 1024
)

// ConfigureSHM applies the SHM-specific flow-control profile process-wide.
// SHM window and frame size are process-global knobs captured at transport
// construction, so this MUST be called once — before any SHM listener or
// client is created — in BOTH the client (engine) and server processes.
//
// The non-SHM transports (TCP/UDS) carry their window via per-connection
// dial/server options instead; see ClientOptions and ServerOptions.
func ConfigureSHM(p Profile) {
	switch p {
	case Fair:
		shm.ConfigureFlowControlForBench(int(fairWindow), fairFrame)
	default: // Max
		shm.ResetFlowControlForBench()
	}
}

// windowFor returns the flow-control window for the given profile.
func windowFor(p Profile) int32 {
	if p == Fair {
		return fairWindow
	}
	return maxWindow
}

// Listen creates a server-side listener for the given transport.
//
// endpoint meaning:
//   - TCP: ignored; a free 127.0.0.1 port is chosen automatically.
//   - UDS: filesystem path of the unix domain socket.
//   - SHM: shared-memory segment name.
//
// It returns the listener and the dial target the client must use to reach it.
func Listen(kind Kind, endpoint string) (lis net.Listener, dialTarget string, err error) {
	switch kind {
	case TCP:
		lis, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, "", err
		}
		return lis, lis.Addr().String(), nil

	case UDS:
		// Remove a stale socket file from a previous run before binding.
		_ = os.Remove(endpoint)
		lis, err = net.Listen("unix", endpoint)
		if err != nil {
			return nil, "", err
		}
		return lis, "unix:" + endpoint, nil

	case SHM:
		lis, err = shm.NewListener(endpoint, nil)
		if err != nil {
			return nil, "", err
		}
		return lis, "shm://" + endpoint, nil

	default:
		return nil, "", fmt.Errorf("unknown transport %q", kind)
	}
}

// Dial creates a client connection for the given transport and dial target.
//
// Note how little differs: every transport uses grpc.NewClient with insecure
// credentials; only SHM adds the single shm.WithTransport() dial option. The
// profile contributes the flow-control window for TCP/UDS (SHM's window is
// applied process-wide via ConfigureSHM).
func Dial(kind Kind, dialTarget string, p Profile) (*grpc.ClientConn, error) {
	// Allow large payloads (up to the demo's 256 MiB option). The default gRPC
	// receive limit is only 4 MiB, which would reject bigger messages.
	const maxMsgBytes = 512 * 1024 * 1024
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxMsgBytes),
			grpc.MaxCallSendMsgSize(maxMsgBytes),
		),
	}
	if kind == SHM {
		// SHM window/frame are set process-wide by ConfigureSHM; adding a
		// per-dial window here would shadow that, so we leave it out.
		opts = append([]grpc.DialOption{shm.WithTransport()}, opts...)
	} else {
		w := windowFor(p)
		opts = append(opts,
			grpc.WithInitialWindowSize(w),
			grpc.WithInitialConnWindowSize(w),
		)
	}
	return grpc.NewClient(dialTarget, opts...)
}

// ServerOptions returns the grpc.ServerOptions that carry the flow-control
// profile for the given transport. SHM's window is applied process-wide via
// ConfigureSHM, so only TCP/UDS receive per-server window options here.
func ServerOptions(kind Kind, p Profile) []grpc.ServerOption {
	if kind == SHM {
		return nil
	}
	w := windowFor(p)
	return []grpc.ServerOption{
		grpc.InitialWindowSize(w),
		grpc.InitialConnWindowSize(w),
	}
}
