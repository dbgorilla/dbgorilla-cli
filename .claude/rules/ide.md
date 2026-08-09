---
paths:
  - internal/ide/**
---

# internal/ide — MCP client adapters

## Adding a new client

1. New file `internal/ide/<name>.go` implementing the `Writer` interface from
   `ide.go` (or `Hinter` if MCP can only be set up via a UI flow).
2. Append it to `Registry` in `ide.go`.
3. Add entries in `adapters_test.go` pinning the load-bearing facts: top-level
   key (`mcpServers` / `servers` / `mc`), entry shape (client-specific field
   names like `httpUrl` vs `url`), and the config path per scope.
4. Update the README "Supported MCP clients" section.

## WriteMCPConfig merge contract (mandatory)

This code rewrites the user's IDE config files in place — a wrong merge
destroys their settings. Every write must:

- Read the existing config first; never start blank.
- Back up to `<path>.backup.<ts>` (mode 0600) before any write.
- Preserve every other top-level key.
- Preserve every other entry under the MCP key.
- Refuse `.jsonc` or files containing `//` comments (a rewrite would strip the
  comments); the caller falls back to `--print-config`.
- Be idempotent: no write when the existing entry already matches.
