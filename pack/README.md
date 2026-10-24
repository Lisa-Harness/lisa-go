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
        source: git+https://github.com/Lisa-Harness/lisa-go.git@v0.1.0#subdirectory=pack
        pin: v0.1.0

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

The current pack manifest pins `v0.1.0`, which publishes storage and thread
only. The source release workflow now builds fetch, storage, thread and
memory for future tags; update the manifest pins only to a tag whose release
contains the selected binaries and checksums.
