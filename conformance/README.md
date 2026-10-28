# Conformance

Differential tests that drive the canonical Python servers in
[Lisa-Harness/lisa](https://github.com/Lisa-Harness/lisa) and the Go ports
with identical MCP calls over stdio, then compare the normalized results.
`scripts/conformance.sh` also runs the memory store crosscheck
([../connector/memory/crosscheck_test.go](../connector/memory/crosscheck_test.go)).

Any skipped parity test fails the run, so a misconfigured environment can
never pass as green.

## Run locally

```sh
scripts/conformance.sh ../lisa   # syncs ../lisa's uv workspace from its lockfile
LISA_SKIP_SYNC=1 scripts/conformance.sh ../lisa   # reuse an existing .venv
```

## Canonical revision

[LISA_REF](LISA_REF) pins the canonical `lisa` commit that the Go ports are
expected to match. Advance it in the same change that ports the matching
behavior. The pinned commit must be pushed to `Lisa-Harness/lisa` for CI to
fetch it.

## Triggers (event-driven, no schedule)

| Event | Canonical revision | Effect |
| --- | --- | --- |
| Push to a `conformance/**` branch | `LISA_REF` | Iterate on parity without releasing |
| Push a `v*.*.*` tag | `LISA_REF` | Required before release assets build ([release.yml](../.github/workflows/release.yml)) |
| `repository_dispatch` `lisa-changed` | `client_payload.lisa_sha` (full SHA) | Detect upstream drift; never advances the pin |
| `workflow_dispatch` | `lisa_ref` input, else `LISA_REF` | Test an explicit pairing |

### Upstream hook

To trigger on canonical changes, the `lisa` repo sends the exact commit SHA
that changed the server surface, using a token that may only create
dispatches on this repo (ideally a GitHub App installation token):

```yaml
on:
  push:
    branches: [main]
    paths:
      - mcp_server/**
      - sdk/src/sdk/storage/**
      - sdk/src/sdk/memory/**
      - sdk/src/sdk/content/**
      - sdk/src/sdk/thread/**
      - docs/GO_CONNECTOR_SDK.md
      - uv.lock
jobs:
  notify-lisa-go:
    runs-on: ubuntu-latest
    steps:
      - env:
          GH_TOKEN: ${{ secrets.LISA_GO_DISPATCH_TOKEN }}
        run: |
          gh api repos/Lisa-Harness/lisa-go/dispatches \
            -f event_type=lisa-changed \
            -f "client_payload[lisa_sha]=$GITHUB_SHA"
```

## Normalization

Comparison is exact after these documented normalizations:

- JSON payloads are re-marshaled with sorted keys; `null`, missing fields, and
  `[]` stay distinct.
- The per-run data directory and wall-clock timestamps (`modified`, ISO-8601
  values) are masked.
- The Python MCP SDK's `Error executing tool <name>: ` prefix is stripped from
  reference error text.
- Tool surfaces compare names, read-only/destructive annotations, and input
  property/required sets. Descriptions are not compared.
