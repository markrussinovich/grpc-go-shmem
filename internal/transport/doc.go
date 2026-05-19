/*
 * Copyright 2025 gRPC authors.
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
 */

// Package transport contains gRPC-Go's transport plumbing, including the
// experimental shared-memory transport (SHM). This file documents the SHM
// transport at the level of architecture, on-wire layout, lifecycle, and
// runtime tunables. The user-facing entry points (NewShmListener,
// WithShmTransport, ShmDiscovery*) live in the top-level
// google.golang.org/grpc package and are marked Experimental.
//
// # Goal
//
// SHM provides low-latency, high-throughput gRPC communication between
// two processes (or two goroutines within one process) on the same host
// without traversing the kernel network stack. Frames are written
// directly into a memory region that is mmapped into both peers and
// flow-controlled via in-region ring buffers. The transport is intended
// for the local-loopback case where TCP and UDS impose unnecessary
// per-message kernel work.
//
// # Architecture overview
//
//	Process A (client)                Process B (server)
//	+------------------+             +------------------+
//	|  ShmClient       |             |  ShmServer       |
//	|  Transport       |             |  Transport       |
//	+------+-----------+             +----------+-------+
//	       |   read/write frames                |
//	       v                                    v
//	+-------------------------------------------------+
//	|     Data segment  /dev/shm/grpc_shm_<rand>      |
//	|  +----------------+    +----------------+       |
//	|  | Ring A (C->S)  |    | Ring B (S->C)  |       |
//	|  |  ring header   |    |  ring header   |       |
//	|  |  ring buffer   |    |  ring buffer   |       |
//	|  +----------------+    +----------------+       |
//	+-------------------------------------------------+
//
// Each ClientConn has its own data segment. A separate, smaller "control
// segment" is created by the listener and used for the initial dial /
// accept handshake (segment-name exchange, capability negotiation). See
// shm_listener.go for the control-segment lifecycle.
//
// # Segment layout
//
//	Offset       Length    Field
//	0x00          8        SegmentMagic "GRPCSHM\0"
//	0x08          4        SegmentVersion (uint32, currently 1)
//	0x0C          4        flags
//	0x10          8        totalSize
//	0x18          8        ringAOff
//	0x20          8        ringACap (power-of-two)
//	0x28          8        ringBOff
//	0x30          8        ringBCap (power-of-two)
//	0x38          4        serverPID
//	0x3C          4        clientPID
//	0x40          4        serverReady flag
//	0x44          4        clientReady flag
//	0x48          4        closed flag
//	0x4C          4        pad
//	0x50          4        maxStreams
//	0x54-0x7F    44        reserved
//
// At the offsets carried by the header, each Ring has its own header
// (capacity, monotonic widx/ridx, waiter counts, futex sequence words)
// followed by the ring data area. RingHeader fields are defined in
// shm_segment.go.
//
// On creation, the server writes Magic, Version, capacities, and PIDs,
// then sets serverReady. On open, the client validates Magic and Version
// (see ValidateSegmentHeader) before touching any other field.
//
// # Wire frames
//
// Frames carried in each ring follow gRPC-over-HTTP/2 semantics adapted
// to a byte-stream-free medium: each frame has a fixed 9-byte header
// (Type/Flags/Length/StreamID) followed by Length bytes of payload. The
// frame layer is in shm_frame.go. Supported frame types include the
// usual HTTP/2 set (HEADERS, DATA, WINDOW_UPDATE, RST_STREAM, PING,
// GOAWAY, SETTINGS) plus a few SHM-specific control frames (Handshake
// Init/Resp, Connect/ConnectAck for stream setup).
//
// Stream multiplexing follows HTTP/2 stream-ID semantics: odd IDs are
// client-initiated, even IDs reserved. Concurrency is bounded by the
// header's maxStreams field.
//
// # Connection lifecycle
//
//  1. Listen. NewShmListener creates a control segment (a small
//     SegmentMagic-tagged region) at a caller-supplied path. The server
//     starts accepting via Accept.
//  2. Dial. The client calls DialShm with the same segment name. It
//     opens and validates the control segment, then exchanges a small
//     CONNECT frame to learn the dynamic name of a private data segment
//     the server allocates per-connection.
//  3. Handshake. The client opens the data segment, mmaps it, and the
//     SHM security handshaker (shm_security.go) exchanges identity
//     tokens over the data rings, producing a ShmAuthInfo carried up
//     via credentials.AuthInfo.
//  4. Steady state. Both sides run a writer loop (loopyWriter
//     equivalent) and a reader loop on their respective ring. Streams
//     are created, framed, and torn down per HTTP/2 semantics.
//  5. Close. Either side may set the segment's closed flag and signal
//     the wake primitive. The peer drains in-flight frames and unmaps;
//     the server unlinks the segment file from /dev/shm.
//
// # Wake primitives
//
// Ring backpressure is the canonical flow-control primitive. When a
// reader finds the ring empty (or a writer finds it full), it must park
// efficiently rather than spin. Three wake primitives are available:
//
//   - Futex (default, Linux). A uint32 sequence word in the ring
//     header. FUTEX_WAIT/FUTEX_WAKE on Linux gives sub-microsecond wake
//     latency with no per-connection file descriptor cost.
//   - Eventfd (default, Linux). A pair of eventfds per data segment
//     (one per direction) registered with Go's netpoller. Integrates
//     with select/epoll, scales to many connections, ~1µs wake
//     latency.
//   - Windows events. Named events created with CreateRingEvents are
//     the cross-mapping wake primitive on Windows where futex is
//     unavailable.
//
// All wake-primitive selection is centralized in shm_config.go (env
// vars) and chosen at process start. The hot path never calls
// os.Getenv.
//
// # Flow control
//
// Two modes are supported:
//
//   - HTTP/2 WINDOW_UPDATE (default). The transport advertises
//     INITIAL_WINDOW_SIZE in SETTINGS and the standard send-quota +
//     WINDOW_UPDATE accounting governs how much data each side may
//     have in flight. This matches the wire behaviour of the HTTP/2
//     transport and makes SHM/UDS/TCP bench comparisons honest.
//   - "no-WU" (default in v3.4, toggle via ConfigureShmNoWindowUpdate).
//     Sender skips acquireSendQuota and does not emit WINDOW_UPDATE;
//     receiver drops incoming WINDOW_UPDATE. The ring's natural
//     backpressure becomes the only flow-control limit. This is the
//     v3.4 baseline described in shm-rfc/. Both peers MUST be in the
//     same mode.
//
// # Security
//
// SHM operates between processes on the same host, so a cooperating
// peer is assumed to have proven locality by mapping the named segment.
// On top of that locality proof, ShmSecurityHandshaker exchanges
// per-side identity strings (default "pid:<getpid>"; configurable
// through credentials/shm.Options.Identity) which surface in
// ShmAuthInfo.RemoteIdentity. Callers can supply
// credentials/shm.Options.VerifyIdentity to reject unknown peers.
//
// The implementation does NOT defend against an attacker with write
// access to /dev/shm. Sites with that threat model must restrict
// /dev/shm permissions via the OS.
//
// # Resource footprint
//
// At steady state, a single ClientConn over SHM holds:
//
//   - 1 mmapped region per data segment (256 MiB by default).
//   - 0 open file descriptors for the segment file (the fd is closed
//     immediately after mmap; the kernel keeps the inode alive via the
//     VMA mapping).
//   - 2 eventfds per data segment (1 per direction) for the wake
//     primitive. ON by default on Linux. Toggle off for tests via
//     ConfigureShmEventfdWakerForBench.
//
// The control segment adds one mmapped region per listener; same fd
// model.
//
// # Cross-platform
//
// Linux is the primary target (futex, eventfd, /dev/shm). Windows is
// supported via memory-mapped files + named events. macOS lacks futex;
// the gRFC's Open Issues section tracks a kqueue-based design but it is
// not implemented today; macOS builds compile but skip the
// platform-specific wake fast paths.
//
// # Runtime tunables
//
// All environment variables read by production SHM code are declared
// and documented in shm_config.go. Test and benchmark scaffolding
// (BENCH_PROFILE, BENCH_DIRTY_DEFAULT_POOL, SHM_SPIN_ITERS,
// SHM_BENCH_CPU, SHM_BENCH_ZC) is local to its own files and not part
// of the production runtime API.
//
// # Ring buffer SPSC properties
//
// The foundation Ring type is a Single-Producer/Single-Consumer
// lock-free buffer:
//
//   - Capacity is rounded to a power of two so wrap-around uses a
//     single bitwise-AND.
//   - The writer owns widx, the reader owns ridx; each side only reads
//     the other's index via atomic load, eliminating contention.
//   - All I/O calls (Read, Write, ReserveWrite, PeekRead) are
//     non-blocking and return partial progress. Blocking semantics are
//     layered on top via the wake primitives described above.
//   - Cache-line padding between the producer- and consumer-owned
//     fields avoids false sharing.
//
// # Where to read next
//
//   - shm_segment.go      on-disk layout, header validation, segment lifecycle
//   - shm_ring.go / ring.go  lock-free SPSC ring buffer
//   - shm_frame.go        gRPC-over-SHM frame layout
//   - shm_listener.go     server-side accept + control segment
//   - shm_dialer.go       client-side dial + data-segment open
//   - shm_security.go    handshake / identity exchange
//   - shm_config.go      centralized env-var tunables
//   - shm-rfc/           the gRFC proposal in narrative form
package transport
