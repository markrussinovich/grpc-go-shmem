<#
.SYNOPSIS
  Internal scenario sweep for the gRPC Transport Showdown demo.
  Runs the Go and .NET engines across every transport, profile, and payload,
  prints a result table, and flags any errors or cases where SHM does not win.

.DESCRIPTION
  This is a DEV-ONLY helper. It is not part of the shipped/zipped demo -- copy it
  next to a built `dist` (or `dist-arm64`) folder and run it to sanity-check a
  machine (e.g. an ARM box) before a talk.

  It drives the engines directly (same binaries the web shell spawns):
    Go    :  <dist>\demo.exe --role engine --transport all ...
    .NET  :  <dist>\dotnet-engine\ShmDemo.Engine.exe --transport all ...
  Each engine emits NDJSON; this script parses the `result` / `error` events.

.PARAMETER DistDir
  Folder containing demo.exe (and optionally dotnet-engine\). Defaults to the
  script's own folder, then a `dist` subfolder, then a sibling `dist`.

.PARAMETER Payloads
  Payload sizes in bytes to sweep. Accepts plain numbers.

.PARAMETER Profiles
  fair, max, or both (default both).

.PARAMETER Langs
  go, dotnet, or both (default both; dotnet is skipped if not bundled).

.PARAMETER WarmupMs / MeasureMs
  Per-phase timing. Defaults match the web shell (600 / 2500).

.PARAMETER TimeoutSec
  Per-case wall-clock budget before the engine is declared hung and its process
  tree is killed. 0 (default) auto-scales with warmup+measure.

.EXAMPLE
  .\test-all.ps1
.EXAMPLE
  .\test-all.ps1 -DistDir .\dist-arm64 -Payloads 65536,1048576 -Profiles max
#>
[CmdletBinding()]
param(
    [string]   $DistDir,
    [long[]]   $Payloads = @(1024, 4096, 65536, 262144, 1048576, 4194304, 16777216, 67108864, 268435456),
    [ValidateSet('fair', 'max')]
    [string[]] $Profiles = @('fair', 'max'),
    [ValidateSet('go', 'dotnet')]
    [string[]] $Langs = @('go', 'dotnet'),
    [int]      $WarmupMs = 600,
    [int]      $MeasureMs = 2500,
    # Per-case wall-clock budget before the engine is declared hung and killed.
    # 0 = auto: scales with warmup+measure across the 3 transports, with
    # headroom for process start, connection setup, and large-payload final
    # iterations, so big payloads do not false-positive.
    [int]      $TimeoutSec = 0
)

$ErrorActionPreference = 'Stop'

function Resolve-DistDir {
    param([string]$Hint)
    $candidates = @()
    if ($Hint) { $candidates += $Hint }
    $here = $PSScriptRoot
    $candidates += $here
    $candidates += (Join-Path $here 'dist')
    $candidates += (Join-Path $here 'dist-arm64')
    $candidates += (Join-Path (Split-Path $here -Parent) 'dist')
    foreach ($c in $candidates) {
        if ($c -and (Test-Path (Join-Path $c 'demo.exe'))) {
            return (Resolve-Path $c).Path
        }
    }
    throw "Could not find demo.exe. Pass -DistDir pointing at the folder that contains demo.exe. Tried: $($candidates -join '; ')"
}

function Format-Bytes {
    param([long]$n)
    if ($n -ge 1MB) { return ("{0}MB" -f ($n / 1MB)) }
    if ($n -ge 1KB) { return ("{0}KB" -f ($n / 1KB)) }
    return "${n}B"
}

# Runs one engine invocation and returns parsed result/error rows.
function Invoke-Engine {
    param(
        [string]$Exe,
        [string[]]$BaseArgs,
        [string]$Lang,
        [string]$Profile,
        [long]$Payload,
        [int]$TimeoutSec
    )
    $args = $BaseArgs + @(
        '--transport', 'all',
        '--payload', "$Payload",
        '--profile', $Profile,
        '--warmup-ms', "$WarmupMs",
        '--measure-ms', "$MeasureMs"
    )
    $rows = @()
    # Run the engine as a child process with a hard wall-clock budget so a
    # deadlocked transport cannot hang the whole sweep. stdout/stderr are
    # redirected to temp files (Start-Process cannot capture inline). On
    # timeout the entire process TREE is killed (the engine spawns server
    # children) and the case is reported as a hang.
    $outFile = [System.IO.Path]::GetTempFileName()
    $errFile = [System.IO.Path]::GetTempFileName()
    $proc = $null
    try {
        try {
            $proc = Start-Process -FilePath $Exe -ArgumentList $args -PassThru `
                -NoNewWindow -RedirectStandardOutput $outFile -RedirectStandardError $errFile
        }
        catch {
            return [pscustomobject]@{ Rows = @(); Fatal = $_.Exception.Message; Hung = $false }
        }
        if (-not $proc.WaitForExit($TimeoutSec * 1000)) {
            # Hung: kill the engine and every child it spawned.
            & taskkill.exe /T /F /PID $proc.Id 2>$null | Out-Null
            try { $proc.WaitForExit(5000) | Out-Null } catch { }
            return [pscustomobject]@{
                Rows  = @()
                Fatal = "HANG: no exit within ${TimeoutSec}s (killed process tree)"
                Hung  = $true
            }
        }
        $out = Get-Content -LiteralPath $outFile -ErrorAction SilentlyContinue
    }
    finally {
        Remove-Item -LiteralPath $outFile, $errFile -Force -ErrorAction SilentlyContinue
    }
    foreach ($line in $out) {
        $t = "$line".Trim()
        if (-not $t.StartsWith('{')) { continue }
        try { $ev = $t | ConvertFrom-Json } catch { continue }
        switch ($ev.type) {
            'result' {
                $rows += [pscustomobject]@{
                    Lang      = $Lang
                    Profile   = $Profile
                    Payload   = $Payload
                    Transport = $ev.transport
                    P50us     = [double]$ev.latencyP50Us
                    P99us     = [double]$ev.latencyP99Us
                    MBps      = [double]$ev.mbPerSec
                    MsgPerSec = [double]$ev.msgPerSec
                    CPUper1M  = [double]$ev.cpuSecPer1M
                    Error     = ''
                }
            }
            'error' {
                $rows += [pscustomobject]@{
                    Lang = $Lang; Profile = $Profile; Payload = $Payload
                    Transport = $ev.transport; P50us = $null; P99us = $null
                    MBps = $null; MsgPerSec = $null; CPUper1M = $null
                    Error = "$($ev.error)"
                }
            }
        }
    }
    return [pscustomobject]@{ Rows = $rows; Fatal = $null; Hung = $false }
}

# ---- main ---------------------------------------------------------------

$dist = Resolve-DistDir -Hint $DistDir
$goExe = Join-Path $dist 'demo.exe'
$dotnetExe = Join-Path $dist 'dotnet-engine\ShmDemo.Engine.exe'

Write-Host "Dist     : $dist" -ForegroundColor Cyan
Write-Host "Go engine: $goExe"
if (Test-Path $dotnetExe) {
    Write-Host ".NET eng : $dotnetExe"
}
else {
    Write-Host ".NET eng : (not bundled -- skipping dotnet)" -ForegroundColor DarkYellow
    $Langs = $Langs | Where-Object { $_ -ne 'dotnet' }
}
Write-Host ("Profiles : {0}   Payloads: {1}   warmup={2}ms measure={3}ms" -f `
    ($Profiles -join ','), ($Payloads.ForEach({ Format-Bytes $_ }) -join ','), $WarmupMs, $MeasureMs)

# Per-case hang budget. Auto: 3 transports x (warmup+measure), x4 headroom for
# process start, connection setup, and slow large-payload final iterations,
# floored at 60s.
$caseTimeoutSec = if ($TimeoutSec -gt 0) {
    $TimeoutSec
}
else {
    [int][math]::Max(60, 3 * ($WarmupMs + $MeasureMs) / 1000.0 * 4)
}
Write-Host ("Timeout  : {0}s per case (hang detection)" -f $caseTimeoutSec)
Write-Host ""

$all = New-Object System.Collections.Generic.List[object]
$errors = New-Object System.Collections.Generic.List[object]
$shmLosses = New-Object System.Collections.Generic.List[object]
$ratios = New-Object System.Collections.Generic.List[object]
$hangs = New-Object System.Collections.Generic.List[object]

foreach ($lang in $Langs) {
    foreach ($profile in $Profiles) {
        foreach ($payload in $Payloads) {
            $label = "{0,-6} {1,-4} {2,-6}" -f $lang, $profile, (Format-Bytes $payload)
            Write-Host -NoNewline ("  running {0} ... " -f $label)
            if ($lang -eq 'go') {
                $res = Invoke-Engine -Exe $goExe -BaseArgs @('--role', 'engine') -Lang go -Profile $profile -Payload $payload -TimeoutSec $caseTimeoutSec
            }
            else {
                $res = Invoke-Engine -Exe $dotnetExe -BaseArgs @() -Lang dotnet -Profile $profile -Payload $payload -TimeoutSec $caseTimeoutSec
            }
            if ($res.Fatal) {
                if ($res.Hung) {
                    Write-Host "HANG: killed after ${caseTimeoutSec}s" -ForegroundColor Red
                    $hangs.Add([pscustomobject]@{ Case = $label; Detail = $res.Fatal })
                }
                else {
                    Write-Host "FATAL: $($res.Fatal)" -ForegroundColor Red
                }
                $errors.Add([pscustomobject]@{ Case = $label; Detail = $res.Fatal })
                continue
            }
            $rowsByT = @{}
            foreach ($r in $res.Rows) {
                $all.Add($r)
                if ($r.Error) {
                    $errors.Add([pscustomobject]@{ Case = "$label $($r.Transport)"; Detail = $r.Error })
                }
                elseif ($r.Transport) {
                    $rowsByT[$r.Transport] = $r
                }
            }
            # SHM-wins check (throughput).
            $shm = $rowsByT['shm']; $uds = $rowsByT['uds']; $tcp = $rowsByT['tcp']
            if ($shm) {
                $rivals = @($uds, $tcp) | Where-Object { $_ }
                $maxRival = ($rivals | Measure-Object -Property MBps -Maximum).Maximum
                # Record SHM speedup vs each rival for the ratios summary.
                $vsTcp = if ($tcp -and $tcp.MBps -gt 0) { $shm.MBps / $tcp.MBps } else { $null }
                $vsUds = if ($uds -and $uds.MBps -gt 0) { $shm.MBps / $uds.MBps } else { $null }
                $ratios.Add([pscustomobject]@{
                        Lang     = $lang; Profile = $profile; Payload = $payload
                        ShmMBps  = $shm.MBps
                        TcpMBps  = if ($tcp) { $tcp.MBps } else { $null }
                        UdsMBps  = if ($uds) { $uds.MBps } else { $null }
                        VsTcp    = $vsTcp
                        VsUds    = $vsUds
                    })
                if ($null -ne $maxRival -and $shm.MBps -le $maxRival) {
                    $shmLosses.Add([pscustomobject]@{ Case = $label; ShmMBps = $shm.MBps; RivalMBps = $maxRival })
                    Write-Host ("ok (SHM NOT winning: {0:n0} <= {1:n0} MB/s)" -f $shm.MBps, $maxRival) -ForegroundColor Yellow
                }
                else {
                    $ratio = if ($maxRival -gt 0) { $shm.MBps / $maxRival } else { [double]::PositiveInfinity }
                    Write-Host ("ok (SHM {0:n0} MB/s, {1:n1}x)" -f $shm.MBps, $ratio) -ForegroundColor Green
                }
            }
            elseif ($res.Rows.Count -gt 0 -and -not ($res.Rows | Where-Object Error)) {
                Write-Host "ok" -ForegroundColor Green
            }
            else {
                Write-Host "no result" -ForegroundColor Red
            }
        }
    }
}

Write-Host ""
Write-Host "================ RESULTS ================" -ForegroundColor Cyan
$all | Where-Object { -not $_.Error } |
    Sort-Object Lang, Profile, Payload, @{ Expression = { @('tcp', 'uds', 'shm').IndexOf($_.Transport) } } |
    Format-Table `
        Lang, Profile,
    @{ N = 'Payload'; E = { Format-Bytes $_.Payload }; A = 'right' },
    Transport,
    @{ N = 'P50us'; E = { '{0:n1}' -f $_.P50us }; A = 'right' },
    @{ N = 'MB/s'; E = { '{0:n1}' -f $_.MBps }; A = 'right' },
    @{ N = 'CPU/1M'; E = { '{0:n3}' -f $_.CPUper1M }; A = 'right' } `
        -AutoSize

Write-Host "================ SUMMARY ================" -ForegroundColor Cyan
Write-Host ("Cases run : {0}   result rows: {1}" -f `
    ($Langs.Count * $Profiles.Count * $Payloads.Count), ($all | Where-Object { -not $_.Error }).Count)

if ($ratios.Count -gt 0) {
    Write-Host ""
    Write-Host "SHM speedup (throughput) vs UDS and TCP, every case:" -ForegroundColor Cyan
    $ratios |
        Sort-Object Lang, Profile, Payload |
        Format-Table `
            Lang, Profile,
        @{ N = 'Payload'; E = { Format-Bytes $_.Payload }; A = 'right' },
        @{ N = 'SHM MB/s'; E = { '{0:n1}' -f $_.ShmMBps }; A = 'right' },
        @{ N = 'UDS MB/s'; E = { if ($null -ne $_.UdsMBps) { '{0:n1}' -f $_.UdsMBps } else { '-' } }; A = 'right' },
        @{ N = 'TCP MB/s'; E = { if ($null -ne $_.TcpMBps) { '{0:n1}' -f $_.TcpMBps } else { '-' } }; A = 'right' },
        @{ N = 'vs UDS'; E = { if ($null -ne $_.VsUds) { '{0:n1}x' -f $_.VsUds } else { '-' } }; A = 'right' },
        @{ N = 'vs TCP'; E = { if ($null -ne $_.VsTcp) { '{0:n1}x' -f $_.VsTcp } else { '-' } }; A = 'right' } `
            -AutoSize

    $vsUdsVals = $ratios | Where-Object { $null -ne $_.VsUds } | Select-Object -ExpandProperty VsUds
    $vsTcpVals = $ratios | Where-Object { $null -ne $_.VsTcp } | Select-Object -ExpandProperty VsTcp
    if ($vsUdsVals) {
        $u = $vsUdsVals | Measure-Object -Minimum -Maximum -Average
        Write-Host ("  vs UDS : min {0:n1}x   avg {1:n1}x   max {2:n1}x" -f $u.Minimum, $u.Average, $u.Maximum)
    }
    if ($vsTcpVals) {
        $t = $vsTcpVals | Measure-Object -Minimum -Maximum -Average
        Write-Host ("  vs TCP : min {0:n1}x   avg {1:n1}x   max {2:n1}x" -f $t.Minimum, $t.Average, $t.Maximum)
    }
}

if ($shmLosses.Count -gt 0) {
    Write-Host ""
    Write-Host "SHM did NOT win throughput in these cases:" -ForegroundColor Yellow
    $shmLosses | Format-Table Case, @{ N = 'SHM MB/s'; E = { '{0:n1}' -f $_.ShmMBps } }, @{ N = 'Rival MB/s'; E = { '{0:n1}' -f $_.RivalMBps } } -AutoSize
}
else {
    Write-Host "SHM won throughput in every case." -ForegroundColor Green
}

if ($hangs.Count -gt 0) {
    Write-Host ""
    Write-Host ("HANGS (killed after {0}s -- a transport deadlocked):" -f $caseTimeoutSec) -ForegroundColor Red
    $hangs | Format-Table Case, Detail -AutoSize -Wrap
}

if ($errors.Count -gt 0) {
    Write-Host ""
    Write-Host "ERRORS:" -ForegroundColor Red
    $errors | Format-Table Case, Detail -AutoSize -Wrap
    exit 1
}
else {
    Write-Host "No errors." -ForegroundColor Green
    exit 0
}
