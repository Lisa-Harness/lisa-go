# go-handoff: Lisa → lisa-go

This document transfers the Go Connector SDK work into this repo. It was
written by an agent working across both repos (`Lisa-Framework/lisa` Python,
this repo). It assumes you, the receiving agent, work **primarily in this
repo** and use `C:\Code\lisa` as the Python source of truth to port against.

## 1. The vision (why this repo exists)

Lisa is an AI agent harness: kernel (contracts + turn loop), SDK (domain
logic), runtime (composition) — all Python, deliberately. The decision,
made and confirmed with the project owner:

- **Kernel, SDK, runtime stay Python.** Python is the extensibility
  mechanism (packs are trusted in-process Python discovered via entry
  points); a port would fork the source of truth.
- **MCP servers go to Go.** They are wire-boundary services (stdio
  JSON-RPC), I/O-bound, and spawning them as subprocesses per turn/
  session is where Python's interpreter/venv cost actually bites. Go
  gives small static binaries + instant cold start. Steady-state turn
  speed is model-API-bound and was never the point.
- **Naming:** `lisa-harness-sdk` (Python) = the **Pack SDK** (author
  Lisa's extensibility). This repo = the **Go Connector SDK**
  (`github.com/Lisa-Harness/lisa-go`) — connect to / embed Lisa. One
  language-neutral contract, two audiences; see
  `../lisa/docs/GO_CONNECTOR_SDK.md` (in the Python repo) which is the
  **contract source of truth**. This repo's `PROTOCOL.md` pins the
  contract version (v1, stdio-only) implemented here.

The end state: Python extensibility inside, Go-served edge outside.

## 2. The contract (what makes a server "first-party")

Read `../lisa/docs/GO_CONNECTOR_SDK.md` in full. Non-negotiables:

- Spawn shape: `<command_path> --data-dir <dir> [--log-level ...] [--readonly] [args…]`,
  declared in the host's `mcp_servers.yaml` as `runtime_kind: binary`.
  **No kernel change is needed** — `RuntimeKind.BINARY` already exists.
- `--data-dir` is required and is the only path-configuration vector
  (env vars are for behavior only, never paths).
- Logging: JSONL to `LISA_MCP_LOG_PATH`; fallback `<cwd>/<server-id>.jsonl`;
  never bare stderr unless `LISA_LOG_STDERR=1`.
- Safety invariants of every ported primitive are **non-bypassable**:
  weakening an SSRF gate, a sandbox, or the redaction policy to make a
  port easier is a defect, not a shortcut.

## 3. What is DONE and verified (as of this handoff)

Three first-party servers + the Connector SDK core exist here, and
`go build ./...`, `go vet ./...`, `go test ./...` are **all green**:

| Path | What it is | Verified |
| --- | --- | --- |
| `connector/contract.go` | CLI/LaunchEnv/Server types for the contract | build+vet |
| `connector/cli.go` | `ParseCLI` / `NewFlagSet` (extensible: servers add their own flags) | build+vet |
| `connector/logging.go` | Contract JSON logging incl. stderr-mirror opt-in | build+vet |
| `connector/fetch/fetcher.go` | Port of `lisa/sdk/content/_fetcher.py`: SSRF gate (scheme/host/private-IP), redirect-hop re-validation via `CheckRedirect`, L1 map cache, L2 JSON-per-entry disk cache, conditional GET, stale-fallback on transport error (never on HTTP-status error), honest UA + navigation headers, max_bytes clamp | e2e vs example.com |
| `cmd/lisa-fetch/{main.go,main_test.go}` | `fetch_url` + `invalidate_fetch_cache` over stdio; e2e tests spawn the real binary via `mcp.CommandTransport` | **all tests PASS** |
| `connector/storage/storage.go` (+ `storage_test.go`) | Port of `lisa/sdk/storage/_backend.py`: files sandbox under `data_dir/data/files`, `is_safe_relative_path` rejection, case-insensitive fnmatch deny-globs, `PathRejected`, byte-faithful binary-suffix writes, text trailing-newline normalization, manifest pair (`config/models.yaml`, `state/models.proposed.yaml`) | unit tests PASS |
| `cmd/lisa-storage/{main.go,main_test.go}` | 10 tools matching the Python server incl. model-manifest pair; repeatable `--deny-glob` flag; `--readonly` | **all tests PASS** (path escape, deny-glob, readonly, round-trip) |
| `connector/thread/{backend,readout,redaction,util}.go` | Read path of `lisa/sdk/thread/_backend.py`: segment-path derivation, sparse index, zstd sealed segments (+ `.dict.zstd` dictionary), `load_recent`/`load_range`/excerpt/manifest, `RedactionPolicy` + `ResolveRedactionPolicy` env contract (RAW-vs-projected split preserved — load_* return raw; the SERVER projects/redacts) | compiles; used by cmd tests |
| `cmd/lisa-thread/{main.go,main_test.go}` | 6 read-only tools (`thread_read_range`, `thread_read_recent`, `thread_search`, `thread_read_content`, `thread_find_content`, `thread_task_view`); `--thread-dir` + `--user-slug`; redaction-env handling | **all tests PASS** (incl. redaction-on-projection) |

Measurements from the fetch pilot: static binary ~12.6 MB, subprocess
initialize round-trip ~855 ms (cold-ish). No like-for-like Python
comparison was run (owner said skip it — the direction is already
decided); do not re-litigate, but re-measuring on this machine is fine.

Dependency: `github.com/modelcontextprotocol/go-sdk v1.8.0`,
`github.com/klauspost/compress` (zstd). Go 1.27 (toolchain at
`C:\Program Files\Go\bin\go.exe`; PATH doesn't persist between calls).

## 4. Known debt / sharp edges (read before touching)

1. **`rawSchema` duplication risk**: each `cmd/*` package declares its own
   tiny `rawSchema` helper. If you add a server, either redeclare locally
   (current pattern, fine) or lift into `connector`. Watch for duplicate
   top-level declarations when merging agent-written files (this bit us).
2. `cmd/lisa-fetch` still uses the older one-shot `connector.ParseCLI` —
   storage/thread use `NewFlagSet` + extra flags. Consistency pass is fine
   but not urgent.
3. **`connector/thread` was assembled across interrupted sessions.**
   `readout.go` in particular was mid-write and later repaired to make the
   module build. It compiles, vets clean, and passes the cmd-level redaction
   tests, but it hasn't been line-reviewed against `_readout.py`/`_search.py`
   /`_taskview.py`/`_results.py` the way backend.go was reviewed against
   `_backend.py`. Before calling thread "done-done", diff those two files'
   semantics (paged content, lexical search scoring, task-view shapes).
4. Test artifacts (`*.jsonl` logs, `tmp-data/`, `bin/`) are untracked
   clutter — add a `.gitignore` (`*.jsonl` at repo root may be too broad;
   prefer `tmp-data/`, `bin/`, and the per-server `*.jsonl` fallthrough) and
   make the first commit.
5. **No git commits and no remote yet.** `git init` exists, master has zero
   commits. First commit should be the verified green tree (after
   gitignore), using this message shape:

   ```
   Initial Connector SDK + lisa-fetch/lisa-storage/lisa-thread ports

   Co-authored-by: Copilot <223556219+Copilot@users.noreply.github.com>
   ```

## 5. Next phase (what to do next)

In order, per the confirmed plan:

1. **Make it commit-able**: `.gitignore`, remove test clutter, first
   commit, push when a remote is decided (`Lisa-Harness/lisa-go`).
2. **Register the binaries with a real Python runtime run** in `C:\Code\lisa`:
   add `lisa-fetch` (then storage/thread) as `runtime_kind: binary` specs in
   `mcp_servers.yaml` pointed at the built exes, and run the runtime test
   that exercises them through `mcp_pool`. This is the last proof the
   contract holds end-to-end.
3. **Line-review `connector/thread/readout.go`** against its Python
   counterparts (see debt #3). Fix semantic drift; redaction must stay exact.
4. **CI**: a small workflow (build + vet + test) mirroring the Python
   repo's CI.
5. **Publish the Connector SDK** (the `publish-connector-sdk` task): the
   server-builder side is in `connector/`; the remaining piece is the
   **host/embed side** — spawn the pool, drive turns — plus docs and an
   example. Keep it contract-bound; never bind to Python internals
   (pydantic shapes, entry-point discovery).

## 6. Source-of-truth map (Python → here)

| Python (C:\Code\lisa) | Here |
| --- | --- |
| `docs/GO_CONNECTOR_SDK.md` | (contract) — do not fork; bind to it |
| `sdk/src/sdk/content/_fetcher.py` | `connector/fetch/fetcher.go` |
| `sdk/src/sdk/storage/_backend.py` | `connector/storage/storage.go` |
| `sdk/src/sdk/thread/_backend.py` + `_readout.py` + `_search.py` + `_taskview.py` + `_results.py` + `redaction.py` | `connector/thread/*` |
| `mcp_server/src/mcp_server/fetch/server.py` | `cmd/lisa-fetch/main.go` |
| `mcp_server/src/mcp_server/storage/server.py` | `cmd/lisa-storage/main.go` |
| `mcp_server/src/mcp_server/thread/server.py` | `cmd/lisa-thread/main.go` |
| `mcp_server/src/mcp_server/config/cli.py` | `connector/cli.go` |
| `sdk/src/sdk/logging_setup.py` | `connector/logging.go` |
| `kernel/src/kernel/mcp_launch/policy.py` (RuntimeKind.BINARY) | (no Go equivalent needed) |

When porting or changing a tool, diff-test against the Python server's
behavior — the Python repo is the source of truth for semantics; this repo
owns the Go implementation and its tests.