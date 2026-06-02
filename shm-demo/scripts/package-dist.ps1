<#
.SYNOPSIS
  Package a built dist folder into a single .zip for fast transfer to another
  machine (e.g. an ARM box over slow RDP).

.DESCRIPTION
  DEV-ONLY helper. RDP copy is slow mostly because the self-contained
  dotnet-engine\ holds ~350 small runtime files (~120 MB). Two modes:

    Full (default)  -- zip the whole dist into ONE file. Use this the first
                       time, or after you upgrade the .NET SDK / change the
                       fork's NuGet dependencies (which can touch the runtime
                       files). 353 files -> 1 file + compression.

    -Incremental    -- zip ONLY the files you actually rebuild on a code change
                       (demo.exe + ShmDemo.Engine.* + Grpc.Net.SharedMemory.*),
                       a ~18 MB zip. The bulky runtime is unchanged, so you do
                       NOT need to re-send it. On the target, unzip OVER the
                       existing dist folder to overwrite just those files.

  Output: <repo>\out\<distName>[-inc]-<arch>.zip

.PARAMETER DistDir
  The dist folder to package. Default: dist-arm64 (falls back to dist).

.PARAMETER Incremental
  Package only the project-owned files (fast iteration). Requires that the full
  package was sent at least once before.

.PARAMETER OutFile
  Override the output zip path.

.EXAMPLE
  .\package-dist.ps1                         # full zip of dist-arm64
.EXAMPLE
  .\package-dist.ps1 -Incremental           # tiny zip, code-change only
.EXAMPLE
  .\package-dist.ps1 -DistDir .\dist         # full zip of x64 dist
#>
[CmdletBinding()]
param(
    [string] $DistDir,
    [switch] $Incremental,
    [string] $OutFile
)

$ErrorActionPreference = 'Stop'

$repo = Split-Path $PSScriptRoot -Parent

# Resolve the dist folder.
if (-not $DistDir) {
    foreach ($c in @('dist-arm64', 'dist')) {
        $p = Join-Path $repo $c
        if (Test-Path (Join-Path $p 'demo.exe')) { $DistDir = $p; break }
    }
}
if (-not $DistDir -or -not (Test-Path (Join-Path $DistDir 'demo.exe'))) {
    throw "Could not find a dist folder with demo.exe. Pass -DistDir."
}
$DistDir = (Resolve-Path $DistDir).Path
$distName = Split-Path $DistDir -Leaf

# Pick the files to include.
# Project-owned files = everything you rebuild on a code change. Keep this list
# in sync with what build-dist.ps1 produces.
$projectGlobs = @(
    'demo.exe',
    'dotnet-engine\ShmDemo.Engine.exe',
    'dotnet-engine\ShmDemo.Engine.dll',
    'dotnet-engine\ShmDemo.Engine.pdb',
    'dotnet-engine\ShmDemo.Engine.deps.json',
    'dotnet-engine\ShmDemo.Engine.runtimeconfig.json',
    'dotnet-engine\ShmDemo.Engine.staticwebassets.endpoints.json',
    'dotnet-engine\Grpc.Net.SharedMemory.dll',
    'dotnet-engine\Grpc.Net.SharedMemory.pdb'
)

$outDir = Join-Path $repo 'out'
New-Item -ItemType Directory -Force $outDir | Out-Null

if ($Incremental) {
    $files = @()
    foreach ($g in $projectGlobs) {
        $full = Join-Path $DistDir $g
        if (Test-Path $full) { $files += (Get-Item $full) }
    }
    if (-not $files) { throw "No project-owned files found under $DistDir." }
    if (-not $OutFile) { $OutFile = Join-Path $outDir "$distName-inc.zip" }

    if (Test-Path $OutFile) { Remove-Item $OutFile -Force }
    # Stage into a temp tree that mirrors the dist layout so the zip extracts
    # straight over the existing folder.
    $stage = Join-Path ([System.IO.Path]::GetTempPath()) ("pkg_" + [guid]::NewGuid().ToString('N'))
    try {
        foreach ($g in $projectGlobs) {
            $src = Join-Path $DistDir $g
            if (-not (Test-Path $src)) { continue }
            $dst = Join-Path $stage $g
            New-Item -ItemType Directory -Force (Split-Path $dst -Parent) | Out-Null
            Copy-Item $src $dst
        }
        # Bundle the scenario-sweep script so the target can self-test.
        $testScript = Join-Path $PSScriptRoot 'test-all.ps1'
        if (Test-Path $testScript) { Copy-Item $testScript (Join-Path $stage 'test-all.ps1') }
        Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $OutFile -CompressionLevel Optimal
    }
    finally {
        if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
    }
    Write-Host "Incremental package: code-change files only." -ForegroundColor Cyan
}
else {
    if (-not $OutFile) { $OutFile = Join-Path $outDir "$distName.zip" }
    if (Test-Path $OutFile) { Remove-Item $OutFile -Force }
    Compress-Archive -Path (Join-Path $DistDir '*') -DestinationPath $OutFile -CompressionLevel Optimal
    # Append the scenario-sweep script alongside the dist contents.
    $testScript = Join-Path $PSScriptRoot 'test-all.ps1'
    if (Test-Path $testScript) {
        Compress-Archive -Path $testScript -DestinationPath $OutFile -Update
    }
    Write-Host "Full package: entire dist (runtime + app)." -ForegroundColor Cyan
}

$zip = Get-Item $OutFile
$src = (Get-ChildItem $DistDir -Recurse -File | Measure-Object Length -Sum).Sum
Write-Host ("  source : {0:n1} MB" -f ($src / 1MB))
Write-Host ("  zip    : {0:n1} MB  ->  {1}" -f ($zip.Length / 1MB), $zip.FullName) -ForegroundColor Green
Write-Host ""
if ($Incremental) {
    Write-Host "On the target, extract OVER the existing dist folder:" -ForegroundColor Gray
    Write-Host ("  Expand-Archive -Force {0} -DestinationPath <existing-dist>" -f (Split-Path $OutFile -Leaf)) -ForegroundColor Gray
    Write-Host "Then self-test from inside that folder:  .\test-all.ps1" -ForegroundColor Gray
}
else {
    Write-Host "On the target, extract to a fresh folder:" -ForegroundColor Gray
    Write-Host ("  Expand-Archive {0} -DestinationPath <dest>" -f (Split-Path $OutFile -Leaf)) -ForegroundColor Gray
    Write-Host "Then self-test from inside that folder:  .\test-all.ps1" -ForegroundColor Gray
}
