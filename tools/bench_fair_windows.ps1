#requires -Version 7
<#
.SYNOPSIS
    Windows fair-bench + resource-usage collector for grpc-go-shmem v3.4.

.DESCRIPTION
    Runs the same SHM/TCP bench matrix Linux runs, plus snapshots
    HandleCount / WorkingSet / mmap-equivalent stats during a steady-state
    bench cell. UDS not available on Windows (use Named Pipe TCP loopback
    if needed; not covered here).

    Designed to be run from C:\src\grpc-go-shmem on the Windows VM.

.PARAMETER OutputDir
    Where to write logs. Defaults to bench_win_fair under the repo root.

.PARAMETER FullMatrix
    If set, runs the complete matrix (Unary+Stream+Concurrent x Shm+TCP).
    Otherwise, only the "hot cell" subset is run plus resource snapshots.

.EXAMPLE
    pwsh -File tools\bench_fair_windows.ps1 -FullMatrix
    pwsh -File tools\bench_fair_windows.ps1
#>

param(
    [string]$OutputDir = "$PSScriptRoot\..\bench_win_fair",
    [switch]$FullMatrix
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repoRoot
$OutputDir = Resolve-Path -Path $OutputDir -ErrorAction SilentlyContinue
if (-not $OutputDir) {
    $OutputDir = (New-Item -ItemType Directory -Force -Path "$repoRoot\bench_win_fair").FullName
}
Write-Host "Repo:   $repoRoot"
Write-Host "Output: $OutputDir"

# --- Cleanup helper -----------------------------------------------------
function Invoke-PreClean {
    Get-Process -Name 'shmemtcp*','go' -ErrorAction SilentlyContinue |
        Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Milliseconds 500
    Get-ChildItem $env:TEMP -Filter 'grpc_shm_*' -ErrorAction SilentlyContinue |
        Remove-Item -Force -ErrorAction SilentlyContinue
}

# --- Env (v3.4 baseline: no-WU + eventfd waker are now ON by default in
# the SHM transport, not env-driven. Only BENCH_PROFILE etc. remain.)
function Set-BenchEnv {
    param([switch]$DirtyPool)
    'BENCH_PROFILE','SHM_BENCH_CPU','BENCH_DIRTY_DEFAULT_POOL' | ForEach-Object {
        Remove-Item "Env:$_" -ErrorAction SilentlyContinue
    }
    $env:BENCH_PROFILE   = 'fair-default'
    $env:SHM_BENCH_CPU   = '1'
    if ($DirtyPool) { $env:BENCH_DIRTY_DEFAULT_POOL = '1' }
}

# --- Full matrix bench --------------------------------------------------
function Invoke-FullMatrix {
    param([string]$Label, [switch]$DirtyPool)
    Invoke-PreClean
    Set-BenchEnv -DirtyPool:$DirtyPool
    $logFile = Join-Path $OutputDir "$Label.txt"
    Write-Host "[$Label] starting full matrix (DirtyPool=$DirtyPool) -> $logFile"
    $args = @(
        'test',
        '-bench=^BenchmarkGRPC(Shm|Unix|TCP)(Unary|Stream|Concurrent)$',
        '-benchtime=2s', '-count=1', '-run=^$', '-timeout=2700s',
        '.\benchmark\shmemtcp\'
    )
    & go @args *> $logFile
    Invoke-PreClean
    $count = (Select-String -Path $logFile -Pattern '^BenchmarkGRPC' | Measure-Object).Count
    Write-Host "[$Label] done. Cells captured: $count"
}

# --- Resource snapshot --------------------------------------------------
function Invoke-ResourceSnapshot {
    param(
        [string]$CellPattern,    # e.g. '^BenchmarkGRPCShmConcurrent$/^streams=1000$/^size=65536-'
        [string]$CellLabel       # e.g. 'shm_1000x64k'
    )
    Invoke-PreClean
    Set-BenchEnv
    $snapDir = Join-Path $OutputDir "resources\$CellLabel"
    New-Item -ItemType Directory -Force -Path $snapDir | Out-Null
    $benchLog = Join-Path $snapDir 'bench.log'
    $info     = Join-Path $snapDir 'INFO.txt'

    Write-Host "[$CellLabel] launching long bench cell..."
    $benchErr = Join-Path $snapDir 'bench.err'
    $proc = Start-Process -FilePath 'go' -ArgumentList @(
        'test',
        "-bench=$CellPattern",
        '-benchtime=20s', '-count=1', '-run=^$', '-timeout=180s',
        '.\benchmark\shmemtcp\'
    ) -RedirectStandardOutput $benchLog -RedirectStandardError $benchErr `
      -NoNewWindow -PassThru

    "go PID = $($proc.Id)" | Out-File $info
    "pattern = $CellPattern" | Out-File $info -Append

    # Wait for shmemtcp.test child to appear (the actual test binary)
    $testProc = $null
    for ($i = 0; $i -lt 30 -and -not $testProc; $i++) {
        Start-Sleep -Milliseconds 700
        $testProc = Get-Process -Name 'shmemtcp.test*' -ErrorAction SilentlyContinue | Select-Object -First 1
    }
    if (-not $testProc) {
        "FAILED: shmemtcp.test not spawned" | Out-File $info -Append
        $proc | Wait-Process
        return
    }
    "test PID = $($testProc.Id)" | Out-File $info -Append

    Start-Sleep -Seconds 3   # steady state

    1..3 | ForEach-Object {
        $snap = $_
        $out  = Join-Path $snapDir "snap$snap.txt"
        $now  = Get-Date -Format 'yyyy-MM-dd HH:mm:ss'
        $p    = Get-Process -Id $testProc.Id -ErrorAction SilentlyContinue
        if (-not $p) {
            "snap ${snap}: process gone" | Add-Content $info
            return
        }
        @"
=== TIMESTAMP $now ===

=== Process ===
PID            : $($p.Id)
HandleCount    : $($p.HandleCount)
Threads        : $($p.Threads.Count)
WorkingSet64   : $([math]::Round($p.WorkingSet64 /1MB,2)) MB
PrivateMem64   : $([math]::Round($p.PrivateMemorySize64 /1MB,2)) MB
PagedMem64     : $([math]::Round($p.PagedMemorySize64  /1MB,2)) MB
VirtualMem64   : $([math]::Round($p.VirtualMemorySize64/1MB,2)) MB
CPU(sec)       : $([math]::Round($p.TotalProcessorTime.TotalSeconds,2))

=== Memory regions count ===
$( (Get-Process -Id $p.Id).Modules | Measure-Object | Select-Object -ExpandProperty Count ) loaded modules

=== Active TCP connections (count by state) ===
$( (Get-NetTCPConnection -OwningProcess $p.Id -ErrorAction SilentlyContinue | Group-Object -Property State | ForEach-Object { '{0,-15} {1}' -f $_.Name,$_.Count }) -join "`n" )

=== TCP connections sample ===
$( (Get-NetTCPConnection -OwningProcess $p.Id -ErrorAction SilentlyContinue | Select-Object -First 10 | Format-Table -AutoSize | Out-String).Trim() )
"@ | Out-File $out
        Start-Sleep -Seconds 2
    }

    $proc | Wait-Process
    "" | Add-Content $info
    "=== BENCH RESULT ===" | Add-Content $info
    Select-String -Path $benchLog -Pattern '^BenchmarkGRPC' | ForEach-Object { $_.Line } | Add-Content $info
    Write-Host "[$CellLabel] done."
}

# --- Run ----------------------------------------------------------------
try {
    # Build sanity
    go build .\internal\transport\ .\benchmark\shmemtcp\
    if ($LASTEXITCODE -ne 0) { throw "build failed" }

    if ($FullMatrix) {
        Invoke-FullMatrix -Label 'A_fair_default'
        Invoke-FullMatrix -Label 'B_fair_dirty' -DirtyPool
    }

    # Resource snapshots on 4 hot cells x 2 transports
    $cells = @(
        @{ Pattern = '^BenchmarkGRPCShmUnary$/^size=64-';            Label = 'shm_unary_64B' }
        @{ Pattern = '^BenchmarkGRPCShmUnary$/^size=65536-';         Label = 'shm_unary_64K' }
        @{ Pattern = '^BenchmarkGRPCShmUnary$/^size=1MB-';           Label = 'shm_unary_1M' }
        @{ Pattern = '^BenchmarkGRPCShmConcurrent$/^streams=1000$/^size=65536-'; Label = 'shm_1000x64K' }
        @{ Pattern = '^BenchmarkGRPCTCPUnary$/^size=64-';            Label = 'tcp_unary_64B' }
        @{ Pattern = '^BenchmarkGRPCTCPUnary$/^size=65536-';         Label = 'tcp_unary_64K' }
        @{ Pattern = '^BenchmarkGRPCTCPUnary$/^size=1MB-';           Label = 'tcp_unary_1M' }
        @{ Pattern = '^BenchmarkGRPCTCPConcurrent$/^streams=1000$/^size=65536-'; Label = 'tcp_1000x64K' }
    )
    foreach ($c in $cells) {
        Invoke-ResourceSnapshot -CellPattern $c.Pattern -CellLabel $c.Label
    }

    # Summary
    Write-Host ""
    Write-Host "############### SUMMARY ###############"
    Get-ChildItem $OutputDir | Format-Table Name, Length, LastWriteTime -AutoSize
    if (Test-Path "$OutputDir\A_fair_default.txt") {
        Write-Host ""
        Write-Host "### A (default) ###"
        Select-String "$OutputDir\A_fair_default.txt" -Pattern '^BenchmarkGRPC' | ForEach-Object { $_.Line }
    }
    if (Test-Path "$OutputDir\B_fair_dirty.txt") {
        Write-Host ""
        Write-Host "### B (dirty) ###"
        Select-String "$OutputDir\B_fair_dirty.txt" -Pattern '^BenchmarkGRPC' | ForEach-Object { $_.Line }
    }
    Write-Host ""
    Write-Host "### Resource snapshots ###"
    Get-ChildItem "$OutputDir\resources" -Directory -ErrorAction SilentlyContinue | ForEach-Object {
        $info = Join-Path $_.FullName 'INFO.txt'
        if (Test-Path $info) {
            Write-Host "--- $($_.Name) ---"
            Get-Content $info | Select-Object -First 4
            Write-Host ""
        }
    }
}
finally {
    Pop-Location
}
