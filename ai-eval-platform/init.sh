#!/bin/bash
set -e

echo "=== EvalForge Harness Initialization ==="

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
echo "=== Running smoke tests (48 checks) ==="
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
