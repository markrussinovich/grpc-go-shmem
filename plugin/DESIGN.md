# SHM Transport Plugin — Detailed Design

Status: **implemented (in-module POC)**. Implementation base: grpc-go-shmem **`origin/master`**
(monolithic SHM). The branch `feat/shm-plugin-poc` is cut from `origin/master`. The §4 "core
changes" (registry selection + stream-interface promotion + the optional-WriteProto capability) are
**done and green on Linux** (build/vet/e2e/SHM tests); the plugin reuses the engine via an in-module
adapter. What remains: the three-arm benchmark numbers and relocating the engine out of `internal/`
for a truly external split. Decision on the message-typed boundary is settled: **Position A** —
byte-based interface, `INLINE_TX` excluded as a first-party-only optimization (see §4, §6, §9).

## 1. Goal

Package the existing SHM transport as an **external plugin** on an exported pluggable-transport API,
and measure it against the monolithic build to show the API layer adds little overhead. This is the
"prove it with an implementation" artifact, not a spec.

Non-goals: finalizing the public API; cross-OS ABI; re-deriving performance.

## 2. Requirements

1. Reuse the SHM engine from `origin/master` — rings, bootstrap, wake, framing.
2. Expose a clean API structurally aligned with L37 "Go Custom Transports" (grpc/proposal#103): a
   name-keyed registry plus transport/stream interfaces, so the plugin depends on an exported surface
   rather than `internal/*`.
3. Plugin in its own module/folder with a README.
4. Base = `origin/master`.

## 3. How grpc-go drives a transport today (ground truth)

The seam must fit the real model, verified on `origin/master`:

- **Client, concrete.** `csAttempt.transportStream` is a concrete `*transport.ClientStream`
  (`stream.go`). Reads go through `parser{r: stream}` calling `ReadMessageHeader([]byte)` then
  `Read(n int) (mem.Buffer)`, each feeding `updateWindow(n)` (`internal/transport/transport.go`).
- **Server, push.** grpc drives the server with
  `ServerTransport.HandleStreams(ctx, func(*ServerStream))` (`server.go`); there is **no**
  `Accept()`/`ServeTransport`. Per-stream goroutines are spun inside that callback (`handleStream`),
  with stats / channelz / quotas / graceful-stop owned by `grpc.Server`.
- **Transport contract.** A server transport must satisfy the **unexported** `internalServerTransport`
  (`write`, `writeProto`, `writeHeader`, `writeStatus`, `adjustWindow`, `updateWindow`,
  `incrMsgRecv`), all taking `s *ServerStream` (`internal/transport/transport.go`).
- **Selection is hardcoded.** `clientconn.go createTransport` branches on `IsShmEnabled(addr)`;
  `NewServerTransport` builds HTTP/2 unless the accepted `net.Conn` implements the ad-hoc
  `ServerTransportProvider`. There is no registry, and `resolver.Address` has no transport-type field.

Implication: a plugin is blocked today because selection is hardcoded **and** the stream types it
must produce/consume are concrete structs with unexported coupling.

## 4. The seam — and the core changes it requires

This is **not** a pure add-on; grpc-go core stream plumbing changes. Three changes:

1. **Client registry (L37).** Add `resolver.Address.TransportType` and
   `transport/client.{Register,Get}`. `createTransport` looks up a builder by `TransportType`
   instead of the `IsShmEnabled` branch.
2. **Promote concrete streams to interfaces.** `csAttempt.transportStream` becomes an interface
   `transport.ClientStream`; `HandleStreams`'s callback takes an interface `transport.ServerStream`.
   The interfaces expose the contract core already uses — the `parser` recv pair, the window hook,
   the write path, plus the retry / header / compression methods core calls — so `recv` / `parser`
   and the byte write path route through them. `WriteProto` (INLINE_TX) is deliberately left OUT of
   the interface and kept as an optional first-party capability (Position A, see §9).
   This touches grpc-go core hot paths (`stream.go` `recv`/`csAttempt`, `server.go` `handleStream`),
   not just the HTTP/2 transport.
3. **Server builder via `HandleStreams` (push).** Register a `ServerTransportBuilder` whose product
   implements `HandleStreams`; the plugin's listener feeds accepted connections to it. (L37's
   pull-model `ServeTransport` / `TransportListener.Accept` is the eventual target, but the push
   model matches current grpc-go and the engine already implements `HandleStreams`, so the POC uses
   push.)

Exported interfaces — byte/stream-based to match current grpc-go's real contract and reuse the
engine; L37's message-based granularity is deferred (§9):

```go
// google.golang.org/grpc/transport/client + .../server  (exported aliases over the
// in-tree contract; re-export CallHdr, WriteOptions, ConnectOptions, ServerConfig, …)

package client
func Register(name string, b Builder) // name == resolver.Address.TransportType
type Builder interface {
    Build(connectCtx, ctx context.Context, addr resolver.Address, opts BuildOptions) (ClientTransport, error)
}
type ClientTransport interface {
    NewStream(ctx context.Context, callHdr *transport.CallHdr, h stats.Handler) (ClientStream, error)
    Close(err error); GracefulClose()
    Error() <-chan struct{}; GoAway() <-chan struct{}
    GetGoAwayReason() (GoAwayReason, string); Peer() *peer.Peer
}
type ClientStream interface {
    // write — byte-based ONLY. No WriteProto: INLINE_TX is an optional first-party
    // capability detected by assertion (writeproto_fastpath.go), not part of the contract.
    Write(hdr []byte, data mem.BufferSlice, opts *transport.WriteOptions) error
    // recv (the parser contract core already uses)
    ReadMessageHeader(h []byte) error
    Read(n int) (mem.BufferSlice, error)
    RecvCompress() string
    Header() (metadata.MD, error)
    Trailer() metadata.MD
    Status() *status.Status
    // lifecycle / retry (called from stream.go shouldRetry/finish)
    Context() context.Context
    Done() <-chan struct{}
    Unprocessed() bool
    TrailersOnly() bool
    BytesReceived() bool
    Close(err error)
}

package server
func Register(name string, b Builder)
type Builder interface {
    // conn is the accepted connection the plugin's listener produced (for SHM, the bootstrap shim
    // carrying the segment), mirroring today's NewServerTransport(conn, config).
    Build(conn net.Conn, opts BuildOptions) (ServerTransport, error)
}
type ServerTransport interface {
    HandleStreams(ctx context.Context, onStream func(ServerStream))
    Drain(debugData string)
    Close(err error)
    Peer() *peer.Peer
}
type ServerStream interface {
    // write — byte-based ONLY (no WriteProto; see the client note above)
    Write(hdr []byte, data mem.BufferSlice, opts *transport.WriteOptions) error
    WriteStatus(st *status.Status) error
    SendHeader(md metadata.MD) error
    SetHeader(md metadata.MD) error
    SetTrailer(md metadata.MD) error
    Header() (metadata.MD, error)
    Trailer() metadata.MD
    HeaderWireLength() int
    // recv (parser contract)
    ReadMessageHeader(h []byte) error
    Read(n int) (mem.BufferSlice, error)
    RecvCompress() string
    // identity / compression (called from server.go handleStream)
    Method() string
    Context() context.Context
    SetContext(ctx context.Context)
    SendCompress() string
    SetSendCompress(string) error
    ContentSubtype() string
    ClientAdvertisedCompressors() []string
}
```

These list the load-bearing subset; the full set core calls today is larger (see §9.2). Flow-control
is part of this contract too — the window hooks `updateWindow` / `adjustWindow` / `incrMsgRecv` (§6)
move onto the stream alongside these. `BuildOptions` carries what `ConnectOptions`/`ServerConfig`
carry today: stats handlers, tap, keepalive, initial window sizes, transport credentials/handshaker,
and the `onClose`/`Drain` lifecycle callbacks.

The **engine's** data path (marshal-into-ring, parse-in-place, wake) is unchanged; what changes is
**core's** stream plumbing (concrete → interface) and the selection registry. The benchmark (§8)
measures exactly this: the same engine reached through interfaces (plugin) vs through concrete
structs (monolithic).

## 5. Plugin module + reuse boundary

```
plugin/                       separate Go module → external repo later
  README.md  DESIGN.md
  go.mod                      requires google.golang.org/grpc (the seam) only
  shm/                        builder.go listener.go stream_client.go stream_server.go register.go
  engine/                     the SHM engine, relocated from internal/transport
```

Reuse is real but **not** a verbatim move — the engine references grpc-internal symbols that must be
promoted or shimmed:

| Reused as-is (algorithms + most code) | Must be promoted / adapted |
|---|---|
| ring (`ring.go`, `ringbuf.go`, `ring_zc_multi.go`); wake (`shm_dataseg_wake_*`, `shm_futex_*`, `shm_event_windows`); bootstrap (`shm_fdpass_*`, `shm_ctl_lock_*`); framing (`h2_codec.go`, `shm_frame_writer.go`); config | `mem.BufferSlice`/`mem.Buffer`, `CallHdr`, `WriteOptions`, `metadata.MD`; the window hooks (`updateWindow`/`adjustWindow`/`incrMsgRecv`); and the engine's current dependence on concrete `*ClientStream`/`*ServerStream` → the new interfaces |

So requirement #1 holds as "reuse the engine algorithms and most code behind a thin adapter," with
the adapter boundary named above — not a zero-diff move.

| Exposed API | Backed by (`origin/master`) |
|---|---|
| `client.TransportBuilder.Build` | `NewShmClient` / `DialShm` (`shm_aware_dialer.go`, `shm_dialer.go`) |
| `client.ClientTransport` / `ClientStream` | `ShmClientTransport` write/read path |
| `server.ServerTransportBuilder` / `ServerTransport` | `ShmListener` + `ShmServerTransport.HandleStreams` |
| `server.ServerStream` | `ShmServerTransport` `write*` path |

## 6. Selection, credentials, flow control (the glue maintainers will ask about)

- **Address selection.** The client picks the plugin via `resolver.Address.TransportType == "shm"`
  (set by a custom resolver or the `shm://` scheme), replacing today's `IsShmEnabled(addr)` attribute
  check. The plugin registers under that name; core does not know about SHM. Once an address selects a
  registered Builder, a `Build` error propagates to the connection-failure path — there is no
  automatic fallback to the HTTP/2 transport.
- **Credentials.** SHM bootstraps over UDS+SCM_RIGHTS, so the transport-credentials handshake runs
  **inside** `TransportBuilder.Build` (as `NewShmClient` already validates SHM-compatible creds), and
  the resulting `AuthInfo` is exposed via `ClientTransport.Peer()`. `RequireTransportSecurity` is
  honored there; the server handshaker config arrives through `BuildOptions`.
- **Flow control.** The window hooks (`updateWindow` / `adjustWindow` / `incrMsgRecv`) move onto the
  exported `ClientStream` / `ServerStream` contract, so accounting lives wholly inside the plugin's
  stream and core's `transportReader` calls them through the interface. This is the load-bearing part
  of the seam: closing it on the interface is what makes the engine externalizable.

## 7. Phases

| Phase | On | Output | Status |
|---|---|---|---|
| P0 | branch off `origin/master` | monolithic SHM builds + benches green (baseline) | done |
| P1 | grpc-go branch | registry + `TransportType`; concrete streams promoted to **byte-based** interfaces (recv / window / retry methods); `WriteProto` made an **optional capability** (assertion), NOT part of the interface; HTTP/2 + SHM rerouted through the interface | **done** |
| P2 | `plugin/` | `plugin/shm` implements the seam via an in-module adapter that hides `WriteProto` (Position A); registers `"shm"`. Engine **not yet relocated** out of `internal/` | partial (adapter done; relocation pending) |
| P3 | both | three-arm benchmark (INLINE value / adapter overhead / Position-A cost) | pending |
| P4 | `plugin/` | README + results | README done; results pending |

POC scope: unary + streaming, Linux, one capability set. `IsShmEnabled` is kept (additive) so the
monolithic path remains as the benchmark's first-party arm.

## 8. Benchmark methodology

Three paths on the **same engine commit**: plugin (via interfaces), monolithic (`origin/master`,
concrete streams), TCP/UDS (stock). Held constant: payload sizes (e.g. 1K / 64K / 256K / 1M / 16M),
GOMAXPROCS, warmup iterations, single-stream vs concurrent mode, frame-size mode, flow-control
window, capability set. The plugin-vs-monolithic delta isolates the cost of routing through
interfaces; that number is the claim.

## 9. Open questions

1. **Message- vs byte-based stream.** The seam above is byte/stream-based to match current grpc-go
   and reuse the engine; L37's `SendMsg(OutgoingMessage)` / `RecvMsg(IncomingMessage)` is the
   eventual granularity (and the natural home for marshal-into-ring). Decide whether the POC ships
   byte-based or already exposes the message form (retry still needs a byte fallback — A6).
2. **Export-surface size.** The exported `ClientStream` / `ServerStream` must cover the *full* method
   set core calls today — not just recv / write / window but also the retry (`Done` / `Status` /
   `Unprocessed` / `TrailersOnly` / `BytesReceived`), header, and compression methods used in
   `stream.go` / `server.go`. That is a sizable surface; the open decision is whether to export it
   wholesale or first refactor core's call sites onto a narrower contract.
3. **Server pull model.** Whether to add L37's `ServeTransport` / `Accept` now or stay on
   `HandleStreams` for the POC.
