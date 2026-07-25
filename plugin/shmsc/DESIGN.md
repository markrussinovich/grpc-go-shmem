# shmsc — Detailed Design

This document describes how `plugin/shmsc` is built and why it is built that way. For usage,
platform scope and the security model, see [README.md](README.md).

## 1. Goal

Prove that the exported pluggable-transport API in
[`google.golang.org/grpc/experimental/transport`](../../experimental/transport) is sufficient to
implement a non-trivial, high-performance transport **outside** the grpc-go core.

The proof obligation is deliberately strict: `shmsc` must not import
`google.golang.org/grpc/internal/*`. If the API were insufficient, the only way to finish the
transport would be to reach into grpc-go's internals — so the absence of those imports is the
evidence. A guard test enforces it (§6).

## 2. Requirements

1. **Self-contained.** No `google.golang.org/grpc/internal/*` imports; the module owns its engine
   rather than delegating back into in-tree code.
2. **Non-disturbing.** The pre-existing in-tree ("monolith") SHM transport and the default HTTP/2
   path keep working unchanged, with no behavioral coupling to this plugin.
3. **Real transport, not a shim.** Full framing, flow control, ring buffers, segment lifecycle and
   cross-process wakeup, on Linux and Windows.
4. **Fail-closed.** Neither transport selection nor credential handling may silently downgrade.

## 3. How grpc-go drives a transport

Core does not speak "shared memory"; it speaks a stream contract. Per RPC it needs to: create a
stream from a `CallHdr`, write pre-framed bytes, read a message header then a message body, observe
headers/trailers/status, and be told when the stream and the connection finish.

Before this work those operations were typed against the concrete
`*internal/transport.ClientStream` / `*ServerStream`. A transport living outside `internal/` could
therefore never be substituted, no matter how complete it was. Widening those call sites to
interfaces (`ClientStreamIface` / `ServerStreamIface`) is the enabling change, and it is the only
change to core semantics: no wire format, no RPC behavior, and no default-path code is altered.

## 4. The seam

```
   grpc-go core  (clientconn.go / server.go / stream.go)
        |
        |  resolver.Address.TransportType selects a Builder
        v
   experimental/transport/{client,server}      <-- the public contract
        |
        |  internal/transport/d1_adapter.go translates both directions
        v
   plugin/shmsc  (nested module)  -> internal/engine (owned SHM engine)
```

**Why the contract is byte-based.** The mandatory send path takes already-framed bytes
(`Write(hdr, data, opts)`), and the receive path hands ring-backed bytes upward
(`ReadMessageHeader` + `Read` returning a ref-counted `mem.BufferSlice`, which is how read-side
zero-copy survives the boundary). A byte contract is portable across gRPC implementations; a
message-typed contract is not.

**The optional capability.** Marshalling an application message straight into transport-owned memory
(INLINE_TX) removes a copy but requires a protobuf runtime that can serialize into caller-provided
memory — which not every gRPC language has. It is therefore *not* in the mandatory method set.
Instead a stream MAY implement `ProtoWriteStream`; core detects it by interface assertion
([writeproto_fastpath.go](../../writeproto_fastpath.go)) and transparently falls back to `Write`
when it is absent or declines. `shmsc` implements it on both the client and server stream, so it
keeps INLINE_TX exactly like the first-party transport.

**Translation.** [d1_adapter.go](../../internal/transport/d1_adapter.go) maps `CallHdr`, write
options, GOAWAY reasons, accepted-compressor resolution and stream errors — including
transparent-retry classification, which must survive the boundary or retries misbehave. Because a
Builder is third-party code, adapter and wiring treat its output as untrusted: a nil transport or
stream returned with a nil error is rejected with an error rather than wrapped into a non-nil shell
that would panic inside core.

## 5. Selection

The client selects on `resolver.Address.TransportType`; the listener tags accepted connections with
the same name (`shmsc.Name == "shmsc"`).

Selection is **fail-closed**: a non-empty transport type with no registered Builder is a connection
error, and a tagged server connection whose type is unregistered is closed rather than handed to the
HTTP/2 parser. An empty type takes grpc-go's default path. An explicit transport selector must never
quietly change the protocol on the wire — that is a correctness *and* a security property, and it is
covered by dispatch-level tests that dial a real HTTP/2 server with an unregistered type and assert
the RPC fails rather than succeeding over the fallback.

## 6. Module boundary and the guard

`shmsc` is a nested module (`plugin/shmsc/go.mod`) so it stays out of the root module's build while
still being vetted and tested by CI. It requires a released grpc-go plus a local `replace`, matching
the convention of this repository's other nested modules.

`TestNoInternalImports` is the self-containment proof. It parses **every** `.go` file in the module
regardless of build tags — so a platform-specific file cannot smuggle an import past it — rejects
any `google.golang.org/grpc/internal` import (aliased, blank and dot imports included, since it
inspects parsed import paths rather than text), and rejects any `//go:linkname` whose target is not
in `runtime`. That last check matters because `go:linkname` is the one form of hidden linkage an
import scan cannot see; the engine uses exactly one, `runtime.procyield`, on a spin path that is
disabled by default.

## 7. Credentials

The shared-memory channel is insecure in this version, and the plugin is fail-closed about it rather
than silently downgrading:

- A non-insecure `TransportCredentials` on either side is **rejected**, so grpc-go is never left
  believing a channel is secure while bytes travel over an unauthenticated segment.
- Per-RPC credentials are otherwise fully applied — audience construction, and gRFC A54
  restricted-control-plane-code normalization — and one whose `RequireTransportSecurity()` is true is
  refused at RPC time, which is where the stock HTTP/2 transport enforces it.

## 8. Lifecycle

grpc-go closes only the server transport after serving a connection; it never closes the raw conn.
A transport that allocated per-connection OS resources must therefore release them from its own
teardown, so the server transport carries an `onClose` hook wired by the Builder that unlinks the
segment and drops the listener's reference. Without it every completed connection leaks a segment.
Segment close is CAS-idempotent so the listener and transport paths can both run safely.

## 9. Known limitations

Tracked in the package doc ([shmsc.go](shmsc.go)) and summarised in [README.md](README.md):
insecure-only, no `credentials.RequestInfo` injection for per-RPC credentials, no transport-level
`OutHeader`/`InHeader`/`InTrailer` stats, several `BuildOptions` not honored, and malformed frames
failing a stream rather than the connection (the peer shares the segment and is trusted).

## 10. Open questions

- Should the experimental API grow a way to inject `credentials.RequestInfo`, so out-of-tree
  transports can support credentials that read it from context?
- Should transport-level stats events be expressible across the boundary, or is losing them
  acceptable for a pluggable transport?
- What belongs in a reusable cross-runtime conformance suite before the API can be called stable?
