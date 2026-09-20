# rly Product and Technical Specification

**Status:** Draft v0.1  
**Date:** September 19, 2026  
**Product name:** rly, pronounced "relay"  
**Primary interface:** Conversational command-line application  

## 1. Executive summary

`rly` is a local conversational runtime for coordinating multiple coding-agent CLIs. A developer talks to one interface in natural language. `rly` interprets the request, assigns work to appropriate agents, preserves their sessions, brokers communication among them, coordinates Git worktrees, tracks uncertain quota, and presents a single traceable result.

The core product hypothesis is:

> A coding agent can recognize that another specialist would be more effective for part of a task, request help through `rly`, receive the result in its existing session, and continue—while the developer retains control over routing, cost, permissions, and changes to the repository.

The initial product should prove this hypothesis deeply rather than attempt autonomous software development in general.

## 2. Goals

### 2.1 Product goals

1. Provide a natural-language terminal experience comparable to modern coding-agent CLIs.
2. Coordinate two or more heterogeneous coding-agent CLIs in one task.
3. Allow agents to ask, delegate, review, respond, and notify through a controlled local broker.
4. Preserve agent sessions so follow-up work does not repeatedly rebuild repository context.
5. Route work using capabilities, availability, quota, task risk, and user policy.
6. Use Git and files as shared state; use messages primarily for intent and compact results.
7. Make every routing decision, message, command, file change, and test result inspectable.
8. Keep the developer in control through natural-language constraints, slash commands, approvals, and hard limits.

### 2.2 MVP success criteria

The MVP is successful when a developer can:

1. Start `rly` inside a Git repository and describe a coding task naturally.
2. Have one agent implement a change.
3. Have that agent request targeted assistance from a second agent through `rly`.
4. Resume the first agent's existing session with the response.
5. Run tests, request a diff-only review, and apply review feedback.
6. Inspect a complete trace explaining what happened, which agent was used, and why.
7. Enforce a task budget and agent-specific reserve without agents bypassing those policies.

## 3. Non-goals for the MVP

The MVP will not include:

- A web UI.
- Cross-machine or distributed execution.
- Remote workers or a distributed message broker.
- A learning or ML-based router.
- Fully autonomous task-DAG generation for arbitrary projects.
- Unbounded debates, broadcasts, or recursive delegation.
- A generic third-party plugin marketplace.
- Semantic indexing of entire repositories.
- A replacement for Git, build tools, or coding-agent sandboxes.
- Guaranteed exact quota reporting when an upstream CLI does not expose it.
- Identical capabilities across all agent adapters.

## 4. Product principles

### 4.1 The user talks to rly

The product presents one conversational identity. Users should not need to select an agent for every task. They may override routing when they care.

### 4.2 Agents communicate through rly

Agents must not launch or call one another directly. All inter-agent requests pass through the coordinator, which applies routing, quota, permissions, recursion, and lifecycle policies.

### 4.3 Messages carry intent; the workspace carries state

Messages should reference files, commits, diffs, artifacts, and test runs rather than copying large amounts of repository content.

### 4.4 Sessions are valuable state

An existing session that understands the repository is preferred over a new session when capability, policy, and context remain suitable.

### 4.5 User instructions are hard constraints

Instructions such as "do not change tests," "use Codex for implementation," or "do not spend more than 20k Claude tokens" must be represented as enforceable session or task policy, not left only in conversation history.

### 4.6 Expensive collaboration is explicit and bounded

Broadcast, debate, premium-agent use, and recursive delegation require policy checks. Defaults should favor targeted consultations over broad fan-out.

## 5. Primary user experience

### 5.1 Interactive mode

Running `rly` with no subcommand opens a repository-aware REPL:

```text
$ rly

rly
repo: payments-api    branch: feature/idempotency
agents: 3 available   task: none

› fix the flaky payment integration test

I'll inspect the failure before changing code.
→ implementer: codex

Codex found a likely transaction race and requested a concurrency review.
→ debugger: claude (estimated 4.8k tokens)

Claude identified a non-atomic existence check and insert.
→ codex: session resumed

✓ fix applied
✓ regression test added
✓ 214 tests passed

› why did you use claude?

The request was a narrow, high-risk concurrency question. Claude had the
highest eligible debugging score, sufficient quota, and an estimated cost
below this task's automatic-consultation threshold.
```

### 5.2 Natural-language controls

The user may modify execution using ordinary language:

```text
› don't modify public APIs
› use gemini for exploration and codex for implementation
› conserve claude unless codex fails twice
› stop the reviewer
› show me what claude found
› revert the database part
› ask another agent to review only the current diff
```

Recognized policies are confirmed briefly and stored as structured constraints.

### 5.3 Slash commands

Slash commands provide deterministic runtime control:

| Command | Purpose |
|---|---|
| `/help` | Show available commands and examples. |
| `/status` | Show the current task, step, agents, and repository state. |
| `/agents` | Show configured agents, health, capabilities, and active sessions. |
| `/quota` | Show quota estimates, confidence, reserves, and reset times. |
| `/trace` | Show the ordered task and communication trace. |
| `/diff` | Show changes associated with the current task. |
| `/policy` | Show or change session and task policies. |
| `/tasks` | List recent and active tasks. |
| `/stop` | Stop an active step or the task after confirmation when needed. |
| `/undo` | Revert the most recent rly-managed change using a recoverable Git operation. |
| `/compact` | Compact conversational context while preserving structured state. |
| `/exit` | Leave the REPL without deleting task or session state. |

### 5.4 Noninteractive and scripting mode

```bash
rly run "fix the flaky payment test"
rly run "upgrade Spring Boot to 3.5" --auto
rly ask architect "review the transaction design"
rly review --scope current-diff
rly status --json
rly trace task-47 --json
rly policy set claude.min-reserve=20%
git diff main | rly review --stdin --json
```

Exit codes must be stable and documented:

| Code | Meaning |
|---:|---|
| 0 | Task completed successfully. |
| 1 | Task failed. |
| 2 | Invalid input or configuration. |
| 3 | User approval is required. |
| 4 | Agent unavailable or adapter failure. |
| 5 | Budget or policy prevented execution. |
| 6 | Task was cancelled. |
| 7 | Repository or worktree conflict. |

## 6. Functional requirements

### 6.1 Conversation and intent

`rly` must classify user input into one of these intent classes:

| Intent | Examples | Handler |
|---|---|---|
| Task | "Fix the race condition." | Planner and agent runtime |
| Query | "What are you doing?" | Local state and trace |
| Control | "Stop." or "Use Codex instead." | Coordinator |
| Policy | "Conserve Claude." | Policy engine |
| Repository operation | "Show the diff." | Git runtime |
| Clarification | "Which service do you mean?" | Conversation layer |

Deterministic parsing should handle slash commands, stop/cancel, status, diff, trace, and direct policy syntax. A lightweight model-backed controller may handle ambiguous natural language. The controller must produce a typed intent and may not execute shell commands or mutate the repository directly.

### 6.2 Task lifecycle

A task has these states:

```text
CREATED → PLANNING → RUNNING → VERIFYING → REVIEWING → COMPLETED
               ↘ WAITING_FOR_USER
               ↘ BLOCKED
               ↘ FAILED
               ↘ CANCELLED
```

Each state transition must be persisted and traced. A task may be resumed after process restart unless its underlying agent session cannot be resumed.

### 6.3 Inter-agent operations

The protocol exposes five operations:

| Operation | Semantics | Typical completion |
|---|---|---|
| `ASK` | Request a bounded answer or diagnosis without ownership transfer. | Synchronous or short asynchronous response |
| `DELEGATE` | Assign ownership of a concrete subtask. | Artifact, diff, or commit |
| `REVIEW` | Evaluate a defined scope against stated criteria. | Findings and verdict |
| `RESPOND` | Complete or update a prior request. | Structured result plus natural-language summary |
| `NOTIFY` | Send a one-way state or compatibility update. | Delivery acknowledgement |

Agents address capabilities or roles rather than specific vendors by default:

```json
{
  "type": "ASK",
  "to": {
    "capability": "database-concurrency",
    "priority": "high"
  },
  "objective": "Determine whether concurrent requests can create duplicate payments",
  "artifact_refs": [
    {"kind": "file", "value": "src/payment/PaymentService.java"},
    {"kind": "test_run", "value": "testrun-91"}
  ],
  "constraints": ["Do not modify files", "Return a diagnosis only"]
}
```

The router resolves the request to a concrete agent. An explicit user mapping always overrides automatic selection when the mapped agent is eligible.

### 6.4 Message envelope

Every message must contain:

```json
{
  "schema_version": "1.0",
  "message_id": "msg-01J...",
  "task_id": "task-47",
  "correlation_id": "req-782",
  "parent_message_id": null,
  "type": "ASK",
  "from": {"role": "implementer", "session_id": "ses-c392"},
  "to": {"capability": "database-concurrency"},
  "created_at": "2026-09-19T22:10:31Z",
  "deadline_at": "2026-09-19T22:15:31Z",
  "priority": "high",
  "payload": {
    "objective": "Review transaction handling",
    "question": "Can concurrent requests create duplicate payments?",
    "constraints": ["No code changes"]
  },
  "artifact_refs": [],
  "git_refs": [{"repo_id": "repo-1", "commit": "a812ef", "diff": "HEAD~1..HEAD"}],
  "budget": {"max_tokens": 8000, "max_duration_seconds": 300},
  "delegation": {"depth": 1, "max_depth": 2},
  "status": "PENDING"
}
```

Required message statuses are `PENDING`, `ROUTING`, `RUNNING`, `COMPLETED`, `FAILED`, `DENIED`, `EXPIRED`, and `CANCELLED`.

Messages are append-only. Corrections and status changes are recorded as events rather than destructive updates to the original payload.

### 6.5 Agent responses

A response should be concise and structured enough for machines while retaining a readable summary:

```json
{
  "type": "RESPOND",
  "correlation_id": "req-782",
  "status": "COMPLETED",
  "summary": "The existence check and insert are not atomic.",
  "findings": [
    {
      "severity": "high",
      "file": "src/payment/PaymentService.java",
      "line": 143,
      "issue": "Concurrent requests can pass the existence check before either inserts.",
      "recommendation": "Use a database uniqueness constraint and atomic insert-on-conflict behavior."
    }
  ],
  "artifact_refs": [],
  "confidence": 0.91
}
```

Malformed or incomplete responses are retained for diagnosis. The adapter may attempt one repair or normalization pass before marking the request failed.

### 6.6 Agent-initiated communication

Agents receive tools corresponding to the protocol operations. The preferred integration is a local MCP server owned by `rly`:

```text
ask_agent
delegate_task
request_review
respond_to_request
notify_agent
get_request_status
```

Where MCP is unavailable, an adapter may use a structured JSONL output channel or a reserved output marker. The coordinator validates every request; an agent tool call is a request, not authorization to execute another agent.

### 6.7 Persistent sessions

For each agent, `rly` records:

- Adapter and agent identity.
- Upstream session identifier.
- Repository and worktree.
- Assigned role and current task.
- Creation and last-use times.
- Resumability and health.
- Context summary and relevant artifacts.
- Accumulated usage estimates.

The coordinator should resume a healthy compatible session when:

- It belongs to the same repository and task, or an explicitly continuable follow-up.
- Its worktree and Git base remain valid.
- The requested role is compatible.
- No policy demands a fresh context.

A session must be replaced when it is corrupted, non-resumable, points to deleted workspace state, violates a new isolation policy, or exceeds configured age/context limits.

### 6.8 Adapter contract

The orchestration engine must not contain vendor-specific conditionals. Each coding CLI implements an `AgentAdapter` contract conceptually equivalent to:

```text
detect() -> AgentInstallation
capabilities() -> AdapterCapabilities
health() -> HealthStatus
start(request, context) -> SessionHandle
resume(session, request) -> RunHandle
stream(run) -> EventStream
cancel(run) -> CancelResult
usage(session or run) -> QuotaObservation
normalize(event) -> AgentEvent
```

Adapter capabilities include:

- Noninteractive invocation.
- PTY requirement.
- Structured output.
- Session creation and resume.
- MCP or tool support.
- Usage or quota reporting.
- File-editing support.
- Sandbox and approval modes.
- Cancellation behavior.

The runtime must degrade gracefully. For example, a non-resumable adapter can still execute isolated `ASK` or `REVIEW` requests.

### 6.9 Routing

Routing evaluates only eligible agents. Eligibility applies hard constraints first:

1. User agent selection or exclusion.
2. Required adapter capability.
3. Required permission or sandbox level.
4. Availability and health.
5. Remaining hard budget and reserve.
6. Delegation depth and communication limits.

The MVP uses an explainable weighted score:

```text
score =
    0.35 × capability_fit
  + 0.20 × session_context_value
  + 0.15 × quota_headroom
  + 0.10 × expected_reliability
  + 0.10 × latency_fit
  + 0.10 × cost_fit
  - risk_penalties
```

Scores range from 0 to 1. Weights are configurable. The router records candidate scores, exclusions, selected agent, expected cost, and the policy that authorized execution.

Risk changes routing behavior. High-risk work such as authentication, data loss, migrations, money movement, or concurrency may require a stronger capability score, a review step, or user approval.

### 6.10 Quota model

Quota is uncertain and represented explicitly:

```text
QuotaState
  remaining_tokens: optional integer
  remaining_fraction: optional decimal
  reset_at: optional timestamp
  confidence: EXACT | ESTIMATED | INFERRED | UNKNOWN
  observed_at: timestamp
  source: adapter | cli_output | historical_estimate | user_override
```

The tracker records observations and estimated consumption for each run. When exact usage is unavailable, estimates use input size, output size, elapsed work, and historical adapter behavior. Estimated quota must never be displayed as exact.

Before a task enters planning, the coordinator inventories every configured
agent/model pair. Each row records installation and usability, an optional
remaining percentage, confidence, and its data source. Provider-reported quota
windows may be displayed as exact; if no exact percentage is exposed, the CLI,
REPL, and JSON output use `UNKNOWN`/`null` and retain the source and diagnostic.
The complete inventory is persisted in an `agent.inventory_discovered` trace
event before the `PLANNING` state transition.

Policies include:

- Minimum reserve per agent.
- Maximum tokens or estimated cost per task.
- Automatic consultation threshold.
- Approval threshold.
- Strategy: `conservative`, `balanced`, or `quality-first`.
- Behavior when quota is unknown: `allow`, `ask`, or `deny`.

### 6.11 Approvals and intervention

The coordinator pauses when a proposed operation exceeds policy:

```text
Claude consultation requested
Purpose: review a high-risk schema migration
Estimated usage: 18k tokens
Remaining estimate: 43k tokens
Automatic threshold: 10k tokens

[y] approve once  [n] deny  [a] approve similar requests for this task
```

Interactive users may also intervene when:

- An agent repeats the same failure.
- A task has been blocked beyond a threshold.
- Two agents provide materially conflicting recommendations.
- A worktree cannot merge cleanly.
- A potentially destructive command requires approval.

`--auto` removes ordinary prompts but does not bypass non-overridable safety policy. Its behavior must be fully configurable and documented.

### 6.12 Git and worktrees

Git is the source-control layer. `rly` must not invent a parallel merge system.

For mutation tasks, the default workspace layout is:

```text
~/.rly/worktrees/<repo-id>/<task-id>/
  implementer/
  delegate-1/
  reviewer/          # optional; read-only review may use an existing tree
```

Rules:

1. A role that writes code receives its own worktree unless explicitly sharing a single sequential worktree.
2. Reviews default to a commit, diff, or read-only view.
3. Delegates return a commit or patch plus test evidence.
4. The coordinator validates a clean base before applying or merging work.
5. Conflicts pause for the originating agent or user; the runtime does not silently discard changes.
6. Existing uncommitted user changes are preserved and never overwritten.
7. Cleanup occurs only after task completion and according to retention policy.

### 6.13 Verification and review

The planner identifies validation commands from repository configuration, user instruction, or agent proposals. Commands run through the local runtime rather than being trusted solely from agent claims.

Evidence includes:

- Command, working directory, start and end times.
- Exit code.
- Bounded stdout and stderr with a pointer to full logs.
- Test counts when parsable.
- Associated commit or diff.

A review request must specify its scope and focus, for example:

```text
scope: current_diff
focus: [correctness, concurrency, backward_compatibility]
mode: findings_only
```

The coordinator may skip a review only when task policy permits it and records the reason.

### 6.14 Trace

`rly trace` presents a chronological, replayable record:

```text
19:03:12  user          requested fix for issue #891
19:03:13  planner       classified debugging / high risk
19:03:13  router        implementer → codex
19:05:42  codex         requested database-concurrency help
19:05:42  router        debugger → claude
                         reason: capability 0.96, estimated 4.8k, policy allowed
19:06:31  claude        found non-atomic check and insert
19:06:32  rly           response delivered to codex session ses-c392
19:07:04  codex         applied fix and regression test
19:07:42  verifier      214 tests passed
19:08:01  task          completed
```

JSON output exposes the same underlying events for automation.

## 7. System architecture

### 7.1 Components

```text
User / stdin
    │
    ▼
Conversation layer
    │ typed intent
    ▼
Coordinator ─── Policy engine ─── Quota tracker
    │
    ├── Planner and router
    ├── Message broker
    ├── Session manager
    ├── Git/worktree manager
    ├── Process and sandbox runtime
    └── Trace/event store
             │
             ▼
        Agent adapters
      ┌──────┼──────┐
      ▼      ▼      ▼
   Agent A Agent B Agent C
```

### 7.2 Control plane and execution plane

The control plane owns conversation, policies, routing, budgets, sessions, messages, and trace. The execution plane owns agent processes, local commands, PTYs, worktrees, files, and cancellation.

No execution-plane process may create another agent process without a validated control-plane request.

### 7.3 Local daemon

The MVP should use a local daemon to preserve task and session coordination across terminal invocations.

- Unix: Unix domain socket at a runtime-directory path such as `$XDG_RUNTIME_DIR/rly/rly.sock`, with a user-private fallback.
- Windows: named pipe in a later supported milestone.
- Transport: length-delimited JSON or JSONL for MVP.
- Authentication: operating-system user ownership and restrictive filesystem permissions.
- Startup: lazy start from the CLI, with explicit `rly daemon start|stop|status` controls.

The protocol must include a version handshake so client and daemon incompatibility fails clearly.

### 7.4 Persistence

SQLite is the authoritative metadata and event store for the MVP. Filesystem artifacts hold large logs, patches, summaries, and transcripts.

Suggested layout:

```text
~/.rly/
  config.yaml
  state.db
  tasks/<task-id>/
    artifacts/
    logs/
    summaries/
  worktrees/<repo-id>/<task-id>/
```

Suggested tables:

| Table | Purpose |
|---|---|
| `repositories` | Repository identity and paths. |
| `tasks` | Task state, policy, and result. |
| `steps` | Planned and executed units of work. |
| `agents` | Configured agent definitions. |
| `sessions` | Upstream session handles and health. |
| `messages` | Immutable protocol envelopes. |
| `events` | Ordered trace events. |
| `artifacts` | File, diff, commit, log, and result references. |
| `runs` | Process executions and outcomes. |
| `quota_observations` | Exact and estimated quota records. |
| `approvals` | Approval requests and decisions. |

Every mutable entity uses optimistic versioning. Events use a monotonic sequence per task for deterministic replay.

## 8. Configuration

Example configuration:

```yaml
version: 1

agents:
  architect:
    adapter: agent-a
    command: agent-a
    capabilities:
      architecture: 1.0
      debugging: 0.95
      review: 1.0
    quota:
      min_reserve_fraction: 0.20

  builder:
    adapter: agent-b
    command: agent-b
    capabilities:
      java: 0.95
      implementation: 1.0
      refactoring: 0.90

  scout:
    adapter: agent-c
    command: agent-c
    capabilities:
      exploration: 1.0
      search: 1.0
      mechanical_edits: 0.90

routing:
  strategy: balanced
  unknown_quota: ask
  automatic_consultation_max_tokens: 10000

communication:
  max_agents_per_task: 4
  max_delegation_depth: 2
  max_messages_per_task: 20
  max_review_rounds: 2
  allow_recursive_delegation: false
  allow_broadcast: false

tasks:
  default_timeout_minutes: 45
  retain_worktrees_days: 7
  require_review_for_high_risk: true

permissions:
  default_mode: workspace-write
  network: adapter-default
  destructive_commands: require_approval
```

Configuration precedence, highest first:

1. Explicit command-line flags.
2. Current natural-language task policy.
3. Repository configuration at `.rly/config.yaml`.
4. User configuration at `~/.rly/config.yaml`.
5. Product defaults.

## 9. Safety and guardrails

### 9.1 Hard limits

The runtime enforces:

- Maximum active agents per task.
- Maximum delegation depth.
- Maximum agent messages and review rounds.
- Per-agent and per-task budgets.
- Process duration and idle timeouts.
- Output and log size limits.
- Allowed worktree and repository roots.
- User-defined denied agents, commands, paths, and network modes.

### 9.2 Recursion and deadlock prevention

Each request carries a delegation depth and ancestry chain. A request is denied when it exceeds depth, repeats a capability cycle without new evidence, or targets a session waiting on the requester.

The broker detects wait cycles. It may resolve a cycle by converting one request to asynchronous delivery, cancelling the newest dependent request, or asking the user. It must never allow two sessions to wait indefinitely on each other.

### 9.3 Repository safety

- Never overwrite existing uncommitted user changes.
- Never run destructive Git operations implicitly.
- Capture repository status before and after each mutating step.
- Keep agent write scope within its assigned worktree.
- Require approval for commands classified as destructive unless explicitly preauthorized.
- Redact configured secrets from logs and messages.

### 9.4 Prompt and tool-boundary safety

Repository content and agent output are untrusted input. They cannot modify coordinator policy merely by containing instructions. Only authenticated user input, configuration, and validated tool calls may change routing or permissions.

## 10. Failure handling

| Failure | Required behavior |
|---|---|
| Agent process exits unexpectedly | Preserve logs, mark run failed, retry according to policy, and resume or replace session. |
| Structured output is malformed | Retain raw output and attempt one bounded normalization pass. |
| Session resume fails | Mark session unhealthy and offer or start a fresh session with a compact handoff. |
| Quota becomes insufficient | Stop new work on that agent and reroute, wait, or request approval. |
| Agent request times out | Notify requester and allow it to continue, retry, or escalate. |
| Worktree conflict | Preserve both sides and pause for agent or user resolution. |
| Daemon restarts | Replay persisted state and reconcile live or orphaned processes. |
| User cancels | Propagate cancellation, terminate within a grace period, and preserve recoverable state. |
| Test fails | Return evidence to the responsible session; apply bounded retry policy. |
| Policy changes mid-task | Apply to future operations immediately; interrupt active work only when required for safety. |

Retries must be bounded and visible in the trace. Repeating the same prompt without new evidence does not count as a useful retry.

## 11. Observability and privacy

### 11.1 Local observability

The MVP records:

- Task and step duration.
- Agent selection and candidate scores.
- Session reuse rate.
- Message latency and failure rate.
- Exact or estimated token usage with confidence.
- Retry and cancellation counts.
- Test and review outcomes.
- Human approvals and interventions.

### 11.2 Privacy defaults

- State remains local by default.
- Telemetry is disabled unless explicitly enabled.
- Logs avoid environment variables and known secret patterns.
- Full prompts and agent transcripts have a configurable retention period.
- Trace output distinguishes between metadata, summaries, and sensitive raw content.

## 12. Internal module boundaries

An implementation should maintain boundaries equivalent to:

```text
cmd/
  repl
  run
  ask
  review
  agents
  status
  trace
  policy

core/
  conversation
  coordinator
  planner
  router
  policy

protocol/
  messages
  events
  validation

agents/
  adapter
  registry
  implementations/

runtime/
  daemon
  process
  pty
  session
  sandbox
  cancellation

workspace/
  repository
  git
  worktree
  artifacts

quota/
  provider
  tracker
  estimator

store/
  sqlite
  migrations

telemetry/
  trace
  metrics
  redaction
```

No module outside `agents/implementations` may depend on the semantics or output format of a specific coding CLI.

## 13. MVP delivery plan

### Phase 0: Feasibility spikes, 1–2 days

- Verify noninteractive invocation for two selected CLIs.
- Verify session creation and resumption.
- Verify structured event or tool integration.
- Measure cancellation and PTY behavior.
- Document available quota signals.

**Exit criterion:** Two adapters can start, stream, cancel, and resume or explicitly report that resume is unavailable.

### Phase 1: Single-agent conversational runtime, 2–3 days

- CLI and REPL.
- Local daemon and SQLite migrations.
- Task, run, session, and event models.
- One production-quality adapter.
- Status, stop, trace, and JSON output.

**Exit criterion:** A user can run and continue a task through one stable agent session.

### Phase 2: Inter-agent ASK and RESPOND, 2–3 days

- Second adapter.
- Local broker and message schema.
- Agent-facing MCP tools or structured fallback.
- Capability-based fixed routing.
- Response injection into the originating session.
- Depth, timeout, and message-count limits.

**Exit criterion:** Agent A requests help from a capability, Agent B answers, and Agent A resumes with the answer.

### Phase 3: Worktrees, delegation, and review, 3–5 days

- Worktree creation and cleanup.
- `DELEGATE`, `REVIEW`, and `NOTIFY`.
- Commit/diff/artifact handoffs.
- Local verification commands.
- Conflict and cancellation handling.

**Exit criterion:** An implementer and reviewer collaborate safely on one task with an inspectable diff and test evidence.

### Phase 4: Quota-aware routing and usable UX, 3–5 days

- Quota providers and estimator.
- Explainable weighted router.
- Reserves, budgets, and approval thresholds.
- Natural-language policy extraction.
- `/quota`, `/policy`, polished trace, and recovery.

**Exit criterion:** Routing honors capability and quota policy, explains its choice, and survives daemon restart.

### Expected effort

- Convincing two-agent proof of concept: 3–5 focused days.
- Usable MVP: approximately 2–4 focused weeks for one experienced engineer.
- Reliable daily driver: approximately 6–10 weeks, driven mainly by adapter edge cases, recovery, and cross-platform behavior.

These estimates assume two initially supported CLIs with workable noninteractive and session interfaces.

## 14. MVP acceptance scenarios

### Scenario A: Targeted consultation

1. User asks `rly` to diagnose and fix a concurrency test.
2. Implementer starts in a managed worktree.
3. Implementer issues `ASK` for database-concurrency expertise.
4. Router selects an eligible second agent and records why.
5. Second agent returns a diagnosis without modifying files.
6. Implementer session resumes and applies the fix.
7. Runtime executes tests and reports evidence.
8. Trace shows every step and message.

### Scenario B: User-controlled routing

1. User says, "Use Agent C for exploration, Agent B for implementation, and Agent A only for final review."
2. `rly` confirms and persists the task policy.
3. All subsequent routing follows the mapping.
4. If a mapped agent is unavailable, `rly` asks rather than silently substituting another agent.

### Scenario C: Quota protection

1. Premium agent reserve is 20 percent.
2. An agent requests a consultation that would cross the reserve.
3. The coordinator denies or requests approval according to policy.
4. The requester receives a structured denial and can continue or ask for an alternative capability.

### Scenario D: Delegated implementation

1. Implementer delegates regression tests.
2. Delegate receives a separate worktree and bounded scope.
3. Delegate returns a commit and test result.
4. Coordinator applies the commit or reports a conflict.
5. Implementer is notified and continues in its existing session.

### Scenario E: Recovery

1. The daemon stops during a running task.
2. On restart, `rly` reconstructs the task from SQLite and events.
3. It reconciles or marks orphaned processes.
4. Resumable agent sessions remain usable; non-resumable work is handed off through a compact artifact.
5. No repository changes are silently lost or overwritten.

## 15. Decisions deferred beyond the MVP

- Learned capability scores based on repository-specific outcomes.
- Automatic multi-step DAG planning and parallel scheduling.
- Explicit debate and multi-agent synthesis workflows.
- Broadcast requests to all agents.
- Remote execution and team-shared coordinators.
- Cross-repository tasks.
- Rich terminal UI beyond a line-oriented REPL.
- Public adapter SDK and plugin distribution.
- Organization-level policy and audit export.
- Cost accounting across subscription and API-priced agents.

## 16. Open implementation decisions

The following decisions should be resolved during Phase 0:

1. Which two coding CLIs offer the strongest initial combination of noninteractive mode, resumable sessions, structured output, and tool integration?
2. Which implementation language best supports a portable single binary, PTY control, SQLite, and MCP? Go and Rust are leading candidates; implementation speed and library maturity should decide.
3. Can the first two agents both consume an `rly` MCP server, or is a JSONL fallback required immediately?
4. Which quota signals are available without screen scraping or brittle parsing?
5. Should mutating tasks always use worktrees, or may the user opt into the current working tree for simple sequential work?
6. Which controller model, if any, should parse ambiguous natural-language policy while minimizing latency and quota use?
7. Which commands and filesystem operations are classified as destructive by default?

## 17. Product positioning

Short description:

> `rly` is a conversational CLI runtime that coordinates coding agents, relays work among specialists, and spends their limited reasoning capacity deliberately.

Concise tagline:

> **rly — relay work across coding agents.**

The product's differentiating mechanisms are:

1. Structured inter-agent communication through a user-controlled broker.
2. Capability- and quota-aware scheduling across heterogeneous coding CLIs.
3. Persistent sessions and file-based handoffs that avoid repeatedly rebuilding context.
4. A natural-language interface backed by deterministic policies and an inspectable trace.
