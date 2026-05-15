# Security Policy

## Supported versions

| Version | Supported |
|---------|-----------|
| latest  | Yes       |

## Reporting a vulnerability

**Do NOT open a public GitHub issue for security vulnerabilities.**

Use [GitHub's private vulnerability reporting](https://github.com/dbgorilla/dbgorilla-cli/security/advisories/new) instead. We will acknowledge receipt within 48 hours and provide a detailed response within 7 days.

## Security model

The CLI is a thin client that talks to a customer's DBGorilla deployment over HTTPS. The relevant trust surface:

- **Auth tokens** live in the OS keychain (Keychain on macOS, Secret Service on Linux, Credential Manager on Windows). A `0600` file fallback is used only when the keychain is unavailable; the CLI prints a warning in that case.
- **Passwords** are read from stdin with terminal echo disabled when invoked interactively. They are never accepted as command-line flags (which would leak them to shell history and `ps` listings).
- **API key for MCP** is fetched on demand from the backend and either piped to `claude mcp add` (which stores it in Claude Code's own config) or written to `~/.claude/settings.json` directly. The CLI never persists the key in its own config files.
- **DSNs / connection strings** are never collected by v0.1.0 and not sent over the wire.

## TLS

- The default code path performs full TLS verification against the system trust store.
- `--insecure` / `-k` (or persisted `insecure: true` in user config) disables certificate verification. It is intended only as a stopgap for development environments with self-signed certificates while IT installs the internal CA. The CLI prints a warning during `setup-ide` when running in this mode to reduce the chance of it shipping to production unaware.
- `NODE_TLS_REJECT_UNAUTHORIZED=0` is **not** recommended for the corresponding Claude Code TLS issue — it disables verification for every Node HTTPS connection. Use `NODE_EXTRA_CA_CERTS` instead, which adds the internal CA to Node's trust store without weakening verification elsewhere. See the README's TLS section.

## Supply chain protections

- **SHA-pinned GitHub Actions** in all workflows to prevent tag-hijacking.
- **Dependabot** monitors Go modules and GitHub Actions weekly.
- **CodeQL** runs on every push, every PR, and weekly.
- **goreleaser** signs release archives and produces SLSA-style provenance.
- **Homebrew tap formula** is auto-updated from goreleaser on tag — no manual edits to the formula repo.
- **Minimal CI permissions** — each workflow declares only the permissions it needs.

## Out of scope

- Self-hosted DBGorilla deployments are responsible for their own backend security. The CLI trusts whatever URL the user configures.
- Compromise of a developer's machine is out of scope: any local attacker can read the OS keychain or `claude mcp` config the same way the CLI does.
