#requires -Version 7
<#
.SYNOPSIS
    Large-frame + spin-wait sensitivity bench on Windows.

.DESCRIPTION
    Mark asked: "Can you do a run with large frames and spin wait to show
    how much it improves?" This script mirrors the Linux companion
    (tools/large_frame_spin_bench_linux.sh) and produces three matched
    result files so the contribution of each SHM-only tuning knob is
    isolated against the same fair-default baseline:

        A  fair_default     — 16 KiB frame, no spin (current fair config)
        B  fair_1Mframe     — 1 MiB frame, no spin
        C  fair_1Mframe_spin— 1 MiB frame, SHM_SPIN_ITERS=2000 (light)

    Spin and large-frame are SHM-only tunings; TCP and UDS keep their
    HTTP/2 spec defaults across all three cells, so the comparison stays
    apples-to-apples. AF_UNIX is available on Windows 10+ (matches Linux).

.PARAMETER OutputDir
    Where to write logs. Defaults to bench_win_1Mframe_spin under the
    repo root.

.EXAMPLE
    pwsh -File tools\large_frame_spin_bench_windows.ps1
#>

param(
    [string]$OutputDir = "$PSScriptRoot\..\bench_win_1Mframe_spin"
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repoRoot
if (-not (Test-Path $OutputDir)) {
    New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null
}
$OutputDir = (Resolve-Path $OutputDir).Path
Write-Host "Repo:   $repoRoot  HEAD=$(git rev-parse --short HEAD)"
Write-Host "Output: $OutputDir"

function Invoke-PreClean {
    Get-Process -Name 'shmemtcp*','go' -ErrorAction SilentlyContinue |
        Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Milliseconds 500
    Get-ChildItem $env:TEMP -Filter 'grpc_shm_*' -ErrorAction SilentlyContinue |
        Remove-Item -Force -ErrorAction SilentlyContinue
}

function Set-CommonEnv {
    # Reset everything we touch, then set the common baseline.
    'BENCH_PROFILE','SHM_BENCH_CPU','BENCH_DIRTY_DEFAULT_POOL',
        'SHM_MAX_FRAME_SIZE','SHM_SPIN_ITERS' | ForEach-Object {
        Remove-Item "Env:$_" -ErrorAction SilentlyContinue
    }
    $env:BENCH_PROFILE = 'fair-default'
    $env:SHM_BENCH_CPU = '1'
}

function Invoke-Cell {
    param(
        [string]$Label,
        [string]$MaxFrame,   # '' to leave unset
        [string]$SpinIters   # '' to leave unset
    )
    Set-CommonEnv
    if ($MaxFrame) { $env:SHM_MAX_FRAME_SIZE = $MaxFrame }
    if ($SpinIters) { $env:SHM_SPIN_ITERS = $SpinIters }
    Invoke-PreClean
    $log = Join-Path $OutputDir "$Label.txt"
    Write-Host "[$(Get-Date -Format HH:mm:ss)] $Label start  SHM_MAX_FRAME_SIZE=$($env:SHM_MAX_FRAME_SIZE) SHM_SPIN_ITERS=$($env:SHM_SPIN_ITERS)"
    $goArgs = @(
        'test',
        '-bench=^BenchmarkGRPC(Shm|Unix|TCP)(Unary|Stream|Concurrent)$',
        '-benchtime=2s', '-count=1', '-run=^$', '-timeout=2700s',
        '.\benchmark\shmemtcp\'
    )
    & go @goArgs *> $log
    $cells = (Select-String -Path $log -Pattern '^BenchmarkGRPC' | Measure-Object).Count
    Write-Host "[$(Get-Date -Format HH:mm:ss)] $Label done. Cells: $cells"
    Invoke-PreClean
}

# ---- Cell A: baseline (16 KiB frame, no spin) ----
Invoke-Cell -Label 'A_fair_default'      -MaxFrame ''        -SpinIters ''

# ---- Cell B: 1 MiB frame only ----
Invoke-Cell -Label 'B_fair_1Mframe'      -MaxFrame '1048576' -SpinIters ''

# ---- Cell C: 1 MiB frame + light spin ----
Invoke-Cell -Label 'C_fair_1Mframe_spin' -MaxFrame '1048576' -SpinIters '2000'

Pop-Location

Write-Host ""
Write-Host "DONE. Three result files under $OutputDir :"
Get-ChildItem $OutputDir -Filter '*.txt' | Format-Table Name, Length -AutoSize
