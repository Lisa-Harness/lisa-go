# lisa-go — Go Connector SDK

Connect to / embed the [Lisa Harness](https://github.com/Lisa-Harness/lisa)
agent runtime from Go. This is the **Connector SDK**; sibling to the Python
Pack SDK (`lisa-harness-sdk`), which authors Lisa's extensibility.

Two audiences, one language-neutral contract
([lisa/docs/GO_CONNECTOR_SDK.md](https://github.com/Lisa-Harness/lisa/blob/main/docs/GO_CONNECTOR_SDK.md)):

| Want to… | Use | Language |
| --- | --- | --- |
| Author packs, modes, operations, tools | `lisa-harness-sdk` (Python Pack SDK) | Python |
| Write a first-party MCP server | this repo, `cmd/<server>/` over `connector/<primitive>/` | Go |
| Delegate the Python runtime's MCP pool to a Go binary | optional broker backend (spec only; see [PHASE5_HOSTSIDE.md](PHASE5_HOSTSIDE.md)) | Python adapter + Go broker |

## Layout

```text
connector/          Connector SDK: spawn, stdio MCP client, contract CLI/logging
cmd/lisa-fetch/     Phase 1 pilot — lisa-fetch MCP server in Go
cmd/lisa-storage/   Phase 2 — sandboxed storage backend
cmd/lisa-thread/    Phase 2 — thread read path
cmd/lisa-memory/    Phase 2 — memory backend (SQLite store, backups, recall)
pack/               Installable Lisa pack for the storage/thread/memory providers
PROTOCOL.md         contract version this module implements
```

The Connector SDK grew out of the `lisa-fetch` pilot: `cmd/lisa-fetch` proves
the server-side API, then `connector/` is hardened and used to port
`lisa-storage`, `lisa-thread` and `lisa-memory`. The Go provider pack declares
these as `runtime_kind: binary`; the Python runtime downloads a pinned release
asset, verifies its checksum, and caches it before launch.

## Status

Contract **v1** implemented end-to-end: `lisa-fetch` (pilot), `lisa-storage`,
`lisa-thread` (read path) and `lisa-memory` are ported and verified —
`go build ./...`, `go vet ./...`, `go test ./...` green, with e2e tests that
spawn the real binaries over stdio exactly as the runtime does.
`connector/thread` is line-reviewed against the Python sources with a
structural crosscheck that runs the reference `ThreadReader` on identical
fixtures. `connector/memory` carries the same two validation layers: ~30
store tests mirroring the Python `sdk/memory/tests/test_store.py` suite, plus
a **differential crosscheck** ([py_probe.py](connector/memory/py_probe.py) +
[crosscheck_test.go](connector/memory/crosscheck_test.go)) that builds the
fixture with the real Python `SqliteMemoryStore`, then replays 17 identical
operations in Go on the same DB and compares canonical JSON output —
run it via [connector/memory/run_crosscheck.ps1](connector/memory/run_crosscheck.ps1)
(requires the lisa repo's venv). The crosscheck caught and locked two wire
shape parity points: no `expires_at` field on records (Python's pydantic
shape omits it; validity surfaces through `valid_until`) and `[]`-never-null
for empty record lists. `connector/memory` ports `SqliteMemoryStore`
(schema v2, surface-scoped isolation, tier TTL expiry, lexical-overlap
ranking, opportunistic access tracking, point-in-time SQL backup dumps and
the embedding sidecar for semantic KNN recall — `modernc.org/sqlite`, pure
Go, no cgo, keeping the release matrix static-build). The Python framework
and the Pack SDK remain the source of truth for domain logic and
extensibility; this SDK never reimplements them.

Remaining: port the remaining first-party content, catalog and guard
servers; and implement the optional `lisa-mcp-broker` backend. The
broker would consume Python-resolved configurations and own launch/MCP pool
mechanics, selected through a runtime switch; turns, packs, policy and
approvals stay Python. The revised scope, distribution requirements and
verification gates are in [PHASE5_HOSTSIDE.md](PHASE5_HOSTSIDE.md). These
additions are **spec only**, not implemented. Standalone Go manifest
loading and embedding are deferred. Requires Go 1.22+ (the memory port uses
no newer toolchain than the existing floor).
