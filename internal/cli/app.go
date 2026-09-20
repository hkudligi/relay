package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/agents/agy"
	"github.com/harsha/relay/internal/agents/codex"
	"github.com/harsha/relay/internal/agents/cursor"
	"github.com/harsha/relay/internal/core"
	"github.com/harsha/relay/internal/model"
	"github.com/harsha/relay/internal/store"
)

const (
	ExitOK               = 0
	ExitFailed           = 1
	ExitInvalid          = 2
	ExitAgentUnavailable = 4
)

type App struct {
	In       io.Reader
	Out, Err io.Writer
	Getwd    func() (string, error)
	HomeDir  func() (string, error)
	Adapters map[string]agents.Adapter
}

type runOutput struct {
	Inventory []agents.ModelAvailability `json:"inventory"`
	Routing   *model.RouteDecision       `json:"routing,omitempty"`
	Execution *core.Execution            `json:"execution,omitempty"`
	Task      *model.Task                `json:"task,omitempty"`
	Error     string                     `json:"error,omitempty"`
}

func New() *App {
	return &App{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Getwd: os.Getwd, HomeDir: os.UserHomeDir, Adapters: map[string]agents.Adapter{"codex": codex.New(""), "agy": agy.New(""), "cursor": cursor.New("")}}
}

func (a *App) Run(ctx context.Context, args []string) int {
	root := flag.NewFlagSet("rly", flag.ContinueOnError)
	root.SetOutput(a.Err)
	state := root.String("state", "", "SQLite state database")
	if err := root.Parse(args); err != nil {
		return ExitInvalid
	}
	dbPath, err := a.statePath(*state)
	if err != nil {
		return a.fail(err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return a.fail(err)
	}
	defer db.Close()
	repo, err := a.Getwd()
	if err != nil {
		return a.fail(err)
	}
	repo, _ = filepath.Abs(repo)
	svc := core.New(db)
	rest := root.Args()
	if len(rest) == 0 {
		return a.repl(ctx, svc, repo)
	}
	return a.command(ctx, svc, repo, rest)
}

func (a *App) statePath(explicit string) (string, error) {
	if explicit != "" {
		p, err := filepath.Abs(explicit)
		return p, err
	}
	home, err := a.HomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".rly", "state.db"), nil
}

func (a *App) command(ctx context.Context, svc *core.Service, repo string, args []string) int {
	switch args[0] {
	case "help", "--help", "-h":
		a.help()
		return ExitOK
	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		fs.SetOutput(a.Err)
		jsonOut := fs.Bool("json", false, "emit JSON")
		agentName := fs.String("agent", "auto", "agent adapter (codex, agy, cursor, or auto)")
		plannerName := fs.String("planner", "", "planning adapter to run before the executor (codex, agy, cursor, or auto)")
		strategy := fs.String("strategy", core.StrategyBalanced, "routing strategy (balanced, conservative, or quality-first)")
		minReserve := fs.Float64("min-reserve", 15.0, "minimum token reserve percentage")
		sandbox := fs.String("sandbox", string(agents.SandboxWorkspaceWrite), "sandbox mode (read-only or workspace-write)")
		skipGit := fs.Bool("skip-git-check", false, "allow Codex outside a Git repository")
		if err := fs.Parse(args[1:]); err != nil {
			return ExitInvalid
		}
		objective := strings.TrimSpace(strings.Join(fs.Args(), " "))
		if objective == "" {
			fmt.Fprintln(a.Err, "rly run: objective is required")
			return ExitInvalid
		}
		if *agentName != "auto" {
			if _, ok := a.Adapters[*agentName]; !ok {
				fmt.Fprintf(a.Err, "unknown agent adapter %q\n", *agentName)
				return ExitInvalid
			}
		}
		if *plannerName != "" && *plannerName != "auto" {
			if _, ok := a.Adapters[*plannerName]; !ok {
				fmt.Fprintf(a.Err, "unknown planner adapter %q\n", *plannerName)
				return ExitInvalid
			}
		}
		mode := agents.Sandbox(*sandbox)
		if mode != agents.SandboxReadOnly && mode != agents.SandboxWorkspaceWrite {
			fmt.Fprintf(a.Err, "invalid sandbox %q\n", *sandbox)
			return ExitInvalid
		}
		policy := core.RoutingPolicy{
			Strategy:           *strategy,
			MinReservePercent:  *minReserve,
			UnknownQuota:       "deny",
			RequireFileEditing: (mode == agents.SandboxWorkspaceWrite),
		}
		return a.executeObjectiveWithRouter(ctx, svc, repo, objective, *plannerName, *agentName, policy, agents.Request{Sandbox: mode, SkipGitCheck: *skipGit}, *jsonOut)
	case "agents":
		jsonOut, ok := parseJSONOnly(args[1:], a.Err)
		if !ok {
			return ExitInvalid
		}
		inventory := a.discoverInventory(ctx)
		if jsonOut {
			return a.json(inventory)
		}
		a.printInventory(inventory)
		return ExitOK
	case "status":
		jsonOut, ok := parseJSONOnly(args[1:], a.Err)
		if !ok {
			return ExitInvalid
		}
		st, err := svc.Status(ctx, repo)
		if err != nil {
			return a.fail(err)
		}
		if jsonOut {
			return a.json(st)
		}
		a.printStatus(st)
		return ExitOK
	case "tasks":
		jsonOut, ok := parseJSONOnly(args[1:], a.Err)
		if !ok {
			return ExitInvalid
		}
		ts, err := svc.Tasks(ctx, repo)
		if err != nil {
			return a.fail(err)
		}
		if jsonOut {
			return a.json(ts)
		}
		a.printTasks(ts)
		return ExitOK
	case "memory":
		return a.memoryCommand(ctx, svc, repo, args[1:])
	case "trace":
		fs := flag.NewFlagSet("trace", flag.ContinueOnError)
		fs.SetOutput(a.Err)
		jsonOut := fs.Bool("json", false, "emit JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return ExitInvalid
		}
		if fs.NArg() != 1 {
			fmt.Fprintln(a.Err, "rly trace: task id is required")
			return ExitInvalid
		}
		id := fs.Arg(0)
		if _, err := svc.Task(ctx, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				fmt.Fprintf(a.Err, "task %s not found\n", id)
				return ExitInvalid
			}
			return a.fail(err)
		}
		events, err := svc.Trace(ctx, id)
		if err != nil {
			return a.fail(err)
		}
		if *jsonOut {
			return a.json(events)
		}
		for _, e := range events {
			fmt.Fprintf(a.Out, "%s  %-12s %s\n", e.CreatedAt.Local().Format("15:04:05"), e.Actor, e.Summary)
		}
		return ExitOK
	default:
		fmt.Fprintf(a.Err, "unknown command %q; run 'rly help'\n", args[0])
		return ExitInvalid
	}
}

func (a *App) repl(ctx context.Context, svc *core.Service, repo string) int {
	fmt.Fprintf(a.Out, "rly\nrepo: %s\n\n", filepath.Base(repo))
	scanner := bufio.NewScanner(a.In)
	for {
		fmt.Fprint(a.Out, "› ")
		if !scanner.Scan() {
			fmt.Fprintln(a.Out)
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		switch replCommand(line) {
		case "exit":
			return ExitOK
		case "help":
			a.help()
		case "status":
			st, err := svc.Status(ctx, repo)
			if err != nil {
				return a.fail(err)
			}
			a.printStatus(st)
		case "tasks":
			ts, err := svc.Tasks(ctx, repo)
			if err != nil {
				return a.fail(err)
			}
			a.printTasks(ts)
		default:
			if strings.HasPrefix(line, "/") {
				fmt.Fprintf(a.Out, "Unknown command %q. Try /help.\n", line)
				continue
			}
			if len(a.Adapters) == 0 {
				fmt.Fprintln(a.Err, "rly: default codex adapter is not configured")
				continue
			}
			_ = a.executeObjectiveWithRouter(ctx, svc, repo, line, "", "auto", core.DefaultRoutingPolicy(), agents.Request{Sandbox: agents.SandboxWorkspaceWrite}, false)
		}
	}
	if err := scanner.Err(); err != nil {
		return a.fail(err)
	}
	return ExitOK
}

func replCommand(line string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(line), " "))
	switch normalized {
	case "/exit", "exit", "quit":
		return "exit"
	case "/help":
		return "help"
	case "/status", "status", "status of task":
		return "status"
	case "/tasks", "tasks", "show tasks":
		return "tasks"
	default:
		return ""
	}
}

func (a *App) executeObjective(ctx context.Context, svc *core.Service, repo, objective string, adapter agents.Adapter, request agents.Request, jsonOut bool) int {
	return a.executeObjectiveWithPlanner(ctx, svc, repo, objective, nil, adapter, request, jsonOut)
}

func (a *App) executeObjectiveWithPlanner(ctx context.Context, svc *core.Service, repo, objective string, planner, adapter agents.Adapter, request agents.Request, jsonOut bool) int {
	var plannerName, agentName string
	if planner != nil {
		plannerName = planner.Name()
	}
	if adapter != nil {
		agentName = adapter.Name()
	}
	return a.executeObjectiveWithRouter(ctx, svc, repo, objective, plannerName, agentName, core.DefaultRoutingPolicy(), request, jsonOut)
}

func (a *App) executeObjectiveWithRouter(ctx context.Context, svc *core.Service, repo, objective, plannerName, agentName string, policy core.RoutingPolicy, request agents.Request, jsonOut bool) int {
	inventory := a.discoverInventory(ctx)
	if !jsonOut {
		a.printInventory(inventory)
	}
	t, err := svc.StartTaskWithInventory(ctx, repo, objective, inventory)
	if err != nil {
		return a.fail(err)
	}

	var planner agents.Adapter
	plannerModel := ""
	if plannerName == "auto" {
		planDecision, err := svc.Route(ctx, t.ID, core.RolePlanning, objective, a.Adapters, inventory, policy)
		if err != nil {
			if jsonOut {
				_ = a.json(runOutput{Inventory: inventory, Task: t, Error: err.Error()})
			}
			fmt.Fprintln(a.Err, err)
			return ExitAgentUnavailable
		}
		planner = a.Adapters[planDecision.SelectedAgent]
		plannerModel = planDecision.SelectedModel
	} else if plannerName != "" {
		var exists bool
		planner, exists = a.Adapters[plannerName]
		if !exists {
			fmt.Fprintf(a.Err, "unknown planner adapter %q\n", plannerName)
			return ExitInvalid
		}
		if plannerDecision, routeErr := svc.Route(ctx, t.ID, core.RolePlanning, objective, map[string]agents.Adapter{plannerName: planner}, inventory, policy); routeErr == nil {
			plannerModel = plannerDecision.SelectedModel
		}
	}

	var adapter agents.Adapter
	var primaryDecision *model.RouteDecision
	if agentName == "auto" || agentName == "" {
		execDecision, err := svc.Route(ctx, t.ID, core.RoleImplementation, objective, a.Adapters, inventory, policy)
		if err != nil {
			if !jsonOut {
				fmt.Fprintf(a.Out, "%s  %s\n", t.ID, t.State)
			}
			if jsonOut {
				_ = a.json(runOutput{Inventory: inventory, Task: t, Error: err.Error()})
			}
			fmt.Fprintln(a.Err, err)
			return ExitAgentUnavailable
		}
		adapter = a.Adapters[execDecision.SelectedAgent]
		primaryDecision = execDecision
		request.Model = execDecision.SelectedModel
	} else {
		var exists bool
		adapter, exists = a.Adapters[agentName]
		if !exists {
			fmt.Fprintf(a.Err, "unknown agent adapter %q\n", agentName)
			return ExitInvalid
		}
		primaryDecision, _ = svc.Route(ctx, t.ID, core.RoleImplementation, objective, map[string]agents.Adapter{agentName: adapter}, inventory, policy)
		if primaryDecision != nil {
			request.Model = primaryDecision.SelectedModel
		}
	}

	if !jsonOut {
		if primaryDecision != nil && primaryDecision.SelectedAgent != "" && (agentName == "auto" || agentName == "") {
			modelLabel := routeModelLabel(primaryDecision.SelectedModel)
			fmt.Fprintf(a.Out, "router: %s model=%s (%s)\n", primaryDecision.SelectedAgent, modelLabel, primaryDecision.Rationale)
		}
		if planner != nil {
			fmt.Fprintf(a.Out, "%s  %s → %s → %s\n", t.ID, t.State, planner.Name(), adapter.Name())
		} else {
			fmt.Fprintf(a.Out, "%s  %s → %s\n", t.ID, t.State, adapter.Name())
		}
	}

	emit := func(event agents.Event) {
		if !jsonOut && event.Kind == agents.EventMessage && event.Message != "" {
			fmt.Fprint(a.Out, event.Message)
			if !strings.HasSuffix(event.Message, "\n") {
				fmt.Fprintln(a.Out)
			}
		}
	}

	runExecution := func() (*core.Execution, error) {
		if planner != nil {
			return svc.ExecuteWithPlan(ctx, t, planner, adapter, plannerModel, request, func(event agents.Event) {
				if !jsonOut && event.Kind == agents.EventMessage && event.Message != "" {
					fmt.Fprint(a.Out, "[plan] "+event.Message)
					if !strings.HasSuffix(event.Message, "\n") {
						fmt.Fprintln(a.Out)
					}
				}
			}, emit)
		}
		return svc.Execute(ctx, t, adapter, request, emit)
	}

	execution, runErr := runExecution()
	// A provider with UNKNOWN quota can still reject a planning request at
	// runtime. ExecuteWithPlan returns before the executor in that case, so
	// retry planning once with the next eligible provider.
	if runErr != nil && planner != nil && core.IsQuotaExhausted(runErr) && plannerName == "auto" && len(a.Adapters) > 1 {
		fallbackInventory := filterFailedModel(inventory, planner.Name(), plannerModel)
		fallbackAdapters := adaptersForInventory(a.Adapters, fallbackInventory)
		if fallbackDecision, routeErr := svc.Route(ctx, t.ID, core.RolePlanning, objective, fallbackAdapters, fallbackInventory, policy); routeErr == nil {
			if !jsonOut {
				fmt.Fprintf(a.Out, "retrying after %s quota exhaustion: %s model=%s\n", planner.Name(), fallbackDecision.SelectedAgent, routeModelLabel(fallbackDecision.SelectedModel))
			}
			planner = fallbackAdapters[fallbackDecision.SelectedAgent]
			plannerModel = fallbackDecision.SelectedModel
			execution, runErr = runExecution()
		}
	}
	// UNKNOWN quota providers can still reject a request at runtime. Remove
	// that adapter from the candidate set and make one transparent retry so a
	// healthy provider (for example Codex Luna) gets a chance to run the task.
	if runErr != nil && core.IsQuotaExhausted(runErr) && (agentName == "auto" || agentName == "") && len(a.Adapters) > 1 {
		// A provider can expose multiple models with separate limits. Remove
		// only the model that rejected the request, instead of discarding the
		// entire adapter (for example, Codex Luna may fail while another model
		// remains available). The inventory is deliberately kept immutable for
		// the original trace; this filtered copy is only for recovery routing.
		selectedModel := ""
		if primaryDecision != nil {
			selectedModel = primaryDecision.SelectedModel
		}
		fallbackInventory := filterFailedModel(inventory, adapter.Name(), selectedModel)
		fallbackAdapters := make(map[string]agents.Adapter, len(a.Adapters))
		fallbackAgents := make(map[string]bool, len(fallbackInventory))
		for _, item := range fallbackInventory {
			fallbackAgents[item.Agent] = true
		}
		for name, candidate := range a.Adapters {
			// If filtering removed the provider's only known model, keeping the
			// adapter would make the router treat it as UNKNOWN quota and select
			// it again under the default allow policy.
			if fallbackAgents[name] {
				fallbackAdapters[name] = candidate
			}
		}
		// Unknown quota is denied for primary routing, but a provider that could
		// not expose quota remains a useful recovery option after a measured
		// provider has just failed at runtime.
		fallbackPolicy := policy
		fallbackPolicy.UnknownQuota = "allow"
		if fallbackDecision, routeErr := svc.Route(ctx, t.ID, core.RoleImplementation, objective, fallbackAdapters, fallbackInventory, fallbackPolicy); routeErr == nil {
			if !jsonOut {
				fmt.Fprintf(a.Out, "retrying after %s quota exhaustion: %s model=%s\n", adapter.Name(), fallbackDecision.SelectedAgent, routeModelLabel(fallbackDecision.SelectedModel))
			}
			adapter = fallbackAdapters[fallbackDecision.SelectedAgent]
			primaryDecision = fallbackDecision
			request.Model = fallbackDecision.SelectedModel
			execution, runErr = runExecution()
		}
	}

	if jsonOut {
		output := runOutput{Inventory: inventory, Routing: primaryDecision, Execution: execution}
		if execution == nil {
			output.Task = t
		}
		if runErr != nil {
			output.Error = runErr.Error()
		}
		_ = a.json(output)
	}
	if runErr != nil {
		if errors.Is(runErr, core.ErrAgentUnavailable) {
			fmt.Fprintln(a.Err, runErr)
			return ExitAgentUnavailable
		}
		return a.fail(runErr)
	}
	if !jsonOut {
		fmt.Fprintf(a.Out, "✓ %s (%s)\n", execution.Task.State, execution.Result.SessionID)
	}
	return ExitOK
}

func (a *App) discoverInventory(ctx context.Context) []agents.ModelAvailability {
	names := make([]string, 0, len(a.Adapters))
	seen := make(map[string]bool, len(a.Adapters))
	for _, name := range []string{"codex", "agy", "cursor"} {
		if _, ok := a.Adapters[name]; ok {
			names = append(names, name)
			seen[name] = true
		}
	}
	var additional []string
	for name := range a.Adapters {
		if !seen[name] {
			additional = append(additional, name)
		}
	}
	sort.Strings(additional)
	names = append(names, additional...)
	var inventory []agents.ModelAvailability
	for _, name := range names {
		adapter := a.Adapters[name]
		if discoverer, ok := adapter.(agents.ModelDiscoverer); ok {
			rows := discoverer.DiscoverModels(ctx)
			if len(rows) == 0 {
				installation := adapter.Detect(ctx)
				rows = []agents.ModelAvailability{agents.UnknownAvailability(installation, "adapter model discovery returned no rows", "")}
			}
			inventory = append(inventory, rows...)
			continue
		}
		installation := adapter.Detect(ctx)
		inventory = append(inventory, agents.UnknownAvailability(installation, "adapter installation detection; provider quota unavailable", ""))
	}
	return inventory
}

func (a *App) printInventory(inventory []agents.ModelAvailability) {
	fmt.Fprintln(a.Out, "Agent/model inventory:")
	for _, item := range inventory {
		remaining := "UNKNOWN"
		if item.RemainingPercent != nil {
			remaining = fmt.Sprintf("%.0f%%", *item.RemainingPercent)
		}
		label := item.Model
		if item.Reserve {
			label = agents.ExecutionModel(item.Model)
			if !strings.Contains(strings.ToLower(label), "reserve") {
				label += " (reserve)"
			}
		} else if agents.IsReserveModel(item.Model) {
			label = agents.ExecutionModel(item.Model) + " (reserve)"
		}
		fmt.Fprintf(a.Out, "  %-8s %-24s installed=%-3s usable=%-3s remaining=%-7s confidence=%-7s version=%-12s source=%s\n",
			item.Agent, label, yesNo(item.Installed), yesNo(item.Usable), remaining, item.Confidence, item.Version, item.DataSource)
	}
}

func filterFailedModel(inventory []agents.ModelAvailability, agent, model string) []agents.ModelAvailability {
	filtered := make([]agents.ModelAvailability, 0, len(inventory))
	for _, item := range inventory {
		if item.Agent != agent {
			filtered = append(filtered, item)
			continue
		}
		// A provider with an unknown/default model cannot be safely retried:
		// there is no model identifier to filter, and retaining its UNKNOWN row
		// makes the router eligible for the same provider again. Remove the
		// entire provider in that case. For a known model, preserve other models
		// belonging to the same provider.
		if model == "" || model == "UNKNOWN" {
			continue
		}
		if item.Model == model {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func routeModelLabel(model string) string {
	if model == "" {
		return "default"
	}
	if agents.IsReserveModel(model) {
		return agents.ExecutionModel(model) + " (reserve)"
	}
	return model
}

func adaptersForInventory(adapters map[string]agents.Adapter, inventory []agents.ModelAvailability) map[string]agents.Adapter {
	known := make(map[string]bool)
	for _, item := range inventory {
		known[item.Agent] = true
	}
	filtered := make(map[string]agents.Adapter)
	for name, adapter := range adapters {
		if known[name] {
			filtered[name] = adapter
		}
	}
	return filtered
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func (a *App) printStatus(st model.Status) {
	fmt.Fprintf(a.Out, "repo: %s\n", filepath.Base(st.Repository))
	if st.Task == nil {
		fmt.Fprintln(a.Out, "task: none")
		return
	}
	fmt.Fprintf(a.Out, "task: %s\nstate: %s\nobjective: %s\n", st.Task.ID, st.Task.State, st.Task.Objective)
}

func (a *App) printTasks(ts []model.Task) {
	if len(ts) == 0 {
		fmt.Fprintln(a.Out, "No tasks for this repository.")
		return
	}
	for _, t := range ts {
		fmt.Fprintf(a.Out, "%-18s %-12s %s\n", t.ID, t.State, t.Objective)
	}
}

func (a *App) memoryCommand(ctx context.Context, svc *core.Service, repo string, args []string) int {
	if len(args) > 0 && args[0] == "set" {
		if len(args) < 3 {
			fmt.Fprintln(a.Err, "rly memory set: key and value are required")
			return ExitInvalid
		}
		if err := svc.SetMemory(ctx, repo, args[1], strings.Join(args[2:], " ")); err != nil {
			fmt.Fprintf(a.Err, "rly memory set: %v\n", err)
			return ExitInvalid
		}
		fmt.Fprintf(a.Out, "saved %s\n", args[1])
		return ExitOK
	}
	if len(args) > 0 && args[0] == "delete" {
		if len(args) != 2 {
			fmt.Fprintln(a.Err, "rly memory delete: key is required")
			return ExitInvalid
		}
		if err := svc.DeleteMemory(ctx, repo, args[1]); err != nil {
			fmt.Fprintf(a.Err, "rly memory delete: %v\n", err)
			return ExitInvalid
		}
		fmt.Fprintf(a.Out, "deleted %s\n", args[1])
		return ExitOK
	}
	jsonOut, ok := parseJSONOnly(args, a.Err)
	if !ok {
		return ExitInvalid
	}
	items, err := svc.Memory(ctx, repo)
	if err != nil {
		return a.fail(err)
	}
	if jsonOut {
		return a.json(items)
	}
	if len(items) == 0 {
		fmt.Fprintln(a.Out, "No project memory.")
		return ExitOK
	}
	for _, item := range items {
		fmt.Fprintf(a.Out, "%s: %s\n", item.Key, item.Value)
	}
	return ExitOK
}

func (a *App) json(v any) int {
	enc := json.NewEncoder(a.Out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return a.fail(err)
	}
	return ExitOK
}
func (a *App) fail(err error) int { fmt.Fprintf(a.Err, "rly: %v\n", err); return ExitFailed }
func (a *App) help() {
	fmt.Fprint(a.Out, `rly — relay work across coding agents

Usage:
  rly [--state PATH]
  rly [--state PATH] run [--planner codex|agy|cursor|auto] [--agent codex|agy|cursor|auto] [--strategy balanced|conservative|quality-first] [--min-reserve PCT] [--sandbox MODE] [--json] <objective>
  rly [--state PATH] agents [--json]
  rly [--state PATH] status [--json]
  rly [--state PATH] tasks [--json]
  rly [--state PATH] memory [--json]
  rly [--state PATH] memory set <key> <value>
  rly [--state PATH] memory delete <key>
  rly [--state PATH] trace [--json] <task-id>
  rly help

In the REPL, enter an objective to run it automatically optimized for token
availability and agent efficacy, or use /help, /status, /tasks, or /exit.
The phrases "status", "status of task", "tasks", and "show tasks" are also
recognized as commands.
`)
}

func parseJSONOnly(args []string, errOut io.Writer) (bool, bool) {
	fs := flag.NewFlagSet("", flag.ContinueOnError)
	fs.SetOutput(errOut)
	v := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return false, false
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errOut, "unexpected arguments")
		return false, false
	}
	return *v, true
}
