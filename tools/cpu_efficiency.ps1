$linA = Get-Content tmp\fair_results\A_fair_default.txt
$winA = Get-Content tmp\fair_results\A_fair_default_windows.txt

function Extract($lines, $pattern) {
    $line = $lines | Where-Object { $_ -match [regex]::Escape($pattern) } | Select-Object -First 1
    if (-not $line) { return @{ MBs='-'; Cpu='-'; CpuNs='-'; Agg='-' } }
    $out = @{ MBs='-'; Cpu='-'; CpuNs='-'; Agg='-' }
    if ($line -match '(\d+(?:\.\d+)?)\s+MB/s') { $out.MBs = [math]::Round([double]$matches[1], 1) }
    if ($line -match '(\d+(?:\.\d+)?)\s+%cpu') { $out.Cpu = [math]::Round([double]$matches[1], 0) }
    if ($line -match '(\d+)\s+cpu-ns/op') { $out.CpuNs = [int64]$matches[1] }
    if ($line -match '(\d+(?:\.\d+)?)\s+aggregate-MB/s') { $out.Agg = [math]::Round([double]$matches[1], 1) }
    return $out
}

function PrintRow($label, $shm, $uds, $tcp) {
    if ($null -eq $uds) {
        '{0,-12} | SHM: {1,7} MB/s, {2,5}% CPU, {3,12} cpu-ns/op | TCP: {4,7} MB/s, {5,5}% CPU, {6,12} cpu-ns/op' -f $label, $shm.MBs, $shm.Cpu, $shm.CpuNs, $tcp.MBs, $tcp.Cpu, $tcp.CpuNs
    } else {
        '{0,-12} | SHM: {1,7} MB/s, {2,5}% CPU, {3,12} cpu-ns/op | UDS: {4,7} MB/s, {5,5}% CPU, {6,12} cpu-ns/op | TCP: {7,7} MB/s, {8,5}% CPU, {9,12} cpu-ns/op' -f $label, $shm.MBs, $shm.Cpu, $shm.CpuNs, $uds.MBs, $uds.Cpu, $uds.CpuNs, $tcp.MBs, $tcp.Cpu, $tcp.CpuNs
    }
}

$unarySizes = @(
    @{ K='64B';   sub='size=64-' },
    @{ K='4K';    sub='size=4096-' },
    @{ K='64K';   sub='size=65536-' },
    @{ K='256K';  sub='size=262144-' },
    @{ K='1M';    sub='size=1MB-' },
    @{ K='16M';   sub='size=16MB-' },
    @{ K='256M';  sub='size=256MB-' }
)

Write-Host ''
Write-Host '=================================================='
Write-Host '  LINUX UNARY: MB/s / %CPU / cpu-ns/op'
Write-Host '=================================================='
foreach ($s in $unarySizes) {
    $shm = Extract $linA "ShmUnary/$($s.sub)"
    $uds = Extract $linA "UnixUnary/$($s.sub)"
    $tcp = Extract $linA "TCPUnary/$($s.sub)"
    PrintRow $s.K $shm $uds $tcp
}

Write-Host ''
Write-Host '=================================================='
Write-Host '  WINDOWS UNARY: MB/s / %CPU / cpu-ns/op'
Write-Host '=================================================='
foreach ($s in $unarySizes) {
    $shm = Extract $winA "ShmUnary/$($s.sub)"
    $tcp = Extract $winA "TCPUnary/$($s.sub)"
    PrintRow $s.K $shm $null $tcp
}

$concSizes = @(
    @{ K='64B';   sub='size=64-' },
    @{ K='4K';    sub='size=4096-' },
    @{ K='64K';   sub='size=65536-' },
    @{ K='256K';  sub='size=262144-' },
    @{ K='1M';    sub='size=1048576-' }
)

Write-Host ''
Write-Host '=================================================='
Write-Host '  LINUX 1000-STREAM CONCURRENT'
Write-Host '=================================================='
foreach ($s in $concSizes) {
    $shm = Extract $linA "ShmConcurrent/streams=1000/$($s.sub)"
    $uds = Extract $linA "UnixConcurrent/streams=1000/$($s.sub)"
    $tcp = Extract $linA "TCPConcurrent/streams=1000/$($s.sub)"
    '{0,-6} | SHM agg={1,6} MB/s, {2,5}% CPU | UDS agg={3,6} MB/s, {4,5}% CPU | TCP agg={5,6} MB/s, {6,5}% CPU' -f $s.K, $shm.Agg, $shm.Cpu, $uds.Agg, $uds.Cpu, $tcp.Agg, $tcp.Cpu
}

# Efficiency: MB throughput per 1% CPU (higher = better)
Write-Host ''
Write-Host '=================================================='
Write-Host '  LINUX UNARY CPU EFFICIENCY = MB/s / (CPU%/100)'
Write-Host '   (i.e., MB delivered per CPU-core fully utilised)'
Write-Host '=================================================='
'{0,-6} | {1,-12} | {2,-12} | {3,-12} | {4,-10} | {5,-10}' -f 'Size','SHM (MB/s/core)','UDS (MB/s/core)','TCP (MB/s/core)','SHM/UDS','SHM/TCP'
foreach ($s in $unarySizes) {
    $shm = Extract $linA "ShmUnary/$($s.sub)"
    $uds = Extract $linA "UnixUnary/$($s.sub)"
    $tcp = Extract $linA "TCPUnary/$($s.sub)"
    if ($shm.MBs -eq '-' -or $shm.Cpu -eq '-' -or $shm.Cpu -eq 0) { continue }
    $shmE = [math]::Round([double]$shm.MBs / ([double]$shm.Cpu / 100), 1)
    $udsE = if ($uds.MBs -ne '-' -and $uds.Cpu -ne 0) { [math]::Round([double]$uds.MBs / ([double]$uds.Cpu / 100), 1) } else { '-' }
    $tcpE = if ($tcp.MBs -ne '-' -and $tcp.Cpu -ne 0) { [math]::Round([double]$tcp.MBs / ([double]$tcp.Cpu / 100), 1) } else { '-' }
    $rUds = if ($udsE -ne '-') { [math]::Round($shmE / $udsE, 2) } else { '-' }
    $rTcp = if ($tcpE -ne '-') { [math]::Round($shmE / $tcpE, 2) } else { '-' }
    '{0,-6} | {1,-15} | {2,-15} | {3,-15} | {4,-10} | {5,-10}' -f $s.K, $shmE, $udsE, $tcpE, $rUds, $rTcp
}

Write-Host ''
Write-Host '=================================================='
Write-Host '  LINUX 1000-STREAM CONCURRENT CPU EFFICIENCY'
Write-Host '   = aggregate-MB/s / (CPU%/100)'
Write-Host '=================================================='
'{0,-6} | {1,-15} | {2,-15} | {3,-15} | {4,-10} | {5,-10}' -f 'Size','SHM','UDS','TCP','SHM/UDS','SHM/TCP'
foreach ($s in $concSizes) {
    $shm = Extract $linA "ShmConcurrent/streams=1000/$($s.sub)"
    $uds = Extract $linA "UnixConcurrent/streams=1000/$($s.sub)"
    $tcp = Extract $linA "TCPConcurrent/streams=1000/$($s.sub)"
    if ($shm.Agg -eq '-' -or $shm.Cpu -eq '-' -or $shm.Cpu -eq 0) { continue }
    $shmE = [math]::Round([double]$shm.Agg / ([double]$shm.Cpu / 100), 1)
    $udsE = [math]::Round([double]$uds.Agg / ([double]$uds.Cpu / 100), 1)
    $tcpE = [math]::Round([double]$tcp.Agg / ([double]$tcp.Cpu / 100), 1)
    $rUds = [math]::Round($shmE / $udsE, 2)
    $rTcp = [math]::Round($shmE / $tcpE, 2)
    '{0,-6} | {1,-15} | {2,-15} | {3,-15} | {4,-10} | {5,-10}' -f $s.K, $shmE, $udsE, $tcpE, $rUds, $rTcp
}
