---
description: "Use when building, testing, or running any code in this repo (go build, go vet, go test, gofmt, or e2e/integration tests): exact commands, version, and verification workflow."
applyTo: "**/*.go"
---

# Build & Test Workflow (lisa-go)

## Version & Tooling
Go 1.25 (toolchain `go 1.25.0` in `go.mod`). Locate the binary with
`Get-Command go` first — PATH may not be set in the shell.

## Verification Loop
After any change to Go code, run all three and require green:

```powershell
go build ./...
go vet ./...
go test ./...
```

- Run the smallest targeted `go test ./connector/...</pkg>` first; escalate to `./...` when targeted results look wrong or touched packages share invariants.
- Never skip `go vet` — it has caught real defects in this repo (lock copies, shadowing).

## Tests
- Table-driven with `[]struct{ name, input, want }` and `t.Run(tc.name, ...)`.
- Call `t.Parallel()` in tests and sub-tests that do not mutate shared state; use `t.TempDir()`, never hand-rolled temp dirs.
- Filesystem/JSON edge cases: `testing/iotest` and `testing/fstest`. No mocking framework.
- Tests must not depend on execution order or real clocks/network timing. Never silence a flaky test with `t.Skip`, longer timeouts, or weaker assertions — fix the cause.
- Hot-path benchmarks: `testing.B` with `b.ReportAllocs()`.

## E2E Pattern (cmd/* tests)
`cmd/lisa-fetch/main_test.go` is the reference. It:
1. `go build -o` the binary into `t.TempDir()` (`.exe` suffix on Windows).
2. Spawns it with the contract CLI (`--data-dir <temp>`, etc.).
3. Drives it over stdio with `mcp.CommandTransport{Command: cmd}` and the SDK client.
Copy this pattern for any new `cmd/<server>` server; do not invent a thinner fake.

## Known Debt
- Do not `go test ./cmd/... && go test ./connector/...`-style splits — invariants span both.
- `rawSchema` helper is duplicated per `cmd/*` package: local redeclare is fine; merging or de-duplicating is optional cleanup, not required by this instruction.