---
description: "Use when writing, editing, or reviewing Go source in this repo (*.go): package layout, naming, godoc, error handling, interfaces, concurrency, and performance rules."
applyTo: "**/*.go"
---

# Go Core Rules (lisa-go)

## Project Context
Idiomatic Go, standard library first, minimal external dependencies. Only
`github.com/modelcontextprotocol/go-sdk` (MCP), `github.com/klauspost/compress`
(zstd), and `golang.org/x/*` are acceptable additions without asking.

## Code Style & Structure
- Run `gofmt` and `goimports` on every save — no manually formatted files.
- Package layout: `cmd/` entrypoints, `connector/` private SDK, `connector/<server>/` primitive packages.
- Name packages short, lowercase, single-word; never `util`, `helper`, `common`.
- Exported identifiers need godoc comments starting with the identifier name: `// Server spawns a first-party MCP server.`
- Use early returns; a function body should rarely exceed 3 levels of indentation.
- Prefer named return values only when they clarify documentation.
- Declare empty slices as `var t []T` (nil), not `[]T{}`, unless JSON encoding needs a literal `[]`.
- Prefer small, local changes over broad refactors.

## Error Handling
- Every error must be handled; never assign to `_` unless intentionally discarding (document why).
- Wrap errors with context using `fmt.Errorf("loading config: %w", err)`.
- Define package-level sentinel errors with `errors.New` for conditions callers need to branch on; see `connector/storage.PathRejected` for a carried-metadata example.
- Distinguish errors with `errors.Is` (sentinel) and `errors.As` (type assertions) — never compare `.Error()` strings.
- Error strings are lowercase and carry no ending punctuation (`fmt.Errorf("reading %s", name)`), since they get embedded inside other messages.
- No `panic` in connector/cmd library code — return errors; `panic` only in `main` for genuinely unrecoverable setup failures.
- Handle errors at the boundary with enough context to act; propagate otherwise.

## Interfaces & Types
- Define interfaces at the point of use (consuming package), not the implementing package.
- Keep interfaces small — single-method like `io.Reader` is idiomatic.
- Accept interfaces, return concrete types from constructors.
- Use defined types (`type UserID int64`) to prevent accidentally mixing domain values.

## Concurrency
- Pass `context.Context` as the first parameter of every function that performs I/O or can be cancelled.
- `context.WithTimeout` for external calls; always defer the returned `cancel`.
- Never store a `context.Context` in a struct field; pass it through parameters (the only exception is matching a std-library or third-party interface).
- Protect shared state with `sync.Mutex`, minimal critical sections.
- `sync.Once` for lazy singletons; `sync.Map` only when the access pattern genuinely matches.
- Never start a goroutine without a clear stop mechanism (context cancellation, closed channel, or WaitGroup).

## Performance
- Profile before optimizing — no premature tuning.
- `strings.Builder` for multi-step string assembly; avoid `+=` in loops.
- Prefer buffered I/O (`bufio`) for file and network streams.
- Pre-allocate slices with `make([]T, 0, knownLen)` when the final length is known.