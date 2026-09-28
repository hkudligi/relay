package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/model"
)

type runActorView struct {
	Role    string
	Adapter string
	Model   string
	Status  string
	Session string
	Detail  string
}

type runRenderer struct {
	out         io.Writer
	interactive bool
	color       bool
	taskID      string
	taskState   model.TaskState
	objective   string
	inventory   []agents.ModelAvailability
	planner     *runActorView
	executor    *runActorView
	delegated   string
	started     time.Time
	lines       int
}

func newRunRenderer(out io.Writer, task *model.Task, objective string, inventory []agents.ModelAvailability, plannerName, plannerModel, executorName, executorModel string) *runRenderer {
	renderer := &runRenderer{
		out:         out,
		interactive: isTerminalWriter(out),
		color:       supportsColor(out),
		objective:   objective,
		inventory:   inventory,
		started:     time.Now(),
	}
	if task != nil {
		renderer.taskID = task.ID
		renderer.taskState = task.State
	}
	if plannerName != "" {
		renderer.planner = &runActorView{Role: "planner", Adapter: plannerName, Model: routeModelLabel(plannerModel), Status: "queued"}
	}
	renderer.executor = &runActorView{Role: "executor", Adapter: executorName, Model: routeModelLabel(executorModel), Status: "queued"}
	return renderer
}

func (r *runRenderer) Start() {
	if r == nil {
		return
	}
	if r.interactive {
		r.render()
		return
	}
	fmt.Fprintf(r.out, "rly run %s  %s\n", r.taskID, r.taskState)
	if summary := compactInventorySummary(r.inventory); summary != "" {
		fmt.Fprintf(r.out, "models  %s\n", summary)
	}
	if r.planner != nil {
		fmt.Fprintf(r.out, "plan    %s (%s)\n", r.planner.Adapter, r.planner.Model)
	}
	fmt.Fprintf(r.out, "exec    %s (%s)\n", r.executor.Adapter, r.executor.Model)
}

func (r *runRenderer) Phase(role string) {
	if actor := r.actor(role); actor != nil {
		actor.Status = "running"
		actor.Detail = ""
	}
	if r.interactive {
		r.render()
		return
	}
	if actor := r.actor(role); actor != nil {
		fmt.Fprintf(r.out, "%-7s %s (%s) running\n", role, actor.Adapter, actor.Model)
	}
}

func (r *runRenderer) Event(role string, event agents.Event) {
	actor := r.actor(role)
	if actor == nil {
		return
	}
	switch event.Kind {
	case agents.EventMessage:
		if strings.TrimSpace(event.Message) == "" {
			return
		}
		actor.Status = "streaming"
		actor.Detail = latestProgressLine(event.Message)
		r.renderOrPrint(role, actor.Detail)
	case agents.EventResult:
		detail := latestProgressLine(event.Message)
		if detail == "" {
			detail = strings.TrimSpace(event.Type)
		}
		if detail == "" {
			return
		}
		actor.Status = "responding"
		actor.Detail = detail
		r.renderOrPrint(role, detail)
	case agents.EventProgress:
		detail := latestProgressLine(event.Message)
		if detail == "" {
			detail = strings.TrimSpace(event.Type)
		}
		if state, ok := event.Data["state"].(string); ok && state != "" && state != detail {
			detail = appendDetail(detail, state)
		}
		if detail == "" {
			return
		}
		actor.Status = "working"
		actor.Detail = detail
		r.renderOrPrint(role, detail)
	case agents.EventSession:
		actor.Status = "connected"
		actor.Session = event.SessionID
		if actualModel, _ := event.Data["model"].(string); actualModel != "" {
			actor.Model = routeModelLabel(actualModel)
		}
		detail := "session started"
		if event.SessionID != "" {
			detail += " " + event.SessionID
		}
		actor.Detail = detail
		r.renderOrPrint(role, detail)
	case agents.EventError:
		actor.Status = "error"
		actor.Detail = strings.TrimSpace(event.Message)
		if actor.Detail == "" {
			actor.Detail = "error"
		}
		r.renderOrPrint(role, actor.Detail)
	}
}

func (r *runRenderer) Heartbeat(role string, elapsed time.Duration) {
	if actor := r.actor(role); actor != nil {
		actor.Status = "waiting"
		actor.Detail = "waiting for provider output, elapsed " + elapsed.Round(time.Second).String()
		if r.interactive {
			r.render()
		} else {
			fmt.Fprintf(r.out, "%-7s %s\n", role, actor.Detail)
		}
	}
}

func (r *runRenderer) Retry(role, adapter, reason, nextAgent, nextModel string) {
	detail := fmt.Sprintf("retry after %s %s: %s (%s)", adapter, reason, nextAgent, routeModelLabel(nextModel))
	if actor := r.actor(role); actor != nil {
		actor.Status = "rerouting"
		actor.Adapter = nextAgent
		actor.Model = routeModelLabel(nextModel)
		actor.Detail = detail
	}
	if r.interactive {
		r.render()
		return
	}
	fmt.Fprintln(r.out, detail)
}

func (r *runRenderer) Delegation(count int) {
	if count <= 0 {
		return
	}
	r.delegated = fmt.Sprintf("%d vendor task(s) running", count)
	if r.interactive {
		r.render()
		return
	}
	fmt.Fprintf(r.out, "delegate %d vendor task(s)\n", count)
}

func (r *runRenderer) DelegationDone() {
	r.delegated = "completed"
	if r.interactive {
		r.render()
		return
	}
	fmt.Fprintln(r.out, "delegate completed")
}

func (r *runRenderer) Finish(state model.TaskState, session string) {
	if r.executor != nil {
		r.executor.Status = strings.ToLower(string(state))
		if session != "" {
			r.executor.Session = session
		}
	}
	if r.interactive {
		r.render()
		fmt.Fprintln(r.out)
	}
	fmt.Fprintf(r.out, "done    %s", state)
	if session != "" {
		fmt.Fprintf(r.out, " (%s)", session)
	}
	fmt.Fprintln(r.out)
}

func (r *runRenderer) Fail(err error) {
	if r == nil || err == nil {
		return
	}
	if r.executor != nil {
		r.executor.Status = "failed"
		r.executor.Detail = err.Error()
	}
	if r.interactive {
		r.render()
		fmt.Fprintln(r.out)
		return
	}
	fmt.Fprintf(r.out, "failed  %s\n", err)
}

func (r *runRenderer) actor(role string) *runActorView {
	if r == nil {
		return nil
	}
	switch role {
	case "planner":
		return r.planner
	default:
		return r.executor
	}
}

func (r *runRenderer) renderOrPrint(role, detail string) {
	if r.interactive {
		r.render()
		return
	}
	if detail != "" {
		fmt.Fprintf(r.out, "%-7s %s\n", role, detail)
	}
}

func (r *runRenderer) render() {
	if r.lines > 0 {
		fmt.Fprintf(r.out, "\x1b[%dA", r.lines)
		for i := 0; i < r.lines; i++ {
			fmt.Fprint(r.out, "\x1b[2K\r")
			if i < r.lines-1 {
				fmt.Fprint(r.out, "\x1b[1B")
			}
		}
		fmt.Fprintf(r.out, "\x1b[%dA", r.lines-1)
	}
	lines := r.panelLines()
	for _, line := range lines {
		fmt.Fprintln(r.out, line)
	}
	r.lines = len(lines)
}

func (r *runRenderer) panelLines() []string {
	elapsed := time.Since(r.started).Round(time.Second)
	lines := []string{
		fmt.Sprintf("rly  %s  %s  elapsed %s", r.taskID, statusColor(r.color, r.taskState), elapsed),
		"objective  " + oneLine(r.objective, 76),
	}
	if summary := compactInventorySummary(r.inventory); summary != "" {
		lines = append(lines, "models     "+oneLine(summary, 76))
	}
	if r.planner != nil {
		lines = append(lines, actorLineColor(r.color, r.planner))
	}
	if r.executor != nil {
		lines = append(lines, actorLineColor(r.color, r.executor))
	}
	if r.delegated != "" {
		lines = append(lines, "delegate  "+r.delegated)
	}
	return lines
}

func actorLine(actor *runActorView) string {
	return actorLineColor(false, actor)
}

// actorLineColor renders one actor row. The plain form is used by tests and
// documentation; the colored form highlights the status token when the run
// renderer paints a color-capable terminal.
func actorLineColor(color bool, actor *runActorView) string {
	parts := []string{fmt.Sprintf("%-9s %s", actor.Role, actorStatusColor(color, actor.Status))}
	if actor.Adapter != "" {
		parts = append(parts, actor.Adapter)
	}
	if actor.Model != "" {
		parts = append(parts, "("+actor.Model+")")
	}
	if actor.Session != "" {
		parts = append(parts, "session "+actor.Session)
	}
	if actor.Detail != "" {
		parts = append(parts, "- "+oneLine(actor.Detail, 64))
	}
	return strings.Join(parts, " ")
}

func compactInventorySummary(inventory []agents.ModelAvailability) string {
	if len(inventory) == 0 {
		return ""
	}
	parts := make([]string, 0, len(inventory))
	for _, item := range inventory {
		if len(parts) >= 4 {
			parts = append(parts, fmt.Sprintf("+%d more", len(inventory)-len(parts)))
			break
		}
		model := routeModelLabel(item.Model)
		if model == "" || model == "UNKNOWN" {
			model = "default"
		}
		quota := "UNKNOWN"
		if item.RemainingPercent != nil {
			quota = fmt.Sprintf("%.0f%%", *item.RemainingPercent)
		}
		status := "ready"
		if !item.Installed {
			status = "missing"
		} else if !item.Usable {
			status = "blocked"
		}
		parts = append(parts, fmt.Sprintf("%s/%s %s %s", item.Agent, model, quota, status))
	}
	return strings.Join(parts, " | ")
}

func oneLine(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len([]rune(value)) <= limit {
		return value
	}
	if limit <= 1 {
		return value[:limit]
	}
	runes := []rune(value)
	return string(runes[:limit-1]) + "…"
}

func isTerminalWriter(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// supportsColor reports whether ANSI SGR sequences may be emitted to w. Color
// requires a character device (terminal) and honors the NO_COLOR convention
// plus the TERM=dumb convention used by test harnesses and CI logs. Pipes,
// files, and in-memory buffers never receive escape sequences, and --json
// output is produced separately, so machine-readable output stays unchanged.
func supportsColor(w io.Writer) bool {
	if !isTerminalWriter(w) {
		return false
	}
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	return os.Getenv("TERM") != "dumb"
}

// ANSI SGR color codes used by the TL;DR status rendering.
const (
	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
	ansiCyan   = "\x1b[36m"
)

// colorize wraps text in an SGR sequence when color is enabled; otherwise the
// text is returned unchanged so plain output stays byte-identical.
func colorize(color bool, code, text string) string {
	if !color || text == "" {
		return text
	}
	return code + text + ansiReset
}

// statusColor maps a task state to its semantic TL;DR color: green for done,
// red for failed or cancelled, yellow for attention states, blue for active
// work, and cyan for not-yet-started states.
func statusColor(color bool, state model.TaskState) string {
	var code string
	switch state {
	case model.TaskCompleted:
		code = ansiGreen
	case model.TaskFailed, model.TaskCancelled:
		code = ansiRed
	case model.TaskWaitingForUser, model.TaskBlocked:
		code = ansiYellow
	case model.TaskPlanning, model.TaskRunning, model.TaskVerifying, model.TaskReviewing:
		code = ansiBlue
	default:
		code = ansiCyan
	}
	return colorize(color, code, string(state))
}

// actorStatusColor maps a run-panel actor status to its semantic color,
// mirroring the task-state palette so the TL;DR reads at a glance.
func actorStatusColor(color bool, status string) string {
	var code string
	switch status {
	case "error", "failed":
		code = ansiRed
	case "waiting", "rerouting":
		code = ansiYellow
	case "running", "streaming", "working", "responding":
		code = ansiBlue
	case "connected":
		code = ansiGreen
	default:
		code = ansiCyan
	}
	return colorize(color, code, status)
}
