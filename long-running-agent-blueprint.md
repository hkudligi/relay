# Long-Running Coding Agent Blueprint

## Executive recommendation

Treat the coding agent as a short-lived, replaceable worker and the task as a durable workflow.

Use a workflow orchestrator such as Temporal for durable execution, retries, timers, human-input signals, and parallel work. Keep the source of truth in durable task artifacts and repository state, not in one agent's conversation context.

The design goal is not to keep one context alive indefinitely. It is to make progress recoverable when an agent reaches its context limit, crashes, times out, or is replaced.

## Goals

- Continue work across many agent contexts and process restarts.
- Preserve decisions, discoveries, constraints, and evidence.
- Resume from a precise checkpoint rather than restarting exploration.
- Support retries, human decisions, parallel investigation, and review gates.
- Prevent agents from claiming completion without verifiable evidence.
- Keep the first version understandable and operable by one team.

## Non-goals

- Eliminating the need for task decomposition.
- Making arbitrary shell commands automatically safe to retry.
- Allowing multiple agents to edit shared files without coordination.
- Treating summaries as a replacement for tests, commits, or repository state.

## System model

```text
User goal
   |
   v
Durable workflow (Temporal or equivalent)
   |
   +--> task state, phase, dependencies, retries, user questions
   |
   +--> bounded agent activities
           |
           +--> planner
           +--> explorer
           +--> implementer
           +--> verifier
           +--> reviewer
   |
   v
Repository + durable task artifacts + evidence
```

The workflow owns progression. Agents perform bounded work and return structured results. Repository changes and task artifacts are persisted before a phase is considered complete.

## Durable task record

Each long-running task should have a durable record with at least:

- Stable task ID.
- User objective and acceptance criteria.
- Current workflow phase and status.
- Parent/child task relationships.
- Relevant repository revision or workspace identity.
- Decisions made and alternatives rejected.
- Known risks, blockers, and unresolved questions.
- Artifacts produced by each phase.
- Commands run and their results.
- Last successful checkpoint.
- Next recommended action.

The record may be stored in a database, but human-readable Markdown artifacts should remain available for inspection and recovery.

## Suggested artifact layout

```text
.relay/tasks/<task-id>/
  objective.md       # User intent and definition of done
  plan.md            # Phases, dependencies, and approach
  state.json         # Machine-readable workflow state
  decisions.md       # Durable decisions and rationale
  findings.md        # Repository and domain discoveries
  progress.md        # Completed work and remaining work
  questions.md       # Questions waiting for the user
  evidence/          # Test, build, review, and command results
  handoffs/          # Context packets passed between workers
```

The repository itself remains authoritative for code. The task artifacts explain why the code is in its current state and what remains to be done.

## Workflow phases

### 1. Intake

Capture the user's objective, scope, constraints, acceptance criteria, and any required decisions. Do not begin implementation until the objective is sufficiently testable.

### 2. Planning

Produce a phased plan with dependencies, likely files, validation commands, risks, and explicit stopping conditions. Split work into units that fit comfortably within one agent context.

### 3. Exploration

Inspect the repository and record findings. Exploration should produce references to relevant files, existing patterns, interfaces, and test locations rather than leaving discoveries only in chat.

### 4. Implementation

Assign an implementer a narrow work unit. The worker should make changes, run focused checks, and update the handoff with changed files, assumptions, and remaining concerns.

### 5. Verification

Run focused tests first, then broader checks. Record exact commands and outcomes. Failed verification should create a retry or diagnostic branch, not silently advance the workflow.

### 6. Integration

Reconcile parallel work, resolve conflicts, confirm that assumptions remain consistent, and run integration-level validation.

### 7. Review

Use a separate review pass for correctness, scope, maintainability, security, and acceptance criteria. Review findings should become explicit follow-up work or documented non-blocking risks.

### 8. Completion

Mark the task complete only when every acceptance criterion has evidence and the repository is in the expected state.

## Agent contract

Every agent invocation should receive:

- Task objective and acceptance criteria.
- Current phase and assigned work unit.
- Relevant durable findings and decisions.
- Repository revision and workspace constraints.
- Files or components in scope.
- Required validation commands.
- Explicit output format.

Every invocation should return:

```text
status: complete | blocked | failed | needs_review
summary: concise description of work performed
files_changed: list of paths
artifacts_written: list of paths
evidence: commands and outcomes
decisions: new decisions or assumptions
risks: known risks or uncertainty
remaining_work: unfinished items
next_action: recommended continuation
```

An agent's natural-language claim is not sufficient evidence for completion.

## Context handoffs

Before a context ends, write a continuation packet containing:

1. The current objective.
2. What is already known.
3. What was changed.
4. What was verified.
5. What failed and why.
6. Constraints that must not be lost.
7. The single best next action.

The next agent should read the packet, inspect the current repository state, and verify that the packet matches reality before continuing. This final consistency check prevents stale summaries from becoming the source of truth.

## Temporal boundary

Temporal should own durable control flow, not detailed coding knowledge.

Good Temporal responsibilities:

- Start and advance task phases.
- Schedule bounded agent activities.
- Retry transient failures with limits and backoff.
- Wait for user responses through signals.
- Run independent exploration or validation branches in parallel.
- Enforce timeouts and escalation policies.
- Record workflow-level history.

Agent activities should own:

- Repository inspection.
- Code changes.
- Test execution.
- Review and diagnosis.
- Writing task artifacts and evidence.

Activities must be designed with idempotency in mind. A retry must not blindly duplicate commits, migrations, external messages, or other side effects.

## State transitions

Use explicit states such as:

```text
CREATED
  -> PLANNING
  -> EXPLORING
  -> IMPLEMENTING
  -> VERIFYING
  -> INTEGRATING
  -> REVIEWING
  -> COMPLETE

Any active state may transition to:
  BLOCKED
  WAITING_FOR_USER
  FAILED
  CANCELLED
```

Each transition should record who or what caused it, the checkpoint revision, supporting evidence, and the next permitted transition.

## Human-in-the-loop behavior

When a meaningful decision is required, pause in a durable `WAITING_FOR_USER` state. Store:

- The question.
- The available options.
- The recommendation.
- The impact of each option.
- What work is safe to continue in parallel.

Do not leave a worker running while waiting for a user response. Resume the workflow from the persisted decision after the user responds.

## Parallelism policy

Parallelize independent investigation, test generation, and isolated component work. Serialize changes to shared interfaces, architectural files, migrations, and release configuration.

Every parallel branch needs:

- A clearly defined scope.
- A branch-specific output artifact.
- An integration owner.
- A conflict-resolution phase.

More agents do not necessarily mean faster completion; integration cost should be treated as part of the plan.

## Completion gates

Do not advance to completion until the workflow can answer yes to the applicable gates:

- Is every acceptance criterion checked?
- Are the relevant tests passing?
- Does the build or type check pass?
- Were changed files reviewed for unintended scope?
- Are known risks documented?
- Is the task state consistent with the repository?
- Is there a clear final artifact or commit for the user?

## Failure and recovery

Recovery should distinguish among:

- **Transient failure:** retry the same activity.
- **Context exhaustion:** create a new continuation packet and invoke a fresh worker.
- **Repository conflict:** pause and run an integration activity.
- **Bad plan:** return to planning with findings preserved.
- **Missing user decision:** wait for a signal.
- **Repeated failure:** escalate with evidence instead of retrying indefinitely.

The orchestrator should preserve failed attempts as evidence. Deleting failure history makes future agents repeat the same investigation.

## Rollout strategy

### Stage 1: Artifact-backed continuation

Implement the task and handoff artifact conventions without introducing an orchestration service. Prove that a fresh agent can resume a task from the artifacts.

Completed:

- New tasks now initialize `.relay/tasks/<task-id>/` with `objective.md`, `plan.md`, `state.json`, `decisions.md`, `findings.md`, `progress.md`, `questions.md`, and empty `evidence/` and `handoffs/` directories so continuation context has durable repository-local artifacts from the start.
- Terminal-backed agent runs now write `.rly/terminal/run-*/state.json` with command, workspace, lifecycle paths, status, session ID, exit code, error, and timestamps. Terminal lifecycle progress events include the `state_path`, giving follow-on workers and diagnostics a durable machine-readable artifact for each terminal-owned run.

### Stage 2: Explicit state machine

Add machine-readable phases, completion gates, retry limits, and structured agent results.

Completed:

- Task `state.json` now uses schema version 2 with explicit workflow phase/status, durable completion gates, retry limits/counts, structured agent results, checkpoint, and next-action fields.
- New task artifacts initialize the Stage 2 state machine when `.relay/tasks/<task-id>/` is created.
- Agent runs append structured planner/implementer results to the task artifact, including status, session, exit code, usage, response summary, timing, and orchestration metadata.
- Failed agent results increment role-specific retry counters and preserve retry-oriented next-action guidance.
- Task state transitions update artifact phase/status and terminal-state gates so continuation workers can resume from machine-readable state.

### Stage 3: Durable orchestration

Introduce Temporal when tasks need process-independent retries, long waits, timers, parallel branches, or reliable recovery from worker failure.

In progress:

- Task artifacts now persist an `orchestration` decision in `state.json`, defaulting bounded single-process work to the local coordinator.
- The service records an `orchestration.selected` event before agent execution so recovery workers can see which backend owns the workflow lifecycle.
- The core orchestration assessor selects Temporal for task graphs or hints that require process-independent retries, long waits, timers, human-input signals, parallel branches, or reliable recovery from worker failure.

### Stage 4: Operational hardening

Add observability, cost limits, cancellation, access controls, idempotency checks, and dashboards for blocked or aging tasks.

In progress:

- Task `state.json` now uses schema version 3 with an `operations` section for observability counters, task cost limits, cancellation metadata, cancellation access controls, and idempotency records.
- `rly run --max-total-tokens` records a task-level token limit and stops before launch when estimated usage would exceed the configured cap; planner-to-executor flows pass the limit into the orchestrator token budget.
- `rly cancel` provides actor-checked, idempotency-key-aware cancellation and records durable cancellation metadata in task artifacts.
- `rly ops` surfaces blocked, failed, waiting, and aging non-terminal tasks for operational review.

This sequence validates the core model before taking on Temporal's operational complexity.

## Key metrics

- Percentage of tasks resumed successfully after context replacement.
- Number of agent invocations per completed task.
- Time spent waiting for user input.
- Retry rate by activity type.
- Percentage of work units completed with evidence.
- Repeated-failure rate.
- Integration conflict rate.
- Cost and latency per completed task.
- Tasks stuck in each workflow state.

## Open design questions

- Where should task artifacts live: repository, database, object storage, or a hybrid?
- Should each task use an isolated workspace or branch?
- What is the maximum retry budget for each worker type?
- Which operations require human approval?
- How should secrets and external service access be scoped per activity?
- What is the minimum evidence required for each project type?
- When should a task be automatically decomposed into child workflows?

## Final principle

The durable unit is the workflow, not the agent session. Context windows become an implementation detail: each worker does a bounded amount of work, records durable evidence, and hands control back to the workflow so another worker can continue safely.
