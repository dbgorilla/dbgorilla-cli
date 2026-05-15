# Contributing to dbgorilla-cli

Thanks for your interest in contributing!

## Getting started

1. Fork the repository.
2. Clone your fork: `git clone https://github.com/<your-user>/dbgorilla-cli.git`
3. Install the Go version declared in `go.mod` (or newer — Go's compatibility guarantee makes minor-version mismatches harmless). Check with `go version`.
4. Build: `go build -o dbg .`
5. Run: `./dbg --help`

## Project structure

```
cmd/                     Cobra commands (one file per top-level command)
internal/
  api/                   HTTP client for the DBGorilla backend
  auth/                  Keychain credentials + device-flow + password login
  config/                Layered config resolution + persistence
  ide/                   IDE adapters (currently Claude Code only)
scripts/install.sh.tmpl  Templated install script served by the backend
.goreleaser.yml          Cross-platform release builds + Homebrew tap
```

Most user-facing changes touch `cmd/*.go`. Most plumbing lives under `internal/`. Keep `cmd/` thin (parse flags, call internal) and put non-trivial logic in `internal/`.

## Building and testing

```sh
go build -o dbg .         # build
go test ./...             # run tests
go vet ./...              # static check
```

Tests are unit-style and require no external services. If you add behavior that depends on the backend, prefer a `httptest`-driven fake over hitting a real server.

## Code style

- `gofmt` / `goimports` clean (`go vet` enforces a subset of this).
- Cobra commands: short, scannable `Short`; multi-paragraph `Long` only when the behavior isn't obvious from the name.
- Error messages should tell the user what to do next, not just what went wrong (`"not logged in. Run: dbg login"` rather than `"unauthenticated"`).
- No new dependencies without a brief note in the PR explaining why.

## Submitting a pull request

1. Create a feature branch from `main`: `git checkout -b feat/my-thing`
2. Make your changes; add or update tests where reasonable.
3. Run `go test ./... && go vet ./...` locally.
4. Commit with a clear message. We use the loose convention `feat:`, `fix:`, `docs:`, `chore:`, `test:` as commit prefixes — not strict conventional commits, just enough for the auto-generated changelog to group things sensibly.
5. Push and open a PR. Fill in the template.

## Reporting issues

Open a GitHub issue with the bug-report template. Include:

- What you tried (the exact command line).
- What happened (output, error messages).
- What you expected.
- Output of `dbg version` and `dbg doctor` if relevant.
- Your OS and Go version (if building from source).

## Security issues

Don't open a public issue for security vulnerabilities. See [SECURITY.md](SECURITY.md) for the private reporting path.

## License

By contributing, you agree that your contributions are licensed under the MIT License.
