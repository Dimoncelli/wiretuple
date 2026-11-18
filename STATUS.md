# wiretuple — Milestone Status Tracker

Legend: `[ ]` pending · `[~]` in progress · `[x]` done.

## Phase 1 — System Design & Micro-Specifications

- [x] Repo baseline (AGENTS.md, RESUME.md, STATUS.md, go.mod, LICENSE, Makefile)
- [x] System design & atomic micro-specs
- [x] Independent design review & approval of the spec

## Phase 2 — M1–M7 Implementation

- [x] M1 `internal/compiler` — schema model + bidirectional compiler
- [x] M2 `internal/gate` — break-even decision
- [x] M3 `internal/rehydrator` — incremental SSE + non-streaming rehydration
- [x] M4 `internal/proxy` + `cmd/wiretuple/main.go` — reverse proxy + protocol coverage
- [x] M5 `internal/shadow` — passive audit + zero-dep SQLite writer
- [x] M6 `cmd/wiretuple-bench` + `benchmarks/` — deterministic benchmark suite
- [x] M7 integration & hardening — `go vet`, `go test -race ./...`, `make` targets

## Phase 3 — Hostile Audit & `-race`

- [x] Independent security pass: exfiltration, allowlist, cancellation
- [x] Change review: diff and contracts
- [x] Release check: full gate run (`go vet`, `go test -race ./...`, `make bench`)

## Phase 4 — GitHub & CI

- [x] Create `github.com/Dimoncelli/wiretuple` (MIT)
- [x] CI workflow (`ci.yml`)

## Phase 5 — Launch Kit

- [x] README (usage, `OPENAI_BASE_URL`/`ANTHROPIC_BASE_URL`, break-even tuning)
- [x] Release notes v0.1.0
