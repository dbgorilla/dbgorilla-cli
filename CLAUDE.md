# CLAUDE.md

## What this is

A Go CLI (`dbg`) that connects MCP-compatible IDE/agent clients to a
self-hosted DBGorilla deployment: `dbg login` then `dbg setup-ide` handles
authentication, MCP API-key provisioning, and writing the per-client MCP
config entry.

Naming mismatch worth knowing: brand **DBGorilla**, binary **`dbg`**, build
output `dbgorilla` (from `./cmd/dbgorilla`), Go module
`github.com/dbgorilla/dbgorilla-cli`.

## Layout

- `cmd/` — Cobra commands, one top-level command per file. Keep them thin:
  parse flags, call `internal/`, render output.
- `internal/api` — HTTP client for the backend (shared transport, User-Agent,
  redirect policy).
- `internal/auth` — OS keychain credentials, OAuth device flow, password login.
- `internal/config` — XDG-aware persisted settings (`~/.config/dbgorilla/cli.toml`).
- `internal/ide` — IDE/agent adapters. Adding a client = one new file + a
  `Registry` entry; see `.claude/rules/ide.md`.
- `scripts/` — `install.sh.tmpl` served by the backend for one-paste install.

## Build & test

    go build -o dbgorilla ./cmd/dbgorilla
    go test ./...

CI also runs golangci-lint v2 (`.golangci.yml`).

## Conventions

- Error messages tell the user what to do next
  (`"not logged in. Run: dbg login"`), not just what failed.
- The `✓` / `⚠` markers in `setup-ide` and `doctor` output are intentional —
  keep them. (Local exception to the org no-emoji rule.)
- No new dependencies without a brief note in the PR explaining why.

## Public-repo discipline

This repo is public. Do not commit:

- Customer names, deployment URLs, or tenant identifiers.
- Internal release-branch names, dev-environment paths, or internal-only
  feature flags.
- Test fixtures with real-looking secrets (use `https://x/mcp/` and
  `key123`-style placeholders — see existing tests).
- Coordination metadata from sibling repos or chat tools.

When in doubt, scrub it. Everything in `cmd/`, `internal/`, and tests is
visible to anyone with `git clone` access.
