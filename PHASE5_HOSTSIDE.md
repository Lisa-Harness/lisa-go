# Phase 5 SPEC - Optional Go MCP broker

Status: **SPEC ONLY; IMPLEMENTATION DEFERRED**. Revised 2026-10-24.

Owner-approved direction: Python resolves server configuration; Go owns
subprocess launch and MCP connection/pool mechanics. Ship a broker binary
alongside the MCP server binaries. The Python runtime selects its existing
Python backend or the Go backend through an explicit switch.

This supersedes the earlier Go-embedder-only proposal. It does not authorize
implementing the broker, changing the Python runtime, or porting servers as
part of this documentation task.

### Prompt startup is a separate workstream

The Python framework's catalog/content adapters now reuse long-lived stdio
sessions rather than spawning a Python server per call. These synchronous
framework sessions remain outside this broker's coverage.

Further work is specified in the Python framework's
[prompt startup proposal](https://github.com/Lisa-Harness/lisa/blob/main/docs/SPEC/PROMPT_STARTUP.md):
cache-refresh and runtime-lifecycle ownership first, then in-process
catalog/content access, measured import/bootstrap improvements, and separate
`--no-thread` correctness work. That proposal is **SPEC ONLY**, not broker
implementation authorization. Catalog/content Go wrappers around Python are
not a startup optimization; a broker alone does not remove their import cost.

## 1. Architecture and ownership

```text
Python CLI host (unchanged)
    |
Python runtime + kernel
    | turns, models, packs, context, policy, approvals, receipts
    | resolves manifest + pack contributions + capability owners
    | compiles concrete server configurations
    |
    +-- mcp_backend=python --> existing Python pool
    |
    +-- mcp_backend=go -----> thin Python pool adapter
                                  | versioned private stdio JSON-RPC
                             lisa-mcp-broker
                                  | launch, discovery, routing,
                                  | connections, restart, shutdown
                                  +-- Go MCP server binaries
                                  +-- Python / other stdio servers
```

The Go backend replaces execution mechanics for the selected runtime session;
it is not a second pool running beside the Python pool over the same servers.
The Python CLI host and abstract runtime remain necessary. Switching pool
backends and selecting a Go implementation of a server are independent:
either backend can launch Go or Python server executables.

| Python retains | Go broker owns |
| --- | --- |
| Manifest loading, expansion, pack discovery and activation | Launching already-resolved commands without a shell |
| Capability winner selection, memory alias definitions, policy decisions | Mechanical tool filtering, publication and routing from supplied rules |
| Argument compilation, environment resolution, cwd and log-path selection | Applying arguments, environment, cwd, directory creation and timeouts |
| Turn loop, models, context assembly, approvals, effect receipts | MCP initialization, discovery, calls, connections and bounded recovery |
| Python synthetic tool handlers and their dispatch | Per-server operational state and catalog snapshots |

No YAML loader or manifest compiler is required in Go for this phase.
`connector/manifest`, a standalone Go host, and `examples/embed` are deferred
independent extensions, not prerequisites. The pool library can later serve
a Go embedder without moving Python's policy rules into this repository.

### Contract distinction

The MCP server-facing Connector Contract remains v1, pinned by
[PROTOCOL.md](PROTOCOL.md). The runtime-to-broker protocol is a **new,
separately versioned interface**, not an existing v1 contract surface.
Document and coordinate that interface in `Lisa-Harness/lisa` before enabling
the Go backend there. Neither side serializes Python classes or imports
Python-internal types as a wire format.

## 2. Deliverables and repository boundaries

In this repository:

```text
cmd/lisa-mcp-broker/  executable; framing and lifecycle entrypoint
connector/broker/    versioned wire DTOs, protocol handling
connector/launch/    execution of resolved configurations, not compilation
connector/pool/      reusable MCP pool and catalog mechanics
```

Reuse existing Connector SDK logging, implementation metadata and MCP
transport helpers where applicable. The broker has its own CLI: it is not
an MCP tool server and must not blindly adopt server-only required flags.

In `Lisa-Harness/lisa`, as coordinated follow-up work:

- Add a Python adapter implementing the pool behavior actually consumed by
  the kernel/runtime. Introduce a shared pool protocol where annotations
  currently require concrete `MCPToolPool`.
- Add runtime selection and broker executable configuration.
- Continue using the existing Python configuration/compilation path.
- Preserve Python synthetic-tool registration, catalog merging and dispatch.
- Map broker results/errors to existing kernel types and notifications.
- Update runtime documentation and run the same behavioral suite against
  both backends.

This work is separate from the parallel agent's registration of individual
Go server binaries. Do not overwrite or duplicate that registration work.

## 3. Runtime switch and configuration handoff

Proposed configuration names (final spelling coordinated in the Python repo):

- `mcp_backend: python | go`, default `python`.
- `mcp_broker_path`: explicitly resolved executable path for the Go backend.

Selection is fixed for a session. A missing executable, incompatible protocol
or unsupported configuration fails Go-backend initialization explicitly;
there is **no silent fallback** and no mid-call backend switching. Rollback
means starting a new session with the Python backend.

### Server mix is independent of the pool backend

Two orthogonal selections compose any Go/Python server combination:

1. **Which implementation runs each server.** A Go server is a
   `runtime_kind: binary` manifest entry; the Python one stays `venv_bin`.
   For capability servers (storage, thread, memory) one owner is chosen at
   compile time — host-manifest default, or session
   `providers.capabilities.<name>` (see `runtime/capability_servers.py`;
   losing candidates are not registered at all). Non-capability servers are
   selected by editing the owning manifest. Every mix is a YAML/session-plan
   edit, never a code change, and applies per session.
   Note: `fetch_page` today is an in-process pack tool, not a manifest
   server; there is no server-level Go/Python selection for it until it is
   registered as one (a manifest-entry decision for the runtime repo,
   not a broker concern).
2. **Which pool launches the resolved set**: `mcp_backend`.

| Goal | Manifest / session plan | mcp_backend |
| --- | --- | --- |
| All Go servers, existing pool | Go entry selected for every server/capability | `python` |
| All Python servers, Go pool | unchanged | `go` |
| Mixed (Go thread + Python content + Go memory) | per-server/per-capability selection | either |

Granularity is the server/capability, not the tool: a capability publishes
one owner at a time, and within a server `tool_policy` filters tools but
cannot split a server across implementations. "Go by default" means the Go
entries are the default candidates in the owning manifests — a registration
decision (owned by the parallel registration agent), independent of the
broker. Both backends launch whichever Go and Python entries the runtime
resolved.

Python supplies an ordered list of resolved server records containing:

- Unique runtime server id; concrete executable and ordered argument array.
- Resolved cwd, data directory and `ensure_data_dir`.
- Explicit child environment with a documented inheritance/removal policy.
  Prefer a complete Python-resolved environment snapshot so ambient broker
  variables cannot reintroduce intentionally absent values.
- Warm/required flags and resolved launch/call timeouts.
- Bare-name allow/deny rules, capability publication mapping and resolved
  aliases. Python has already chosen capability winners.
- Any metadata needed to preserve current tool publication and introspection.

An empty allow-list is distinct from an absent allow-list. Preserve empty
strings, nulls and absent fields where their meanings differ.
Python performs optional-binary omission before handoff and reports the skip.
Go validates the complete request before starting children, rejects duplicate
ids, preserves list order, and never re-expands variables or appends contract
arguments. Launch exactly what was compiled.

### Transport support

Broker v1 targets **stdio subprocesses**, independent of their original
manifest runtime kind. A resolved Python, Node or Go command is just a
command; Go does not reconstruct a venv or apply manifest-specific rules.

HTTP and Python `in_process` transports are not supported initially.
Preflight the entire configuration and report unsupported transports before
spawning anything. Python must explicitly compile a valid stdio alternative
where supported, or the user must select the Python backend. Do not silently
drop unsupported servers or partition them between two hidden pools.

## 4. Broker protocol v1

Use UTF-8 newline-delimited JSON-RPC 2.0 over private stdin/stdout pipes.
Stdout is protocol-only; diagnostics go to structured logs, never into the
response stream. No TCP listener or model-visible broker tools.

The implementation must publish exact request/response schemas, frame and
in-flight limits, stable error codes, and cross-language fixtures before
runtime integration. Reject oversized frames and malformed input explicitly;
never truncate tool results silently. Broker protocol version and executable
build version are separate values.

Required operations:

| Method | Required behavior |
| --- | --- |
| `initialize` | Negotiate protocol version/features/limits; return build identity; do not start server children |
| `pool.start` | Accept resolved configs once; validate, start warm servers, return catalog and state |
| `tools.list` | Discover tools with optional server-id restriction; preserve current lazy-connect behavior |
| `tools.call` | Accept tool id, JSON arguments and optional explicit server id; return the full supported MCP result shape |
| `pool.restart` | Explicit server recovery subject to existing restart budgets |
| `pool.snapshot` | Catalog metadata and per-server operational state, errors and restart counts |
| `requests.cancel` | Cancel an identified request; report cancellation distinctly |
| `pool.close` | Stop admission, settle/cancel outstanding work, close sessions and children; acknowledge shutdown |

Responses carry matching ids; out-of-order completion is allowed. Request
deadlines and cancellation cross the adapter/broker boundary. A timeout or
disconnect is an uncertain outcome for a write: **never replay a tool call
automatically**. Restarting a connection is not permission to repeat a call.

Keep JSON-RPC/protocol failures, pool/transport failures, MCP tool errors and
caller cancellation distinguishable. Preserve tool content, structured
content and annotations needed by the current Python consumers; do not
reduce every result to text. Unsupported required result features fail
explicitly rather than disappearing.

Catalog/state snapshots carry monotonically increasing revisions. The
adapter maintains local metadata for synchronous consumers such as
`tool_spec`, `tools_by_server`, failure sets and restart counts. Supply
revisioned change notifications and refresh snapshots after state-changing
operations; do not synchronously block on IPC from those accessors.
Catalog metadata must preserve read-only hints used for receipt decisions.

Credentials may be needed in child environments but must never appear in
logs, snapshots, diagnostics or error messages. Snapshots expose operational
metadata, not command/environment dumps containing secrets.

## 5. Pool behavior and adapter compatibility

Port behavior from the **current** Python implementation, not just its
docstrings or this earlier spec. It has evolved during parallel work.

Reference files in `C:\Code\lisa`:

| Surface | Reference |
| --- | --- |
| Pool, routing, synthetic handlers, recovery and introspection | `kernel/src/kernel/tools/mcp_client.py`, `MCPToolPool` |
| Host-owned long-lived pool seam | `kernel/src/kernel/turn_hooks.py`, `ToolHooks.mcp_tool_pool` |
| Runtime compilation, env and log paths | `runtime/src/runtime/mcp_pool.py` |
| Manifest and command compiler | `kernel/src/kernel/mcp_launch/` |
| Capability owner selection (one live implementation per capability) | `runtime/src/runtime/capability_servers.py` |
| Ambient field injection at dispatch (proxy policy) | `kernel/src/kernel/mcp_launch/policy.py` (`ProxyPolicy`), `runtime/src/runtime/ambient_resolvers.py` |
| Version-pinned binary download and cache | `kernel/src/kernel/mcp_binary.py`, `runtime/src/runtime/mcp_binary_cache.py` |
| Separately connected content/catalog/guard adapters | `kernel/src/kernel/context/mcp_content_registry.py`, `mcp_catalog.py`, `mcp_guard.py` |

Required parity includes warm startup retries, required/non-required failure
behavior, cold discovery and failure suppression, restart-window limits,
server-scoped calls, bare-name fallback, capability tool ids, alias routing,
tool filtering, catalog collisions and introspection.

**Proxy/ambient parity.** The pool now strips ambient fields from
model-facing schemas and injects them at dispatch time from host-registered
resolvers (`user_slug`, `mode_id`, `app_id`) per server `proxy_policy`.
Schema projection happens at discovery; arg injection happens per call.
An adapter must preserve both sides: projected tool metadata in catalog
snapshots (ambient fields absent) and injected arguments on dispatch.
Resolvers remain Python-side; only their injected values cross the wire.
Capability selection, memory-alias ownership and provider names are
Python-resolved facts the adapter carries, not decisions it makes.

**Binary downloads stay Python-owned.** `resolve_mcp_binary_configs`
(runtime.py) already downloads, checksum-verifies and caches
`binary_download` servers before any spawn; `command` is resolved before
pool construction. The broker never re-implements this. Resolved-config
records arrive with a concrete executable path (cache path included) and
the broker launches it. A Python-owned cache means the Python side can
invalidate and re-download without any Go-side state.

Python synthetic handlers remain in the adapter and run before subprocess
routing, preserving existing precedence. Their definitions/callables do not
cross the wire. Merge their tool metadata with the broker catalog and
preserve explicit server targeting for aliases.

Inventory every pool consumer before implementation, including cached
metadata, synthetic registration, context managers and per-turn pools.
Do not claim that replacing `MCPToolPool` automatically replaces the separate
content/catalog/guard sessions. Inventory those paths and document which
remain Python-owned in v1; preserve their internal-only tool access even
when the model-facing allow-list is empty. Moving those sessions behind the
broker requires explicit server-scoped adapter wiring and tests.

Per-server concurrency must follow the current Python/MCP semantics.
Do not impose new serialization or promise unlimited concurrency without
parity tests. Propagate deadlines and stop all owned goroutines on close.

## 6. Process lifecycle and distribution

One broker is owned by each runtime pool lifecycle, normally long-lived
across turns. The runtime owns broker startup, handshake, readiness,
shutdown and exit monitoring. The broker owns its MCP child processes.

On required-startup failure, clean up children already started. On stdin
EOF or parent loss, stop accepting requests and clean up children. Bound
graceful shutdown, then terminate only the owned process tree. Implement
and test Windows process-tree ownership (for example Job Objects) and the
equivalent supported POSIX behavior; killing unrelated processes by name is
never acceptable.

Broker death fails in-flight requests explicitly and updates runtime
introspection. Recovery is explicit; no automatic replay or success-shaped
fallback. Repeated close is safe.

Distribute the broker and available MCP binaries in versioned OS/architecture
archives with checksums, licenses and build identity. Keep archives
self-contained for Go executables; Python servers still require their
existing Python environment. Confirm supported targets and dependency
constraints before promising static/cross-platform builds.

Runtime installation resolves executable paths explicitly, never from cwd.
Handshake compatibility, not matching release strings alone, determines
whether the runtime can use a broker. Release packaging must allow a
runtime deployment to pin a known-compatible archive.

## 7. Remaining server-port scope

The owner approved porting remaining first-party servers; implementation
is deferred until after this spec task. Existing ports: fetch, storage and
the thread MCP read surface.

| Candidate | Required scope and compatibility gates |
| --- | --- |
| `lisa-content` | Authored-text reads, roots and cache invalidation; preserve traversal safety, merged-root ordering and internal kernel access |
| `lisa-catalog` | Modes, operations, tools, roots, invalidation and diagnostics; preserve collision behavior and pack-contributed content semantics |
| `lisa-guard` | Input/output validation and backend status; preserve backend selection, result format and blocked audit logging; never substitute passthrough for an unsupported backend |
| `lisa-memory` | SQLite records, scope, add/search/list/get/delete/retire/count; investigate schema, backups, embeddings and migrations before choosing dependencies |

Suggested execution order: content, catalog, guard, memory, revisited after
dependency inventory. Memory remains pack-owned, with the same synthetic
aliases and provider selection; third-party Mem0 is not a first-party port.
Thread writing is not part of the current thread MCP tool surface and is
not implicitly added to this scope.

Catalog/root discovery and guard backends may rely on Python extension
loading. Determine the language-neutral resolved input boundary before each
port. Do not reimplement Python entry-point discovery or silently drop
extensions. If parity requires a Python-side export/adapter, specify and
coordinate it explicitly. A native executable is not proof of a fully
native dependency path.

Every server port needs its own source inventory, parity fixtures, safety
tests and real stdio e2e test before registration. Preserve the existing
server contract and published tool ids. Obtain required dependency approval
before adding SQLite, YAML or other new libraries. No domain-contract
redesign is authorized by this porting work.

## 8. Verification and acceptance gates

1. Freeze shared wire fixtures and run Go/Python conformance tests:
   handshake mismatch, malformed/oversized frames, ids, concurrency,
   cancellation, deadline propagation and stable error mapping.
2. Feed the same Python-compiled fixture configs to both backends. Compare
   actual command/argument order, child env including removed variables,
   cwd, data-directory creation and log paths. Include polluted parent-env
   regressions and paths with spaces.
3. Run backend-parameterized pool tests: discovery, warm retries, required
   failure cleanup, cold failures, budgets, collisions, filtering, aliases,
   scoped routing, synthetic tools and read-only metadata.
4. Use real Go and Python stdio servers in e2e tests. Validate parity of
   complete results, errors and introspection; test catalog/content clients
   independently where their sessions remain outside the broker.
5. Exercise child crash, broker crash, parent loss, cancellation and shutdown
   on Windows and supported POSIX targets. Assert no owned process leaks and
   no retry of an uncertain write.
6. Test runtime switch defaults, explicit Go selection, missing/incompatible
   binary, unsupported transport rejection and new-session rollback.
7. Compare measured cold startup, warm-turn overhead, discovery latency,
   tool-call latency and memory for both backends on identical fixtures.
   Report IPC overhead and server-startup gains separately. Do not claim a
   speed improvement without measurements.
8. Follow repository build/test instructions for Go changes, and the Python
   repo's instructions for adapter changes. Keep normal CI network-free.
   Required release acceptance must run cross-language tests; missing
   reference tooling is not a passing parity result.
9. Smoke-test extracted release archives and checksum verification on each
   supported release target.

## 9. Implementation decisions still required

- Freeze the exact resolved-config/result schemas, protocol errors and
  negotiated limits; account for every active Python pool consumer.
- Confirm the release OS/architecture matrix and process-tree strategy.
- Finalize Python option names, executable installation and compatibility
  policy with the runtime owner.
- Decide whether separate content/catalog/guard sessions stay outside the
  broker for v1; document the resulting coverage precisely.
- Approve dependencies only after the server and broker implementation
  inventories. A YAML dependency is no longer a broker prerequisite.

### Release gating for the provider pack (registration workstream)

Status as of 2026-10-24: the first three gates are satisfied — the repo is
public, `v0.1.0` is tagged, and the release workflow published 12 binaries
plus `checksums.txt`. The remaining gates:

- **Asset coverage.** `release.yml` builds only `lisa-storage` and
  `lisa-thread`; the Go fetch server has no release asset, so it cannot be
  selected via `binary_download` today. Add it to the matrix before any
  fetch registration.
- **Pack hygiene.** The pack must stay in sync with the Go servers'
  published surface: `capability_tools`/`default_available_tools` were
  verified against the host manifest's built-ins with the real Python
  loader (storage 10/10, thread 6/6, ambient parity restored).
- **Live validation.** `v0.1.0` was validated live from the public
  release: `resolve_mcp_binary_configs` downloaded and SHA-256-verified the
  assets, `MCPToolPool` warm-started both binaries, published capability
  ids (`storage.*` / `thread.*`), and round-tripped real tool calls. The
  pack installs from the public
  `git+https://…@v0.1.0#subdirectory=pack` URL and its
  `lisa.extensions.content_roots` entry point resolves with the manifest
  shipped alongside. Re-run this live test on any future release tag
  before the runtime fleet upgrades its pin.

## 10. Definition of done

- [ ] Broker/pool implementation, wire schemas and conformance fixtures exist.
- [ ] Python adapter and explicit switch are integrated and documented in
      the Python repository; the Python default remains available.
- [ ] No duplicate pack/manifest/capability-selection implementation in Go.
- [ ] Existing safety checks, policy gates, aliases, receipts and synthetic
      handlers are preserved; unsupported configurations fail explicitly.
- [ ] Cross-backend parity, real-server e2e, lifecycle/process-tree and
      packaging acceptance tests pass.
- [ ] Performance measurements are published without assuming an IPC speedup.
- [ ] Supported release archives contain the broker and available MCP
      binaries with checksums, licenses and build identity.
- [ ] README and this spec distinguish implemented, deferred and unsupported
      surfaces. Remaining server ports are tracked independently, not
      implied complete by broker delivery.
