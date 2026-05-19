#requires -Version 7
<#
.SYNOPSIS
    Windows resource snapshot collector. Skips bench A/B (already captured).
#>
param(
    [string]$OutputDir = "$PSScriptRoot\..\bench_win_fair"
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repoRoot

if (-not (Test-Path $OutputDir)) { New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null }
$OutputDir = (Resolve-Path $OutputDir).Path

function Invoke-PreClean {
    Get-Process -Name 'shmemtcp*','go' -ErrorAction SilentlyContinue |
        Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Milliseconds 500
    Get-ChildItem $env:TEMP -Filter 'grpc_shm_*' -ErrorAction SilentlyContinue |
        Remove-Item -Force -ErrorAction SilentlyContinue
}

function Set-BenchEnv {
    'BENCH_PROFILE','SHM_BENCH_CPU','BENCH_DIRTY_DEFAULT_POOL' | ForEach-Object {
        Remove-Item "Env:$_" -ErrorAction SilentlyContinue
    }
    $env:BENCH_PROFILE   = 'fair-default'
    $env:SHM_BENCH_CPU   = '1'
}

function Invoke-ResourceSnapshot {
    param([string]$CellPattern, [string]$CellLabel)
    Invoke-PreClean
    Set-BenchEnv
    $snapDir = Join-Path $OutputDir "resources\$CellLabel"
    if (Test-Path $snapDir) { Remove-Item $snapDir -Recurse -Force }
    New-Item -ItemType Directory -Force -Path $snapDir | Out-Null
    $benchLog = Join-Path $snapDir 'bench.log'
    $benchErr = Join-Path $snapDir 'bench.err'
    $info     = Join-Path $snapDir 'INFO.txt'

    Write-Host "[$CellLabel] launching..."
    $proc = Start-Process -FilePath 'go' -ArgumentList @(
        'test',
        "-bench=$CellPattern",
        '-benchtime=25s', '-count=1', '-run=^$', '-timeout=180s',
        '.\benchmark\shmemtcp\'
    ) -RedirectStandardOutput $benchLog -RedirectStandardError $benchErr `
      -NoNewWindow -PassThru

    "go PID = $($proc.Id)" | Out-File $info
    "pattern = $CellPattern" | Out-File $info -Append

    # Wait for stable shmemtcp.test child PID
    $testProc = $null
    $prevPid = -1
    $stable = 0
    for ($i = 0; $i -lt 40 -and -not $testProc; $i++) {
        Start-Sleep -Milliseconds 800
        $candidates = Get-Process -Name 'shmemtcp.test*' -ErrorAction SilentlyContinue
        if ($candidates) {
            $cand = $candidates | Select-Object -First 1
            if ($cand.Id -eq $prevPid) {
                $stable++
                if ($stable -ge 1) { $testProc = $cand }
            } else {
                $stable = 0
                $prevPid = $cand.Id
            }
        }
    }
    if (-not $testProc) {
        "FAILED: shmemtcp.test not stable" | Out-File $info -Append
        $proc | Wait-Process
        return
    }
    "test PID = $($testProc.Id)" | Out-File $info -Append

    Start-Sleep -Seconds 3

    1..3 | ForEach-Object {
        $snap = $_
        $out  = Join-Path $snapDir "snap${snap}.txt"
        $now  = Get-Date -Format 'yyyy-MM-dd HH:mm:ss'
        $p    = Get-Process -Id $testProc.Id -ErrorAction SilentlyContinue
        if (-not $p) {
            "snap ${snap}: process gone" | Add-Content $info
            return
        }
        $tcpConns = Get-NetTCPConnection -OwningProcess $p.Id -ErrorAction SilentlyContinue
        $tcpByState = if ($tcpConns) {
            ($tcpConns | Group-Object State |
                ForEach-Object { "{0,-15} {1}" -f $_.Name, $_.Count }) -join "`n"
        } else { '(none)' }
        $sample = if ($tcpConns) {
            ($tcpConns | Select-Object -First 8 |
                Format-Table -AutoSize | Out-String).Trim()
        } else { '(none)' }

        @"
=== TIMESTAMP $now snap=$snap ===

PID            : $($p.Id)
HandleCount    : $($p.HandleCount)
Threads        : $($p.Threads.Count)
WorkingSet_MB  : $([math]::Round($p.WorkingSet64 / 1MB, 2))
PrivateMem_MB  : $([math]::Round($p.PrivateMemorySize64 / 1MB, 2))
PagedMem_MB    : $([math]::Round($p.PagedMemorySize64 / 1MB, 2))
VirtualMem_MB  : $([math]::Round($p.VirtualMemorySize64 / 1MB, 2))
CPU_sec        : $([math]::Round($p.TotalProcessorTime.TotalSeconds, 2))

=== TCP connections by state ===
$tcpByState

=== TCP connections (head 8) ===
$sample
"@ | Out-File $out
        Start-Sleep -Seconds 2
    }

    $proc | Wait-Process
    "" | Add-Content $info
    "=== BENCH RESULT ===" | Add-Content $info
    Select-String -Path $benchLog -Pattern '^BenchmarkGRPC' |
        ForEach-Object { $_.Line } | Add-Content $info
    Write-Host "[$CellLabel] done"
}

try {
    $cells = @(
        @{ Pattern = '^BenchmarkGRPCShmUnary$/^size=64$';                          Label = 'Shm_unary_64B' }
        @{ Pattern = '^BenchmarkGRPCShmUnary$/^size=65536$';                       Label = 'Shm_unary_64K' }
        @{ Pattern = '^BenchmarkGRPCShmUnary$/^size=1MB$';                         Label = 'Shm_unary_1M' }
        @{ Pattern = '^BenchmarkGRPCShmConcurrent$/^streams=1000$/^size=65536$';   Label = 'Shm_conc_1000x64K' }
        @{ Pattern = '^BenchmarkGRPCUnixUnary$/^size=64$';                         Label = 'Unix_unary_64B' }
        @{ Pattern = '^BenchmarkGRPCUnixUnary$/^size=65536$';                      Label = 'Unix_unary_64K' }
        @{ Pattern = '^BenchmarkGRPCUnixUnary$/^size=1MB$';                        Label = 'Unix_unary_1M' }
        @{ Pattern = '^BenchmarkGRPCUnixConcurrent$/^streams=1000$/^size=65536$';  Label = 'Unix_conc_1000x64K' }
        @{ Pattern = '^BenchmarkGRPCTCPUnary$/^size=64$';                          Label = 'TCP_unary_64B' }
        @{ Pattern = '^BenchmarkGRPCTCPUnary$/^size=65536$';                       Label = 'TCP_unary_64K' }
        @{ Pattern = '^BenchmarkGRPCTCPUnary$/^size=1MB$';                         Label = 'TCP_unary_1M' }
        @{ Pattern = '^BenchmarkGRPCTCPConcurrent$/^streams=1000$/^size=65536$';   Label = 'TCP_conc_1000x64K' }
    )
    foreach ($c in $cells) {
        Invoke-ResourceSnapshot -CellPattern $c.Pattern -CellLabel $c.Label
    }
    Write-Host "ALL DONE"
}
finally {
    Pop-Location
}
