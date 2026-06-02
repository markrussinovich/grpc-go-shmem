<#
.SYNOPSIS
  Rebuild the demo distribution from source (Go demo + optional self-contained
  .NET engine).

.DESCRIPTION
  DEV-ONLY helper. The Go demo always builds straight from this repo:
    - go.mod has `replace google.golang.org/grpc => ../`, so `go build` always
      recompiles the enclosing grpc-go-shmem SHM fork.

  The .NET engine is OPTIONAL and lives in a SEPARATE repository
  (grpc-dotnet-shm). Choose how to obtain it with -Dotnet:

    none   (default)  Build only the Go demo. The web shell still runs; the
                      ".NET" toggle just reports "not bundled". Pick this when
                      you only need the Go-vs-Go transport comparison.

    local             Build the .NET engine against a grpc-dotnet-shm checkout
                      you already have. Pass its path with -GrpcDotnetShmDir
                      (or rely on the default sibling location next to this
                      repo). The checkout is left untouched.

    repo              Clone grpc-dotnet-shm into a temp folder, build the
                      engine against it, then delete the clone. Nothing is left
                      behind except the built binaries in dist\. Override the
                      source with -DotnetShmRepo / -DotnetShmRef.

  Output: dist\ (x64) and/or dist-arm64\ (arm64), each with demo.exe and,
  unless -Dotnet none, a self-contained dotnet-engine\ (no .NET install needed
  on the target).

.PARAMETER Arch
  x64, arm64, or both (default both).

.PARAMETER Dotnet
  none | local | repo  (default none). How to obtain the .NET engine.

.PARAMETER GrpcDotnetShmDir
  For -Dotnet local: path to an existing grpc-dotnet-shm checkout. Defaults to
  a sibling checkout next to grpc-go-shmem (..\..\grpc-dotnet-shm).

.PARAMETER DotnetShmRepo
  For -Dotnet repo: git URL to clone. Default: the canonical grpc-dotnet-shm.

.PARAMETER DotnetShmRef
  For -Dotnet repo: branch/tag/commit to check out. Default: master.

.PARAMETER Clean
  Delete the target dist folder(s) before building.

.EXAMPLE
  .\build-dist.ps1                              # Go only, x64 + arm64
.EXAMPLE
  .\build-dist.ps1 -Arch x64 -Dotnet local      # + .NET from sibling checkout
.EXAMPLE
  .\build-dist.ps1 -Dotnet local -GrpcDotnetShmDir D:\src\grpc-dotnet-shm
.EXAMPLE
  .\build-dist.ps1 -Dotnet repo -DotnetShmRef my-branch
#>
[CmdletBinding()]
param(
    [ValidateSet('x64', 'arm64', 'both')]
    [string] $Arch = 'both',
    [ValidateSet('none', 'local', 'repo')]
    [string] $Dotnet = 'none',
    [string] $GrpcDotnetShmDir,
    [string] $DotnetShmRepo = 'https://github.com/markrussinovich/grpc-dotnet-shm.git',
    [string] $DotnetShmRef = 'master',
    [switch] $Clean
)

$ErrorActionPreference = 'Stop'

$repo = Split-Path $PSScriptRoot -Parent
$engineProj = Join-Path $repo 'dotnet\ShmDemo.Engine\ShmDemo.Engine.csproj'

# ---------------------------------------------------------------------------
# Resolve the grpc-dotnet-shm location for the chosen .NET mode. For 'repo' we
# clone into a temp folder now and delete it in the finally block at the end.
# $shmDir ends up pointing at a usable checkout (or stays $null for 'none').
# ---------------------------------------------------------------------------
$shmDir = $null
$shmClone = $null   # set only in 'repo' mode; deleted on exit
switch ($Dotnet) {
    'local' {
        if (-not $GrpcDotnetShmDir) {
            # Default: sibling checkout next to grpc-go-shmem.
            # $repo = ...\grpc-go-shmem\shm-demo, so go up two levels to the
            # parent of grpc-go-shmem, then into grpc-dotnet-shm.
            $GrpcDotnetShmDir = Join-Path (Split-Path (Split-Path $repo -Parent) -Parent) 'grpc-dotnet-shm'
        }
        if (-not (Test-Path (Join-Path $GrpcDotnetShmDir 'src\Grpc.Net.SharedMemory\Grpc.Net.SharedMemory.csproj'))) {
            throw "grpc-dotnet-shm not found at '$GrpcDotnetShmDir'. Pass -GrpcDotnetShmDir <path> or use -Dotnet repo."
        }
        $shmDir = (Resolve-Path $GrpcDotnetShmDir).Path
        Write-Host "Using local grpc-dotnet-shm: $shmDir" -ForegroundColor Cyan
    }
    'repo' {
        $shmClone = Join-Path ([System.IO.Path]::GetTempPath()) ("grpc-dotnet-shm_" + [guid]::NewGuid().ToString('N'))
        Write-Host "Cloning $DotnetShmRepo ($DotnetShmRef) -> $shmClone" -ForegroundColor Cyan
        & git clone --depth 1 --branch $DotnetShmRef $DotnetShmRepo $shmClone
        if ($LASTEXITCODE -ne 0) { throw "git clone failed for $DotnetShmRepo ($DotnetShmRef)" }
        $shmDir = $shmClone
    }
}

function Build-Target {
    param(
        [string] $GoArch,   # amd64 | arm64
        [string] $DotnetRid, # win-x64 | win-arm64
        [string] $OutDir
    )
    Write-Host ""
    Write-Host "==== Building $OutDir ====" -ForegroundColor Cyan

    if ($Clean -and (Test-Path $OutDir)) {
        Write-Host "  cleaning $OutDir"
        Remove-Item -Recurse -Force $OutDir
    }
    New-Item -ItemType Directory -Force $OutDir | Out-Null

    # --- Go demo (recompiles the enclosing grpc-go-shmem via go.mod replace) ---
    Write-Host "  go build ($GoArch) ..." -ForegroundColor Gray
    $demoExe = Join-Path $OutDir 'demo.exe'
    $env:GOOS = 'windows'
    $env:GOARCH = $GoArch
    try {
        Push-Location $repo
        & go build -o $demoExe ./cmd/demo
        if ($LASTEXITCODE -ne 0) { throw "go build failed ($GoArch)" }
    }
    finally {
        Pop-Location
        Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
        Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
    }
    Write-Host "    -> $demoExe" -ForegroundColor Green

    # --- .NET engine (self-contained; rebuilds Grpc.Net.SharedMemory fork) ---
    if ($Dotnet -ne 'none') {
        Write-Host "  dotnet publish ($DotnetRid, self-contained) ..." -ForegroundColor Gray
        $engineOut = Join-Path $OutDir 'dotnet-engine'
        & dotnet publish $engineProj -c Release -r $DotnetRid `
            --self-contained true -p:PublishReadyToRun=true `
            -p:GrpcDotnetShmDir=$shmDir -o $engineOut
        if ($LASTEXITCODE -ne 0) { throw "dotnet publish failed ($DotnetRid)" }

        $coreclr = Join-Path $engineOut 'coreclr.dll'
        if (Test-Path $coreclr) {
            Write-Host "    -> $engineOut (self-contained, no .NET install needed)" -ForegroundColor Green
        }
        else {
            Write-Host "    WARNING: coreclr.dll not found in $engineOut -- not self-contained!" -ForegroundColor Yellow
        }
    }
    else {
        Write-Host "  (skipping .NET engine; -Dotnet none)" -ForegroundColor DarkYellow
    }
}

try {
    $targets = @()
    if ($Arch -in @('x64', 'both')) {
        $targets += , @('amd64', 'win-x64', (Join-Path $repo 'dist'))
    }
    if ($Arch -in @('arm64', 'both')) {
        $targets += , @('arm64', 'win-arm64', (Join-Path $repo 'dist-arm64'))
    }

    foreach ($t in $targets) {
        Build-Target -GoArch $t[0] -DotnetRid $t[1] -OutDir $t[2]
    }

    Write-Host ""
    Write-Host "==== Done ====" -ForegroundColor Cyan
    foreach ($t in $targets) {
        $out = $t[2]
        $hasGo = Test-Path (Join-Path $out 'demo.exe')
        $hasNet = Test-Path (Join-Path $out 'dotnet-engine\ShmDemo.Engine.exe')
        Write-Host ("  {0,-12} demo.exe={1}  dotnet-engine={2}" -f `
            (Split-Path $out -Leaf), $(if ($hasGo) { 'yes' } else { 'NO' }), $(if ($hasNet) { 'yes' } else { 'no' }))
    }
    Write-Host ""
    Write-Host "Sanity-check a build:  .\scripts\test-all.ps1 -DistDir .\dist" -ForegroundColor Gray
}
finally {
    # 'repo' mode: remove the throwaway clone so nothing is left behind.
    if ($shmClone -and (Test-Path $shmClone)) {
        Write-Host "Removing temp clone $shmClone" -ForegroundColor DarkGray
        Remove-Item -Recurse -Force $shmClone -ErrorAction SilentlyContinue
    }
}
