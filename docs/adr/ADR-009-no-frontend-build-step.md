# ADR-009: No build step for the frontend

**Date:** 2026-10-17

## Context

This decision was made when the SPEC was written (2026-08-13). It is recorded here because the
SPEC now states only the rule (§2). The SPEC gave no reason for it at the time.

The frontend is one page, a stylesheet, one script and a service worker. Nag is a single Go
binary in a `FROM scratch` image, maintained by one person.

## Decision

The frontend is hand-written HTML, CSS and JavaScript, embedded with `//go:embed` and served
as written. There is no bundler, no transpiler, no `package.json` and no Node toolchain.

## Consequences

- The repository needs only Go to build and test. There are no npm dependencies to update.
- The file that the browser runs is the file in the repository, so a stack trace points to
  the source line.
- There is no JS test runner. Go tests check what they can read (CSS tokens, the manifest,
  the link pattern, the formatter copies), and the rest of the frontend is checked by hand.
  A browser test must run without a build step too.
- Code shared by the page and the service worker is copied, not imported. A Go test keeps
  the two copies identical.
