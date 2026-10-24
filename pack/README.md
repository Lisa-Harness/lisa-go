# Go MCP provider pack

This pack contributes Go implementations of Lisa's `storage`, `thread` and
`memory` capabilities. Install it from the same pinned Go repository tag as
the binary release:

```yaml
resources:
  packs:
    required: [lisa-pack-go-connectors]
    auto_install:
      lisa-pack-go-connectors:
        source: git+https://github.com/Lisa-Harness/lisa-go.git@v0.2.0#subdirectory=pack
        pin: v0.2.0

providers:
  capabilities:
    storage: lisa-pack-go-connectors
    thread: lisa-pack-go-connectors
    memory: lisa-pack-go-connectors
```

The runtime downloads only selected capability implementations. It verifies
the release asset against `checksums.txt` and caches it under
`<data_dir>/cache/mcp-binaries/`; subsequent starts use the verified cached
file. The release workflow publishes Linux, macOS, and Windows
`amd64`/`arm64` binaries when a `v*.*.*` tag is pushed.

The pack manifest pins `v0.2.0`, whose release workflow builds fetch, storage,
thread and memory for all six targets (24 binaries plus `checksums.txt`).
Fetch is distributed as a standalone MCP server; this pack registers only
the storage, thread and memory capabilities. `v0.1.0` contains only storage
and thread binaries.
