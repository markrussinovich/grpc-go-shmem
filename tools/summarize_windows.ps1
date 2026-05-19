$rows = @()
Get-ChildItem bench_win_fair\resources -Directory | ForEach-Object {
    $snap2 = Join-Path $_.FullName 'snap2.txt'
    $h = $t = $ws = $priv = $virt = $est = ''
    if (Test-Path $snap2) {
        $c = Get-Content $snap2 -Raw
        $m = [regex]::Match($c, 'HandleCount\s+:\s+(\d+)')
        if ($m.Success) { $h = $m.Groups[1].Value }
        $m = [regex]::Match($c, 'Threads\s+:\s+(\d+)')
        if ($m.Success) { $t = $m.Groups[1].Value }
        $m = [regex]::Match($c, 'WorkingSet_MB\s+:\s+([\d.]+)')
        if ($m.Success) { $ws = $m.Groups[1].Value }
        $m = [regex]::Match($c, 'PrivateMem_MB\s+:\s+([\d.]+)')
        if ($m.Success) { $priv = $m.Groups[1].Value }
        $m = [regex]::Match($c, 'VirtualMem_MB\s+:\s+([\d.]+)')
        if ($m.Success) { $virt = $m.Groups[1].Value }
        $m = [regex]::Match($c, 'Established\s+(\d+)')
        $est = if ($m.Success) { $m.Groups[1].Value } else { '0' }
    }
    $rows += [pscustomobject]@{
        Cell = $_.Name; Handles = $h; Threads = $t
        WS_MB = $ws; Priv_MB = $priv; Virt_MB = $virt; TCP_Est = $est
    }
}
$rows | Format-Table -AutoSize | Tee-Object tmp\fair_results\resources_summary_windows.txt
Write-Host ""
Write-Host "=== bench MB/s per cell ==="
Get-ChildItem bench_win_fair\resources -Directory | ForEach-Object {
    $info = Join-Path $_.FullName 'INFO.txt'
    if (Test-Path $info) {
        $line = (Get-Content $info | Where-Object { $_ -like 'BenchmarkGRPC*' } | Select-Object -Last 1)
        "{0,-22} {1}" -f $_.Name, $line
    }
} | Tee-Object tmp\fair_results\resources_bench_windows.txt
