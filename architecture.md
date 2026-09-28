# rly Architecture and Technology Overview

## Scope

`rly` is a local coordination runtime for coding-agent CLIs. It turns a
natural-language objective into a durable task, routes it to an available
agent/model, streams normalized progress, records the result, and preserves
enough state to inspect or retry the work later.

The current implementation is a local Go application backed by SQLite. It is
not yet a distributed workflow service and does not currently use Temporal.

## Architectural shape

```text
CLI / REPL
    |
    v
Application coordinator (internal/cli)
    |
    +--> inventory discovery
    +--> semantic task profiling
    +--> agent/model routing
    +--> project and task creation
    +--> retry and resume control
    |
    v
Core service (internal/core)
    |
    +--> task lifecycle and state transitions
    +--> project history
    +--> durable memory
    +--> orchestration and dependency scheduling
    +--> artifact and operational metadata
    |
    +------------------+
    |                  |
    v                  v
Agent adapters       SQLite store
internal/agents      internal/store
    |                  |
    +--> Codex         +--> tasks
    +--> Agy           +--> projects
    +--> Cursor        +--> runs
    +--> Freebuff      +--> sessions
                       +--> events / trace
                       +--> project memory
```

The boundaries are deliberately vendor-neutral: the core router and
orchestrator depend on the `agents.Adapter` interface rather than on a
provider-specific SDK.

## Main components

### Command and REPL layer

Location: `cmd/rly`, `internal/cli`

Responsibilities:

- Parse CLI flags and commands.
- Provide the interactive repository-aware REPL.
- Discover installed agents and available models.
- Start runs and render normalized progress.
- Expose task, project, trace, checkpoint, memory, operations, cancellation,
  and resume commands.
- Keep machine-readable `--json` output separate from human-readable output.

The current user-facing commands include:

```text
rly run <objective>
rly projects
rly project <project-id>
rly resume <task-id>
rly tasks
rly trace <task-id>
rly checkpoint <task-id>
rly status
rly ops
rly cancel <task-id>
rly memory ...
```

### Core service and lifecycle

Location: `internal/core/service.go`

The service owns durable task transitions and coordinates the run lifecycle:

```text
CREATE PROJECT/TASK
        |
        v
    PLANNING
        |
        v
     RUNNING
        |
        +--> COMPLETED
        +--> FAILED
        +--> CANCELLED
        +--> BLOCKED / WAITING_FOR_USER
```

Every task has an append-only event trace. Agent runs, provider/model attempts,
routing decisions, session identifiers, failures, memory updates, and lifecycle
transitions are recorded as durable state.

### Projects

Projects are the long-lived grouping scope for a repository. A project groups
multiple task attempts without replacing their individual history.

```text
Project: relay
  ├── Task attempt 1: FAILED
  ├── Task attempt 2: CANCELLED
  └── Task attempt 3: COMPLETED
```

Projects are created automatically per repository and can also be selected
explicitly with `rly run --project <project-id>`. `rly resume <task-id>`
creates a new attempt in the same project, preserving the original task and
trace.

The current resume operation is a new task attempt. The upstream provider
session remains associated with the original task; provider-level conversation
continuation can be added later as a project-scoped session policy.

### Routing

Location: `internal/core/router.go`, `internal/core/semantic.go`

Routing evaluates:

- Agent installation and availability.
- Provider model inventory.
- Quota/headroom and confidence.
- Agent capabilities such as file editing and session resume.
- Role fit for planning, implementation, review, or debugging.
- Configured routing strategy and agent weights.
- A provider-neutral semantic profile of the objective.

The router returns an explainable `RouteDecision` containing the selected
agent/model, rationale, semantic profile, and candidate score breakdown.

If a provider/model fails at runtime, Relay filters the failed model and
routes to the next eligible model or provider. Retries preserve the task's
trace and remain inside the same project history.

### Orchestration

Location: `internal/core/orchestrator.go`, `internal/core/durable_orchestration.go`

The orchestrator runs dependency-aware agent tasks in sequential, parallel, or
automatic mode.

- Read-only independent tasks may run concurrently.
- Workspace-mutating tasks are serialized unless explicitly declared safe and
  disjoint.
- Dependencies are passed through bounded handoff context.
- Token budgets are checked before launching work.
- A failed dependency prevents downstream work from starting.
- Delegated vendor tasks can be launched through the same adapter boundary.

The current durable orchestration metadata describes retry limits, cost
limits, cancellation, access controls, and idempotency. Execution itself is
currently local; an external durable workflow engine has not been integrated.

### Agent adapter layer

Location: `internal/agents`

The common adapter contract supports:

- Detection and version checks.
- Model discovery where available.
- Start and resume operations.
- Streaming normalized events.
- Cancellation.
- Usage reporting.
- Capability reporting.

The common event model includes session, progress, message, error, usage, and
result events. Provider-specific JSON or terminal output is normalized before
it reaches the core service.

Adapters currently include:

| Adapter | Provider CLI | Execution model |
| --- | --- | --- |
| Codex | `codex` | JSON streaming subprocess; supports resume and quota discovery |
| Agy | `agy` | JSON streaming subprocess; supports model selection and conversation IDs |
| Cursor | `agent` / `cursor-agent` | JSON streaming subprocess; supports model selection and resume |
| Freebuff | `freebuff` | TUI driven through PTY/tmux and workspace channel files |

Codex, Agy, and Cursor run headlessly through subprocess pipes. Freebuff is a
special case because its CLI currently requires a terminal UI and communicates
through `.rly/freebuff/run-*` files.

## Persistence architecture

Location: `internal/store/sqlite.go`

SQLite is the local source of truth. The main entities are:

```text
projects
  └── tasks
        ├── events
        ├── runs
        └── sessions

repositories ── project memory
```

### Projects

Projects contain repository identity, display name, lifecycle state, active
task reference, and timestamps.

### Tasks

Tasks contain the objective, project association, repository, lifecycle state,
version, and timestamps. A retry or resume creates another task attempt rather
than mutating away the previous attempt.

### Runs

Runs record adapter, upstream session ID, status, exit code, response, usage,
and start/completion timestamps.

### Sessions

Sessions map an adapter/provider conversation ID to a task and repository.
Healthy sessions are considered by routing when the adapter supports session
resume.

### Events

Events form an append-only per-task trace. They record transitions, routing,
inventory discovery, agent sessions, errors, quota failures, memory changes,
and orchestration decisions.

### Project memory

Memory is repository-scoped durable context containing validated key/value
facts and decisions. Agents can propose constrained updates through an
`<rly-memory>` block; Relay validates and applies updates only after a
successful run.

## Task artifacts

Location: `internal/core/task_artifacts.go`

Each task can have a filesystem artifact workspace containing objective,
planning, progress, decisions, findings, handoffs, evidence, and machine-
readable state. The artifact state is currently schema version 3 and includes
operational metadata for:

- Completion gates.
- Retry limits and attempts.
- Token/cost limits.
- Cancellation and idempotency.
- Access control.
- Orchestration backend metadata.
- Provider/model attempt history, including selected model, session ID, exit
  code, structured error detail, and relevant Freebuff status/result/trace
  paths when available.
- Agent result history.

These artifacts complement SQLite: SQLite is optimized for coordination and
queries, while artifacts are inspectable handoff material for humans and
future agent contexts.

`rly checkpoint <task-id>` renders the current artifact state for humans,
including the latest checkpoint, retry chain, provider attempts, Freebuff run
directory hints, and latest result summary. `rly checkpoint --json <task-id>`
returns the raw `state.json` artifact for automation.

## Execution flows

### Direct task

```text
CLI objective
  -> discover inventory
  -> resolve/create project
  -> create task
  -> route agent/model
  -> compose prompt with project memory
  -> start adapter
  -> normalize streamed events
  -> record provider attempt, run/session/trace, and artifact checkpoint
  -> validate completion
  -> complete, fail, or retry
```

### Planner plus executor

```text
create task
  -> read-only planner
  -> bounded planner handoff
  -> workspace-writing executor
  -> verification/result recording
```

### Retry/resume

```text
failed task
  -> inspect project history
  -> filter failed model/provider
  -> select next eligible route
  -> create new task attempt in same project
  -> retain original task and trace
```

## Frameworks and libraries used

### Language and standard library

- Go 1.26.
- `context` for cancellation and lifecycle propagation.
- `database/sql` for storage access.
- `encoding/json` for provider events, state, and CLI output.
- `os/exec` for provider CLI processes and version/model discovery.
- `flag` for CLI option parsing.
- `sync`, channels, and goroutines for streaming and orchestration.
- `crypto/rand` and `encoding/hex` for local IDs.

### Direct dependencies

| Dependency | Use |
| --- | --- |
| `modernc.org/sqlite` | Pure-Go SQLite driver for durable local state |
| `github.com/creack/pty` | Pseudo-terminal support for Freebuff execution |

### Platform and tooling integrations

- SQLite WAL mode, foreign keys, busy timeouts, and migrations.
- GitHub Actions for build, vet, and test checks.
- Homebrew packaging for distribution.
- `tmux` when available for Freebuff's terminal UI lifecycle.
- Provider-installed CLIs: Codex, Agy, Cursor, and Freebuff.

### Deliberately not used

- Temporal is not currently integrated.
- No cloud database or remote queue is required.
- No provider SDK is used; adapters invoke provider CLIs.
- No web server is required for the current local runtime.

## Reliability boundaries

The current design is strongest at local crash-safe state and transparent
retry routing. The primary remaining boundaries are:

- Provider CLIs can disconnect or return success without completing the user's
  intent; repository-change checks and completion gates reduce this risk.
- A project resume currently creates a fresh task attempt rather than
  guaranteeing provider conversation continuation.
- SQLite is local to one machine/process group; distributed workers would need
  a durable workflow engine or shared state service.
- Freebuff's PTY/file-channel protocol has different failure semantics from
  JSON-streaming adapters.

## Verification

The repository validates the architecture with unit and integration-style Go
tests across adapters, routing, orchestration, SQLite persistence, CLI behavior,
task artifacts, project history, and memory handling:

```sh
GOCACHE=/tmp/relay-go-build-cache go test ./...
```
