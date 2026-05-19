$linA = Get-Content tmp\fair_results\A_fair_default.txt
$winA = Get-Content tmp\fair_results\A_fair_default_windows.txt

function Extract($lines, $pattern) {
    $line = $lines | Where-Object { $_ -match [regex]::Escape($pattern) } | Select-Object -First 1
    if (-not $line) { return @{ MBs='-'; Cpu='-'; CpuNs='-'; Agg='-' } }
    $out = @{ MBs='-'; Cpu='-'; CpuNs='-'; Agg='-' }
    if ($line -match '(\d+(?:\.\d+)?)\s+MB/s')        { $out.MBs   = [math]::Round([double]$matches[1], 1) }
    if ($line -match '(\d+(?:\.\d+)?)\s+%cpu')        { $out.Cpu   = [math]::Round([double]$matches[1], 0) }
    if ($line -match '(\d+)\s+cpu-ns/op')             { $out.CpuNs = [int64]$matches[1] }
    if ($line -match '(\d+(?:\.\d+)?)\s+aggregate-MB/s') { $out.Agg = [math]::Round([double]$matches[1], 1) }
    return $out
}

function Eff($mbs, $cpu) {
    if ($mbs -eq '-' -or $cpu -eq '-' -or [double]$cpu -eq 0) { return '-' }
    return [math]::Round([double]$mbs / ([double]$cpu / 100), 1)
}

$unarySizes = @(
    @{ K='64B';   sub='size=64-' },
    @{ K='4K';    sub='size=4096-' },
    @{ K='64K';   sub='size=65536-' },
    @{ K='256K';  sub='size=262144-' },
    @{ K='1M';    sub='size=1MB-' },
    @{ K='16M';   sub='size=16MB-' }
)
$concSizes = @(
    @{ K='64B';   sub='size=64-' },
    @{ K='4K';    sub='size=4096-' },
    @{ K='64K';   sub='size=65536-' },
    @{ K='256K';  sub='size=262144-' },
    @{ K='1M';    sub='size=1048576-' }
)

Write-Host ''
Write-Host '============ LINUX UNARY MB/s ============'
'{0,-6} | {1,-7} {2,-7} {3,-7}' -f 'Size','SHM','UDS','TCP'
foreach ($s in $unarySizes) {
    '{0,-6} | {1,-7} {2,-7} {3,-7}' -f $s.K,
        (Extract $linA "ShmUnary/$($s.sub)").MBs,
        (Extract $linA "UnixUnary/$($s.sub)").MBs,
        (Extract $linA "TCPUnary/$($s.sub)").MBs
}

Write-Host ''
Write-Host '============ WINDOWS UNARY MB/s (UDS now included) ============'
'{0,-6} | {1,-7} {2,-7} {3,-7}' -f 'Size','SHM','UDS','TCP'
foreach ($s in $unarySizes) {
    '{0,-6} | {1,-7} {2,-7} {3,-7}' -f $s.K,
        (Extract $winA "ShmUnary/$($s.sub)").MBs,
        (Extract $winA "UnixUnary/$($s.sub)").MBs,
        (Extract $winA "TCPUnary/$($s.sub)").MBs
}

Write-Host ''
Write-Host '============ LINUX 1000-stream Concurrent aggregate-MB/s ============'
'{0,-6} | {1,-7} {2,-7} {3,-7}' -f 'Size','SHM','UDS','TCP'
foreach ($s in $concSizes) {
    '{0,-6} | {1,-7} {2,-7} {3,-7}' -f $s.K,
        (Extract $linA "ShmConcurrent/streams=1000/$($s.sub)").Agg,
        (Extract $linA "UnixConcurrent/streams=1000/$($s.sub)").Agg,
        (Extract $linA "TCPConcurrent/streams=1000/$($s.sub)").Agg
}

Write-Host ''
Write-Host '============ WINDOWS 1000-stream Concurrent aggregate-MB/s ============'
'{0,-6} | {1,-7} {2,-7} {3,-7}' -f 'Size','SHM','UDS','TCP'
foreach ($s in $concSizes) {
    '{0,-6} | {1,-7} {2,-7} {3,-7}' -f $s.K,
        (Extract $winA "ShmConcurrent/streams=1000/$($s.sub)").Agg,
        (Extract $winA "UnixConcurrent/streams=1000/$($s.sub)").Agg,
        (Extract $winA "TCPConcurrent/streams=1000/$($s.sub)").Agg
}

Write-Host ''
Write-Host '============ UNARY CPU EFFICIENCY (MB/s/core) ============'
'{0,-6} | {1,-15} {2,-15} {3,-15}' -f 'Size','LinSHM/UDS/TCP','WinSHM/UDS/TCP',''
foreach ($s in $unarySizes) {
    $lshm = Eff (Extract $linA "ShmUnary/$($s.sub)").MBs (Extract $linA "ShmUnary/$($s.sub)").Cpu
    $luds = Eff (Extract $linA "UnixUnary/$($s.sub)").MBs (Extract $linA "UnixUnary/$($s.sub)").Cpu
    $ltcp = Eff (Extract $linA "TCPUnary/$($s.sub)").MBs (Extract $linA "TCPUnary/$($s.sub)").Cpu
    $wshm = Eff (Extract $winA "ShmUnary/$($s.sub)").MBs (Extract $winA "ShmUnary/$($s.sub)").Cpu
    $wuds = Eff (Extract $winA "UnixUnary/$($s.sub)").MBs (Extract $winA "UnixUnary/$($s.sub)").Cpu
    $wtcp = Eff (Extract $winA "TCPUnary/$($s.sub)").MBs (Extract $winA "TCPUnary/$($s.sub)").Cpu
    '{0,-6} | {1,5}/{2,5}/{3,5}    {4,5}/{5,5}/{6,5}' -f $s.K, $lshm, $luds, $ltcp, $wshm, $wuds, $wtcp
}

Write-Host ''
Write-Host '============ 1000-stream Concurrent CPU EFFICIENCY (agg-MB/s/core) ============'
'{0,-6} | {1,-15} {2,-15} {3,-15}' -f 'Size','LinSHM/UDS/TCP','WinSHM/UDS/TCP',''
foreach ($s in $concSizes) {
    $lshm = Eff (Extract $linA "ShmConcurrent/streams=1000/$($s.sub)").Agg (Extract $linA "ShmConcurrent/streams=1000/$($s.sub)").Cpu
    $luds = Eff (Extract $linA "UnixConcurrent/streams=1000/$($s.sub)").Agg (Extract $linA "UnixConcurrent/streams=1000/$($s.sub)").Cpu
    $ltcp = Eff (Extract $linA "TCPConcurrent/streams=1000/$($s.sub)").Agg (Extract $linA "TCPConcurrent/streams=1000/$($s.sub)").Cpu
    $wshm = Eff (Extract $winA "ShmConcurrent/streams=1000/$($s.sub)").Agg (Extract $winA "ShmConcurrent/streams=1000/$($s.sub)").Cpu
    $wuds = Eff (Extract $winA "UnixConcurrent/streams=1000/$($s.sub)").Agg (Extract $winA "UnixConcurrent/streams=1000/$($s.sub)").Cpu
    $wtcp = Eff (Extract $winA "TCPConcurrent/streams=1000/$($s.sub)").Agg (Extract $winA "TCPConcurrent/streams=1000/$($s.sub)").Cpu
    '{0,-6} | {1,5}/{2,5}/{3,5}    {4,5}/{5,5}/{6,5}' -f $s.K, $lshm, $luds, $ltcp, $wshm, $wuds, $wtcp
}
