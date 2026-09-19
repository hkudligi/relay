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
	"strings"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/agents/agy"
	"github.com/harsha/relay/internal/agents/codex"
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

func New() *App {
	return &App{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Getwd: os.Getwd, HomeDir: os.UserHomeDir, Adapters: map[string]agents.Adapter{"codex": codex.New(""), "agy": agy.New("")}}
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
		agentName := fs.String("agent", "codex", "agent adapter (codex or agy)")
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
		adapter, ok := a.Adapters[*agentName]
		if !ok {
			fmt.Fprintf(a.Err, "unknown agent adapter %q\n", *agentName)
			return ExitInvalid
		}
		mode := agents.Sandbox(*sandbox)
		if mode != agents.SandboxReadOnly && mode != agents.SandboxWorkspaceWrite {
			fmt.Fprintf(a.Err, "invalid sandbox %q\n", *sandbox)
			return ExitInvalid
		}
		t, err := svc.StartTask(ctx, repo, objective)
		if err != nil {
			return a.fail(err)
		}
		if !*jsonOut {
			fmt.Fprintf(a.Out, "%s  %s → %s\n", t.ID, t.State, adapter.Name())
		}
		execution, runErr := svc.Execute(ctx, t, adapter, agents.Request{Sandbox: mode, SkipGitCheck: *skipGit}, func(event agents.Event) {
			if !*jsonOut && event.Kind == agents.EventMessage && event.Message != "" {
				fmt.Fprint(a.Out, event.Message)
				if !strings.HasSuffix(event.Message, "\n") {
					fmt.Fprintln(a.Out)
				}
			}
		})
		if *jsonOut && execution != nil {
			_ = a.json(execution)
		}
		if runErr != nil {
			if errors.Is(runErr, core.ErrAgentUnavailable) {
				fmt.Fprintln(a.Err, runErr)
				return ExitAgentUnavailable
			}
			return a.fail(runErr)
		}
		if !*jsonOut {
			fmt.Fprintf(a.Out, "✓ %s (%s)\n", execution.Task.State, execution.Result.SessionID)
		}
		return ExitOK
	case "agents":
		jsonOut, ok := parseJSONOnly(args[1:], a.Err)
		if !ok {
			return ExitInvalid
		}
		installations := make([]agents.Installation, 0, len(a.Adapters))
		for _, name := range []string{"codex", "agy"} {
			if adapter, exists := a.Adapters[name]; exists {
				installations = append(installations, adapter.Detect(ctx))
			}
		}
		if jsonOut {
			return a.json(installations)
		}
		for _, installation := range installations {
			state := "unavailable"
			if installation.Available {
				state = "available"
			}
			fmt.Fprintf(a.Out, "%-8s %-12s %s\n", installation.Name, state, installation.Version)
		}
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
		if len(ts) == 0 {
			fmt.Fprintln(a.Out, "No tasks for this repository.")
			return ExitOK
		}
		for _, t := range ts {
			fmt.Fprintf(a.Out, "%-18s %-12s %s\n", t.ID, t.State, t.Objective)
		}
		return ExitOK
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
		switch line {
		case "/exit", "exit", "quit":
			return ExitOK
		case "/help":
			a.help()
		case "/status":
			st, err := svc.Status(ctx, repo)
			if err != nil {
				return a.fail(err)
			}
			a.printStatus(st)
		case "/tasks":
			ts, err := svc.Tasks(ctx, repo)
			if err != nil {
				return a.fail(err)
			}
			if len(ts) == 0 {
				fmt.Fprintln(a.Out, "No tasks for this repository.")
			} else {
				for _, t := range ts {
					fmt.Fprintf(a.Out, "%-18s %-12s %s\n", t.ID, t.State, t.Objective)
				}
			}
		default:
			if strings.HasPrefix(line, "/") {
				fmt.Fprintf(a.Out, "Unknown command %q. Try /help.\n", line)
				continue
			}
			t, err := svc.StartTask(ctx, repo, line)
			if err != nil {
				return a.fail(err)
			}
			fmt.Fprintf(a.Out, "→ %s created; waiting for an agent adapter (%s)\n", t.ID, t.State)
		}
	}
	if err := scanner.Err(); err != nil {
		return a.fail(err)
	}
	return ExitOK
}

func (a *App) printStatus(st model.Status) {
	fmt.Fprintf(a.Out, "repo: %s\n", filepath.Base(st.Repository))
	if st.Task == nil {
		fmt.Fprintln(a.Out, "task: none")
		return
	}
	fmt.Fprintf(a.Out, "task: %s\nstate: %s\nobjective: %s\n", st.Task.ID, st.Task.State, st.Task.Objective)
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
  rly [--state PATH] run [--agent codex|agy] [--sandbox MODE] [--json] <objective>
  rly [--state PATH] agents [--json]
  rly [--state PATH] status [--json]
  rly [--state PATH] tasks [--json]
  rly [--state PATH] trace [--json] <task-id>
  rly help

In the REPL, enter an objective or use /help, /status, /tasks, or /exit.
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
