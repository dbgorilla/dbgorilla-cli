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
brew trust dbgorilla/tap   # recent Homebrew requires trusting third-party taps once
brew install dbgorilla/tap/dbgorilla
dbgorilla login
```

On Homebrew versions without a `brew trust` command, skip that line.

Installs the `dbgorilla` binary with a `dbg` alias — use whichever you prefer.

`login` with no flags signs you in to the hosted deployment at `https://app.dbgorilla.com`, which is where the CLI points when nothing else is configured. For a self-hosted deployment, name it once:

```sh
dbgorilla --api-url https://<your-deployment> login
```

The first `dbgorilla login` persists the API URL (and `--insecure` if you pass it) to `~/.config/dbgorilla/cli.toml` (or `$XDG_CONFIG_HOME/dbgorilla/cli.toml`), so every subsequent command runs without flags. Installing from a self-hosted deployment's `install.sh` writes that file for you, so the URL is already set before you log in.

### Go install

```sh
go install github.com/dbgorilla/dbgorilla-cli/cmd/dbgorilla@latest
```

Produces the `dbgorilla` binary (symlink it to `dbg` yourself if you want the short name). Requires a release cut with the `cmd/dbgorilla` layout.

### Manual

Download a binary from the [Releases page](https://github.com/dbgorilla/dbgorilla-cli/releases) and put it on your `PATH`.

## Shell completion

Homebrew installs set up tab completion for bash/zsh/fish automatically — nothing to do.

For Go install / manual installs, generate it yourself:

```sh
# zsh, current shell only
source <(dbgorilla completion zsh)

# zsh, every new shell (macOS/Homebrew paths shown; see `dbgorilla completion zsh --help` for Linux/bash/fish)
dbgorilla completion zsh > $(brew --prefix)/share/zsh/site-functions/_dbgorilla
```

`dbgorilla completion --help` lists bash, zsh, fish, and powershell, each with shell-specific setup instructions.

## Quick start

```sh
dbgorilla login          # sign in (browser-based SSO or username/password)
dbgorilla setup-ide      # configure every detected MCP client (Claude Code, Cursor, VS Code, ...)
dbgorilla doctor         # verify everything works
```

That's it. Restart your IDE/agent and DBGorilla is wired up.

## Supported MCP clients

`dbgorilla setup-ide` auto-detects every supported client installed on your
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
| `dbg completion <shell>` | Print a tab-completion script. See [Shell completion](#shell-completion). |

## Collector on AWS

`dbg collector install --target aws` deploys the collector as a single Fargate task that monitors your RDS/Aurora databases. It runs entirely with **your own** AWS credentials — the CLI reuses whatever `aws sso login` / `AWS_PROFILE` already resolves, and nothing sensitive passes through DBGorilla.

```sh
dbg collector install --target aws                 # discover the database, deploy
dbg collector install --target aws --dry-run       # show the config + validate, deploy nothing
```

### The CloudFormation template

The stack is defined by a template DBGorilla publishes, so you can read exactly what will be created before running anything:

- <https://dbgorilla-cfn-us-east-1.s3.us-east-1.amazonaws.com/collector/fargate/latest.yaml>

The template is versioned independently of the CLI — its version tracks its parameter contract, which changes far more rarely than `dbg` does. A given CLI build deploys one specific version (currently `.../collector/fargate/v1.0.yaml`), and a published version is never rewritten: a contract change means a new version, so an existing install's template can't shift under it.

This published copy is the only one — the CLI carries no template of its own, so the file you read at that URL is exactly the file your account deploys. If it can't be reached, the install stops and tells you to update rather than deploying anything else. `dbg` therefore needs HTTPS egress to `dbgorilla-cfn-us-east-1.s3.us-east-1.amazonaws.com`; if that isn't possible, host the template yourself and pass `--template-url`.

### Launching it yourself

The template takes no injected values, so you can deploy it from the console without the CLI:

1. Write a config — see [`examples/collector-aws.toml`](examples/collector-aws.toml) for every available option.
2. Encode it (stack parameters are single-line, so it's base64):

   ```sh
   dbg collector encode-config collector-aws.toml
   ```

3. Open the [quick-create link](https://console.aws.amazon.com/cloudformation/home#/stacks/quickcreate?templateURL=https://dbgorilla-cfn-us-east-1.s3.us-east-1.amazonaws.com/collector/fargate/latest.yaml&stackName=dbgorilla-collector), paste it into **CollectorConfig**, and fill in the identity DBGorilla minted for you.

Secrets never go in the config: reference them as `${DBG_SERVER_SECRET}` and `${DBG_DB_PASSWORD}` and supply the real values through the `ServerSecret` / `DbPassword` parameters, which the stack stores in Secrets Manager.

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
5. Built-in default: `https://app.dbgorilla.com`, the hosted deployment. `insecure` has no built-in default and stays off.

`api-url` therefore never comes up empty. A Homebrew install can't know a deployment URL, so with nothing configured the CLI targets the hosted product rather than refusing to run. Every layer above outranks it — including the user config file that a self-hosted `install.sh` writes at install time.

If you self-host and installed some other way (`go install`, a downloaded binary, a build from source), set the URL before you log in, or you'll be signing in to the hosted deployment instead of your own:

```sh
dbg config set api-url https://<your-deployment>
```

`dbg config get <key>` shows which layer won the lookup — `source: default` means the built-in above is in use. `dbg doctor` reports the same thing.

### Persisted on successful login

`dbg login` writes both `api-url` (always) and `insecure` (when `--insecure` was explicitly passed) into the user config. This is the "I logged in once with the flags, now everything just works" pattern. Saved values are visible in `~/.config/dbgorilla/cli.toml`.

### Overriding persisted state

- `--api-url https://other` — one-shot override; doesn't change config.
- `--insecure=false` on `dbg login` — turns off any persisted `insecure = true`.
- `dbg config unset insecure` — clears `insecure` without re-logging in.
- `dbg config unset api-url` — clears the saved URL and falls back to the built-in default.

## Compatibility

Requires a DBGorilla deployment that exposes the Keycloak device-flow auth-config endpoint and the MCP API-key endpoints. If you're unsure whether your deployment qualifies, contact your DBGorilla administrator.

## Building from source

For contributors:

```sh
git clone https://github.com/dbgorilla/dbgorilla-cli.git
cd dbgorilla-cli
go build -o dbgorilla ./cmd/dbgorilla
```

Requires the Go version declared in `go.mod` (see the badge at the top of this README for the live value). Released binaries are produced from this same source by goreleaser on every `v*.*.*` tag — the `./dbgorilla` you build locally behaves identically.

Cross-compile for another platform:

```sh
GOOS=darwin GOARCH=arm64 go build -o dbg-darwin-arm64 ./cmd/dbgorilla
GOOS=darwin GOARCH=amd64 go build -o dbg-darwin-amd64 ./cmd/dbgorilla
GOOS=linux  GOARCH=amd64 go build -o dbg-linux-amd64 ./cmd/dbgorilla
GOOS=linux  GOARCH=arm64 go build -o dbg-linux-arm64 ./cmd/dbgorilla
```

## Feedback

Open an [issue](https://github.com/dbgorilla/dbgorilla-cli/issues/new/choose) for bug reports or feature requests. Please include the output of `dbg doctor` (redacting any sensitive values) and your platform.

## License

MIT
