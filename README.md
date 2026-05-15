# dbgorilla

[![test](https://github.com/dbgorilla/dbgorilla-cli/actions/workflows/test.yml/badge.svg)](https://github.com/dbgorilla/dbgorilla-cli/actions/workflows/test.yml)
[![lint](https://github.com/dbgorilla/dbgorilla-cli/actions/workflows/lint.yml/badge.svg)](https://github.com/dbgorilla/dbgorilla-cli/actions/workflows/lint.yml)
[![CodeQL](https://github.com/dbgorilla/dbgorilla-cli/actions/workflows/codeql.yml/badge.svg)](https://github.com/dbgorilla/dbgorilla-cli/actions/workflows/codeql.yml)
[![release](https://img.shields.io/github/v/release/dbgorilla/dbgorilla-cli?logo=github)](https://github.com/dbgorilla/dbgorilla-cli/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/dbgorilla/dbgorilla-cli)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

The DBGorilla CLI. Sign in to a DBGorilla deployment and connect your IDE/agent (Claude Code, Cursor, VS Code, opencode, Gemini CLI) via MCP in three commands.

## Install

### Homebrew

```sh
brew install dbgorilla/tap/dbg
dbg --api-url https://<your-deployment> login
```

The first `dbg login` persists the API URL (and `--insecure` if you pass it) to `~/.config/dbgorilla/cli.toml` (or `$XDG_CONFIG_HOME/dbgorilla/cli.toml`), so every subsequent command runs without flags.

### Manual

Download a binary from the [Releases page](https://github.com/dbgorilla/dbgorilla-cli/releases) and put it on your `PATH`.

## Quick start

```sh
dbg login          # sign in (browser-based SSO or username/password)
dbg setup-ide      # configure every detected MCP client (Claude Code, Cursor, VS Code, ...)
dbg doctor         # verify everything works
```

That's it. Restart your IDE/agent and DBGorilla is wired up.

## Supported MCP clients

`dbg setup-ide` auto-detects every supported client installed on your
machine and configures each one. Pass `--client <slug>` to target a
specific tool, or `--list-clients` to see what's supported and which are
detected.

| Client | Slug | Setup type | Notes |
|---|---|---|---|
| Claude Code | `claude-code` | writer | Prefers `claude mcp add`; falls back to direct file write |
| Cursor | `cursor` | writer | `~/.cursor/mcp.json` (user) or `.cursor/mcp.json` (project) |
| VS Code | `vscode` | writer | `.vscode/mcp.json` (project) by default |
| opencode | `opencode` | writer | `~/.config/opencode/opencode.json` (user) |
| Gemini CLI | `gemini` | writer | `~/.gemini/settings.json` (user) |
| Claude Desktop | `claude-desktop` | manual hint | Remote HTTP MCP requires Settings → Connectors UI flow |

Useful flags:

```sh
dbg setup-ide --list-clients              # what's supported, what's detected
dbg setup-ide --client cursor             # target one
dbg setup-ide --client cursor,vscode      # target several
dbg setup-ide --scope project             # override the per-client default scope
dbg setup-ide --dry-run                   # show what would be written
dbg setup-ide --print-config --client X   # print the entry to paste manually
```

The merge is safe: existing MCP servers and unrelated config keys are
preserved, every write is preceded by a `<path>.backup.<timestamp>`, and
JSONC files (with `//` comments) are refused rather than overwritten.

## Commands

| Command | What it does |
|---|---|
| `dbg login` | Sign in. Auto-detects SSO vs. username/password. |
| `dbg logout` | Clear stored credentials. |
| `dbg whoami` | Show the signed-in user and organization. |
| `dbg setup-ide` | Mint an MCP API key and register DBGorilla in every detected MCP client. See [Supported MCP clients](#supported-mcp-clients). |
| `dbg doctor` | Verify auth, API reachability, MCP key, and per-client config. |
| `dbg config set <key> <value>` | Set `api-url` or `insecure` in user config. |
| `dbg config get <key>` | Show the resolved value and where it came from. |
| `dbg config unset <key>` | Clear a key from the user config. |
| `dbg version` | Print version info. |

## Centralized Claude allowlist

If your org uses a managed Claude allowlist (Team / Enterprise tier on app.claude.com), `dbg setup-ide` may be blocked by policy. Run:

```sh
dbg setup-ide --print-admin-allowlist
```

...and send the output to whoever manages your Claude admin console. Once they allowlist `dbg`, re-run `dbg setup-ide`.

## Configuration

Two persisted settings: `api-url` and `insecure`. Both follow the same priority chain (highest first):

1. Command-line flag (`--api-url`, `--insecure` / `--insecure=false`)
2. Environment variable (`DBGORILLA_API_URL`; there is no `DBGORILLA_INSECURE` env var — persist via `dbg config set insecure true` or pass `--insecure` on each call)
3. `$XDG_CONFIG_HOME/dbgorilla/cli.toml` (per-user; defaults to `~/.config/dbgorilla/cli.toml`; written by `dbg login` and `dbg config set`)
4. `/etc/dbgorilla/cli.toml` (or `/Library/Application Support/dbgorilla/cli.toml` on macOS, `C:\ProgramData\dbgorilla\cli.toml` on Windows) — IT-deployed via MDM, read-only from the CLI

If nothing is configured, `dbgorilla` exits with an actionable error pointing at the layers above.

`dbg config get <key>` shows which layer won the lookup.

### Persisted on successful login

`dbg login` writes both `api-url` (always) and `insecure` (when `--insecure` was explicitly passed) into the user config. This is the "I logged in once with the flags, now everything just works" pattern. Saved values are visible in `~/.config/dbgorilla/cli.toml`.

### Overriding persisted state

- `--api-url https://other` — one-shot override; doesn't change config.
- `--insecure=false` on `dbg login` — turns off any persisted `insecure = true`.
- `dbg config unset insecure` — clears `insecure` without re-logging in.

## Compatibility

Requires a DBGorilla deployment that exposes the Keycloak device-flow auth-config endpoint and the MCP API-key endpoints. If you're unsure whether your deployment qualifies, contact your DBGorilla administrator.

## Building from source

For contributors:

```sh
git clone https://github.com/dbgorilla/dbgorilla-cli.git
cd dbgorilla-cli
go build -o dbg .
```

Requires the Go version declared in `go.mod` (see the badge at the top of this README for the live value). Released binaries are produced from this same source by goreleaser on every `v*.*.*` tag — the `./dbg` you build locally behaves identically.

Cross-compile for another platform:

```sh
GOOS=darwin GOARCH=arm64 go build -o dbg-darwin-arm64 .
GOOS=darwin GOARCH=amd64 go build -o dbg-darwin-amd64 .
GOOS=linux  GOARCH=amd64 go build -o dbg-linux-amd64 .
GOOS=linux  GOARCH=arm64 go build -o dbg-linux-arm64 .
```

## Feedback

Open an [issue](https://github.com/dbgorilla/dbgorilla-cli/issues/new/choose) for bug reports or feature requests. Please include the output of `dbg doctor` (redacting any sensitive values) and your platform.

## License

MIT
