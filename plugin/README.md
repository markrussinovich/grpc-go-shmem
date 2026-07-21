# SHM gRPC Transport Plugin

A shared-memory (SHM) gRPC transport wired in **through an exported, byte-based pluggable transport
API** instead of forked into grpc-go core. This document describes the **plugin architecture** and
the gap between the current **in-module POC** and the intended **external end state**. For the design
rationale see [DESIGN.md](DESIGN.md).

## Architecture

gRPC core is decoupled from any specific transport by two things: a **selection registry** and a
**byte-based stream contract**. A transport plugin plugs into both; the SHM engine stays behind that
contract.

```
        client                                    server
 ┌────────────────────┐                  ┌─────────────────────┐
 │ grpc.ClientConn    │                  │ grpc.Server         │
 │  createTransport   │                  │  newHTTP2Transport  │
 └─────────┬──────────┘                  └──────────┬──────────┘
           │ select by                              │ select by
           │ resolver.Address.TransportType         │ accepted-conn type
           ▼                                        ▼
 transport/client.Get(name)              transport/server.Get(name)
           │  Builder.Build()                       │  Builder.Build()
           ▼                                        ▼
 ┌──────────────────────────────────────────────────────────────┐
 │  exported BYTE-BASED contract                                 │
 │  ClientTransport / ClientStream · ServerTransport/ServerStream│
 │    • Write(hdr, data)          pre-framed bytes out           │
 │    • ReadMessageHeader + Read  ring-backed bytes in (read ZC) │
 │    • headers / status / flow-control / lifecycle              │
 │    • NO WriteProto             (byte-only boundary, see below) │
 └───────────────────────────────┬──────────────────────────────┘
                                 │ implemented by
                                 ▼
                          plugin/shm
                                 │ reuses (unchanged)
                                 ▼
                  internal/transport SHM engine
                  (ring · framing · wake · bootstrap)
```

**Components**

- `transport/client`, `transport/server` — exported registries (`Register(name, Builder)` / `Get`)
  plus the byte-based transport/stream interfaces. This is the seam a plugin implements.
- `plugin/shm` — registers `"shm"` on both sides and adapts the in-tree SHM engine to the byte
  contract. Selected on the client by `resolver.Address.TransportType == "shm"`, on the server by a
  listener that tags accepted connections.
- The SHM **engine** (ring, framing, wake, bootstrap) is reused with its data-path logic **unchanged**;
  the only edits to `internal/transport/shm_*` are the `NewStream` / `HandleStreams` signatures, widened
  to the new interface types.

**Byte-only boundary.** The contract is purely byte-based and shaped to be a cross-language standard:
`Write(hdr, data)` carries already-framed bytes out; `ReadMessageHeader` + `Read` carry ring-backed
bytes in (so read-side zero-copy survives). It deliberately has **no** message-typed
`WriteProto(any)`: marshalling an application message is a codec concern that is not portable across
languages, so that fast path (`INLINE_TX`) stays a first-party-only optimization. Core still uses it
for the built-in transport via an **optional interface assertion** (`writeproto_fastpath.go`); the
plugin's stream wrapper exposes only the byte interface, so the assertion fails and the portable
`Write` path is used (pinned by [`plugin/shm/positionA_test.go`](shm/positionA_test.go)).

**Usage**

```go
import "google.golang.org/grpc/plugin/shm" // its init() registers the "shm" client + server builders

// server: shm.NewListener tags accepted conns so the server selects the SHM builder.
lis, _ := shm.NewListener("my_segment")
s := grpc.NewServer()
go s.Serve(lis)

// client: dial an address whose resolver.Address.TransportType == shm.Name; the
// registry then selects the SHM client builder. Use a resolver that sets that
// field (see plugin/shm/registry_e2e_test.go for a manual-resolver example).
```

## POC now vs. expected end state

| Aspect | In-module POC (this branch) | Expected end state |
|---|---|---|
| Selection registry | exported `transport/{client,server}` | same |
| Byte-based stream contract | core drives streams through it | same |
| Plugin implements the contract | `plugin/shm` via an adapter | same, from a **separate repo** |
| SHM engine location | `internal/transport` (reused; only `NewStream`/`HandleStreams` signatures widened) | relocated to the plugin module / its own repo |
| Plugin's grpc imports | still imports `internal/transport` to **construct** the engine (`NewShmClient`, `NewServerTransport`, `NewShmListener`) | **only** the exported `transport/{client,server}` API — no `internal/*` |
| Exported option types | `transport/{client,server}/types.go` **alias internal** option structs (POC scaffold) | purpose-built, minimal `BuildOptions` |
| Selection field | ad-hoc `resolver.Address.TransportType` | reconciled with L37 "Go Custom Transports" (grpc/proposal#103) |

**What the POC proves today:** grpc-go core can select *and* drive a transport purely through the
exported byte contract, and a plugin can capability-restrict it (drop `INLINE_TX`) without touching
core — with the SHM engine reused unchanged. **What remains for a truly external plugin:** relocate
the engine out of `internal/` and replace the alias option types with designed ones, so a third party
can implement `transport/client.ClientStream` / `transport/server.ServerStream` from scratch with no
`internal/*` import.

## Status

Interface promotion + plugin adapter implemented and green on Linux (build / vet / e2e / SHM tests).
Relocating the engine out of `internal/` and designing the final option API are the remaining steps.
