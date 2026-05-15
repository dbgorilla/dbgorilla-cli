# CLAUDE.md

Guidance for AI agents (Claude Code, Cursor, opencode, etc.) working in
this repository.

## What this is

A small Go CLI that connects MCP-compatible IDE/agent clients to a
self-hosted DBGorilla deployment. The user runs `dbg login`, then
`dbg setup-ide`, and the CLI handles authentication, MCP API-key
provisioning, and writing the per-client MCP config entry.

The brand is **DBGorilla**, the binary is **`dbg`**, the Go module path
is `github.com/dbgorilla/dbgorilla-cli`.

## Layout

```
cmd/                Cobra commands. One top-level command per file.
                    Keep these thin -- parse flags, call internal/, render output.
internal/api        HTTP client for the DBGorilla backend. Shared transport,
                    User-Agent injection, redirect policy.
internal/auth       OS keychain credentials, OAuth device flow, password login.
internal/config     XDG-aware persisted CLI settings (~/.config/dbgorilla/cli.toml).
internal/ide        IDE/agent adapters: detect installed clients and merge
                    MCP entries into their config files. Adding a new client =
                    one new file here + an entry in Registry.
scripts/            install.sh.tmpl served by the backend for one-paste install.
```

## Adding a new MCP client adapter

1. New file in `internal/ide/<name>.go` implementing the `Writer` interface
   from `ide.go` (or `Hinter` if MCP can only be set up via UI flow).
2. Append to `Registry` in `ide.go`.
3. Add entries in `adapters_test.go` pinning the load-bearing facts:
   top-level key (`mcpServers` / `servers` / `mc`), entry shape (any
   client-specific field names like `httpUrl` vs `url`), and config path
   per scope.
4. Update README's Supported Clients section.

The merge contract in `WriteMCPConfig` is mandatory:

- Always read existing config first; never start blank.
- Backup to `<path>.backup.<ts>` (mode 0600) before any write.
- Preserve every other top-level key.
- Preserve every other entry under the MCP key.
- Refuse `.jsonc` or files with `//` comments (would destroy comments) --
  caller falls back to `--print-config`.
- Idempotent re-runs: no write when the existing entry already matches.

## Build & test

```sh
go build -o dbg .
go test ./...
go vet ./...
```

CI also runs golangci-lint v2 (`.golangci.yml`).

## Code style

- `gofmt`/`goimports` clean.
- Error messages tell the user what to do next (`"not logged in. Run: dbg login"`),
  not just what failed.
- Default to no comments. Add one when the WHY is non-obvious -- a
  hidden constraint, a workaround for a documented IDE quirk, etc.
- Don't use emojis except sparingly in user-facing output (the existing
  `✓` / `⚠` markers in setup-ide and doctor are the convention).
- Cobra commands: short scannable `Short`, multi-paragraph `Long` only
  when behaviour isn't obvious from the name.
- No new dependencies without a brief note in the PR explaining why.

## Public-repo discipline

This repo is public. **Do not commit:**

- Customer names, deployment URLs, or tenant identifiers.
- Internal release-branch names, dev-environment paths, or internal-only
  feature flags.
- Test fixtures with real-looking secrets (use `https://x/mcp/` and
  `key123`-style placeholders -- see existing tests).
- Coordination metadata from sibling repos or chat tools.

When in doubt, scrub it. The README and CHANGELOG are the user-visible
face of this project; everything in `cmd/`, `internal/`, and tests is
also visible to anyone with `git clone` access.

## Single-commit history

This repo's main branch is intentionally kept at a single squashed
commit titled "initial commit" until the first stable v1.0 cut. PRs are
squash-merged; if the PR adds material the README should mention, update
the README in the same PR rather than waiting.
