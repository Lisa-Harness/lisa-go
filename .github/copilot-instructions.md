# Go Standard Agent Rules (lisa-go)

Detailed, code-level rules live in on-demand instruction files under
`.github/instructions/` — they auto-attach when you touch matching files:

| File | Covers | Trigger |
| --- | --- | --- |
| `go-core.instructions.md` | Style, error handling, interfaces, concurrency, performance | `*.go` |
| `go-build-test.instructions.md` | Build/vet/test commands, e2e server-test pattern, known debt | build & test tasks |
| `lisa-contract.instructions.md` | MCP server contract, porting from Python, safety invariants | `cmd/**`, `connector/**`, `PROTOCOL.md` |

Go style authority: [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments)
and [Google Go Style Decisions](https://google.github.io/styleguide/go/decisions)
for anything not covered here.
