# shm-demo — gRPC shared-memory transport demo

An interactive, browser-based benchmark that compares gRPC transports
(TCP loopback, Unix-domain / named-pipe sockets, and the **shared-memory**
transport) side by side for latency and throughput across payload sizes.

It is the demo used to showcase the SHM transport work in this fork
(`grpc-go-shmem`) and its .NET counterpart (`grpc-dotnet-shm`).

> **Platform: Windows only.** The demo is developed and supported on Windows.
> The packaging/build scripts are PowerShell, and the per-run CPU-cost metric
> is sampled via Windows APIs (it reports `0` on other platforms). The Go code
> still compiles and the benchmark still runs on Linux/macOS, but CPU numbers
> are unavailable there and it is not exercised on those platforms.

```
go run ./cmd/demo        # build + launch the web shell, then open the URL it prints
```

## Layout: two separate forks

The demo drives **two** independent gRPC SHM implementations, which live in
**two separate repositories**:

| Engine | Language | Source it builds against |
| --- | --- | --- |
| Go    | Go   | the **enclosing** `grpc-go-shmem` checkout (this repo) |
| .NET  | C#   | a **sibling** `grpc-dotnet-shm` checkout (separate repo) |

The Go engine is always available — `go.mod` has
`replace google.golang.org/grpc => ../`, so it compiles directly against the
SHM fork it is nested in. **No extra checkout is needed for the Go-only demo.**

The .NET engine is **optional**. It references
`grpc-dotnet-shm/src/Grpc.Net.SharedMemory`, which is a *different* repo, so you
only need it if you want the cross-language (.NET) comparison.

```
<parent>/
├── grpc-go-shmem/        ← this repo
│   └── shm-demo/         ← you are here
└── grpc-dotnet-shm/      ← optional sibling, only for the .NET engine
```

## Running

### Go-only (no .NET checkout required)

```
go run ./cmd/demo
```

This builds and launches the web shell. The transport toggles cover the Go
implementations; the ".NET" engine simply reports "not bundled" if no .NET
build is present.

> The web UI assets are embedded via `go:embed`. After editing anything under
> `internal/web/`, rebuild the binary (`go build -o demo.exe ./cmd/demo`) for
> the change to take effect.

### Building a distributable bundle

`scripts/build-dist.ps1` produces self-contained `dist\` (x64) and/or
`dist-arm64\` folders containing `demo.exe` (and, optionally, a self-contained
`dotnet-engine\` that needs no .NET install on the target machine).

The **`-Dotnet`** parameter chooses how the optional .NET engine is obtained:

| `-Dotnet` | What it does |
| --- | --- |
| `none` *(default)* | Build only the Go demo. Web shell still runs; ".NET" toggle reports "not bundled". |
| `local` | Build the .NET engine against a `grpc-dotnet-shm` checkout you already have. Path via `-GrpcDotnetShmDir` (defaults to the sibling checkout). The checkout is left untouched. |
| `repo` | Clone `grpc-dotnet-shm` into a temp folder, build, then delete the clone. Override source with `-DotnetShmRepo` / `-DotnetShmRef`. |

Examples:

```powershell
# Go only, both architectures (default)
.\scripts\build-dist.ps1

# Go + .NET, using an existing sibling grpc-dotnet-shm checkout
.\scripts\build-dist.ps1 -Arch x64 -Dotnet local

# Go + .NET, against a checkout in a custom location
.\scripts\build-dist.ps1 -Dotnet local -GrpcDotnetShmDir D:\src\grpc-dotnet-shm

# Go + .NET, cloning grpc-dotnet-shm on the fly and discarding it afterwards
.\scripts\build-dist.ps1 -Dotnet repo -DotnetShmRef my-branch
```

You can also build the .NET engine directly with MSBuild by pointing it at the
fork:

```powershell
dotnet publish dotnet/ShmDemo.Engine/ShmDemo.Engine.csproj `
    -p:GrpcDotnetShmDir=D:\src\grpc-dotnet-shm
```

If `grpc-dotnet-shm` cannot be found, the build fails with a clear message
telling you to set `GrpcDotnetShmDir`.

## How the numbers are measured

- **Latency** — a single bidirectional ping-pong stream with exactly one
  request in flight at a time; reports p50/p99 per round-trip.
- **Throughput** — bounded-in-flight ping-pong (echo), counting bytes in both
  directions, reported as messages/s and MB/s.
- **CPU cost** (`CPU s / 1M msgs`) — **Windows only.** Sampled from per-process
  CPU time via Windows APIs; reported as `0` on other platforms.

Each size is measured over multiple rounds (default 3) and the rounds are
combined robustly: the median is used for 3+ rounds, the worse of two for 2
rounds, and any round that exceeds a 4× timeout is dropped. If every round
times out, that cell is reported as a timeout.

> Note: at very large payloads (e.g. 256 MB) the "latency" number is
> bandwidth-bound — a 256 MB echo moves 512 MB round-trip, so the time reflects
> the achievable memory/transport bandwidth rather than per-message overhead.
