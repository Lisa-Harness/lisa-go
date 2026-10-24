# Phase 5 SPEC — Connector SDK host/embed side

Status: **DEFERRED** (written 2026-10-24, after the port + line-review phase
closed). This document is the execution spec for the remaining Connector SDK
phase. It is written so an agent with no prior context can pick it up cold:
every section carries the file/line references into `C:\Code\lisa` (the
Python source of truth) and into this repo.

Contract basis: `Lisa-Harness/lisa` → `docs/GO_CONNECTOR_SDK.md` (v1, pinned
by `PROTOCOL.md` here). The host side **consumes the same v1 contract** — it
does not add or bump any contract surface.

---

## 1. Purpose and scope

Phase 5 completes the Connector SDK's second audience. Phase 1–4 built the
**server-builder side**: first-party MCP servers (fetch/storage/thread) plus
the contract plumbing (`connector.CLI`, `SetupLogger`, `NewImplementation`).
Phase 5 builds the **host/embed side**: Go code that loads the neutral
`mcp_servers.yaml` manifest, compiles specs into spawnable subprocess
configs, manages the pool lifecycle (warm/cold), and exposes a unified tool
catalog to a Go embedder.

In the Python runtime this role is played by (conceptually, port — never
import):

| Concern | Python reference | Go deliverable |
| --- | --- | --- |
| Manifest load/validate | `kernel/src/kernel/mcp_launch/server_manifest.py` (`load_mcp_server_manifest` :147, `_parse_server_entry` :392) | `connector/manifest` |
| Spec → spawn shape | `kernel/src/kernel/mcp_launch/compiler.py` (`compile_command` :135, `compile_mcp_config` :171, `compile_registry` :281) | `connector/launch` |
| Pool lifecycle | `kernel/src/kernel/tools/mcp_client.py` (`MCPToolPool` :366, warm constants :377) | `connector/pool` |
| Catalog assembly + env + log paths | `runtime/src/runtime/mcp_pool.py` (`compile_mcp_configs` :936, `_mcp_environment` :585, `_with_default_mcp_log_paths` :882) | `connector/pool` + `connector/launch` |
| Neutral data-dir layout | `sdk/src/sdk/layout.py` (`LOGS_DIR` :11, `MCP_LOGS_SUBDIR` :13) | constants in `connector/launch` |

### What this phase is NOT (non-goals, hard boundaries)

- **No kernel turn loop in Go.** "Drive turns" means: the Go embedder
  composes its own conversation loop and calls pool tools — the kernel,
  approvals, packs, receipt contract, and context assembly stay Python
  (`lisa/docs/HOST_INTEGRATION.md` is the boundary reference).
- **No pack system.** Pack manifests, entry-point discovery, pack venvs are
  Pack-SDK/Pack-runtime concerns. A Go host consumes one manifest path it is
  given; it never discovers packs.
- **No pydantic/entry-point binding.** Per the contract's "not part of the
  contract" section: bind to the YAML schema + compiled spawn shape only.
- **No in_process transport.** anyio memory streams are Python-internal; a
  Go pool cannot connect to one. Reject with a clear error.
- **Never weaken server-side safety invariants** — the host side does not
  touch SSRF/sandbox/redaction logic at all.

## 2. Package layout

```
connector/manifest/   YAML manifest load + validation + ${VAR} expansion
connector/launch/     spec → (command, args, env, cwd, log path) compilation
connector/pool/       warm/cold subprocess pool, tool catalog, dispatch
examples/embed/       runnable example (main.go) over the pool
```

Package names follow the repo's naming rules (short, lowercase, domain
words). The existing `connector.Server` / `connector.CLI` /
`connector.SetupLogger` types are reused, not duplicated.

## 3. Manifest layer (`connector/manifest`)

### 3.1 Schema the Go loader must accept

Per `server_manifest.py::_parse_server_entry` (:392). One entry:

| Field | Type / default | Notes |
| --- | --- | --- |
| `server_id` | string, **required** | reject empty/missing |
| `runtime_kind` | string, **required** | one of: `python, uv, venv_bin, node, npx, binary, http, in_process` (from `policy.py` `RuntimeKind` :33) |
| `command_path` | string | BINARY/NODE; `${VAR}`-expanded |
| `venv_dir` | string | VENV_BIN; expanded; entry point = `entry_point` or `server_id`; resolved as `<venv_dir>/Scripts/<ep>` on Windows, `<venv_dir>/bin/<ep>` on POSIX |
| `entry_point` | string | defaults to `server_id` |
| `script_path` | string | NODE |
| `npx_package` | string | NPX |
| `module`, `package`, `uv_directory` | string | PYTHON/UV (see support matrix) |
| `url`, `headers` | string/map | HTTP; expanded |
| `cli_args` | string list | `${VAR}`-expanded **per item** (see incident note :462-471) |
| `env` | map | values expanded; forced overrides for the child env |
| `data_dir_arg` | string | default `"--data-dir"`; `null` → inject nothing |
| `workspace_root_arg` | string | default `"--workspace-root"`; `null` → inject nothing |
| `data_dir_subdir` | string | optional namespace under the host data dir |
| `ensure_data_dir` | bool, default false | pre-create before spawn |
| `warm` / `required` / `disabled` | bool, default false | launch policy |
| `tool_policy` | `{allowed_tools?, denied_tools?}` | bare tool names |
| `capability` | string | capability ownership |
| `tool_map` | map | **requires `capability`** (loader rejects otherwise, :493) |
| `aliases` | list | `{alias → tool}` specs for synthetic publication |
| `default_available_tools` | string list | ambient merge (host/SDK catalog concern — carry, don't act) |
| `cwd` | map | CwdPolicy |
| `timeouts` | map | LaunchTimeouts |
| `tool_filter`, `proxy_policy` | — | see §7 open decisions |

`disabled: true` entries are skipped at parse (:385-388). Top-level
`version: 1` and any `memory:`/`pack_policy:` sections are **accepted and
ignored** by the v1 Go loader (memory packs are Python-runtime-owned); reject
not, warn yes.

### 3.2 `${VAR}` expansion

`_expand_env_value` (:130-145): substitute `${NAME}` from a caller-provided
environment resolver. Missing/empty source behaves per call site — the Go
API takes `func(name string) (string, bool)` and treats `false` as absent.
Absent expansion in a required path field surfaces as a per-entry validation
error **with the entry index and server_id**, not a global abort.

### 3.3 Runtime-kind support matrix (v1 owner decision D2)

| kind | v1 behavior |
| --- | --- |
| `binary` | **full** — the contract's Go kind |
| `venv_bin` | **full** (mechanical path join; lets a Go host spawn the Python first-party fleet from the same manifest — this is the whole point of "embed") |
| `http` | **optional, deferred** unless decision D2 says otherwise |
| `python`, `uv`, `node`, `npx` | **reject at load** with "runtime kind not supported by the Go host" (prevents accidental half-support) |
| `in_process` | **reject** (Python-internal transport) |

## 4. Launch layer (`connector/launch`)

Port `compiler.py::compile_mcp_config` (:171). Inputs: a manifest `ServerSpec`
plus host-supplied data dir / workspace root. Outputs a `launch.Compiled`:

```go
type Compiled struct {
    ServerID      string
    Command       string            // "" means "optional binary absent → skip"
    Args          []string          // base args, then contract args, then cli_args
    Env           launch.ChildEnv   // inherit-parent + forced overrides + absent-removals
    Cwd           string
    DataDirPath   string            // resolved (post-subdir) dir
    EnsureDataDir bool
    Warm, Required bool
    ToolPolicy    launch.ToolPolicy // bare allow/deny lists
    RuntimeKind   string
}
```

Semantics pinned from `compiler.py` + host assembly:

1. **Base args then contract args, in order** (:220-232):
   `[--data-dir <resolved_data_dir>]` → `[--workspace-root <w>]` →
   `[--readonly]` (only when spec declares support + it is set) →
   `spec.cli_args` **last, verbatim**.
2. **Data-dir resolution** (:190-200): `data_dir + data_dir_subdir` when the
   spec has both; no injection when the spec sets `data_dir_arg: null` (or
   the host data dir is absent).
3. **Optional-binary skip** (:120-133): BINARY with empty `command_path`
   after expansion → `Command == ""`; the pool logs a warning and drops the
   server ("degrades to absent") instead of failing startup.
4. **Log-path injection** (`mcp_pool.py::_with_default_mcp_log_paths` :882):
   when the child env has no `LISA_MCP_LOG_PATH`, set
   `<server_data_dir>/logs/mcp/<server_id>.jsonl` (`logs` + `mcp` from
   `sdk/layout.py`; this matches the Python pool so an operator's log
   consolidation works across both). Explicitly-set values win. Not applied
   to `in_process` (n/a for Go).
5. **Env compilation**: inherit parent + forced `env:` overrides. Go's
   `exec.Cmd` inherits by default; encode overrides as explicit
   key→(value | absent) pairs. The "absent" case exists because a stale
   parent-shell value must be removable (the redaction-patterns case,
   `mcp_pool.py::_mcp_environment` :585-641 docstring).
6. **Host environment contract** (`_mcp_environment` :585): when the Go host
   assembles env for manifest expansion it reproduces the same unconditional
   assignment (never `setdefault` — the 2026-08-25 empty-`LISA_USER_SLUG`
   incident) and the absent-when-unresolved semantics:
   `FRAMEWORK_ROOT`, `LISA_TIMEZONE`, `LISA_THREAD_DIR` (unless no-thread),
   `LISA_USER_SLUG` (only when non-empty),
   `LISA_THREAD_REDACTION_ENABLED` (`"1"`/`"0"`),
   `LISA_THREAD_REDACTION_PATTERNS` (line-joined, or **absent**).
7. **`ensure_data_dir`**: create `DataDirPath` (MkdirAll) before spawn.

## 5. Pool layer (`connector/pool`)

Port the warm/cold `MCPToolPool` semantics from `mcp_client.py::MCPToolPool`
(:366). Client-side transport is `mcp.CommandTransport` (the pattern
`cmd/lisa-fetch/main_test.go::connect` already proves).

### 5.1 Lifecycle

- `Start`: spawn every **warm** config once; keep alive across calls.
  Retry-with-backoff `_WARM_STARTUP_ATTEMPTS=3`,
  `_WARM_BACKOFF_SECONDS=(0.2, 0.4, 0.8)` (:379-380). A warm+required
  failure after all attempts is fatal (returns error to the caller). A
  warm non-required failure is recorded and the pool proceeds.
- **Cold configs**: not connected at Start; lazily connected on first
  `ListTools`/`CallTool` that needs them. A cold server that fails to
  connect enters `_cold_failed` and is skipped thereafter ((:464-467 in
  python; mirror the no-respawn-loop rule).
- **Restart budget** (OTP MaxRestarts/MaxSeconds pattern, :385-395): at most
  `_MAX_RESTARTS=3` restarts within `_RESTART_WINDOW_S=60.0` per server;
  beyond that the server is permanently failed and `Restart` returns
  immediately without spawning. Mid-session crashes mark the session dead
  and surface via call errors + `LastErrors`; **automatic restart is not a
  data-path behavior** — recovery is the embedder's explicit `Restart`.
- `Close`: close every session + terminate every subprocess; idempotent.

### 5.2 Catalog and dispatch

- Tools are published namespaced: `<server_id>.<tool>`, or for a
  capability-selected server `<capability>.<tool_map[tool] or tool>`
  (`policy.py` :418-427).
- `tool_policy.allowed_tools`/`denied_tools` filter **bare names before
  namespacing** (`mcp_launch/tool_filter.py::filter_tools`).
- First-registered-wins on bare-name collisions across servers with a
  warning (pool docstring :366-372); alias can pin a call to a specific
  server regardless of collisions (`_tool_index_by_server`).
- `CallTool(name, args)`: route to the owning server; unknown name →
  trigger cold lazy-connect + index refresh, then re-resolve; still unknown
  → a clear not-found error carrying the catalog state.
- `LastErrors()`: per-server last connect/restart failure description,
  URL-stripped, bounded (the introspection-parity surface).

Concurrency: single mutex over the indexes; per-server sessions are owned
exclusively (no concurrent in-flight calls per server session in v1 —
serialize with a per-server semaphore; the Python pool relies on anyio
task semantics that Go must make explicit). Every ctx gets propagated;
every goroutine stops via context or Close.

## 6. Verification plan

Follow the repo's build/test instructions (`.github/instructions/`) and the
session's differential-testing pattern (see `files/crosscheck_driver.py` +
`connector/thread/crosscheck_test.go` — the readout parity harness — for the
approach).

1. **Manifest table tests**: valid manifests, disabled skipping,
   `tool_map` without `capability` rejection, empty `server_id` rejection,
   expansion cases (present/absent var, empty-string parent pollution),
   cli_args item expansion.
2. **Compiler golden tests against the real Python compiler** (the
   differential harness): for a shared fixture manifest, capture
   `command`, `args` (order-sensitive), env keys, log paths, and
   ensure-flag from Python's
   `load_mcp_server_manifest → compile_mcp_config → _with_default_mcp_log_paths`
   on one fixture data dir; assert the Go `launch.Compile` output matches
   structurally. This is the strongest check and mirrors what the readout
   crosscheck did for the server side.
3. **Pool unit tests** (fake server binary): warm retry counts, restart
   budget exhaustion, cold lazy connect + `_cold_failed`, collision
   first-wins, tool-policy filtering (bare-name, pre-namespacing),
   capability/tool_map publication, alias routing.
4. **Pool e2e against a real first-party server**: build `lisa-fetch` into a
   temp dir (the `cmd/lisa-fetch/main_test.go` pattern), point a fixture
   manifest at it, verify: ensure_data_dir created; log JSONL lands at
   `<data_dir>[/subdir]/logs/mcp/lisa-fetch.jsonl`; `fetch_url` round-trip;
   warm restart mid-session; Close terminates the process tree.
5. **CI**: the existing workflow covers it; no network-dependent tests in
   the pool layer.

## 7. Open owner decisions (blockers marked)

1. **(blocking) YAML dependency.** Manifest parsing needs a YAML parser.
   Per the repo's dependency rule (`go-core.instructions.md`), a new module
   beyond MCP/zstd/x-slash needs explicit approval: recommend
   `gopkg.in/yaml.v3` (stdlib-adjacent de-facto standard, maintained).
2. **(blocking) Runtime-kind support matrix** — confirm §3.3 (recommend:
   binary + venv_bin full, http deferred, others reject).
3. **(non-blocking) Example depth** — catalog demo (list + call one tool)
   vs. a small conversation loop exercising a real model API. Recommend the
   demo; a chat loop drags a model client dep into the example.
4. **(non-blocking) Capability competition in the host.** The Python runtime
   *selects* one implementation per capability before the pool sees it. A Go
   host should honor the selected winner's fields (publish under the
   capability prefix) but not run competitions. Confirm.
5. **(non-blocking) Where this spec lives when executed** — recommend
   `docs/HOSTSIDE.md` in this repo at implementation time; `PROTOCOL.md`
   stays untouched (no v1 surface change).

## 8. Definition of done

- [ ] `connector/manifest`, `connector/launch`, `connector/pool` +
      `examples/embed` exist with doc comments stating their invariants.
- [ ] `go build ./...`, `go vet ./...`, `gofmt -l` clean; `go test ./...`
      green including the Python-compiler golden tests (skipped cleanly with
      a loud message when `C:\Code\lisa` or Python is unavailable — skip
      must be a build-time harness check, never a silently weakened
      assertion).
- [ ] e2e: fixture manifest → pool → lisa-fetch binary round-trip with
      log-path + ensure_data_dir verification.
- [ ] README (layout + status) and this doc updated to "done" with the same
      verification rigor as the line-review handoff update.
- [ ] No import of, or format binding to, any Python-internal type (check
      with `git grep -nE 'pydantic|MCPServerSpec|MCPClientConfig|entry_point.*lisa\.extensions'`).

## 9. Known-incident quick reference (why the pedantry)

| Incident (date) | Rule it produced |
| --- | --- |
| 2026-08-25 thread empty-read | `${VAR}` in `cli_args` expands per item; empty parent `LISA_USER_SLUG` must not leak (unconditional assignment) |
| 2026-09-04 storage sandbox-escape | in-process rewrite must preserve `args` (Go host: keep contract args when post-processing configs) |
| 2026-08-30 budget-claim loop | redaction env contract must reach read-only servers intact; patterns var ABSENT when unset |
| headroom pack contract | BINARY + empty `command_path` degrades to absent, never aborts startup |