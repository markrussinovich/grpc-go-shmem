# Shared Memory Transport – Integration Coverage

This directory now tracks the end-to-end state of the shared memory transport, including how it plugs into the standard gRPC client/server APIs.

## What Works

- Shared memory transports implement `ClientTransportProvider`/`ServerTransportProvider`, so `grpc.NewClient` and `grpc.Server.Serve` can run over shm when you supply `grpc.WithShmTransport` and a shm listener.
- Resolver for `shm://` is registered; targets like `shm://routeguide_shm` resolve to `Addr: "shm:routeguide_shm"` for the dialer.
- Dialer wrapper (`WithShmTransport`) constructs a `net.Conn` that hands its shm transport directly to the HTTP/2 stack (which then bypasses HTTP/2 framing for shm).
- Flow control and frame handling are live; examples mirror their TCP counterparts.
- E2E examples run green: `helloworld_shm`, `route_guide_shm`, and the helper script `./run_shmem_examples.sh` (runs all shm demos) all pass.

## How to Exercise It

- Quick check of all shm demos:

   ```bash
   ./run_shmem_examples.sh
   ```

- Individual runs match the TCP examples, with only listener/dialer differences:
   - Servers: create a shm listener (e.g., `transport.NewShmListener(&transport.ShmAddr{Name: "demo"}, ...)`) and pass it to `grpc.Server.Serve`.
   - Clients: dial `shm://<name>` with `grpc.WithShmTransport()` plus credentials (e.g., `insecure.NewCredentials()`).

## Remaining Gaps

- Linux-only: futex-based synchronization is guarded by `//go:build linux`; other platforms fall back to stubs and are not exercised yet.
- Stats hooks: server-side message recv counter is still stubbed; tracing/metrics parity with TCP is incomplete.
- Legacy TODOs: `processFrameData` placeholder is unused but still present; ring-capacity investigation noted in tests (`shm_integration_test.go`).
- Ergonomics: users must opt into `grpc.WithShmTransport` and manually create a shm listener (no automatic scheme-to-listener wiring yet).

Use this file as the canonical status for shm integration versus TCP and to track the small remaining deltas.
