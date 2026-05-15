# Changelog

## v0.1.1 — Initial release

First release of the DBGorilla CLI.

### Commands

- `dbg login` — Sign in via Keycloak SSO (RFC 8628 device flow) with auto-fallback to username/password.
- `dbg logout` — Clear stored credentials.
- `dbg whoami` — Show the signed-in user and organization.
- `dbg setup-ide` — Mint an MCP API key and register DBGorilla in every detected MCP client. Supports Claude Code (via `claude mcp add` or direct write), Cursor, VS Code, opencode, and Gemini CLI. Detects Claude Desktop and prints manual setup instructions (its remote MCP requires the Settings → Connectors UI flow). Use `--list-clients` to see what's supported and detected; `--client <slug>` to target specific tools; `--scope user|project` to override the per-client default; `--print-config` to emit the JSON entry; `--dry-run` to preview without writing. All writes are merged with backups; existing MCP servers and unrelated config keys are preserved; JSONC files are refused rather than overwritten.
- `dbg doctor` — Verify auth, API reachability, MCP key, and IDE config.
- `dbg config {set, get, unset}` — Manage the deployment URL and other settings.
- `dbg version` — Print version info.

### Distribution

- Homebrew tap: `brew install dbgorilla/tap/dbg`
- On-prem install script served from the customer's DBGorilla backend at `/install.sh` — air-gapped friendly.
- Cross-platform binaries on [GitHub Releases](https://github.com/dbgorilla/dbgorilla-cli/releases).

### Compatibility

Requires a DBGorilla deployment that exposes the Keycloak device-flow auth-config endpoint and the MCP API-key endpoints. Contact your DBGorilla administrator if unsure.

### Notes

- API URL resolution: flag > env > user config > system config (IT-deployed via MDM).
- Tokens persist in the OS keychain (Keychain on macOS, Secret Service on Linux, Credential Manager on Windows) with a `0600` file fallback for headless boxes.
- `dbg setup-ide` shells to `claude mcp add` so managed Claude allowlist policies are respected. Use `--print-admin-allowlist` to get the IT-facing snippet for the Claude admin console.

### TLS / private CA

On-prem deployments using an internal CA need two trust-store updates:

1. **OS-level CA trust** for `dbgorilla` itself (deploy via MDM; `--insecure` works as a stopgap).
2. **`NODE_EXTRA_CA_CERTS`** pointing at the CA bundle for Claude Code. Node doesn't read macOS Keychain on its own; without this, Claude Code rejects the MCP server's certificate even if `curl` and Safari trust it. Do not use `NODE_TLS_REJECT_UNAUTHORIZED=0` — it disables verification for every HTTPS connection in the process. See README for details.
