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

This repo is public. It is not only the code that is public: **commit messages,
PR titles and PR descriptions are permanent and world-readable too**, and they
are where this is most often forgotten. A commit message cannot be edited once
it is merged.

Do not publish, in code or in the text around it:

- Customer names, deployment URLs, or tenant identifiers.
- Names of internal environments (staging, preview, per-developer envs) or of
  internal hosts and services.
- People. No "reported by X", no "found by Y", no naming who asked for a change
  or who reviewed it. Describe the defect, not the discovery.
- Internal process detail: how a release is coordinated, what went wrong
  internally, which internal repo or ticket something came from, what an
  internal tool found.
- Internal release-branch names, dev-environment paths, or internal-only
  feature flags.
- Test fixtures with real-looking secrets (use `https://x/mcp/` and
  `key123`-style placeholders — see existing tests).
- Coordination metadata from sibling repos or chat tools.

Publish what a stranger needs to understand the change and use the software:
what behaviour changed, why it matters to them, and what they should do
differently. A behaviour change a user will hit **is** pertinent, including an
unwelcome one — state it plainly rather than omitting it.

Internal notes belong in `CLAUDE.local.md`, which is gitignored.

When in doubt, scrub it. Everything in `cmd/`, `internal/`, tests, and every
commit message and PR description is visible to anyone with `git clone` access.
