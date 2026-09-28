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
	"time"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/agents/agy"
	"github.com/harsha/relay/internal/agents/codex"
	"github.com/harsha/relay/internal/agents/cursor"
	"github.com/harsha/relay/internal/agents/freebuff"
	"github.com/harsha/relay/internal/core"
	"github.com/harsha/relay/internal/model"
	"github.com/harsha/relay/internal/store"
)

// freebuffMaxChannelAge reuses freebuff.MaxChannelAge so the cleanup message
// and the pruning window can never drift apart.
const freebuffMaxChannelAge = freebuff.MaxChannelAge

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
	return &App{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Getwd: os.Getwd, HomeDir: os.UserHomeDir, Adapters: map[string]agents.Adapter{"codex": codex.New(""), "agy": agy.New(""), "cursor": cursor.New(""), "freebuff": freebuff.New("")}}
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
	a.cleanup(ctx, svc, repo)
	rest := root.Args()
	if len(rest) == 0 {
		return a.repl(ctx, svc, repo)
	}
	return a.command(ctx, svc, repo, rest)
}

// cleanup removes old run history automatically before every CLI command so
// the state database and workspace channel files never grow without bound.
// SQLite task history uses the 7-day retention period from core. Freebuff
// channel directories under the workspace's .rly/freebuff root use the same
// period inside the freebuff package. Both are best-effort: a cleanup failure
// is reported but never blocks the requested command. All cleanup output goes
// to standard error so --json consumers always receive machine-readable data
// on standard output.
func (a *App) cleanup(ctx context.Context, svc *core.Service, repo string) {
	if pruned, err := svc.CleanupOldRuns(ctx); err != nil {
		fmt.Fprintf(a.Err, "rly: cleanup old runs: %v\n", err)
	} else if pruned > 0 {
		fmt.Fprintf(a.Err, "cleanup: pruned %d task(s) older than 7 days\n", pruned)
	}
	if pruned, err := freebuff.PruneStaleRunDirectories(repo, time.Now().Add(-freebuffMaxChannelAge)); err != nil {
		fmt.Fprintf(a.Err, "rly: cleanup freebuff channels: %v\n", err)
	} else if pruned > 0 {
		fmt.Fprintf(a.Err, "cleanup: removed %d freebuff run director(y|ies) older than 7 days\n", pruned)
	}
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
		agentName := fs.String("agent", "auto", "agent adapter (codex, agy, cursor, freebuff, or auto)")
		plannerName := fs.String("planner", "", "planning adapter to run before the executor (codex, agy, cursor, or auto; freebuff is execution-only)")
		strategy := fs.String("strategy", core.StrategyBalanced, "routing strategy: balanced, conservative, or quality-first")
		minReserve := fs.Float64("min-reserve", 15.0, "minimum token headroom percentage to reserve before routing")
		maxTotalTokens := fs.Int64("max-total-tokens", 0, "maximum estimated total tokens for this task")
		projectID := fs.String("project", "", "project ID to append this task to")
		agyWeight := fs.Float64("agy-weight", 1.0, "agent priority weight for Agy")
		codexWeight := fs.Float64("codex-weight", 0.9, "agent priority weight for Codex")
		cursorWeight := fs.Float64("cursor-weight", 0.8, "agent priority weight for Cursor")
		freebuffWeight := fs.Float64("freebuff-weight", 0.1, "agent priority weight for Freebuff")
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
			if *plannerName == "freebuff" {
				fmt.Fprintln(a.Err, "freebuff is execution-only and cannot be used as a planner")
				return ExitInvalid
			}
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
			AgentWeights: map[string]float64{
				"agy": *agyWeight, "codex": *codexWeight, "cursor": *cursorWeight, "freebuff": *freebuffWeight,
			},
		}
		return a.executeObjectiveWithRouter(ctx, svc, repo, *projectID, objective, *plannerName, *agentName, policy, agents.Request{Sandbox: mode, SkipGitCheck: *skipGit, MaxTotalTokens: *maxTotalTokens}, *jsonOut)
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
	case "projects":
		jsonOut, ok := parseJSONOnly(args[1:], a.Err)
		if !ok {
			return ExitInvalid
		}
		projects, err := svc.Projects(ctx)
		if err != nil {
			return a.fail(err)
		}
		if jsonOut {
			return a.json(projects)
		}
		a.printProjects(projects)
		return ExitOK
	case "project":
		fs := flag.NewFlagSet("project", flag.ContinueOnError)
		fs.SetOutput(a.Err)
		jsonOut := fs.Bool("json", false, "emit JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return ExitInvalid
		}
		if fs.NArg() != 1 {
			fmt.Fprintln(a.Err, "rly project: project id is required")
			return ExitInvalid
		}
		project, tasks, err := svc.Project(ctx, fs.Arg(0))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				fmt.Fprintf(a.Err, "project %s not found\n", fs.Arg(0))
				return ExitInvalid
			}
			return a.fail(err)
		}
		if *jsonOut {
			return a.json(struct {
				Project model.Project `json:"project"`
				Tasks   []model.Task  `json:"tasks"`
			}{*project, tasks})
		}
		fmt.Fprintf(a.Out, "%s  %s  %s  tasks=%d\n", project.ID, project.State, project.Repository, project.TaskCount)
		for _, task := range tasks {
			fmt.Fprintf(a.Out, "  %s  %-12s %s\n", task.ID, task.State, task.Objective)
		}
		return ExitOK
	case "resume":
		fs := flag.NewFlagSet("resume", flag.ContinueOnError)
		fs.SetOutput(a.Err)
		jsonOut := fs.Bool("json", false, "emit JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return ExitInvalid
		}
		if fs.NArg() != 1 {
			fmt.Fprintln(a.Err, "rly resume: task id is required")
			return ExitInvalid
		}
		previous, err := svc.Task(ctx, fs.Arg(0))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				fmt.Fprintf(a.Err, "task %s not found\n", fs.Arg(0))
				return ExitInvalid
			}
			return a.fail(err)
		}
		if previous.Repository != repo {
			fmt.Fprintf(a.Err, "task %s belongs to %s\n", previous.ID, previous.Repository)
			return ExitInvalid
		}
		return a.executeObjectiveWithRouter(ctx, svc, repo, previous.ProjectID, previous.Objective, "", "auto", core.DefaultRoutingPolicy(), agents.Request{Sandbox: agents.SandboxWorkspaceWrite}, *jsonOut)
	case "ops":
		fs := flag.NewFlagSet("ops", flag.ContinueOnError)
		fs.SetOutput(a.Err)
		jsonOut := fs.Bool("json", false, "emit JSON")
		aging := fs.Duration("aging", 24*time.Hour, "mark non-terminal tasks older than this duration")
		if err := fs.Parse(args[1:]); err != nil {
			return ExitInvalid
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(a.Err, "unexpected arguments")
			return ExitInvalid
		}
		dashboard, err := svc.Operations(ctx, repo, *aging)
		if err != nil {
			return a.fail(err)
		}
		if *jsonOut {
			return a.json(dashboard)
		}
		a.printOperations(dashboard)
		return ExitOK
	case "cancel":
		fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
		fs.SetOutput(a.Err)
		reason := fs.String("reason", "", "cancellation reason")
		idempotencyKey := fs.String("idempotency-key", "", "idempotency key for safe retries")
		jsonOut := fs.Bool("json", false, "emit JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return ExitInvalid
		}
		if fs.NArg() != 1 {
			fmt.Fprintln(a.Err, "rly cancel: task id is required")
			return ExitInvalid
		}
		task, err := svc.CancelTask(ctx, fs.Arg(0), "user", *reason, *idempotencyKey)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				fmt.Fprintf(a.Err, "task %s not found\n", fs.Arg(0))
				return ExitInvalid
			}
			return a.fail(err)
		}
		if *jsonOut {
			return a.json(task)
		}
		fmt.Fprintf(a.Out, "%s  %s\n", task.ID, task.State)
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
			_ = a.executeObjectiveWithRouter(ctx, svc, repo, "", line, "", "auto", core.DefaultRoutingPolicy(), agents.Request{Sandbox: agents.SandboxWorkspaceWrite}, false)
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
	return a.executeObjectiveWithRouter(ctx, svc, repo, "", objective, plannerName, agentName, core.DefaultRoutingPolicy(), request, jsonOut)
}

func (a *App) executeObjectiveWithRouter(ctx context.Context, svc *core.Service, repo, projectID, objective, plannerName, agentName string, policy core.RoutingPolicy, request agents.Request, jsonOut bool) int {
	if plannerName == "freebuff" {
		if jsonOut {
			_ = a.json(runOutput{Error: "freebuff is execution-only and cannot be used as a planner"})
		} else {
			fmt.Fprintln(a.Err, "freebuff is execution-only and cannot be used as a planner")
		}
		return ExitInvalid
	}
	inventory := a.discoverInventory(ctx)
	var t *model.Task
	var err error
	if projectID != "" {
		t, err = svc.StartTaskWithInventoryForProject(ctx, projectID, repo, objective, inventory)
	} else {
		t, err = svc.StartTaskWithInventory(ctx, repo, objective, inventory)
	}
	if err != nil {
		return a.fail(err)
	}

	var planner agents.Adapter
	plannerModel := ""
	if plannerName == "auto" {
		planningAdapters, planningInventory := planningCandidates(a.Adapters, inventory)
		planDecision, err := svc.Route(ctx, t.ID, core.RolePlanning, objective, planningAdapters, planningInventory, policy)
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
		if planDecision, routeErr := svc.Route(ctx, t.ID, core.RolePlanning, objective, map[string]agents.Adapter{plannerName: planner}, inventory, policy); routeErr == nil {
			plannerModel = planDecision.SelectedModel
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

	var renderer *runRenderer
	if !jsonOut {
		plannerLabel := ""
		if planner != nil {
			plannerLabel = planner.Name()
		}
		renderer = newRunRenderer(a.Out, t, objective, inventory, plannerLabel, plannerModel, adapter.Name(), request.Model)
		renderer.Start()
	}

	emit := func(event agents.Event) {
		if renderer != nil {
			renderer.Event("executor", event)
		}
	}

	runExecution := func() (*core.Execution, error) {
		executorPhasePrinted := false
		if renderer != nil {
			if planner != nil {
				renderer.Phase("planner")
			} else {
				renderer.Phase("executor")
			}
		}
		if planner != nil {
			return svc.ExecuteWithPlan(ctx, t, planner, adapter, plannerModel, request, func(event agents.Event) {
				if renderer != nil {
					renderer.Event("planner", event)
				}
			}, func(event agents.Event) {
				if renderer != nil && !executorPhasePrinted {
					renderer.Phase("executor")
					executorPhasePrinted = true
				}
				emit(event)
			})
		}
		return svc.Execute(ctx, t, adapter, request, emit)
	}
	// Provider tools can remain active for a long time without emitting a new
	// stream event. Keep the terminal informative while the execution itself
	// remains fully asynchronous and cancellable through the parent context.
	runExecutionWithHeartbeat := func() (*core.Execution, error) {
		if jsonOut {
			return runExecution()
		}
		type executionOutcome struct {
			execution *core.Execution
			err       error
		}
		outcomes := make(chan executionOutcome, 1)
		started := time.Now()
		go func() {
			execution, err := runExecution()
			outcomes <- executionOutcome{execution: execution, err: err}
		}()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case outcome := <-outcomes:
				return outcome.execution, outcome.err
			case <-ticker.C:
				if renderer != nil {
					renderer.Heartbeat("executor", time.Since(started))
				}
			}
		}
	}

	execution, runErr := runExecutionWithHeartbeat()
	// A provider with UNKNOWN quota can still reject a planning request at
	// runtime. ExecuteWithPlan returns before the executor in that case, so
	// retry planning once with the next eligible provider.
	if runErr != nil && planner != nil && shouldRetryAgent(planner, runErr) && plannerName == "auto" && len(a.Adapters) > 1 {
		fallbackInventory := filterFailedModel(inventory, planner.Name(), plannerModel)
		fallbackAdapters, fallbackInventory := planningCandidates(adaptersForInventory(a.Adapters, fallbackInventory), fallbackInventory)
		if fallbackDecision, routeErr := svc.Route(ctx, t.ID, core.RolePlanning, objective, fallbackAdapters, fallbackInventory, policy); routeErr == nil {
			if renderer != nil {
				renderer.Retry("planner", planner.Name(), retryReason(planner, runErr), fallbackDecision.SelectedAgent, fallbackDecision.SelectedModel)
			}
			planner = fallbackAdapters[fallbackDecision.SelectedAgent]
			plannerModel = fallbackDecision.SelectedModel
			execution, runErr = runExecutionWithHeartbeat()
		}
	}
	// Providers can reject a discovered model at runtime. Remove the failed
	// model and keep routing until another candidate succeeds or is exhausted.
	if runErr != nil && shouldRetryAgent(adapter, runErr) {
		fallbackInventory := inventory
		for runErr != nil && shouldRetryAgent(adapter, runErr) {
			selectedModel := ""
			if primaryDecision != nil {
				selectedModel = primaryDecision.SelectedModel
			}
			nextInventory := filterFailedModel(fallbackInventory, adapter.Name(), selectedModel)
			if len(nextInventory) == len(fallbackInventory) {
				break
			}
			fallbackInventory = nextInventory
			fallbackAdapters := make(map[string]agents.Adapter, len(a.Adapters))
			fallbackAgents := make(map[string]bool, len(fallbackInventory))
			for _, item := range fallbackInventory {
				fallbackAgents[item.Agent] = true
			}
			if agentName != "auto" && agentName != "" {
				fallbackAdapters[adapter.Name()] = adapter
			} else {
				for name, candidate := range a.Adapters {
					if fallbackAgents[name] {
						fallbackAdapters[name] = candidate
					}
				}
			}
			fallbackPolicy := policy
			fallbackPolicy.UnknownQuota = "allow"
			fallbackDecision, routeErr := svc.Route(ctx, t.ID, core.RoleImplementation, objective, fallbackAdapters, fallbackInventory, fallbackPolicy)
			if routeErr != nil {
				break
			}
			if renderer != nil {
				renderer.Retry("executor", adapter.Name(), retryReason(adapter, runErr), fallbackDecision.SelectedAgent, fallbackDecision.SelectedModel)
			}
			adapter = fallbackAdapters[fallbackDecision.SelectedAgent]
			primaryDecision = fallbackDecision
			request.Model = fallbackDecision.SelectedModel
			execution, runErr = runExecutionWithHeartbeat()
		}
	}
	if runErr == nil && execution != nil && adapter.Name() == "freebuff" {
		delegations, delegationErr := core.ParseDelegatedTasks(execution.Result.Response)
		if delegationErr != nil {
			runErr = delegationErr
		} else if len(delegations) > 0 {
			if renderer != nil {
				renderer.Delegation(len(delegations))
			}
			delegatedTasks := core.DelegatedAgentTasks(delegations, repo, request.Sandbox)
			delegated := core.Orchestrator{
				Adapters: a.Adapters,
				Mode:     core.ExecutionParallel,
				OnEvent: func(task core.AgentTask, event agents.Event) {
					if renderer != nil {
						renderer.Event("executor", agents.Event{Kind: event.Kind, Type: event.Type, SessionID: event.SessionID, Message: task.ID + ": " + event.Message, Usage: event.Usage, Data: event.Data, Time: event.Time})
					}
				},
			}
			_, delegationErr = delegated.Run(ctx, delegatedTasks)
			if delegationErr != nil {
				runErr = delegationErr
			} else if renderer != nil {
				renderer.DelegationDone()
			}
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
		if renderer != nil {
			renderer.Fail(runErr)
		}
		if errors.Is(runErr, core.ErrAgentUnavailable) {
			fmt.Fprintln(a.Err, runErr)
			return ExitAgentUnavailable
		}
		return a.fail(runErr)
	}
	if renderer != nil {
		renderer.Finish(execution.Task.State, execution.Result.SessionID)
	}
	return ExitOK
}

// printAgentEvent is the shared human-output bridge for every adapter. Vendor
// adapters normalize streamed tool calls, lifecycle records, and text into
// EventProgress/EventSession/EventMessage; keeping this rendering in one place
// ensures the main terminal shows updates from Codex, Agy, Cursor, Freebuff,
// and third-party adapters consistently.
func printAgentEvent(out io.Writer, role, adapter, model string, event agents.Event) {
	label := fmt.Sprintf("[%s %s model=%s]", role, adapter, routeModelLabel(model))
	switch event.Kind {
	case agents.EventMessage:
		if event.Message == "" {
			return
		}
		fmt.Fprintf(out, "%s %s", label, event.Message)
		if !strings.HasSuffix(event.Message, "\n") {
			fmt.Fprintln(out)
		}
	case agents.EventResult:
		detail := latestProgressLine(event.Message)
		if detail == "" {
			detail = strings.TrimSpace(event.Type)
		}
		if detail != "" {
			fmt.Fprintf(out, "%s response: %s\n", label, detail)
		}
	case agents.EventProgress:
		detail := latestProgressLine(event.Message)
		if detail == "" {
			detail = strings.TrimSpace(event.Type)
		}
		if detail == "" {
			return
		}
		if state, ok := event.Data["state"].(string); ok && state != "" && state != detail {
			detail += " state=" + state
		}
		fmt.Fprintf(out, "%s progress: %s\n", label, detail)
	case agents.EventSession:
		actualModel, _ := event.Data["model"].(string)
		if actualModel != "" {
			fmt.Fprintf(out, "%s session started: %s actual_model=%s\n", label, event.SessionID, actualModel)
			return
		}
		if event.SessionID != "" {
			fmt.Fprintf(out, "%s session started: %s\n", label, event.SessionID)
		} else {
			fmt.Fprintf(out, "%s session started\n", label)
		}
	case agents.EventError:
		if event.Message != "" {
			fmt.Fprintf(out, "%s error: %s\n", label, event.Message)
		} else {
			fmt.Fprintf(out, "%s error\n", label)
		}
	}
}

func latestProgressLine(message string) string {
	lines := strings.Split(strings.ReplaceAll(message, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}

func (a *App) discoverInventory(ctx context.Context) []agents.ModelAvailability {
	names := make([]string, 0, len(a.Adapters))
	seen := make(map[string]bool, len(a.Adapters))
	for _, name := range []string{"codex", "agy", "cursor", "freebuff"} {
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
	fmt.Fprintln(a.Out, "Model availability")
	fmt.Fprintln(a.Out, "┌──────────┬──────────────────────────┬───────────┬────────┬────────────┬──────────┐")
	fmt.Fprintln(a.Out, "│ Agent    │ Model                    │ Remaining │ Usable │ Confidence │ Version  │")
	fmt.Fprintln(a.Out, "├──────────┼──────────────────────────┼───────────┼────────┼────────────┼──────────┤")
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
		if !item.Installed {
			label = appendDetail(label, "not installed")
		}
		fmt.Fprintf(a.Out, "│ %-8s │ %-24s │ %-9s │ %-6s │ %-10s │ %-8s │\n",
			fitCell(item.Agent, 8),
			fitCell(label, 24),
			fitCell(remaining, 9),
			fitCell(yesNo(item.Usable), 6),
			fitCell(item.Confidence, 10),
			fitCell(item.Version, 8))
	}
	fmt.Fprintln(a.Out, "└──────────┴──────────────────────────┴───────────┴────────┴────────────┴──────────┘")
}

func appendDetail(label, detail string) string {
	if label == "" {
		return detail
	}
	return label + " (" + detail + ")"
}

func fitCell(value string, width int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 1 {
		return string(runes[:width])
	}
	return string(runes[:width-1]) + "…"
}

func retryReason(adapter agents.Adapter, err error) string {
	if core.IsQuotaExhausted(err) {
		return "quota exhaustion"
	}
	if adapter.Name() == "freebuff" {
		return "failure"
	}
	return "failure"
}

func shouldRetryAgent(adapter agents.Adapter, err error) bool {
	if err == nil {
		return false
	}
	if adapter.Name() == "freebuff" && errors.Is(err, freebuff.ErrSessionConflict) {
		return false
	}
	// Agy model failures are retried against the next discovered model. Freebuff
	// readiness, idle-timeout, and result-file failures are also operational
	// failures that can be retried by the routing layer.
	return errors.Is(err, core.ErrTaskIncomplete) || core.IsQuotaExhausted(err) || core.IsModelSelectionError(err) || adapter.Name() == "agy" || adapter.Name() == "freebuff"
}

func filterFailedModel(inventory []agents.ModelAvailability, agent, model string) []agents.ModelAvailability {
	filtered := make([]agents.ModelAvailability, 0, len(inventory))
	for _, item := range inventory {
		if item.Agent != agent {
			filtered = append(filtered, item)
			continue
		}
		// Freebuff's model picker is inside the TUI, so a failed PTY run
		// invalidates the whole adapter for this retry, not just one catalog row.
		if agent == "freebuff" {
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
		// Routing IDs may include a reserve-pool suffix while adapters receive
		// the underlying execution model. Remove only the failed pool, not a
		// different reserve/ordinary row for the same provider model.
		if item.Model == model || (agents.ExecutionModel(item.Model) == agents.ExecutionModel(model) && agents.IsReserveModel(item.Model) == agents.IsReserveModel(model)) {
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

func planningCandidates(adapters map[string]agents.Adapter, inventory []agents.ModelAvailability) (map[string]agents.Adapter, []agents.ModelAvailability) {
	filteredAdapters := make(map[string]agents.Adapter, len(adapters))
	for name, adapter := range adapters {
		if name != "freebuff" && adapter.Name() != "freebuff" {
			filteredAdapters[name] = adapter
		}
	}
	filteredInventory := make([]agents.ModelAvailability, 0, len(inventory))
	for _, item := range inventory {
		if item.Agent != "freebuff" {
			filteredInventory = append(filteredInventory, item)
		}
	}
	return filteredAdapters, filteredInventory
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func (a *App) printStatus(st model.Status) {
	color := supportsColor(a.Out)
	fmt.Fprintf(a.Out, "repo: %s\n", filepath.Base(st.Repository))
	if st.Task == nil {
		fmt.Fprintln(a.Out, "task: none")
		return
	}
	fmt.Fprintf(a.Out, "task: %s\nstate: %s\nobjective: %s\n", st.Task.ID, statusColor(color, st.Task.State), st.Task.Objective)
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

func (a *App) printProjects(projects []model.Project) {
	if len(projects) == 0 {
		fmt.Fprintln(a.Out, "No projects.")
		return
	}
	for _, project := range projects {
		active := project.ActiveTaskID
		if active == "" {
			active = "-"
		}
		fmt.Fprintf(a.Out, "%s  %-8s %-24s tasks=%d active=%s\n", project.ID, project.State, project.Name, project.TaskCount, active)
		fmt.Fprintf(a.Out, "  %s\n", project.Repository)
	}
}

func (a *App) printOperations(dashboard core.OperationalDashboard) {
	fmt.Fprintf(a.Out, "repo: %s\n", filepath.Base(dashboard.Repository))
	if len(dashboard.Tasks) == 0 {
		fmt.Fprintln(a.Out, "No blocked or aging tasks.")
		return
	}
	fmt.Fprintln(a.Out, "Needs attention")
	for _, task := range dashboard.Tasks {
		fmt.Fprintf(a.Out, "%-18s %-16s %-12s %s\n", task.ID, task.State, task.Reason, task.Objective)
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
  rly [--state PATH] run [--project ID] [--planner codex|agy|cursor|auto] [--agent codex|agy|cursor|freebuff|auto] [--strategy balanced|conservative|quality-first] [--min-reserve PCT] [--agy-weight N] [--codex-weight N] [--cursor-weight N] [--freebuff-weight N] [--sandbox MODE] [--json] <objective>
  rly [--state PATH] agents [--json]
  rly [--state PATH] status [--json]
  rly [--state PATH] tasks [--json]
  rly [--state PATH] projects [--json]
  rly [--state PATH] project [--json] <project-id>
  rly [--state PATH] resume [--json] <task-id>
  rly [--state PATH] ops [--aging DURATION] [--json]
  rly [--state PATH] cancel [--reason TEXT] [--idempotency-key KEY] [--json] <task-id>
  rly [--state PATH] memory [--json]
  rly [--state PATH] memory set <key> <value>
  rly [--state PATH] memory delete <key>
  rly [--state PATH] trace [--json] <task-id>
  rly help

In the REPL, enter an objective to run it automatically optimized for token
availability and agent efficacy, or use /help, /status, /tasks, or /exit.
The phrases "status", "status of task", "tasks", and "show tasks" are also
recognized as commands.

Run options:
  --strategy balanced       Blend capability fit, session context, quota
                            headroom, and reliability. This is the default.
  --strategy conservative   Prefer token conservation and quota headroom over
                            small capability-score differences.
  --strategy quality-first  Prefer maximum capability and efficacy fit.
  --min-reserve PCT         Keep at least this percentage of measured provider
                            quota in reserve; auto-routing skips agents below
                            the threshold unless no higher-headroom agent is
                            eligible.
  --max-total-tokens N      Stop before launch when estimated task usage would
                            exceed this task-level token limit.
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
