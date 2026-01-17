# Run shared memory gRPC examples on Windows
# Usage: powershell -ExecutionPolicy Bypass -File run_shmem_examples.ps1

$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$examplesDir = Join-Path $repoRoot "examples"
Set-Location $repoRoot

function Remove-SegmentFiles {
    param([string[]]$Names)
    foreach ($name in $Names) {
        foreach ($suffix in @("", "_ctl")) {
            $path = Join-Path $env:TEMP "grpc_shm_${name}${suffix}"
            if (Test-Path $path) {
                Remove-Item $path -Force -ErrorAction SilentlyContinue
            }
        }
    }
}

function Run-Step {
    param(
        [string]$Name,
        [scriptblock]$Action
    )
    Write-Host "=== $Name ==="
    try {
        & $Action
        $code = $LASTEXITCODE
        if ($code -ne 0) {
            throw "exit code $code"
        }
        Write-Host "SUCCESS`n"
        return $true
    }
    catch {
        Write-Warning "$Name failed: $_"
        return $false
    }
}

# Clean leftover segments from prior runs
$segments = @("helloworld_shm", "routeguide_shm", "my_segment", "grpc_echo", "my_service_segment")
Remove-SegmentFiles -Names $segments

$passes = 0
$fails = 0

# Example 1: shm_echo client demo
if (Run-Step "shm_echo_client" {
        Push-Location $examplesDir
        go run ./shm_echo/client
    $code = $LASTEXITCODE
        Pop-Location
    $global:LASTEXITCODE = $code
    }) { $passes++ } else { $fails++ }

# Example 2: shm_client_usage demo
if (Run-Step "shm_client_usage" {
        Push-Location $examplesDir
        go run ./shm_client_usage
    $code = $LASTEXITCODE
        Pop-Location
    $global:LASTEXITCODE = $code
    }) { $passes++ } else { $fails++ }

# Example 3: shm_server_usage demo (10s accept timeout inside)
if (Run-Step "shm_server_usage" {
        Push-Location $examplesDir
        go run ./shm_server_usage
    $code = $LASTEXITCODE
        Pop-Location
    $global:LASTEXITCODE = $code
    }) { $passes++ } else { $fails++ }

# Example 4: helloworld_shm client-server
$hwJob = Start-Job -ScriptBlock {
        param($examples)
        Set-Location $examples
        go run ./helloworld_shm/greeter_server
    } -ArgumentList $examplesDir
Start-Sleep -Seconds 5
if (Run-Step "helloworld_shm client" {
        Push-Location $examplesDir
        go run ./helloworld_shm/greeter_client -name "ShmTest"
    $code = $LASTEXITCODE
        Pop-Location
    $global:LASTEXITCODE = $code
    }) { $passes++ } else { $fails++ }
Stop-Job $hwJob -ErrorAction SilentlyContinue | Out-Null
$hwOutput = Receive-Job $hwJob -Keep -ErrorAction SilentlyContinue
if ($hwOutput) {
    Write-Host "[helloworld server output]"
    $hwOutput
}
Remove-SegmentFiles -Names @("helloworld_shm")

# Example 5: route_guide_shm client-server
$rgJob = Start-Job -ScriptBlock {
        param($examples)
        Set-Location $examples
        go run ./route_guide_shm/server
    } -ArgumentList $examplesDir
Start-Sleep -Seconds 5
if (Run-Step "route_guide_shm client" {
        Push-Location $examplesDir
        go run ./route_guide_shm/client
    $code = $LASTEXITCODE
        Pop-Location
    $global:LASTEXITCODE = $code
    }) { $passes++ } else { $fails++ }
Stop-Job $rgJob -ErrorAction SilentlyContinue | Out-Null
$rgOutput = Receive-Job $rgJob -Keep -ErrorAction SilentlyContinue
if ($rgOutput) {
    Write-Host "[route_guide server output]"
    $rgOutput
}
Remove-SegmentFiles -Names @("routeguide_shm")

Write-Host "Summary: Passed=$passes Failed=$fails"
if ($fails -gt 0) { exit 1 } else { exit 0 }
