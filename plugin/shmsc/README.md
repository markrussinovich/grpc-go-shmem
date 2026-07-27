# shmsc — self-contained shared-memory gRPC transport

`shmsc` is a shared-memory transport for gRPC-Go, implemented entirely against
the exported pluggable-transport API in
[`google.golang.org/grpc/experimental/transport`](../../experimental/transport).

It imports **no** `google.golang.org/grpc/internal/*` package. That constraint is
enforced by a guard test (`TestNoInternalImports`), which parses every Go file in
the module regardless of build tags and also rejects any `//go:linkname` whose
target is not in `runtime`. The module therefore doubles as a working proof that
the exported API is sufficient for a non-trivial transport implemented outside
the grpc-go core.

For how it is built and why, see [DESIGN.md](DESIGN.md).

## Status

Experimental. The API it is written against is experimental and may change or be
removed; this module is not covered by grpc-go's compatibility promise.

## Platform support

Linux and Windows only, matching shared memory itself (same-host IPC). The
segment, ring and wakeup primitives are built only for those platforms.

## Usage

The client selects the transport through `resolver.Address.TransportType`; the
server tags accepted connections by wrapping its listener. Importing the package
registers the builders under the transport type `shmsc`.

Server:

```go
import shmsc "google.golang.org/grpc/plugin/shmsc"

lis, err := shmsc.Listen("my-service")
if err != nil {
    return err
}
s := grpc.NewServer()
pb.RegisterEchoServer(s, &echoServer{})
go s.Serve(lis)
```

Client:

```go
import (
    shmsc "google.golang.org/grpc/plugin/shmsc"
    "google.golang.org/grpc/resolver"
)

// Any resolver that yields an Address with TransportType set works; a manual
// resolver is used here for brevity.
r := manual.NewBuilderWithScheme("example")
r.InitialState(resolver.State{Addresses: []resolver.Address{
    {Addr: "my-service", TransportType: shmsc.Name},
}})

cc, err := grpc.NewClient("example:///my-service",
    grpc.WithResolvers(r),
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)
```

An existing listener can also be tagged directly with `shmsc.NewListener`.

## Security model

The transport is **insecure-only** in this version, and it is fail-closed about
it rather than silently downgrading:

- A real (non-insecure) `TransportCredentials` on either the client or the server
  is **rejected**. gRPC-Go is never left believing a channel is secure while the
  bytes travel over an unauthenticated segment.
- A per-RPC credential whose `RequireTransportSecurity()` is true is **rejected
  at RPC time**, which is where the stock HTTP/2 transport enforces it.
- The segment backing file is created `0600`. OS file permissions are the
  confidentiality and integrity boundary; any process that can open the segment
  can read and write the connection.
- A listener refuses to bind a name whose control segment reports a live server,
  so it can never unlink a running peer's segment and hijack its clients.

Because both endpoints map the same memory, the peer is treated as trusted:
malformed frames surface a stream error rather than tearing down the connection.

## Known limitations

These are deliberate and documented, not bugs. See the package doc in
[`shmsc.go`](shmsc.go) for the full list, which includes: no
`credentials.RequestInfo` injection for per-RPC credentials, no transport-level
`OutHeader`/`InHeader`/`InTrailer` stats, several `BuildOptions` that are not
honored (`UserAgent`, `Dialer`, `BufferPool`, `MaxHeaderListSize`, server
`HeaderTableSize`/`ConnectionTimeout`/keepalive), and one `//go:linkname` to
`runtime.procyield` on the spin path, which is disabled by default.

## Tests

```
cd plugin/shmsc
go test ./...
go test ./... -race
```
