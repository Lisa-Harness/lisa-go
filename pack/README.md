# Go MCP provider pack

This pack contributes Go implementations of Lisa's `storage` and `thread`
capabilities. Install it from the same pinned Go repository tag as the
binary release:

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
```

The runtime downloads only selected capability implementations. It verifies
the release asset against `checksums.txt` and caches it under
`<data_dir>/cache/mcp-binaries/`; subsequent starts use the verified cached
file. The release workflow publishes Linux, macOS, and Windows
`amd64`/`arm64` binaries when a `v*.*.*` tag is pushed.

The first pinned version is `v0.1.0`. It becomes usable after that tag is
published and the release workflow uploads its binaries and checksums.
