# ADR-001: Align go.mod and Dockerfile on Go 1.26, not the spec's literal 1.24

**Date:** 2026-08-19

## Context

SPEC.md §10 writes `FROM golang:1.24` in the Dockerfile sketch. That reads as "recent stable
at spec-writing time" rather than a hard requirement: the actual language floor is Go 1.22,
needed for the `net/http.ServeMux` pattern matching the spec mandates (§2). Nothing in the
spec depends on 1.24 specifically. The local toolchain is Go 1.26.6.

Carrying two Go versions (one in `go.mod`, another in the Docker build image) invites
"works locally, differs in the image" drift for zero benefit.

## Decision

`go.mod` declares `go 1.26` and the M4 Dockerfile builds `FROM golang:1.26`. The spec's
`1.24` is treated as illustrative, not normative. A `mise.toml` at the repo root pins the
local toolchain (`go = "1.26.6"`) so contributors get the same version mise-managed.
(Pre-flight decision from PLAN.md, recorded here.)

## Consequences

- One version everywhere: local builds, CI, and the image compile the same way.
- The Dockerfile deviates textually from §10's sketch; commit 48's review must check the
  deviation is exactly this one line and nothing else.
- Contributors need a Go ≥ 1.26 toolchain even though the code only requires 1.22 features —
  a stricter floor than strictly necessary, accepted for the single-version property.
