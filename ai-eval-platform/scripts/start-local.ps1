# Start EvalForge locally without Docker.
#
#   1. project-local PostgreSQL on 127.0.0.1:55432
#   2. Go API (cgo + DuckDB) on http://localhost:8000
#   3. Vite console on http://127.0.0.1:5173
#
# Logs: data\api.log, data\api.err.log, data\vite.log
# Stop: .\scripts\stop-local.ps1
#
# Run from anywhere:  powershell -ExecutionPolicy Bypass -File .\scripts\start-local.ps1

$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

function Find-PgBin {
    $cmd = Get-Command pg_ctl -ErrorAction SilentlyContinue
    if ($cmd) { return Split-Path -Parent $cmd.Source }
    foreach ($candidate in @(
        "C:\Program Files\PostgreSQL\16\bin",
        "C:\Program Files\PostgreSQL\15\bin",
        "C:\Program Files\PostgreSQL\14\bin"
    )) {
        if (Test-Path (Join-Path $candidate "pg_ctl.exe")) { return $candidate }
    }
    throw "PostgreSQL not found. Install PostgreSQL 16, or add its bin folder to PATH."
}

function Find-Gcc {
    $cmd = Get-Command gcc -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }

    $packages = Join-Path $env:LOCALAPPDATA "Microsoft\WinGet\Packages"
    if (Test-Path $packages) {
        $winlibs = Get-ChildItem $packages -Directory -Filter "BrechtSanders.WinLibs.*" -ErrorAction SilentlyContinue |
            Select-Object -First 1
        if ($winlibs) {
            $gcc = Join-Path $winlibs.FullName "mingw64\bin\gcc.exe"
            if (Test-Path $gcc) { return $gcc }
        }
    }
    throw "gcc not found. The DuckDB driver needs cgo. Install it with: winget install --id BrechtSanders.WinLibs.POSIX.UCRT --exact"
}

function Wait-ForHttp($url, $seconds) {
    $deadline = (Get-Date).AddSeconds($seconds)
    while ((Get-Date) -lt $deadline) {
        try {
            $r = Invoke-WebRequest $url -TimeoutSec 2 -UseBasicParsing
            if ($r.StatusCode -eq 200) { return $true }
        } catch {
            Start-Sleep -Milliseconds 500
        }
    }
    return $false
}

# ---------- PostgreSQL ----------
$pgBin = Find-PgBin
$pgData = Join-Path $root "data\pgdata"
$pgLog = Join-Path $root "data\pg.log"

if (-not (Test-Path $pgData)) {
    Write-Host "Creating the local PostgreSQL cluster in data\pgdata ..."
    & (Join-Path $pgBin "initdb.exe") -D $pgData -U evalforge --auth=trust --encoding=UTF8 --no-locale
    if ($LASTEXITCODE -ne 0) { throw "initdb failed" }
}

& (Join-Path $pgBin "pg_ctl.exe") -D $pgData status *> $null
if ($LASTEXITCODE -ne 0) {
    Write-Host "Starting PostgreSQL on 127.0.0.1:55432 ..."
    $pgArgs = @('-D', "`"$pgData`"", '-o', '"-p 55432 -h 127.0.0.1"', '-l', "`"$pgLog`"", 'start')
    # -Wait can wait for the postgres server itself, which never exits; wait for
    # pg_ctl only, because pg_ctl exits once the server is ready.
    $p = Start-Process -FilePath (Join-Path $pgBin "pg_ctl.exe") -ArgumentList $pgArgs -WindowStyle Hidden -PassThru
    $p.WaitForExit()
    if ($p.ExitCode -ne 0) { throw "pg_ctl start failed; see $pgLog" }
}

foreach ($db in @("evalforge", "evalforge_test")) {
    $exists = & (Join-Path $pgBin "psql.exe") -w -h 127.0.0.1 -p 55432 -U evalforge -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='$db'"
    if ($exists -ne "1") {
        Write-Host "Creating database $db ..."
        & (Join-Path $pgBin "createdb.exe") -w -h 127.0.0.1 -p 55432 -U evalforge $db
        if ($LASTEXITCODE -ne 0) { throw "createdb $db failed" }
    }
}
Write-Host "PostgreSQL is ready."

# ---------- API ----------
$gcc = Find-Gcc
$gccDir = Split-Path -Parent $gcc
$env:PATH = "$gccDir;$env:PATH"
$env:CGO_ENABLED = "1"
$env:CC = $gcc
$env:AUTH_MODE = "off"
$env:LOG_LEVEL = "info"
$env:CONTROL_DB_URL = "postgres://evalforge@127.0.0.1:55432/evalforge?sslmode=disable"

$apiHealthy = $false
try {
    $health = Invoke-RestMethod "http://localhost:8000/health" -TimeoutSec 2
    $apiHealthy = [bool]$health.ok
} catch {}

if ($apiHealthy) {
    Write-Host "API is already running on http://localhost:8000"
} else {
    Write-Host "Building the Go API with cgo ..."
    Push-Location (Join-Path $root "server-go")
    & go build -o evalforge.exe ./cmd/server
    if ($LASTEXITCODE -ne 0) { Pop-Location; throw "go build failed" }
    Pop-Location

    Write-Host "Starting the API on http://localhost:8000 ..."
    $apiOut = Join-Path $root "data\api.log"
    $apiErr = Join-Path $root "data\api.err.log"
    $api = Start-Process -FilePath (Join-Path $root "server-go\evalforge.exe") `
        -WorkingDirectory (Join-Path $root "server-go") `
        -RedirectStandardOutput $apiOut -RedirectStandardError $apiErr `
        -WindowStyle Hidden -PassThru
    Set-Content -Path (Join-Path $root "data\api.pid") -Value $api.Id

    if (-not (Wait-ForHttp "http://localhost:8000/health" 40)) {
        throw "API did not become healthy. Check data\api.log and data\api.err.log."
    }
}
Write-Host "API is healthy."

# ---------- demo data ----------
Write-Host "Seeding demo data (safe to run again) ..."
& python worker-py/seed.py
if ($LASTEXITCODE -ne 0) { throw "seed failed" }

# ---------- console ----------
$webReady = $false
try {
    $r = Invoke-WebRequest "http://127.0.0.1:5173" -TimeoutSec 2 -UseBasicParsing
    $webReady = $r.StatusCode -eq 200
} catch {}

if ($webReady) {
    Write-Host "Console is already running on http://127.0.0.1:5173"
} else {
    if (-not (Test-Path (Join-Path $root "frontend\node_modules"))) {
        Write-Host "Installing frontend dependencies ..."
        Push-Location (Join-Path $root "frontend")
        & npm install
        if ($LASTEXITCODE -ne 0) { Pop-Location; throw "npm install failed" }
        Pop-Location
    }

    Write-Host "Starting the console on http://127.0.0.1:5173 ..."
    $vite = Start-Process -FilePath "npm.cmd" `
        -ArgumentList @("run", "dev", "--", "--host", "127.0.0.1") `
        -WorkingDirectory (Join-Path $root "frontend") `
        -RedirectStandardOutput (Join-Path $root "data\vite.log") `
        -RedirectStandardError (Join-Path $root "data\vite.err.log") `
        -WindowStyle Hidden -PassThru
    Set-Content -Path (Join-Path $root "data\vite.pid") -Value $vite.Id

    if (-not (Wait-ForHttp "http://127.0.0.1:5173" 30)) {
        throw "Console did not start. Check data\vite.log and data\vite.err.log."
    }
}

Write-Host ""
Write-Host "EvalForge is running:"
Write-Host "  Console  http://127.0.0.1:5173"
Write-Host "  API      http://localhost:8000"
Write-Host "  Logs     data\api.log, data\vite.log"
Write-Host "  Stop     .\scripts\stop-local.ps1"
