package core

import (
	"context"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/model"
)

const (
	SemanticCapabilityPlanning         = "planning"
	SemanticCapabilityFileEditing      = "file-editing"
	SemanticCapabilityStructuredOutput = "structured-output"
	SemanticCapabilitySessionResume    = "session-resume"
	SemanticCapabilityCancellation     = "cancellation"
	SemanticCapabilityLongContext      = "long-context"
)

const (
	semanticBackendDeterministic = "deterministic"
	semanticBackendLocalLexical  = "local-lexical"
)

// SemanticTaskInput is the stable input envelope for semantic task analysis.
// Implementations should be side-effect free: they classify and annotate, but
// never route, launch agents, or mutate repository state.
type SemanticTaskInput struct {
	Role      string
	Objective string
}

type SemanticExample struct {
	Text                 string
	TaskKind             string
	Domains              []string
	Operations           []string
	Risks                []string
	RequiredCapabilities []string
	Signals              []string
	Mutation             bool
	LongRunning          bool
	NeedsReview          bool
}

// SemanticLayer produces task semantics and can also classify runtime events
// into monitoring signals. The default implementation is deterministic; future
// implementations can use embeddings, BERT-style classifiers, or local small
// LLMs without changing the router contract.
type SemanticLayer interface {
	AnalyzeTask(context.Context, SemanticTaskInput) (model.SemanticProfile, error)
	ObserveEvent(context.Context, agents.Event) SemanticObservation
}

type SemanticObservation struct {
	Signal          string  `json:"signal,omitempty"`
	Severity        string  `json:"severity,omitempty"`
	Summary         string  `json:"summary,omitempty"`
	SuggestedAction string  `json:"suggested_action,omitempty"`
	Confidence      float64 `json:"confidence,omitempty"`
}

type LocalSemanticLayer struct {
	rules    DeterministicSemanticLayer
	examples []SemanticExample
}

type DeterministicSemanticLayer struct{}

func DefaultSemanticLayer() SemanticLayer {
	return NewLocalSemanticLayer(defaultSemanticExamples())
}

func NewLocalSemanticLayer(examples []SemanticExample) LocalSemanticLayer {
	copied := append([]SemanticExample(nil), examples...)
	return LocalSemanticLayer{rules: DeterministicSemanticLayer{}, examples: copied}
}

func AnalyzeObjective(role, objective string) model.SemanticProfile {
	profile, _ := DefaultSemanticLayer().AnalyzeTask(context.Background(), SemanticTaskInput{Role: role, Objective: objective})
	return profile
}

func (l LocalSemanticLayer) AnalyzeTask(ctx context.Context, input SemanticTaskInput) (model.SemanticProfile, error) {
	profile, err := l.rules.AnalyzeTask(ctx, input)
	if err != nil {
		return profile, err
	}
	profile.Backend = semanticBackendLocalLexical
	best, score := bestSemanticExample(input.Role+" "+input.Objective, l.examples)
	if best == nil || score < 0.18 {
		return profile, nil
	}
	mergeSemanticExample(&profile, *best)
	if score >= 0.35 {
		profile.Confidence = maxFloat(profile.Confidence, 0.88)
	} else {
		profile.Confidence = maxFloat(profile.Confidence, 0.80)
	}
	return profile, nil
}

func (l LocalSemanticLayer) ObserveEvent(ctx context.Context, event agents.Event) SemanticObservation {
	return l.rules.ObserveEvent(ctx, event)
}

func (DeterministicSemanticLayer) AnalyzeTask(_ context.Context, input SemanticTaskInput) (model.SemanticProfile, error) {
	text := strings.ToLower(input.Role + " " + input.Objective)
	profile := model.SemanticProfile{
		Backend:     semanticBackendDeterministic,
		TaskKind:    inferTaskKind(input.Role, text),
		Mutation:    impliesMutation(text),
		LongRunning: containsAny(text, "long-running", "durable", "workflow", "orchestration", "temporal", "background", "daemon", "queue", "scheduler", "resume", "checkpoint"),
		Confidence:  0.72,
	}

	profile.Domains = sortedMatches(text, map[string][]string{
		"agent-orchestration": {"agent", "multi-agent", "orchestration", "coordinator", "router", "routing", "delegate", "handoff"},
		"semantic-routing":    {"semantic", "embedding", "bert", "classifier", "intent", "ontology", "micro llm", "local model"},
		"monitoring":          {"monitor", "observability", "trace", "telemetry", "health", "event", "watchdog", "stalled"},
		"storage":             {"sqlite", "database", "schema", "migration", "persistence", "store"},
		"cli":                 {"cli", "command", "terminal", "jsonl", "stdout", "stderr"},
		"testing":             {"test", "coverage", "verification", "fixture", "regression"},
		"security":            {"auth", "permission", "sandbox", "secret", "token", "policy"},
		"concurrency":         {"concurrency", "race", "parallel", "locking", "goroutine", "mutex"},
	})
	profile.Operations = sortedMatches(text, map[string][]string{
		"design":      {"design", "architect", "come up with", "blueprint", "proposal", "spec"},
		"implement":   {"implement", "add", "build", "wire", "create", "support"},
		"debug":       {"debug", "fix", "bug", "broken", "failure", "panic", "regression"},
		"review":      {"review", "audit", "inspect", "assess", "evaluate"},
		"refactor":    {"refactor", "cleanup", "extract", "deduplicate"},
		"monitor":     {"monitor", "observe", "trace", "alert", "watch"},
		"classify":    {"classify", "semantic", "intent", "embedding", "bert", "model"},
		"orchestrate": {"orchestrate", "schedule", "route", "delegate", "workflow"},
	})
	profile.Risks = sortedMatches(text, map[string][]string{
		"data-loss":       {"delete", "migration", "drop", "overwrite", "destructive"},
		"security":        {"auth", "permission", "sandbox", "secret", "token", "credential"},
		"cost":            {"cost", "budget", "quota", "tokens", "expensive"},
		"concurrency":     {"concurrency", "race", "parallel", "deadlock", "locking"},
		"routing-quality": {"routing", "router", "model selection", "classifier", "semantic"},
	})
	profile.Signals = sortedMatches(text, map[string][]string{
		"quota-pressure":              {"quota", "capacity", "rate limit", "usage limit", "token budget"},
		"blocked":                     {"blocked", "waiting for user", "clarification", "human input"},
		"verification-failed":         {"test failed", "failing test", "build failed", "verification failed"},
		"completion-without-evidence": {"done without", "no evidence", "unverified"},
	})
	profile.RequiredCapabilities = inferRequiredCapabilities(input.Role, text, profile)
	profile.NeedsReview = len(profile.Risks) > 0 || containsAny(text, "review", "approval", "high-risk", "production")
	if len(profile.Domains)+len(profile.Operations)+len(profile.Risks) >= 5 {
		profile.Confidence = 0.84
	}
	return profile, nil
}

func (DeterministicSemanticLayer) ObserveEvent(_ context.Context, event agents.Event) SemanticObservation {
	text := strings.ToLower(event.Type + " " + event.Message)
	switch {
	case event.Kind == agents.EventError && IsQuotaExhaustedMessage(text):
		return SemanticObservation{Signal: "quota-pressure", Severity: "warning", Summary: "agent reported quota or capacity pressure", SuggestedAction: "reroute to an eligible model with quota headroom", Confidence: 0.90}
	case event.Kind == agents.EventError:
		return SemanticObservation{Signal: "agent-error", Severity: "error", Summary: "agent emitted an error event", SuggestedAction: "classify retryability before launching dependent work", Confidence: 0.78}
	case containsAny(text, "blocked", "waiting for user", "need clarification", "cannot proceed"):
		return SemanticObservation{Signal: "blocked", Severity: "warning", Summary: "agent appears blocked or waiting", SuggestedAction: "surface a coordinator question or unblock dependency", Confidence: 0.74}
	case containsAny(text, "tests failed", "test failure", "failing test", "build failed", "go test failed"):
		return SemanticObservation{Signal: "verification-failed", Severity: "error", Summary: "verification appears to have failed", SuggestedAction: "route a debugger or keep the task in verification", Confidence: 0.76}
	case containsAny(text, "complete", "done", "finished") && !containsAny(text, "test", "verified", "evidence"):
		return SemanticObservation{Signal: "completion-without-evidence", Severity: "info", Summary: "completion language lacks explicit verification evidence", SuggestedAction: "request evidence before marking the task complete", Confidence: 0.61}
	default:
		return SemanticObservation{}
	}
}

func inferTaskKind(role, text string) string {
	switch {
	case role == RolePlanning || containsAny(text, "design", "architect", "blueprint", "proposal", "come up with"):
		return "design"
	case role == RoleReview || containsAny(text, "review", "audit"):
		return "review"
	case role == RoleDebugging || containsAny(text, "debug", "fix", "broken", "panic"):
		return "debug"
	case containsAny(text, "monitor", "observe", "trace"):
		return "monitoring"
	case containsAny(text, "classify", "semantic", "embedding", "bert"):
		return "classification"
	default:
		return "implementation"
	}
}

func inferRequiredCapabilities(role, text string, profile model.SemanticProfile) []string {
	required := map[string]bool{}
	if role == RolePlanning || profile.TaskKind == "design" {
		required[SemanticCapabilityPlanning] = true
	}
	if profile.Mutation || containsAny(text, "implement", "add", "build", "fix", "edit", "write") {
		required[SemanticCapabilityFileEditing] = true
	}
	if profile.LongRunning {
		required[SemanticCapabilitySessionResume] = true
		required[SemanticCapabilityCancellation] = true
		required[SemanticCapabilityLongContext] = true
	}
	if containsAny(text, "json", "schema", "typed", "trace", "monitor", "deterministic") {
		required[SemanticCapabilityStructuredOutput] = true
	}
	return sortedKeys(required)
}

func bestSemanticExample(text string, examples []SemanticExample) (*SemanticExample, float64) {
	query := tokenSet(text)
	var best *SemanticExample
	var bestScore float64
	for i := range examples {
		score := weightedTokenOverlap(query, tokenSet(examples[i].Text))
		if score > bestScore {
			bestScore = score
			best = &examples[i]
		}
	}
	return best, bestScore
}

func weightedTokenOverlap(query, example map[string]bool) float64 {
	if len(query) == 0 || len(example) == 0 {
		return 0
	}
	var overlap int
	for token := range query {
		if example[token] {
			overlap++
		}
	}
	if overlap == 0 {
		return 0
	}
	precision := float64(overlap) / float64(len(query))
	recall := float64(overlap) / float64(len(example))
	return (0.65 * precision) + (0.35 * recall)
}

func tokenSet(text string) map[string]bool {
	parts := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	tokens := map[string]bool{}
	for _, part := range parts {
		if len(part) < 3 || semanticStopWords[part] {
			continue
		}
		tokens[part] = true
	}
	return tokens
}

func mergeSemanticExample(profile *model.SemanticProfile, example SemanticExample) {
	if example.TaskKind != "" && profile.TaskKind == "implementation" {
		profile.TaskKind = example.TaskKind
	}
	profile.Domains = mergeStrings(profile.Domains, example.Domains)
	profile.Operations = mergeStrings(profile.Operations, example.Operations)
	profile.Risks = mergeStrings(profile.Risks, example.Risks)
	profile.RequiredCapabilities = mergeStrings(profile.RequiredCapabilities, example.RequiredCapabilities)
	profile.Signals = mergeStrings(profile.Signals, example.Signals)
	profile.Mutation = profile.Mutation || example.Mutation
	profile.LongRunning = profile.LongRunning || example.LongRunning
	profile.NeedsReview = profile.NeedsReview || example.NeedsReview
}

func mergeStrings(existing, added []string) []string {
	items := map[string]bool{}
	for _, item := range existing {
		if item != "" {
			items[item] = true
		}
	}
	for _, item := range added {
		if item != "" {
			items[item] = true
		}
	}
	return sortedKeys(items)
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func impliesMutation(text string) bool {
	return containsAny(text, "implement", "add", "build", "create", "edit", "fix", "refactor", "wire", "change", "update", "support")
}

func sortedMatches(text string, vocabulary map[string][]string) []string {
	found := map[string]bool{}
	for label, needles := range vocabulary {
		for _, needle := range needles {
			if strings.Contains(text, needle) {
				found[label] = true
				break
			}
		}
	}
	return sortedKeys(found)
}

func sortedKeys(items map[string]bool) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func semanticCapabilityFit(caps agents.Capabilities, required []string) float64 {
	if len(required) == 0 {
		return 1.0
	}
	var satisfied int
	for _, capability := range required {
		switch capability {
		case SemanticCapabilityPlanning, SemanticCapabilityLongContext:
			satisfied++
		case SemanticCapabilityFileEditing:
			if caps.FileEditing {
				satisfied++
			}
		case SemanticCapabilityStructuredOutput:
			if caps.StructuredOutput {
				satisfied++
			}
		case SemanticCapabilitySessionResume:
			if caps.SessionResume {
				satisfied++
			}
		case SemanticCapabilityCancellation:
			if caps.Cancellation {
				satisfied++
			}
		}
	}
	return math.Round((float64(satisfied)/float64(len(required)))*1000) / 1000
}

func defaultSemanticExamples() []SemanticExample {
	return []SemanticExample{
		{
			Text:                 "route coding task to best available agent using quota headroom model capability and session affinity",
			TaskKind:             "classification",
			Domains:              []string{"agent-orchestration", "semantic-routing"},
			Operations:           []string{"classify", "orchestrate"},
			Risks:                []string{"routing-quality"},
			RequiredCapabilities: []string{SemanticCapabilityStructuredOutput},
			NeedsReview:          true,
		},
		{
			Text:                 "detect stalled background workflow retry after process restart with checkpoint resume and cancellation",
			Domains:              []string{"agent-orchestration", "monitoring"},
			Operations:           []string{"monitor", "orchestrate"},
			RequiredCapabilities: []string{SemanticCapabilitySessionResume, SemanticCapabilityCancellation, SemanticCapabilityLongContext, SemanticCapabilityStructuredOutput},
			Signals:              []string{"blocked"},
			LongRunning:          true,
			NeedsReview:          true,
		},
		{
			Text:                 "record trace telemetry for agent errors quota pressure rate limits failed tests and missing verification evidence",
			Domains:              []string{"monitoring", "testing"},
			Operations:           []string{"monitor", "debug"},
			Risks:                []string{"cost", "routing-quality"},
			RequiredCapabilities: []string{SemanticCapabilityStructuredOutput},
			Signals:              []string{"quota-pressure", "verification-failed", "completion-without-evidence"},
			NeedsReview:          true,
		},
		{
			Text:                 "change sqlite schema migration persistence transaction cleanup retention without losing durable task memory",
			Domains:              []string{"storage"},
			Operations:           []string{"implement"},
			Risks:                []string{"data-loss"},
			RequiredCapabilities: []string{SemanticCapabilityFileEditing, SemanticCapabilityStructuredOutput},
			Mutation:             true,
			NeedsReview:          true,
		},
		{
			Text:                 "inspect authentication permissions sandbox secrets policy before allowing workspace mutation",
			TaskKind:             "review",
			Domains:              []string{"security"},
			Operations:           []string{"review"},
			Risks:                []string{"security", "data-loss"},
			RequiredCapabilities: []string{SemanticCapabilityStructuredOutput},
			NeedsReview:          true,
		},
	}
}

var semanticStopWords = map[string]bool{
	"and": true, "are": true, "but": true, "can": true, "for": true, "from": true, "has": true,
	"into": true, "not": true, "the": true, "then": true, "this": true, "that": true, "use": true,
	"using": true, "with": true, "without": true, "task": true, "work": true,
}
