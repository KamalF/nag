# ADR-001: Align go.mod and Dockerfile on Go 1.26, not the spec's literal 1.24

**Date:** 2026-08-19

Superseded by [ADR-002](ADR-002-go-version-1.27.md): the version moved to Go 1.27. The
single-version principle below still applies.

## Context

SPEC.md §10 writes `FROM golang:1.24` in the Dockerfile sketch. The sketch uses 1.24 as
"the recent stable release when the spec was written", not as a hard requirement. The real
minimum is Go 1.22, because the spec requires the pattern matching of `net/http.ServeMux`
(§2). Nothing in the spec depends on 1.24 specifically. The local toolchain is Go 1.26.6.

Two Go versions (one in `go.mod`, another in the Docker build image) can make a build that
works locally and behaves differently in the image. They give no benefit in return.

## Decision

`go.mod` declares `go 1.26` and the M4 Dockerfile builds `FROM golang:1.26`. The spec's
`1.24` is treated as illustrative, not normative. A `mise.toml` at the repo root pins the
local toolchain (`go = "1.26.6"`) so that contributors get the same version through mise.
(Pre-flight decision from PLAN.md, recorded here.)

## Consequences

- One version everywhere: local builds, CI, and the image compile the same way.
- The Dockerfile deviates textually from §10's sketch. The review of commit 48 must check
  that the deviation is exactly this one line and nothing else.
- Contributors need a Go ≥ 1.26 toolchain even though the code only requires 1.22 features.
  This minimum is higher than necessary, and we accept it to keep a single version.
