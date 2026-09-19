# rly

`rly` is a local conversational runtime for coordinating coding-agent CLIs. The
current Phase 1 slice includes a repository-aware CLI/REPL, durable SQLite task
state, an append-only trace, and streaming adapters for Codex and Google
Antigravity (`agy`).

## Build and try it

Install with Homebrew:

```sh
brew install hkudligi/tap/rly
```

Or build from source:

```sh
go build ./cmd/rly
./rly
```

Useful commands:

```sh
./rly run "fix the flaky test"
./rly run --agent agy "review the current package"
./rly agents
./rly status
./rly tasks
./rly trace <task-id>
./rly status --json
```

State is stored in `~/.rly/state.db` by default. Use `--state PATH` before the
subcommand to select another database.

Codex is the default adapter. Both integrations use their machine-readable
streaming modes, retain the upstream session ID, normalize usage, and propagate
cancellation. Use `--sandbox read-only` for analysis-only tasks; mutation tasks
default to `workspace-write`. `rly run` returns exit code `4` when the selected CLI
is not installed or cannot report its version.

See [rly-product-technical-spec.md](rly-product-technical-spec.md) for the full
product and technical specification.
