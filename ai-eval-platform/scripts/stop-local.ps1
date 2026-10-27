# Stop the processes started by scripts\start-local.ps1.
# Data in data\pgdata and data\telemetry.duckdb is kept.

$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

function Stop-FromPidFile($name) {
    $pidFile = Join-Path $root "data\$name.pid"
    if (-not (Test-Path $pidFile)) { return }
    $procId = Get-Content $pidFile -ErrorAction SilentlyContinue
    if ($procId) {
        Write-Host "Stopping $name (PID $procId) ..."
        & taskkill /PID $procId /T /F *> $null
    }
    Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
}

Stop-FromPidFile "vite"
Stop-FromPidFile "api"

$cmd = Get-Command pg_ctl -ErrorAction SilentlyContinue
$pgBin = if ($cmd) { Split-Path -Parent $cmd.Source } else { $null }
if (-not $pgBin) {
    foreach ($candidate in @(
        "C:\Program Files\PostgreSQL\16\bin",
        "C:\Program Files\PostgreSQL\15\bin",
        "C:\Program Files\PostgreSQL\14\bin"
    )) {
        if (Test-Path (Join-Path $candidate "pg_ctl.exe")) { $pgBin = $candidate; break }
    }
}

$pgData = Join-Path $root "data\pgdata"
if ($pgBin -and (Test-Path $pgData)) {
    & (Join-Path $pgBin "pg_ctl.exe") -D $pgData status *> $null
    if ($LASTEXITCODE -eq 0) {
        Write-Host "Stopping PostgreSQL ..."
        & (Join-Path $pgBin "pg_ctl.exe") -D $pgData -m fast stop
    }
}

Write-Host "EvalForge stopped. Data is still in data\."
