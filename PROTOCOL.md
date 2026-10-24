# Protocol

`github.com/Lisa-Harness/lisa-go` implements the language-neutral Lisa MCP
server contract defined at:

- Repo: `Lisa-Harness/lisa`
- Doc: `docs/GO_CONNECTOR_SDK.md`
- Contract version: **v1** (stdio-only transport)

This module binds to that document's YAML manifest schema, CLI flag surface,
logging env contract, stdio MCP protocol, and declared tool surface. It does
**not** bind to Python pydantic types, Python entry-point discovery, or any
SDK-internal helper.

Bumping the contract in a way that breaks an existing Go server requires a
coordinated change to that document and a `CHANGELOG.md` entry in `lisa`.
