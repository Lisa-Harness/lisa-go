# lisa-go — Go Connector SDK

Connect to / embed the [Lisa Harness](https://github.com/Lisa-Harness/lisa)
agent runtime from Go. This is the **Connector SDK**; sibling to the Python
Pack SDK (`lisa-harness-sdk`), which authors Lisa's extensibility.

Two audiences, one language-neutral contract
([lisa/docs/GO_CONNECTOR_SDK.md](https://github.com/Lisa-Harness/lisa/blob/main/docs/GO_CONNECTOR_SDK.md)):

| Want to… | Use | Language |
| --- | --- | --- |
| Author packs, modes, operations, tools | `lisa-harness-sdk` (Python Pack SDK) | Python |
| Write a first-party MCP server, or embed Lisa as a host | `github.com/Lisa-Harness/lisa-go` (Connector SDK) | Go |

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

Phase 0 (contract) done in the `lisa` repo; scaffold in place. Requires Go 1.22+.
The Python framework and the Pack SDK remain the source of truth for domain
logic and extensibility; this SDK never reimplements them.
