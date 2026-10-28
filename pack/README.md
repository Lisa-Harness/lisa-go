# Go MCP provider pack

This pack contributes Go implementations of Lisa's `storage`, `thread` and
`memory` capabilities. Install it from a release tag; that tag is the only
version selector:

```yaml
resources:
  packs:
    required: [lisa-pack-go-connectors]
    auto_install:
      lisa-pack-go-connectors:
        source: git+https://github.com/Lisa-Harness/lisa-go.git@v0.2.0#subdirectory=pack

providers:
  capabilities:
    storage: lisa-pack-go-connectors
    thread: lisa-pack-go-connectors
    memory: lisa-pack-go-connectors
```

The pack's version comes from the git tag it is built from (`hatch-vcs`), and
its manifest omits `binary_download.version`, so Lisa downloads the binaries
from the release matching the installed pack (`v<pack version>`). Installs
from a branch or untagged commit get a development version with no matching
release and fail at startup. Cutting a release is just pushing a `vX.Y.Z` tag.

If startup reports a development pack version despite installing from a release
tag, a cached build may predate the tag on that commit. Rebuild just the pack
without reusing the cache (adjust the data directory and release tag as needed):

```sh
uv --no-cache pip install --python ~/.lisa/venv/bin/python --no-deps \
  --reinstall-package lisa-pack-go-connectors \
  'git+https://github.com/Lisa-Harness/lisa-go.git@v0.2.1#subdirectory=pack'
```

Then run `lisa mcp lisa-storage-go` to verify startup and tool discovery.

The runtime downloads only selected capability implementations. It verifies
the release asset against `checksums.txt` and caches it under
`<data_dir>/cache/mcp-binaries/`; subsequent starts use the verified cached
file. The release workflow publishes Linux, macOS, and Windows
`amd64`/`arm64` binaries when a `v*.*.*` tag is pushed.

Releases from `v0.2.0` build fetch, storage, thread and memory for all six
targets. Fetch is distributed as a standalone MCP server; this pack registers
only the storage, thread and memory capabilities. `v0.1.0` contains only
storage and thread binaries.

The storage provider includes `edit_file` in both its capability contract
and default tools. It replaces exact text without newline normalization,
rejects ambiguous matches unless `replace_all` is set, and accepts
`expected_sha256` from `stat_file` or a prior edit to reject stale content.
Edits preserve permissions and stage replacements atomically beside the target.
The Go storage backend supports UTF-8 text only.
