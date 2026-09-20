# rly
`rly` is a local conversational runtime for coordinating coding-agent CLIs. The
current Phase 1 slice includes a repository-aware CLI/REPL, durable SQLite task
state, durable project memory, an append-only trace, and streaming adapters for Codex, Cursor (`agent` / `cursor-agent`), and Google
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
./rly run --agent cursor "review the current package"
./rly run --planner cursor --agent agy "implement the planned feature"
./rly agents
./rly agents --json
./rly status
./rly tasks
./rly memory
./rly memory set test.command "go test ./..."
./rly memory delete test.command
./rly trace <task-id>
./rly status --json
```

State is stored in `~/.rly/state.db` by default. Use `--state PATH` before the
subcommand to select another database.

## Conversational REPL

Run `./rly` with no subcommand to open the repository-aware REPL. Any input that
is not a recognized command is immediately executed as a task by the default
Codex adapter with `workspace-write` sandboxing, just like `rly run`:

```text
$ ./rly
rly
repo: relay

› fix the flaky cache test
Agent/model inventory:
  codex    gpt-5.6-sol              installed=yes usable=yes remaining=UNKNOWN confidence=UNKNOWN version=0.x source=codex app-server account/rateLimits/read
task-...  PLANNING → codex
...streamed Codex response...
✓ COMPLETED (session-id)
```

REPL executions stream agent messages as they arrive and persist the same task
state, run result, upstream session, project-memory updates, and trace events as
`rly run`. An unavailable Codex installation or a failed run is reported on
standard error; the REPL stays open so status can be inspected or another task
can be entered.

Before every CLI or REPL task enters planning, `rly` discovers every configured
agent and its provider-visible models. It prints whether each agent/model is
installed and usable, its remaining availability percentage, a confidence
label, and the data source. Codex percentages come from
`account/rateLimits/read`; when multiple provider windows apply, the displayed
number is the lowest remaining percentage. If a provider does not expose an
exact percentage, `rly` prints `UNKNOWN` rather than estimating one. `agy`
and Cursor currently fall into this category.

The inventory is recorded as the `agent.inventory_discovered` event before the
task's `PLANNING` transition. `rly trace --json <task-id>` returns the full
structured inventory in that event. `rly agents --json` returns the current
inventory directly, while `rly run --json ...` returns an object containing
both `inventory` and `execution` (or `task` and `error` when execution cannot
start).

The following inputs are handled as commands and never create tasks:

| Input | Action |
| --- | --- |
| `/status`, `status`, `status of task` | Show the latest task and its state. |
| `/tasks`, `tasks`, `show tasks` | List recent tasks for this repository. |
| `/help` | Show CLI and REPL help. |
| `/exit` | Leave the REPL. |

Conversational command matching is case-insensitive and ignores repeated
whitespace. The existing `exit` and `quit` aliases also leave the REPL. Other
slash-prefixed input is reported as an unknown command rather than submitted to
an agent.

## Project memory

Memory is stored in SQLite under the absolute repository path, so facts from one
checkout are never loaded into another. Before every agent run, `rly` adds the
current repository's memory to the prompt. A successful agent can update it by
returning the constrained `<rly-memory>` JSON block described in its prompt;
`rly` removes that block from the recorded response, validates it, applies the
changes atomically, and records a `project_memory.updated` trace event. Failed
runs and malformed updates do not change memory.

Use `rly memory` (or `rly memory --json`) to inspect the current repository's
entries. `rly memory set <key> <value>` provides a manual correction path and
`rly memory delete <key>` forgets stale context. Keys are lowercase identifiers
up to 80 bytes using letters, digits, `.`, `-`, or `_`; values are limited to
2,000 bytes. Store stable facts and decisions such as build commands,
architectural constraints, and chosen conventions—not secrets, guesses, task
progress, or full transcripts.

Codex is the default adapter. Both integrations use their machine-readable
streaming modes, retain the upstream session ID, normalize usage, and propagate
cancellation. Use `--sandbox read-only` for analysis-only tasks; mutation tasks
default to `workspace-write`. `rly run` returns exit code `4` when the selected CLI
is not installed or cannot report its version.

To use two agents for one task, pass `--planner codex --agent agy` (or `--planner auto --agent auto`). Codex
inspects the repository in read-only mode and produces an implementation plan;
that plan is then included in the prompt sent to `agy`, which performs the
workspace changes. Both runs are persisted under the same task and appear in
`rly trace <task-id>`.

## Token availability & efficacy routing

`rly` includes an explainable multi-agent router that optimizes execution based on token availability and agent efficacy:

- **Token & quota optimization**: Discovers live provider rate limits and quota headroom. Agents with exhausted quota (`0%`) or below reserve thresholds are automatically protected and deprioritized or excluded.
- **Efficacy & role matching**: Routes tasks to specialists based on role fit (e.g. `agy` for planning, analysis, and architecture; `codex` for code implementation, test writing, and refactoring).
- **Routing strategies**:
  - `balanced` (default): Blends capability fit, session context, quota headroom, and reliability.
  - `conservative`: Prioritizes token conservation and headroom over minor capability differences.
  - `quality-first`: Prioritizes maximum capability and efficacy fit.
- **Trace observability**: Every routing decision, rationale, and candidate score breakdown is recorded as a `routing.selected` trace event and exposed via `rly trace --json` and `rly run --json`.

See [rly-product-technical-spec.md](rly-product-technical-spec.md) for the full
product and technical specification.
