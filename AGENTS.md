# AGENTS.md — wiretuple (working rules for all agents)

wiretuple is an open-source HTTP reverse proxy in Go that cuts LLM Structured
Outputs and Function Calling output tokens by 35%–60% losslessly via
positional tuples. End users only set `OPENAI_BASE_URL` or
`ANTHROPIC_BASE_URL`.

## Hard rules (non-negotiable)

1. **Zero external dependencies.** `go.mod` MUST have zero `require`
   directives. Standard library only (`net/http`, `encoding/json`,
   `unicode/utf8`, `sync`, `io`, `bufio`, `strconv`, `time`, `flag`,
   `os`, `path/filepath`, `math`, `sort`, `hash/crc32`, `encoding/binary`).
   No cgo. If a task seems to need a third-party module, stop and design a
   stdlib solution instead.
2. **English only.** Every file, code comment, log line, error message,
   commit message, doc, schema example, and benchmark inside the repo is
   written in English.
3. **Review boundary.** Design notes and contracts come first, implementation
   follows, then an independent security pass on the proxy and data surface, a
   change review, and a final release check. No single contributor signs off
   their own work.
4. **Privacy.** Never mention employers, logistics companies, internal
   corporate systems, or client names in any file, schema example, benchmark,
   or doc. Use only generic domains (e-commerce, fintech, dev-tools) such as
   `store.example`, `payments.example`, `api.example`. `example.com` /
   `example.org` (RFC 2606) are fine.
5. **Attribution rule.** Never use job titles for the creator; describe the
   role as `Dimoncelli` or "creator". Prohibited titles include the common
   senior software job names, in any language.

## Toolchain

Go 1.23 or newer is required. `go test -race` needs a C toolchain; on Windows,
`zig cc` works as the external linker. Set `CGO_ENABLED=1` and `CC` to that
compiler before running the race gate. Local paths are never committed.

## Gates (must pass before "done")

- `go vet ./...` — clean.
- `go test ./...` — all green.
- `go test -race ./...` — all green (race detector on; see PATH note above).
- `make bench` — runs and prints the savings table with bit-for-bit
  structural JSON equality checks.

## Conventions

- Layout: `cmd/wiretuple`, `cmd/wiretuple-bench`, `internal/{gate,compiler,
  rehydrator,shadow,proxy}`, `benchmarks/`.
- Every package ships a `*_test.go` with table-driven tests. Edge cases in the
  rehydrator (UTF-8 splits, escaped quotes/backslashes, brackets/commas inside
  strings, nested tuples, model violations) MUST have a test.
- No commit/push/deploy unless explicitly approved by the user.
