# Code Improvement Suggestions

Scope: full review of the Go codebase (`cmd/rly`, `internal/cli`, `internal/core`, `internal/agents/*`, `internal/store`, `internal/model`) as of 2026-09-23. Suggestions are ordered roughly by priority within each section. File references are to the current tree.

## 1. Structure and duplication

**1.1 Split `internal/cli/app.go` (888 lines) and `internal/core/service.go` (680 lines).**
`App.executeObjectiveWithRouter` is a single ~250-line function containing routing, phase banners, two independent retry/fallback paths, and JSON vs. plain output interleaved. Extract at least:
- `routePlanner(...)` and `routeExecutor(...)` returning `(adapter, model, decision, error)`.
- `retryWithFallback(current func() (*core.Execution, error), ...)` for the two nearly identical recovery blocks (planner retry and executor retry share the filter → re-route → re-run shape).
This would make the fallback logic unit-testable without driving the whole CLI.

**1.2 Factor the repeated run-recording pipeline in `service.go`.**
`Plan`, `Execute`, and `ExecuteWithPlan` each re-implement the same sequence: `Detect` → unavailable event → load memory → started event → run → sanitize memory block → `RecordRun` → quota-exhausted event → transition. Extract helpers:
- `usageMap(agents.Usage) map[string]any` (built inline three times with identical keys).
- `recordUnavailable(ctx, taskID, adapter, installation)`.
- `recordQuotaExhausted(ctx, taskID, adapter, err)`.
- `finishRun(ctx, task, adapter, result, started, role)` for RecordRun + status transition.
This removes ~100 lines and eliminates the risk of the three paths drifting apart (they already differ subtly: `Plan` records `planner_model` differently than `ExecuteWithPlan`).

**1.3 Single source for the 7-day retention constant.**
`7 * 24 * time.Hour` is defined three times: `core.retentionPeriod`, `freebuff.maxChannelAge`, and `cli.freebuffMaxChannelAge` (which exists only to print a message). Have the CLI import the constant from `core` or `freebuff`, or add a shared `internal/retention` package; today a change to one silently desyncs the others and the human-readable "7 days" strings.

**1.4 Deduplicate event-collection logic in `internal/agents/process.go`.**
`collectProcess` and `readTerminalEvents` duplicate the same normalization/state machine (session capture, last-message capture, error-termination, result/usage capture). Extract `applyEvent(result *Result, event Event) (sawResult bool)` so a fix to one path (e.g., preserving an earlier provider error after a terminal result) cannot be missed in the other.

**1.5 Redundant double sanitization of planner output.**
`ExecuteWithPlan` wraps the planner in `sanitizedPlannerAdapter` *and* re-sanitizes every `EventMessage` inside the `OnEvent` callback. `parseMemoryUpdate` is idempotent so it is harmless, but pick one layer (the adapter wrapper is cleaner) and delete the other to reduce per-event work and confusion.

## 2. Correctness and robustness

**2.1 Event channels can deadlock if the consumer stops reading.**
In `collectProcess` (`process.go`), `freebuff.run.loop`, and `sanitizedPlannerRun.Events`, `events <- event` is a blocking send on a 32-buffered channel with no `select` on `ctx.Done()`. If the orchestrator's `OnEvent` (or a CLI that only calls `Wait()`) stops draining, the producer goroutine blocks forever and `Wait()` never returns; cancellation also cannot unblock the send. Guard every send with:
```go
select {
case events <- event:
case <-ctx.Done():
    // drain-and-exit
}
```
This is the highest-value robustness fix in the process layer.

**2.2 `killTerminalPID` uses a fixed 100ms sleep instead of waiting for exit.**
After `kill -TERM`, sleep, then unconditionally `kill -KILL`. Prefer polling the pid (or the `exit` file) with a short deadline before escalating, so fast-exiting agents are not killed mid-cleanup and slow ones get the full grace period.

**2.3 Ignored time-parse errors in `store.scanTask`/`Events`/`ProjectMemory`.**
`t.CreatedAt, _ = time.Parse(...)` silently yields zero timestamps on malformed rows. Since `stamp()` writes RFC3339Nano consistently, a parse failure indicates corruption; returning the error (or at least logging once) is safer than showing `0001-01-01`.

**2.4 "429" as a quota-exhaustion substring is over-broad.**
`core.IsQuotaExhaustedMessage` matches `429` anywhere in an error string; a path like `/tmp/1429cache` or a version string would false-positive and trigger recovery re-routing. Match `429` only with token boundaries (e.g., `\b429\b`) or when adjacent to `HTTP`/`status`.

**2.5 `ExecuteWithPlan` planner-failure path skips executor detection.**
Executor `Detect` happens up front (good), but when the planner fails and the CLI retries with a fallback planner, the *new* planner is never re-detected before `runExecution` is invoked a second time. `Service.Plan`/`ExecuteWithPlan` do re-detect, so this is only an inconsistency risk, but wiring the fallback through the same detect path would keep behavior uniform.

**2.6 REPL discards the execution exit code.**
`_ = a.executeObjectiveWithRouter(...)` in `repl` ignores the returned exit code. Printing `✗` / a hint on `ExitAgentUnavailable` in the REPL (as the non-REPL path does on stderr) would make silent agent-unavailable failures visible.

**2.7 `rly help` opens the database and runs cleanup.**
`App.Run` opens SQLite and calls `a.cleanup` before dispatching, so even `rly help` pays the cost (and can print cleanup lines to stderr). Dispatch a pure help path before `store.Open`.

## 3. Store and data model

**3.1 Consider relaxing `SetMaxOpenConns(1)`.**
The comment says single-connection is deliberate for the MVP, but WAL mode is already enabled, which supports concurrent readers with a single writer. `SetMaxOpenConns(4)` plus `PRAGMA busy_timeout=5000` would remove a serialization bottleneck for parallel orchestration (`Orchestrator` fan-out + `AppendEvent` from `OnEvent` callbacks) while keeping writes safe. Also set `PRAGMA synchronous=NORMAL` (standard for WAL) to cut fsync cost.

**3.2 `nextSequence` does `MAX(sequence)+1` per insert.**
With concurrent writers this is a classic lost-update pattern. With one connection it is safe, but if 3.1 is adopted, switch to `INSERT ... SELECT COALESCE(MAX(sequence),0)+1 ...` inside the same transaction (already the case) *and* keep the `UNIQUE(task_id, sequence)` constraint as the backstop — worth an explicit comment, or a dedicated per-task counter table.

**3.3 `PruneOldRuns` table-name interpolation.**
`DELETE FROM ` + table` uses a fixed literal slice, so it is safe today; a small hardening step is to keep the whitelist as a package-level `var` with a comment, or use three explicit statements, so a future refactor cannot inject an arbitrary name.

**3.4 Missing index for the prune query.**
`PruneOldRuns` scans `tasks WHERE updated_at < ?`; the existing index is `(repository, updated_at DESC)`. A single-column index on `updated_at` (or reusing the primary scan) avoids a full table scan once history grows.

## 4. Router vendor-neutrality

**4.1 Remove `freebuff` name checks from the core.**
`evaluateQuota` special-cases `agent == "freebuff"` inside `internal/core/router.go`, and `planningCandidates` / `filterFailedModel` hard-code `"freebuff"` in `internal/cli/app.go`. The core is documented as vendor-neutral. Introduce an adapter capability, e.g. `QuotaVisibility: "none" | "measured"` (or `PublishesCatalogWithoutQuota bool`) on `agents.Capabilities`, and let the router/CLI branch on that. Third-party adapters without quota APIs then get the same treatment for free.

**4.2 `evaluateEfficacy` hard-codes score bonuses per agent-name substring.**
`strings.Contains(agentLower, "codex")` etc. bakes vendor scores into core. Options, in increasing effort: (a) move the table into `RoutingPolicy` so it is configurable; (b) derive scores from `Capabilities` (StructuredOutput, FileEditing, Streaming) with small vendor tweaks layered on top. This also removes the awkward "contains agy" matching that any adapter with "agy" in its name would inherit.

**4.3 Make the token-estimate heuristic explicit.**
`estimatePromptUsage` assumes ~4 chars/token, which is only used for budget gating. Document the assumption at the call sites in the README/spec (users comparing budget numbers to provider-reported usage will otherwise be confused), and consider applying the same estimate to handoff content added by `attachDependencyContext` (it currently estimates the post-attachment prompt — good — but the budget ledger then double-counts the base prompt on retries).

## 5. CLI ergonomics

**5.1 Deduplicate the JSON/plain error-output pairs.**
Many sites do:
```go
if jsonOut { _ = a.json(runOutput{..., Error: ...}) }
fmt.Fprintln(a.Err, err)
return ExitX
```
Extract `a.failRun(runOutput, err, exitCode)` so error payloads stay consistent between modes (today the plain path sometimes prints to `Out` first, sometimes not).

**5.2 Document `--min-reserve` and strategies in `rly help`.**
The run flags exist (`--strategy`, `--min-reserve`) but the help text (see `a.help`) should enumerate the three strategies and what the reserve threshold does; this is the most configuration-heavy surface of the CLI.

**5.3 Consider `--yes`/quiet modes for cleanup noise.**
Cleanup messages go to stderr on *every* invocation ("pruned N task(s)"). Once a repository ages past 7 days, every run prints a line. Consider printing only on the first pruning after a change, or gating behind a `--verbose` flag, keeping the current behavior as default-safe.

## 6. Freebuff adapter

**6.1 Constants for the polling intervals.**
`ticker := time.NewTicker(400 * time.Millisecond)` (adapter), `100ms` (terminal tailing), `15s` heartbeat, and the readiness timeouts are scattered; group them in one `const` block with comments so tuning the PTY latency does not require hunting.

**6.2 Bound `trace.log` growth for long runs.**
`channel.trace` appends indefinitely (heartbeats every 15s, pane snapshots). For a 45-minute max-runtime run this is fine, but a runaway heartbeat with large `diagnostic` payloads could grow it; consider trimming the pane snapshot to the last N bytes at trace time (there is already `clipDiagnostic` — apply it consistently, including to `file_state`).

**6.3 `Resume` returns a descriptive error — surface it in routing.**
`freebuff.Resume` always fails, and `Capabilities.SessionResume=false` encodes that. If a future orchestration feature resumes sessions, ensure the router consults the capability rather than the error string; a unit test pinning that behavior would be cheap insurance.

## 7. Testing and CI

**7.1 No CI workflow exists.**
Add a minimal GitHub Actions workflow: `go build ./...`, `go vet ./...`, `go test ./...` on macOS and Linux (terminal-backed code paths are darwin-specific and worth at least compile-checking on both). The repo has 14 test files covering adapters, core, and store — a CI gate would keep them meaningful.

**7.2 Add golangci-lint (or at least `go vet` in CI) with `errcheck` scoped carefully.**
The codebase intentionally ignores many `AppendEvent` errors (`_ =`). That is a defensible trace-only decision, but it should be explicit: either a lint exception with a comment, or a tiny `s.logEvent(...)` helper that centralizes the ignore decision.

**7.3 Table-driven tests for `parseMemoryUpdate` edge cases.**
Cover: memory block at start/end, multiple blocks (only the last is used — `strings.LastIndex`), unterminated block, unknown fields, duplicate keys across upsert/delete, and whitespace-only payload. These behaviors are subtle and currently only partially exercised.

**7.4 Prompt-composition golden tests.**
`composePrompt` / `composePlanPrompt` / `composeExecutionPrompt` are the contract with every vendor agent. Golden-file tests make accidental contract changes visible in review — especially valuable since the freebuff adapter embeds the same protocol text into `prompt.md`.

**7.5 Orchestrator property test for workspace-write serialization.**
`chooseBatch` guarantees mutating tasks never share a batch. A test that generates random task graphs (read-only/mutating × dependencies) and asserts the invariant across many seeds would protect the most safety-critical scheduling rule in the project.

## 8. Documentation

**8.1 Note the terminal-mode macOS requirement in the README.**
`StartProcessInTerminal` falls back to headless on non-darwin or missing `osascript`, and `--terminal=false` disables tabs; a one-line note in the README's run section would set expectations for Linux users.

**8.2 Document the `.rly/` workspace artifacts.**
`.rly/freebuff/run-*` channels and `.rly/terminal/run-*` lifecycle directories are created inside the user's repository and pruned after 7 days. The README mentions freebuff channels; adding the terminal lifecycle dirs (and a suggestion to gitignore `.rly/`) would prevent surprise commits. `.gitignore` already covers `/.rly`, so recommend the same to downstream users of installed binaries.

## Quick-win summary

| # | Change | Effort | Impact |
|---|--------|--------|--------|
| 2.1 | Non-blocking event sends with ctx.Done | Small | Prevents hangs under cancellation |
| 1.3 | Single retention constant | Trivial | Prevents config drift |
| 2.4 | Word-boundary "429" match | Trivial | Avoids false quota routing |
| 3.1 | WAL + busy_timeout + more conns | Small | Unlocks orchestrator parallelism |
| 4.1 | Capability-based vendor branches | Medium | Restores core vendor-neutrality |
| 1.2 | Extract run-recording helpers | Medium | Cuts ~100 duplicated lines |
| 7.1 | CI workflow | Small | Keeps 14 test files green |
