# Changelog

## Unreleased

### Changed

- `collector install --target docker` now defaults to `--ssl-mode disable`.
  The flag default (`verify-full`) is right for RDS and Aurora but cannot work
  against the database a local-dev user actually has: stock Postgres ships with
  `ssl=off`, so the documented `dbg collector install` failed on every standard
  local setup with "server does not support SSL". An explicit `--ssl-mode`
  still wins, and `--target aws` is unchanged. The guided flow now also asks
  for the TLS mode instead of leaving it flag-only.
- A missing `pg_stat_statements` in `shared_preload_libraries` is now a
  preflight **warning** rather than a hard failure. The collector runs and
  reports schema topology without it; it gates query-performance data only.
  Blocking meant a new user had to `ALTER SYSTEM` and restart their database
  before seeing anything work.

### Fixed

- Preflight's remediation for a failed connection pointed the wrong way: a
  server with TLS *disabled* was told to set `--ssl-mode require (or
  verify-full)`, which fails again. The advice now matches the failure, and
  names `disable` where that is the fix.
- `collector install` no longer reports success over a collector that is
  crash-looping. It re-checks the container's state and restart count after
  start, prints the container's own last output, and names the likely cause
  (commonly a config directory the container runtime cannot bind-mount)
  instead of blaming a private CA.
- Collector secrets fall back to a `0600` file when the OS keyring is
  unavailable, mirroring how login tokens are already stored. Previously the
  install aborted *after* the collector identity had been provisioned, leaving
  an orphaned identity server-side on headless Linux, WSL, and CI hosts.
- Backend errors carrying an HTML body (an SPA catch-all, proxy, or login
  portal answering instead of the API) no longer dump the page into the
  terminal. The CLI names the situation and points at `dbg config get api-url`;
  long JSON bodies are truncated.

- The AWS-target grant instructions (printed after install, or run by
  `--run-grant`) now include `GRANT pg_read_all_data` (PostgreSQL 14+). Without
  it the schema-topology scraper fails on every cycle -- its `pg_dump` needs
  SELECT on the monitored tables, which `pg_monitor` does not confer. Note this
  permits the collector to read table contents; installations that must forbid
  that can omit the grant at the cost of the topology feature. The grant runs
  last, so on PostgreSQL 13 and older every other grant still lands.

## v0.4.0

### Added

- `collector install --target aws` deploys the collector as an AWS Fargate task
  instead of a local Docker container, for monitoring RDS and Aurora Postgres.
  It discovers the database, its subnets, and its security groups, then creates
  a CloudFormation stack holding the ECS cluster, task, IAM roles, log group,
  and a Secrets Manager secret for the collector's credentials. `--target
  docker` remains the default and is unchanged.
- Multi-database collectors. One Fargate collector can monitor several
  databases: pass `--config` a TOML file with a `[[database]]` entry per
  database, or pick them from a checklist when the CLI finds more than one.
- Network-path verification before deploy. The CLI reads the VPC's
  security-group rules and reports whether the collector will actually be
  admitted to each database on its port, with the exact ingress rule to add
  when it won't. This catches the "deployed successfully but silently cannot
  connect" case before the stack is created.
- IAM database authentication by default, with `rds-db:connect` scoped to each
  database's resource ID rather than granted account-wide. The CLI prints the
  SQL a database admin must run, or runs it directly with `--run-grant`.
  `--db-password` opts a database into password authentication instead, and the
  password is stored in Secrets Manager rather than the task definition.
- Per-database query-analysis controls. `--commands` selects which analysis
  queries (`execute_query`, `explain`) the collector may run against each
  database; an interactive checklist appears when the flag is omitted, and
  `--enable-commands=false` forbids them outright.
- The AWS-target lifecycle commands: `collector status`, `logs`, `start`,
  `stop`, `restart`, `upgrade`, and `uninstall` all operate on the Fargate
  deployment when the collector was installed with `--target aws`. The target
  is recorded at install time, so these need no extra flag.
- `collector encode-config`, which encodes a collector config for the
  CloudFormation `CollectorConfig` parameter — for launching the stack by hand
  from the AWS console rather than through the CLI.
- The CloudFormation template is published at a stable URL and versioned on its
  own contract (currently v1.0), independent of the CLI's version. Customers
  can read and security-review the exact file their account will deploy before
  running anything, and `--template-url` deploys a self-hosted copy instead.

### Changed

- The default collector image moves to 0.3.3, which carries RDS certificates in
  the container's system trust root. This matters for the AWS target, where the
  CLI defaults to `verify-full` TLS against RDS and Aurora endpoints.

### Notes

- The AWS target requires HTTPS access to the published template. A CLI that
  cannot reach it fails with an explanatory error rather than deploying a
  possibly-stale local copy; use `--template-url` where egress is restricted.

## v0.3.1

### Changed

- Collector config renames the `keycloak_base_url` key to `auth_base_url`, and
  `collector install` gains an `--auth-url` flag (the old `--keycloak-url` flag
  is deprecated). The former name is still read as a fallback, so existing
  `collector.toml` files and scripts keep working; update them to `auth_base_url`
  at your convenience.

## v0.3.0

### Added

- Colorized status output across commands (login, whoami, doctor, setup-ide,
  collector, config, logout), via a new `--color`/`--no-color` flag pair.
  Color auto-detects a real terminal and honors `NO_COLOR` and `TERM=dumb`,
  so piped or incompatible output stays plain text.

### Fixed

- `dbg login` now honors Ctrl-C during the password-mode credential prompt
  instead of ignoring the interrupt.
- `dbg upgrade` runs `brew upgrade` against the `dbgorilla` formula rather
  than a nonexistent `dbg` formula.
- Preflight gates the `pg_stat_statements` check on the Postgres maintenance
  database, avoiding false failures where the extension isn't present
  cluster-wide.

## v0.2.0

### Added

- `dbg collector` — install and manage a local-dev collector.
- `go install github.com/dbgorilla/dbgorilla-cli/cmd/dbgorilla@latest` support
  via the `cmd/dbgorilla` layout.

### Fixed

- Auth: SSO/device-flow sessions now refresh at Keycloak rather than the
  backend.
- `dbg version` falls back to Go build info for `go install` builds where
  ldflags aren't injected.
- `setup-ide` Claude Code registration is now idempotent; added a
  topology-permission preflight, corrected the OTLP port, and a `--ca-cert`
  hint for private-CA deployments.

## v0.1.4 — Initial release

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
