# wiretuple — Technical Executive Summary (v0.1.0)

**wiretuple** is an open-source HTTP reverse proxy, written in Go with **zero
external dependencies**, compiled into a single static binary. It cuts the
output tokens of LLM **Structured Outputs** and **Function Calling** by
**35%–60%** — losslessly and transparently — by converting repeated JSON
objects into **positional tuples**.

End users change nothing in their application: they point `OPENAI_BASE_URL`
(or `ANTHROPIC_BASE_URL`) at the wiretuple proxy.

## Problem

Structured-output schemas make a model emit the same object keys again and
again. For a result array of N records with K fields, the model spends tokens
on `"field_name":` N×K times. Those repeated keys are pure overhead: the
receiver already knows them from the schema.

## The positional bijection

A repeated object

```json
{"items": [{"id": "a1", "total": 12.5, "currency": "EUR"}, ...]}
```

is bijective with a tuple-array form

```json
{"items": [["a1", 12.5, "EUR"], ...]}
```

given a fixed legend that maps each array position to its original key:

```
Tuple: [id, total, currency]
```

The proxy (1) rewrites the *request schema* into the positional form and
injects the legend once, and (2) rewrites the *response* back into full
objects. The mapping is a bijection: no information is lost, and the round
trip is bit-for-bit structurally identical.

## Protocol coverage

- OpenAI `/v1/chat/completions` — `response_format` `json_schema` and
  `tools[]` function calling.
- Anthropic `/v1/messages` — `tools[]` `input_schema` and structured output
  blocks.
- Both `stream: false` and `stream: true` (SSE), with an incremental
  rehydrator that adds sub-millisecond overhead and never buffers the whole
  stream.

## System design overview

```
client ──> internal/proxy ──> upstream (OpenAI / Anthropic)
                │
     ┌──────────┼──────────────┐
     │ internal/gate           │  break-even decision (activate vs passthrough)
     │ internal/compiler       │  schema -> tuple schema + reversible plan
     │ internal/rehydrator     │  tuple SSE/JSON -> object SSE/JSON (incremental)
     │ internal/shadow         │  passive audit + zero-dep SQLite persistence
     └─────────────────────────┘
```

- **gate** estimates token savings before compilation and activates only when
  net savings exceed a configurable break-even threshold; otherwise the proxy
  is a zero-cost passthrough.
- **compiler** produces an exact reversible `CompiledPlan` (tuple schema
  with `prefixItems` + `items: false` for Draft 2020-12, deterministic
  `Tuple: [k1, k2, ...]` legend, nested tuples, rehydration tree).
- **rehydrator** is a byte/rune-level state machine (no regex, no end-of-stream
  buffering) that handles multi-byte UTF-8 split across SSE chunks, escaped
  quotes/backslashes, brackets/commas inside strings, nested tuples, and
  degrades gracefully on model shape violations.
- **shadow** (`--shadow`) audits traffic without touching a byte and persists
  metrics to a zero-dependency SQLite database file.
- **bench** ships deterministic, realistic schemas (e-commerce invoice, dev-tool
  PR/issue batch, MCP tool definitions) and verifies rehydration with
  bit-for-bit structural equality.

## Status

v0.1.0 — Phases 1–5 complete (system design, M1–M7 implementation, hostile audit, CI, and v0.1.0 release). See `STATUS.md`.
