#!/bin/bash
set -e

cd "$(dirname "$0")"

echo "=== EvalForge Harness Initialization ==="

# ---------- cgo toolchain ----------
# Telemetry is DuckDB, and go-duckdb links a C++ library, so the Go build and
# tests must run with cgo enabled and a C compiler on PATH.
export CGO_ENABLED=1

if ! command -v gcc >/dev/null 2>&1; then
  # WinLibs installed with winget is not added to PATH automatically.
  winlibs="$LOCALAPPDATA/Microsoft/WinGet/Packages/BrechtSanders.WinLibs.POSIX.UCRT_Microsoft.Winget.Source_8wekyb3d8bbwe/mingw64/bin"
  for dir in "$winlibs" "/c/msys64/mingw64/bin" "/c/mingw64/bin"; do
    if [ -x "$dir/gcc.exe" ] || [ -x "$dir/gcc" ]; then
      export PATH="$dir:$PATH"
      break
    fi
  done
fi

if ! command -v gcc >/dev/null 2>&1; then
  echo "ERROR: no C compiler found. The DuckDB driver needs cgo."
  echo "  Windows:        winget install --id BrechtSanders.WinLibs.POSIX.UCRT --exact"
  echo "  Debian/Ubuntu:  sudo apt-get install build-essential"
  echo "  macOS:          xcode-select --install"
  exit 1
fi
export CC="${CC:-$(command -v gcc)}"
echo "cgo: CGO_ENABLED=$CGO_ENABLED CC=$CC"

# The Go tests use the local PostgreSQL on :55432 unless TEST_CONTROL_DB_URL is
# set. Warn early instead of failing with a wall of connection errors.
pg_bin=""
for dir in "/c/Program Files/PostgreSQL/16/bin" "/c/Program Files/PostgreSQL/15/bin"; do
  if [ -x "$dir/pg_isready.exe" ]; then
    pg_bin="$dir"
    break
  fi
done
if [ -n "$pg_bin" ] && ! "$pg_bin/pg_isready.exe" -h 127.0.0.1 -p 55432 -q; then
  echo "WARNING: PostgreSQL is not answering on 127.0.0.1:55432."
  echo "         Start the local cluster first (see README: From source, without Docker)."
fi

# ---------- Go API Server ----------
echo ""
echo "=== Building Go API server ==="
cd server-go
go build -o evalforge.exe ./cmd/server
echo "Build OK"

echo ""
echo "=== Running Go tests ==="
go test ./... -count=1 -timeout 120s
echo "Go tests OK"
cd ..

# ---------- Smoke Tests ----------
echo ""
echo "=== Running smoke tests (69 checks) ==="
python scripts/smoke_api.py
echo "Smoke tests OK"

echo ""
echo "=== Running auth tests (44 checks) ==="
python scripts/auth_test.py
echo "Auth tests OK"

echo ""
echo "=== Running MCP tests (23 checks) ==="
python scripts/mcp_test.py
echo "MCP tests OK"

echo ""
echo "=== Verification Complete ==="
echo ""
echo "Next steps:"
echo "1. Read feature_list.json to see current feature state"
echo "2. Pick ONE unfinished feature to work on"
echo "3. Implement only that feature"
echo "4. Re-run verification before claiming done"
