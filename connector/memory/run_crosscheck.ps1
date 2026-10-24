# Runs the lisa-memory differential crosscheck: builds the fixture with the
# REAL Python sdk.memory.store, then replays the identical operations in Go
# against the same DB (connector/memory/crosscheck_test.go).
#
# Requires the lisa repo's virtualenv at C:/Code/lisa/.venv.
# Usage (PowerShell):  ./connector/memory/run_crosscheck.ps1
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..\..")

$python = "C:\Code\lisa\.venv\Scripts\python.exe"
if (-not (Test-Path $python)) {
    Write-Error "lisa venv python not found at $python"
    exit 1
}

$capture = Join-Path $env:TEMP "memory-crosscheck.json"
& $python (Join-Path $PSScriptRoot "py_probe.py") $capture
if ($LASTEXITCODE -ne 0) { exit 1 }

$env:LISA_MEMORY_CROSSCHECK_FILE = $capture
$env:PATH = "C:\Program Files\Go\bin;$env:PATH"
go test ./connector/memory/ -run TestGoMemoryCrossCheck -count=1
exit $LASTEXITCODE