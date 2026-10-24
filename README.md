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
| Embed Lisa's MCP tool fleet in a Go app | this repo's host/embed side (planned; see [PHASE5_HOSTSIDE.md](PHASE5_HOSTSIDE.md)) | Go |

## Layout

```text
connector/          Connector SDK: spawn, stdio MCP client, contract CLI/logging
cmd/lisa-fetch/     Phase 1 pilot — lisa-fetch MCP server in Go
cmd/lisa-storage/   Phase 2 — sandboxed storage backend
cmd/lisa-thread/    Phase 2 — thread read path
PROTOCOL.md         contract version this module implements
```

The Connector SDK grew out of the `lisa-fetch` pilot: `cmd/lisa-fetch` proves
the server-side API, then `connector/` is hardened and used to port
`lisa-storage` and `lisa-thread`. A Go server needs **no kernel change** — the
runtime's `binary` runtime kind spawns it directly.

## Status

Contract **v1** implemented end-to-end: `lisa-fetch` (pilot), `lisa-storage`,
and `lisa-thread` (read path) are ported and verified — `go build ./...`,
`go vet ./...`, `go test ./...` green, with e2e tests that spawn the real
binaries over stdio exactly as the runtime does. `connector/thread` is
line-reviewed against the Python sources with a structural crosscheck that
runs the reference `ThreadReader` on identical fixtures. The Python framework
and the Pack SDK remain the source of truth for domain logic and
extensibility; this SDK never reimplements them.

Remaining: register the binaries with a live Python runtime run
(`mcp_servers.yaml` `runtime_kind: binary`; owned by a parallel agent), and
the host/embed side of the SDK — spec'd, deferred executable plan in
[PHASE5_HOSTSIDE.md](PHASE5_HOSTSIDE.md). Requires Go 1.22+.
