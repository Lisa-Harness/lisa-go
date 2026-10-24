---
description: "Use when porting Lisa primitives from Python (lisa-harness) to Go in this repo, writing or modifying MCP servers under cmd/, or touching connector/ and the Lisa server contract (stdio spawn, flags, logging, safety invariants)."
applyTo: ["cmd/**/*.go", "connector/**/*.go", "PROTOCOL.md"]
---

# Lisa Connector Contract & Porting Rules (lisa-go)

## What This Repo Is
Go Connector SDK for the Lisa Harness: first-party MCP servers spawned as
`runtime_kind: binary` subprocesses over stdio. The neutral contract lives at
`Lisa-Harness/lisa` → `docs/GO_CONNECTOR_SDK.md`; `PROTOCOL.md` pins
contract **v1** (stdio-only). Kernel/SDK/runtime stay Python in `lisa`
— **never reimplement harness or domain logic here**.

## Non-Bypassable Safety Invariants
Weakening any of these to make a port easier is a defect, not a shortcut:
- **SSRF gate** (`connector/fetch`): scheme/host/private-IP checks, redirect-hop re-validation, `absoluteMaxBytes` ceiling.
- **Files sandbox** (`connector/storage`): reject absolute paths, `..` segments, and any resolution escaping `<data_dir>/files`; case-insensitive fnmatch deny-globs; `PathRejected` is distinct from malformed-path rejection.
- **Redaction** (`connector/thread`): `load_*` return RAW entries (lossless read contract); the **server** applies `RedactionPolicy` on projection. Never mutate the raw log; never redact inside the read path.
Every ported primitive's doc comment states its invariants — preserve that text when editing.

## Server Anatomy (`cmd/<server>/`)
Reference: `cmd/lisa-thread/main.go` (newest, has extra flags).
1. `connector.ParseCLI(serverName)` for standard flags only, or `connector.NewFlagSet(serverName)` + register extras on the returned `*flag.FlagSet` + `fs.Parse(os.Args[1:])` when the server adds flags (e.g. `--deny-glob`, `--thread-dir`).
2. `connector.SetupLogger(serverName, cli.LogLevel)` + `defer closeLog()`. JSONL logging goes to `LISA_MCP_LOG_PATH`, fallback `<cwd>/<server-id>.jsonl`; bare stderr only with `LISA_LOG_STDERR=1`.
3. `mcp.NewServer(connector.NewImplementation("..."), nil)`.
4. Tool args as small structs with snake_case JSON tags mirroring the Python tool signatures; pointer/optional fields as `*T` with `omitempty`.
5. Domain logic lives in the `connector/<server>` package — the server is a thin facade.

## Command & Config Rules
- Spawn shape: `<command_path> --data-dir <dir> [--log-level ...] [--readonly] [args…]`.
- `--data-dir` is required and the only path-configuration vector. Env vars carry **behavior** config only, never paths (see the redaction env contract in `connector/thread/ResolveRedactionPolicy`).
- Repeatable flags use the `multiFlag` (`fs.Var`) pattern, as for `--deny-glob`.

## Build/Version Discipline
- Bumping the contract v1 in a breaking way requires a coordinated change to `GO_CONNECTOR_SDK.md` and a `CHANGELOG.md` entry in `lisa` — flag it, don't do it silently.
- `connector/fetch`, `connector/storage`, and `connector/thread` are faithful ports of named Python modules; keep their package doc comments in sync with the ported file when behavior changes.