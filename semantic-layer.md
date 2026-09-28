# Relay semantic layer

This document describes the semantic layer as it exists in the repository today.
Last checked against the working-tree implementation: 2026-09-27.
It is an explainable classification step around routing and orchestration; it is
not an LLM call, an embedding search service, or an authorization system.

The short version is: Relay converts `(role, objective)` into a
provider-neutral profile, uses that profile as one input to agent scoring, and
uses a small subset of the profile to decide whether the task graph needs
durable orchestration. The classifier is intentionally local and predictable;
it is a routing hint, not a second agent.

## Current status at a glance

| Question | Current answer |
| --- | --- |
| Where does it run? | In process, in `internal/core`; no network or subprocess is involved. |
| What is the default backend? | Deterministic substring rules plus one best-match lexical example, reported as `local-lexical`. |
| What does it return? | A provider-neutral `model.SemanticProfile`. |
| What consumes the profile? | Router efficacy scoring, trace events, and the durable-orchestration assessment used by the planner/executor path. |
| Does it choose an agent by itself? | No. It adjusts one part of candidate scoring; installation, policy, quota, session affinity, and configured weights still apply. |
| Does it enforce safety? | No. Risks and `needs_review` are annotations and scoring signals, not authorization checks. |
| Does it start Temporal? | No. It can persist a decision naming `temporal`, but execution is still local because no Temporal worker/backend is integrated. |
| Does runtime event classification affect execution? | Not currently. `ObserveEvent` exists and is tested, but production service code does not call it. |

## What it does

Relay receives a role and a free-form objective. The semantic layer turns those
inputs into a provider-neutral `model.SemanticProfile`. The router uses that
profile together with adapter capabilities, quota, session affinity, and the
routing policy. The service records the profile in the task trace. The service
also converts selected profile fields into hints for choosing local versus
durable orchestration.

The default implementation is deliberately low-spec and local:

1. Deterministic substring rules classify the text.
2. A small built-in set of semantic examples is compared with lexical token
   overlap.
3. The best matching example is merged additively into the rule-based profile.

There is currently no network request, model download, embedding index, BERT
classifier, or micro-LLM in this path.

### The live request lifecycle

For a normal routed request, the current call sequence is:

1. `core.Route` calls `AnalyzeObjective(role, objective)`.
2. `AnalyzeObjective` creates the default local semantic layer and analyzes the
   combined role/objective text.
3. Deterministic rules produce the initial profile.
4. The lexical wrapper finds at most one matching built-in example and merges
   its labels when the score is at least `0.18`.
5. The router evaluates every eligible adapter. Semantic capability fit,
   review support, and session-resume support adjust efficacy; normal quota,
   policy, session, reliability, cost, and agent-weight logic still applies.
6. With a task ID, the service records `semantic.assessed` and then
   `routing.selected` after a successful route.

The planner/executor path performs a separate analysis using the
`implementer` role to derive durable-orchestration hints. It does not reuse or
cache the routing profile. This is why a trace can contain routing semantics
and an orchestration decision that were produced by two different calls.

The runtime-event side of the interface is separate: `ObserveEvent` can map a
single normalized agent event to a signal such as `quota-pressure` or
`verification-failed`, but the current service does not invoke it for every
event and does not persist a `semantic.observed` event automatically.

## End-to-end flow

```text
role + objective
      |
      v
core.AnalyzeObjective / core.Route
      |
      v
DefaultSemanticLayer()
      |
      v
LocalSemanticLayer.AnalyzeTask
      |
      +--> DeterministicSemanticLayer
      |      task kind, domains, operations, risks, signals,
      |      mutation, long-running, review, capabilities
      |
      +--> bestSemanticExample
             tokenization + weighted lexical overlap
             merge when score >= 0.18
      |
      v
model.SemanticProfile
      |
      +--> candidate efficacy and semantic capability fit
      +--> routing.selected and semantic.assessed trace data
      +--> semanticDurableHints
                |
                v
          local or Temporal orchestration decision
```

The semantic layer itself is side-effect free. It does not route, start an
agent, edit files, write task artifacts, or approve a risky operation.

There are two separate semantic operations:

- **Task analysis** turns a role and objective into a `SemanticProfile`. This is
  wired into routing and durable-orchestration assessment.
- **Event observation** turns one normalized agent event into a
  `SemanticObservation`. This is an available contract today, but it is not yet
  wired into the service event stream.

## Code map

| File | Responsibility |
| --- | --- |
| `internal/core/semantic.go` | Interface, default implementation, rules, examples, event observations, lexical scoring |
| `internal/model/model.go` | Serialized `SemanticProfile` and `RouteDecision` types |
| `internal/core/router.go` | Creates the profile and applies semantic effects to candidate scoring |
| `internal/core/service.go` | Persists `semantic.assessed` and `routing.selected`; applies semantic orchestration hints in the plan/execute path |
| `internal/core/durable_orchestration.go` | Converts profile fields into durable-orchestration requirements |
| `internal/core/semantic_test.go` | Classifier, example merge, capability-fit, and event-observation coverage |

## Stable contract

```go
type SemanticLayer interface {
	AnalyzeTask(context.Context, SemanticTaskInput) (model.SemanticProfile, error)
	ObserveEvent(context.Context, agents.Event) SemanticObservation
}
```

Inputs are intentionally small:

```go
type SemanticTaskInput struct {
	Role      string
	Objective string
}
```

`AnalyzeTask` is for task semantics. `ObserveEvent` is for classifying runtime
events such as quota errors or failed verification. A future embedding,
classifier, or local-model implementation can satisfy this interface without
changing the router contract.

The interface is an extension seam, not runtime dependency injection yet.
`Route` and the planner/executor path call the package helper
`AnalyzeObjective`, which constructs `DefaultSemanticLayer()` on every call.
Using a different implementation therefore currently requires changing that
factory/helper or adding injection at the service/router boundary.

## Profile fields

```go
type SemanticProfile struct {
	Backend              string   `json:"backend,omitempty"`
	TaskKind             string   `json:"task_kind"`
	Domains              []string `json:"domains,omitempty"`
	Operations           []string `json:"operations,omitempty"`
	Risks                []string `json:"risks,omitempty"`
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`
	Signals              []string `json:"signals,omitempty"`
	Mutation             bool     `json:"mutation"`
	LongRunning          bool     `json:"long_running"`
	NeedsReview          bool     `json:"needs_review"`
	Confidence           float64  `json:"confidence"`
}
```

The fields mean:

- `backend`: `local-lexical` for the default wrapper; the inner rules layer
  starts as `deterministic` before the wrapper labels the result.
- `task_kind`: one broad category such as `design`, `implementation`, `debug`,
  `review`, `monitoring`, or `classification`.
- `domains`: subject areas such as `storage`, `monitoring`, `security`,
  `testing`, `cli`, `concurrency`, or `semantic-routing`.
- `operations`: requested work such as `design`, `implement`, `debug`,
  `review`, `refactor`, `monitor`, `classify`, or `orchestrate`.
- `risks`: currently `data-loss`, `security`, `cost`, `concurrency`, and
  `routing-quality`.
- `required_capabilities`: `planning`, `file-editing`, `structured-output`,
  `session-resume`, `cancellation`, and `long-context`.
- `signals`: expected conditions such as `blocked`, `quota-pressure`,
  `verification-failed`, or `completion-without-evidence`.
- `mutation`: whether the objective suggests changing files or state.
- `long_running`: whether the text suggests durable/background work.
- `needs_review`: whether risk or explicit approval/review language is present.
- `confidence`: a heuristic classifier confidence, not a calibrated probability.

Lists are deduplicated and sorted, so trace output is stable.

## What each result can change today

The profile is intentionally broader than the effects currently wired into the
runtime. The following table separates labels Relay records from labels that
alter a decision today. This is useful when a profile looks sensible but the
selected agent or orchestration backend is not what was expected.

| Profile result | Produced by | Current runtime effect | Not a current effect |
| --- | --- | --- | --- |
| `task_kind`, `domains`, `operations`, `risks` | Rules and, when matched, the best lexical example | Included in the route decision and candidate `details` for explanation | No hard eligibility rule and no direct agent selection |
| `required_capabilities` | Rules plus example merge | Computes semantic capability fit; a partial fit can lower role efficacy | Does not reject an otherwise eligible adapter |
| `needs_review` | Any inferred risk or review-like wording; example merge | Adds `0.02` efficacy when an adapter supports structured output | Does not require approval, human review, or a special reviewer |
| `long_running` | Durable/workflow-like wording or example merge | Adds durable orchestration hints; deducts `0.04` efficacy from adapters without session resume | Does not start a Temporal workflow |
| `signals=blocked` or `quota-pressure` | Objective rules or example merge | Adds waiting/retry requirements to durable orchestration assessment | Does not pause the task or reroute a running agent |
| `mutation` | Mutation-like wording or example merge | Contributes to `file-editing` being required | Does not make a write or grant filesystem permission |
| `confidence` | Rule-label density and lexical-match thresholds | Recorded for interpretation | Is not a probability, ranking weight, or eligibility threshold |

There is one important distinction in the `required_capabilities` row:
`file-editing`, `structured-output`, `session-resume`, and `cancellation` are
checked against adapter capability flags. `planning` and `long-context` are
currently counted as satisfied for every adapter, so they express task intent
without differentiating candidates.

## Step 1: deterministic classification

`DeterministicSemanticLayer.AnalyzeTask` lowercases `role + " " + objective`
and checks fixed vocabularies with substring matching. For example:

| Profile output | Example triggers |
| --- | --- |
| `task_kind=design` | planner role, `design`, `architect`, `blueprint`, `proposal` |
| `task_kind=debug` | debugger role, `debug`, `fix`, `broken`, `panic` |
| `task_kind=monitoring` | `monitor`, `observe`, `trace` |
| `task_kind=classification` | `classify`, `semantic`, `embedding`, `bert` |
| `mutation=true` | `implement`, `add`, `build`, `create`, `edit`, `fix`, `refactor`, `change` |
| `long_running=true` | `long-running`, `durable`, `workflow`, `orchestration`, `temporal`, `background`, `daemon`, `queue`, `scheduler`, `resume`, `checkpoint` |
| `needs_review=true` | any risk, or `review`, `approval`, `high-risk`, `production` |

Domains, operations, risks, and signals use similar fixed vocabularies. This
means the behavior is local, auditable, deterministic, and easy to test, but it
also means synonyms that are not in the vocabularies may be missed.

Task-kind precedence is important. The first matching branch wins:

1. planner role or design language -> `design`;
2. reviewer role or review language -> `review`;
3. debugger role or debug language -> `debug`;
4. monitor/observe/trace language -> `monitoring`;
5. classify/semantic/embedding/BERT language -> `classification`;
6. otherwise -> `implementation`.

Roles are compared to the lowercase constants (`planner`, `reviewer`, and
`debugger`) exactly. Objective matching is lowercased, but it is substring
matching rather than word-boundary matching. For example, adding a new short
trigger should be done carefully because it may also match inside an unrelated
word.

The role is not lowercased before these exact comparisons. In practice, callers
should use the exported role constants. A role value such as `Planner` does not
get the planning-role shortcut, although planning language in the objective can
still classify the task as `design`.

Required capabilities are inferred from the profile and text:

- planning role or design task -> `planning`;
- mutation or implementation language -> `file-editing`;
- long-running work -> `session-resume`, `cancellation`, `long-context`;
- JSON, schemas, typed output, traces, monitoring, or deterministic behavior ->
  `structured-output`.

### Role text and substring surprises

Both the rules and lexical matcher inspect `role + " " + objective`. This has
observable consequences beyond the explicit role-to-task-kind checks:

- `implementer` contains `implement`, so every analysis using
  `RoleImplementation` starts with `mutation=true`, operation `implement`, and
  capability `file-editing`, even for an empty or explanatory objective.
- `trace` contains `race`, so a trace-related objective also matches the
  concurrency domain and risk.
- Negation is not interpreted. `Do not delete the database` still produces
  `data-loss` and `needs_review=true`.

For example, with role `implementer` and objective `Explain the trace output`,
the current default returns:

```json
{
  "backend": "local-lexical",
  "task_kind": "monitoring",
  "domains": ["concurrency", "monitoring", "testing"],
  "operations": ["debug", "implement", "monitor"],
  "risks": ["concurrency", "cost", "routing-quality"],
  "required_capabilities": ["file-editing", "structured-output"],
  "signals": ["completion-without-evidence", "quota-pressure", "verification-failed"],
  "mutation": true,
  "long_running": false,
  "needs_review": true,
  "confidence": 0.84
}
```

Here `trace` also matches the telemetry example. Its one shared token scores
`0.65 × 1/4 + 0.35 × 1/14 = 0.1875`, just above the merge threshold. That
single match adds the example's testing domain, debug operation, risks, and
all three signals. Those signals describe a text classification; they do not
prove that quota was exhausted or that verification failed. This profile even
adds a retry requirement in `ExecuteWithPlan`, despite `long_running=false`,
because `quota-pressure` independently creates that requirement.

## Step 2: local lexical example matching

`DefaultSemanticLayer` returns a `LocalSemanticLayer` populated by
`defaultSemanticExamples()`. The examples cover routing/model selection,
stalled workflows, trace telemetry, SQLite migration safety, and security
review.

Matching is not semantic vector similarity. It is a small weighted token-overlap
heuristic:

```text
tokens = lowercase text split on non-letters/digits
remove tokens shorter than 3 characters and the semantic stop-word list

precision = overlap / query-token-count
recall    = overlap / example-token-count
score     = (0.65 * precision) + (0.35 * recall)
```

The best example is used. If its score is below `0.18`, no example is merged.
At `0.18` or above, its labels, capabilities, signals, and boolean flags are
merged additively. An example never removes a rule-derived label. Its
`TaskKind` replaces the default `implementation` only. Confidence is raised to
at least `0.80`, or at least `0.88` when the score is `0.35` or higher.

The wrapper sets `Backend` to `local-lexical` even when no example reaches the
merge threshold. Thus the backend identifies the implementation, not whether a
particular example matched.

An empty role and empty or stop-word-only objective are valid: they receive
the deterministic fallback profile (`task_kind=implementation`, confidence
`0.72`) and no example merge. A nonempty role can still add labels even when
the objective is empty; see the role-matching caveat above. The layer returns
no classification error for normal input; the error in the interface is reserved
for a future backend that can fail.

### The five built-in examples

The default examples are not repository data or learned history. They are
hard-coded in `defaultSemanticExamples()`:

| Scenario | Important labels it can add |
| --- | --- |
| Route a coding task using quota, capability, and session affinity | classification, agent orchestration, semantic routing, structured output, routing-quality review |
| Detect a stalled workflow and resume after restart | monitoring/orchestration, blocked signal, durable capabilities, long-running/review |
| Trace quota errors, failed tests, and missing evidence | monitoring/testing, debug, quota and verification signals, cost/routing risks |
| Change SQLite migration/retention without losing memory | storage implementation, mutation, file editing, structured output, data-loss review |
| Inspect auth/sandbox/secrets before mutation | security review, security/data-loss risks, structured output |

Only the single highest-scoring example is merged. Ties keep the earlier
example because replacement happens only for a strictly greater score.

### Merge semantics

Example matching is intentionally additive:

- list fields are unioned, deduplicated, and sorted;
- booleans use logical OR, so an example can turn a flag on but not off;
- an example task kind replaces only the rule layer's default
  `implementation` kind;
- confidence can only increase;
- backend is always `local-lexical` for the wrapper.

The profile does not expose the matched example or its raw overlap score, so a
trace explains the resulting labels but not which example produced them.

## Worked examples

### Example 1: semantic routing design

```text
role: planner
objective: Come up with a semantic layer for intelligent routing and monitoring using BERT or a local micro LLM
```

The rule layer identifies a design task, semantic routing, monitoring,
classification/design/monitor operations, routing-quality risk, planning, and
structured output. The default backend returns this profile:

```json
{
  "backend": "local-lexical",
  "task_kind": "design",
  "domains": ["agent-orchestration", "monitoring", "semantic-routing"],
  "operations": ["classify", "design", "monitor"],
  "risks": ["routing-quality"],
  "required_capabilities": ["planning", "structured-output"],
  "mutation": false,
  "long_running": false,
  "needs_review": true,
  "confidence": 0.84
}
```

The profile describes the work; it does not itself select an agent.

### Example 2: stalled workflow watchdog

```text
role: implementer
objective: add a watchdog for stalled workflow resume checkpoints
```

Rules detect a mutation, monitoring, implementation, workflow/resume language,
and durable capabilities. With the default examples, lexical matching adds
`monitor`, `orchestrate`, `blocked`, review, and the remaining durable
capabilities from the stalled
workflow example:

```json
{
  "backend": "local-lexical",
  "task_kind": "implementation",
  "domains": ["agent-orchestration", "monitoring"],
  "operations": ["implement", "monitor", "orchestrate"],
  "signals": ["blocked"],
  "required_capabilities": [
    "cancellation", "file-editing", "long-context", "session-resume", "structured-output"
  ],
  "mutation": true,
  "long_running": true,
  "needs_review": true,
  "confidence": 0.88
}
```

Why the example merges: after stop-word removal, the query and example share
`stalled`, `workflow`, and `resume`. With 7 query tokens and 10 example tokens,
the approximate score is:

```text
(0.65 * 3/7) + (0.35 * 3/10) = 0.384
```

That exceeds both the merge threshold (`0.18`) and the high-confidence
threshold (`0.35`). Token matching is exact: `checkpoint` and `checkpoints`
are different tokens because there is no stemming.

### Example 3: storage migration

```text
role: implementer
objective: change sqlite schema migration cleanup without losing durable task memory
```

This produces storage and implementation labels, `mutation=true`, a data-loss
risk, review, file editing, and durable-work capabilities because `durable` is
also a long-running trigger. The migration example can add structured output
and reinforces the data-loss/review interpretation. It does not grant or deny
permission to perform the migration.

The exact default profile for this input is:

```json
{
  "backend": "local-lexical",
  "task_kind": "implementation",
  "domains": ["storage"],
  "operations": ["implement", "refactor"],
  "risks": ["data-loss"],
  "required_capabilities": [
    "cancellation", "file-editing", "long-context", "session-resume", "structured-output"
  ],
  "mutation": true,
  "long_running": true,
  "needs_review": true,
  "confidence": 0.88
}
```

Here `cleanup` also triggers the `refactor` operation. In the
planner/executor path, `long_running=true` supplies retry and worker-recovery
hints, so the stored backend decision is `temporal`; execution still uses the
local orchestrator today.

### Example 4: event observation

```go
obs := DeterministicSemanticLayer{}.ObserveEvent(ctx, agents.Event{
	Kind:    agents.EventError,
	Message: "429 rate limit: model at capacity",
})
```

Result:

```json
{
  "signal": "quota-pressure",
  "severity": "warning",
  "summary": "agent reported quota or capacity pressure",
  "suggested_action": "reroute to an eligible model with quota headroom",
  "confidence": 0.9
}
```

The other current observations are `agent-error`, `blocked`,
`verification-failed`, and `completion-without-evidence`. `ObserveEvent` is
available through the contract, but the service does not currently persist a
first-class `semantic.observed` trace event for every observation.

Observation checks `event.Type + " " + event.Message`, lowercased. Precedence
also matters: a quota-shaped error is classified as `quota-pressure` before the
generic `agent-error` branch. Any other error event becomes `agent-error` even
if its message also contains blocked or failed-test language. Non-error events
are then checked for blocked, verification-failed, and finally
completion-without-evidence language.

## How routing uses the profile

`core.Route` calls `AnalyzeObjective` once, then evaluates every configured
adapter. Semantic data is one input among the normal routing inputs:

1. Adapter availability and policy requirements are checked.
2. Quota eligibility and model selection are evaluated.
3. Role-specific efficacy is adjusted by semantic capability fit.
4. Session affinity and the selected routing strategy contribute to the final
   score.

For an adapter, `semanticCapabilityFit` is the fraction of required
capabilities it supports. A partial fit reduces efficacy by up to `0.12`; a
review-relevant profile gets `+0.02` when the adapter supports structured
output; long-running work gets `-0.04` when the adapter lacks session resume.
The final candidate score still combines capability, session, quota,
reliability, cost, and configured agent weight.

The calculation is:

```text
raw score =
    capability weight * adjusted efficacy
  + session weight    * session value
  + quota weight      * quota headroom
  + reliability weight * reliability
  + cost weight       * cost fit

total score = round(raw score * configured agent weight, 3)
```

| Strategy | Capability | Session | Quota | Reliability | Cost |
| --- | ---: | ---: | ---: | ---: | ---: |
| `balanced` | 0.35 | 0.20 | 0.20 | 0.15 | 0.10 |
| `conservative` | 0.25 | 0.10 | 0.40 | 0.10 | 0.15 |
| `quality-first` | 0.70 | 0.15 | 0.05 | 0.10 | 0.00 |

The semantic layer changes **adjusted efficacy**, the capability term in that
formula. `planning` and `long-context` are currently treated as universally
satisfied in `semanticCapabilityFit`; the other four requirements map to real
adapter capability flags. Consequently, `long-context` documents intent but
does not presently distinguish candidates.

For example, if a profile requires `file-editing` and `structured-output`, an
adapter with only file editing has raw semantic fit `0.5`. That creates an
efficacy deduction of `(1 - 0.5) * 0.12 = 0.06`, before the normal strategy
weights and configured agent weight are applied. It remains eligible unless a
separate hard rule excludes it (for example, quota policy or
`RequireFileEditing`).

For a concrete balanced-strategy comparison, assume two otherwise identical
eligible adapters each have pre-semantic efficacy `0.98`, and the task requires
only those two capabilities. The adapter with both has adjusted efficacy
`0.98`; the adapter with file editing alone has `0.92`. The capability
contribution to the balanced score is therefore `0.35 × 0.98 = 0.343` versus
`0.35 × 0.92 = 0.322`, a `0.021` raw-score difference. Session, quota,
reliability, cost, and agent weight can still outweigh that difference. This
example assumes neither `needs_review` nor `long_running` adds its separate
efficacy adjustment.

Candidates that reach efficacy scoring in the returned `RouteDecision` have
a `details` map like the following. Candidates excluded earlier by installation,
file-editing policy, or quota checks have no semantic details:

```json
{
  "semantic_capability_fit": 0.8,
  "semantic_task_kind": "implementation",
  "semantic_domains": ["agent-orchestration", "monitoring"],
  "semantic_operations": ["implement", "monitor", "orchestrate"],
  "semantic_risks": []
}
```

Those semantic detail fields are present in the decision returned to callers.
The persisted `routing.selected` event stores the full task-level profile plus
a smaller candidate summary; it does not copy each candidate's `details` map.

This is a preference/scoring input, not a hard safety boundary. Hard routing
eligibility still comes from adapter availability, quota, and explicit policy.

## Trace output

When `Service.Route` has a task ID, it appends `semantic.assessed` before the
route result is returned:

```json
{
  "type": "semantic.assessed",
  "actor": "semantic",
  "summary": "semantic profile: implementation 0.88",
  "data": {"profile": {"backend": "local-lexical", "task_kind": "implementation"}}
}
```

After routing, it appends `routing.selected`, including the complete semantic
profile and candidate summaries. The full event is available through normal
task trace interfaces, including `rly trace --json <task-id>` and the JSON run
result. Execution then records the selected provider/model attempts in the task
artifact checkpoint, visible with `rly checkpoint <task-id>` or as raw
`state.json` with `rly checkpoint --json <task-id>`.

If routing finds no eligible agent, `Route` still returns a decision containing
the profile and excluded candidates along with an error. `Service.Route`
therefore records `semantic.assessed`, but returns before recording
`routing.selected`. Both event appends are best-effort: their storage errors are
ignored by this path.

## How orchestration uses the profile

`semanticDurableHints` currently maps these profile facts:

| Profile fact | Orchestration hint |
| --- | --- |
| `long_running=true` | process-independent retries and reliable worker recovery |
| signal `blocked` | human-input/waiting-for-user requirement |
| signal `quota-pressure` | process-independent retries |
| long-running `monitor` or `orchestrate` operation | reinforces worker recovery |

`AssessDurableOrchestration` selects Temporal when any such requirement exists
or when the task graph has parallel branches. Otherwise it selects the local
coordinator for bounded work. The decision is persisted before agent work
starts. In the planner-to-executor service path, the profile is analyzed from
the implementation objective, converted to hints, and passed to this decision:

```go
profile := AnalyzeObjective(RoleImplementation, objective)
hints := semanticDurableHints(profile)
decision := AssessDurableOrchestration(agentTasks, hints)
```

Semantic classification influences the orchestration backend, but it does not
directly launch Temporal or change task state by itself.

More precisely, this is currently a persisted **backend decision**, not a
backend dispatch. The repository does not integrate a Temporal worker, so even
when the decision says `temporal`, the existing in-process `Orchestrator` runs
the planner/executor graph. This distinction matters when interpreting trace
and task-state artifacts.

Task analysis happens independently in the two production paths:

- `Route` analyzes the requested routing role/objective for candidate scoring
  and routing trace data.
- `ExecuteWithPlan` analyzes the original task objective with the
  `implementer` role for orchestration hints.

These calls normally describe the same work, but they do not share or cache a
profile. The orchestration-only analysis does not itself emit
`semantic.assessed`; that event belongs to `Service.Route`.

The single-agent `Service.Execute` path records `localOrchestrationDecision()`
and does not apply semantic hints. A long-running routing profile therefore
does not imply a Temporal decision on every execution path. Also, semantic
hints never set `LongWait` or `Timer`; those are inputs supported by the broader
assessor.

For the watchdog example above, the planner/executor graph is sequential
(`plan` → `execute`), but the profile yields this orchestration decision:

```json
{
  "backend": "temporal",
  "durable": true,
  "reasons": ["task graph requires durable orchestration"],
  "requirements": [
    "process-independent retries",
    "human-input signals",
    "reliable worker recovery"
  ]
}
```

The `blocked` label from the lexical example causes `human-input signals` even
though the objective never explicitly asks for human input. The decision is
stored in task artifacts and the `orchestration.selected` event; execution
still uses the sequential local orchestrator.

## What it does not do

- It does not understand arbitrary meaning beyond substring rules and five
  built-in lexical examples.
- It does not learn from prior tasks, repository memory, or trace history.
- It does not enforce approval for `needs_review` or `risks`.
- It does not make quota decisions or replace adapter capability checks.
- It does not emit a persisted semantic observation event for every runtime
  event.
- It does not call BERT, an embedding service, or a local micro-LLM today.
- It does not include stemming, synonym expansion, phrase embeddings, negation
  handling, or calibrated probabilities.
- It does not expose the matched example or lexical score in the profile.
- It does not inject a selectable semantic backend through `Service` or
  `RoutingPolicy` today.

## Extending it safely

1. Add a deterministic vocabulary entry when behavior should be obvious,
   auditable, and stable.
2. Add a `SemanticExample` when a scenario needs several labels or is better
   represented by a phrase.
3. Give any new profile field a clear operational consumer before adding it.
4. Add tests in `internal/core/semantic_test.go`; add router or service tests
   when the new label changes scores, traces, or orchestration.

The `SemanticLayer` interface is the seam for a future heavier backend. Such a
backend should preserve the provider-neutral profile and the side-effect-free
analysis contract.

For a fully selectable backend, also introduce explicit dependency injection
instead of silently replacing global behavior: pass a `SemanticLayer` into the
router/service, retain the local implementation as the default, define timeout
and fallback behavior, and record the concrete backend in `profile.backend`.
`AnalyzeObjective` currently uses `context.Background()` and discards the
`AnalyzeTask` error. A fallible or expensive backend must address both error
handling and cancellation before being plugged into that helper.

When authoring custom examples, specify related fields explicitly. The merge
does not rerun capability inference or risk-to-review inference: an example
with `LongRunning: true` alone does not add durable capabilities, and an example
with a risk alone does not automatically set `NeedsReview`. The built-in
examples supply the associated fields where needed.

## Inspecting profiles yourself

There is no dedicated semantic-analysis CLI command today. To classify an
objective without starting an agent, run this standalone Go example from a
temporary directory **inside this repository** (Go's `internal` import rules
require it):

```sh
semantic_demo_dir=$(mktemp -d ./semantic-demo.XXXXXX)
cat > "$semantic_demo_dir/main.go" <<'GO'
package main

import (
    "context"
    "encoding/json"
    "os"

    "github.com/harsha/relay/internal/core"
)

func main() {
    profile, err := core.DefaultSemanticLayer().AnalyzeTask(
        context.Background(),
        core.SemanticTaskInput{
            Role: core.RoleImplementation,
            Objective: "add a watchdog for stalled workflow resume checkpoints",
        },
    )
    if err != nil {
        panic(err)
    }
    encoder := json.NewEncoder(os.Stdout)
    encoder.SetIndent("", "  ")
    if err := encoder.Encode(profile); err != nil {
        panic(err)
    }
}
GO
go run "$semantic_demo_dir/main.go"
rm "$semantic_demo_dir/main.go"
rmdir "$semantic_demo_dir"
```

This prints the watchdog profile in Example 2. Replace the role and objective
to inspect another request. Calling `DeterministicSemanticLayer{}` instead of
`DefaultSemanticLayer()` lets you compare rules alone against the default
rules-plus-examples result. Neither call needs an installed agent or writes
Relay task state; Go may populate its build cache.

For an existing task, use the trace command with `jq` to inspect persisted
semantic and orchestration events:

```sh
rly trace --json <task-id> | jq '.[] | select(
  .type == "semantic.assessed" or
  .type == "routing.selected" or
  .type == "orchestration.selected"
) | {type, actor, summary, data}'
```

Use the checkpoint command to inspect the execution-side provider/model attempt
chain that consumed those routing decisions:

```sh
rly checkpoint --json <task-id> | jq '.provider_attempts'
```

The JSON paths differ between interfaces:

| Interface | Profile location |
| --- | --- |
| Returned `RouteDecision` | `.semantic_profile` |
| `rly run --json` output, when routing is included | `.routing.semantic_profile` |
| `semantic.assessed` trace event | `.data.profile` |
| `routing.selected` trace event | `.data.semantic` |

A trace can contain multiple assessments for planning, implementation, and
fallback routing. Use the `routing.selected` event's `.data.role` to identify
the routing role; `semantic.assessed` itself does not include a role or objective.
The current CLI also calls routing for an explicitly named executor, using a
single-adapter candidate set. Direct service callers that execute without
calling `Service.Route` do not get these routing assessment events.

## Verification and debugging

The focused semantic tests can be run with:

```sh
go test ./internal/core -run 'Semantic|Route|DurableOrchestration'
```

The broader integration check is:

```sh
go test ./...
```

When debugging a surprising profile, inspect in this order:

1. the exact role string and lowercased objective;
2. substring matches and task-kind precedence;
3. filtered token sets for the query and each built-in example;
4. whether the best score crossed `0.18` or `0.35`;
5. additive merge behavior;
6. adapter capabilities and policy, which determine how the profile changes
   routing rather than classification itself.
