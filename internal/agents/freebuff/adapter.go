package freebuff

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/harsha/relay/internal/agents"
)

// Adapter runs the Freebuff CLI (https://freebuff.com). Freebuff is a TUI-only
// coding agent: current releases accept no prompt argument and expose no
// print, output-format, or stream-json flags. This adapter therefore runs the
// TUI on a pseudo-terminal and exchanges all task data through temporary
// markdown files in the workspace (prompt.md, status.md, result.md) so rly can
// plan, route, and trace Freebuff work like any other adapter.
type Adapter struct{ Command string }

const (
	freebuffSubmissionTimeout  = 45 * time.Second
	freebuffStartupIdleTimeout = 2 * time.Minute
	freebuffIdleTimeout        = 10 * time.Minute
	freebuffMaxRuntime         = 45 * time.Minute
)

// New builds an adapter. An empty command falls back to the standard
// "freebuff" executable name.
func New(command string) *Adapter {
	if command == "" {
		command = "freebuff"
	}
	return &Adapter{Command: command}
}

func (a *Adapter) Name() string { return "freebuff" }

func (a *Adapter) Detect(ctx context.Context) agents.Installation {
	return agents.Detect(ctx, "freebuff", a.Command)
}

func (a *Adapter) Capabilities() agents.Capabilities {
	// StructuredOutput is false: responses arrive as plain markdown from the
	// result.md channel, not as machine-parseable JSONL from the vendor.
	return agents.Capabilities{
		NonInteractive:   true,
		StructuredOutput: false,
		SessionResume:    false,
		Streaming:        true,
		Cancellation:     true,
		UsageReporting:   false,
		FileEditing:      true,
	}
}

// Start launches Freebuff for a fresh task.
func (a *Adapter) Start(ctx context.Context, r agents.Request) (agents.Run, error) {
	return a.run(ctx, r)
}

// Resume is not supported: the Freebuff CLI can only continue conversations
// inside its own TUI (interactive --continue), and the adapter's per-run file
// channel is fresh each time. rly treats freebuff sessions as non-resumable.
func (a *Adapter) Resume(ctx context.Context, session string, r agents.Request) (agents.Run, error) {
	if session == "" {
		return nil, fmt.Errorf("freebuff session id is required")
	}
	return nil, fmt.Errorf("freebuff sessions cannot be resumed; start a new task instead")
}

func (a *Adapter) run(ctx context.Context, r agents.Request) (agents.Run, error) {
	workspace := r.Workspace
	if strings.TrimSpace(workspace) == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve workspace for freebuff: %w", err)
		}
	}

	ch, err := newChannel(workspace)
	if err != nil {
		return nil, fmt.Errorf("create freebuff channel: %w", err)
	}
	ch.trace("run.created workspace=%q", workspace)
	handoffRequired, err := ch.writeHandoff(r.Prompt)
	if err != nil {
		return nil, fmt.Errorf("write freebuff handoff: %w", err)
	}
	if err := ch.writePrompt(r.Prompt); err != nil {
		return nil, fmt.Errorf("write freebuff prompt: %w", err)
	}
	ch.trace("tui.start command=%q handoff_required=%t", a.Command, handoffRequired)

	session, err := startTUI(a.Command, workspace)
	if err != nil {
		ch.trace("tui.start.error error=%q", err)
		return nil, err
	}
	ch.trace("tui.started")

	// The task pointer pasted into the TUI stays short: the full brief lives
	// in prompt.md where the agent reads it with file tools. A one-line paste
	// avoids OpenTUI large-paste truncation and keeps TUI history readable.
	pointer := fmt.Sprintf(
		"Read the file %s and complete the task it describes exactly as written, including the reporting protocol. If it references a planner handoff, read that handoff before starting.",
		ch.PromptPath(),
	)
	if err := session.pastePrompt(ctx, pointer); err != nil {
		ch.trace("prompt.submit.error error=%q diagnostic=%q", err, session.diagnostic())
		_ = session.cancel()
		return nil, err
	}
	ch.trace("prompt.submit.complete readiness=%s", session.readinessMode())
	ch.trace("tui.pane.after_submit=%q", session.paneSnapshot())

	return newRun(ctx, session, ch, handoffRequired), nil
}

// run streams progress from status.md and completes when result.md appears.
type run struct {
	events          chan agents.Event
	done            chan agents.Result
	session         *tuiSession
	ch              *channel
	handoffRequired bool
	cancel          context.CancelFunc
	// cancelOnce tears the process down exactly once; finishOnce delivers the
	// terminal result exactly once. They must stay separate: cancellation can
	// happen before or after the run loop finishes.
	cancelOnce sync.Once
	finishOnce sync.Once
}

func newRun(ctx context.Context, session *tuiSession, ch *channel, handoffRequired bool) *run {
	ctx, cancel := context.WithCancel(ctx)
	r := &run{
		events:          make(chan agents.Event, 32),
		done:            make(chan agents.Result, 1),
		session:         session,
		ch:              ch,
		handoffRequired: handoffRequired,
		cancel:          cancel,
	}
	go r.loop(ctx)
	return r
}

func (r *run) Events() <-chan agents.Event { return r.events }
func (r *run) Wait() agents.Result         { return <-r.done }
func (r *run) Cancel() error {
	r.cancelOnce.Do(func() {
		_ = r.session.cancel()
		r.cancel()
	})
	return nil
}

// loop tails status.md, emits progress events, and completes when result.md
// appears or the TUI exits. Nothing here parses vendor JSON because the
// vendor emits none.
func (r *run) loop(ctx context.Context) {
	defer close(r.events)

	var offset int64
	started := time.Now()
	lastActivity := started
	var pendingFileResult string
	var pendingNativeResult string
	submitted := false
	acceptedLogged := false
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = r.session.cancel()
			r.finish(agents.Result{ExitCode: -1, Err: ctx.Err(), Error: ctx.Err().Error()})
			return
		case err := <-r.session.waitExit:
			r.ch.trace("tui.exited error=%q", err)
			// Flush status lines written before exit, then settle the outcome:
			// result.md wins if the agent wrote it just before exiting; only a
			// missing result.md means the run died mid-task.
			r.emitStatus(&offset)
			result := agents.Result{ExitCode: 0}
			if response, readErr := r.ch.readResult(); readErr == nil {
				result.Response = strings.TrimSpace(response)
				if result.Response == "" {
					result.Err = errNoResult
					result.Error = errNoResult.Error()
				} else if err != nil {
					result.Err = err
					result.Error = err.Error()
				} else {
					r.events <- agents.Event{Kind: agents.EventResult, Type: "result.md", Message: result.Response}
				}
			} else {
				result.Err = errNoResult
				result.Error = errNoResult.Error()
				if err == nil {
					result.Err = errNoResult
					result.Error = errNoResult.Error()
				} else {
					result.Err = err
					result.Error = err.Error()
				}
				if r.session.cmd.ProcessState != nil {
					result.ExitCode = r.session.cmd.ProcessState.ExitCode()
				}
			}
			r.finish(result)
			return
		case err := <-r.session.fatal:
			r.ch.trace("tui.fatal error=%q", err)
			r.finish(agents.Result{ExitCode: -1, Err: err, Error: err.Error()})
			return
		case <-r.session.activity:
			lastActivity = time.Now()
		case <-ticker.C:
			if !r.session.sessionAlive() {
				err := fmt.Errorf("freebuff TUI session disappeared; pane=%q", r.session.paneSnapshot())
				r.ch.trace("tui.session.disappeared error=%q", err)
				r.finish(agents.Result{ExitCode: -1, Err: err, Error: err.Error()})
				return
			}
			if r.emitStatus(&offset) {
				lastActivity = time.Now()
			}
			if r.handoffRequired && !acceptedLogged && r.ch.accepted() {
				r.ch.trace("handoff.accepted path=%q", r.ch.AcceptedPath())
				acceptedLogged = true
			}
			if !submitted && (!r.handoffRequired || r.ch.accepted()) && r.ch.nativePromptSubmitted(started) {
				r.ch.trace("native.prompt.acknowledged")
				submitted = true
			}
			if r.handoffRequired && !r.ch.accepted() && time.Since(started) >= freebuffSubmissionTimeout {
				err := errFreebuffAcceptanceTimeout
				r.ch.trace("run.timeout kind=handoff_acceptance error=%q", err)
				r.finish(agents.Result{ExitCode: -1, Err: err, Error: err.Error()})
				return
			}
			if !r.handoffRequired && !submitted && time.Since(started) >= freebuffSubmissionTimeout {
				err := errFreebuffSubmissionTimeout
				r.ch.trace("run.timeout kind=prompt_submission error=%q", err)
				r.finish(agents.Result{ExitCode: -1, Err: err, Error: err.Error()})
				return
			}
			if time.Since(lastActivity) >= freebuffIdleTimeout {
				r.ch.trace("run.timeout kind=idle")
				r.finish(agents.Result{ExitCode: -1, Err: errFreebuffIdleTimeout, Error: errFreebuffIdleTimeout.Error()})
				return
			}
			if time.Since(started) >= freebuffMaxRuntime {
				r.ch.trace("run.timeout kind=max_runtime")
				r.finish(agents.Result{ExitCode: -1, Err: errFreebuffMaxRuntime, Error: errFreebuffMaxRuntime.Error()})
				return
			}
		}

		if !r.handoffRequired || r.ch.accepted() {
			if response, err := r.ch.readResult(); err == nil {
				response = strings.TrimSpace(response)
				if response != "" {
					if pendingFileResult == response {
						r.ch.trace("result.detected source=result.md bytes=%d", len(response))
						r.events <- agents.Event{Kind: agents.EventResult, Type: "result.md", Message: response}
						r.finish(agents.Result{ExitCode: 0, Response: response})
						return
					}
					pendingFileResult = response
				}
			} else {
				pendingFileResult = ""
			}
			// Planner-backed runs must complete through result.md. Native chat
			// text is useful as a submission acknowledgement, but it can be an
			// interrupted/partial model response and must not create a false
			// successful orchestration result.
			if !r.handoffRequired {
				if response := r.ch.nativeResponse(started); response != "" {
					if pendingNativeResult == response {
						r.ch.trace("result.detected source=chat-messages.json bytes=%d", len(response))
						r.events <- agents.Event{Kind: agents.EventResult, Type: "chat-messages.json", Message: response}
						r.finish(agents.Result{ExitCode: 0, Response: response})
						return
					}
					pendingNativeResult = response
				}
			}
		}
	}
}

// emitStatus tails new status.md content once per poll and emits it as a
// streamed message event, which the CLI prints and traces like any other
// adapter's progress text.
func (r *run) emitStatus(offset *int64) bool {
	data, newOffset, err := r.ch.statusFrom(*offset)
	if err != nil || data == "" {
		return false
	}
	*offset = newOffset
	r.ch.trace("status.read bytes=%d", len(data))
	r.events <- agents.Event{Kind: agents.EventMessage, Type: "status.md", Message: strings.TrimRight(data, "\n")}
	return true
}

var (
	errNoResult                  = errors.New("freebuff exited without writing result.md")
	errFreebuffSubmissionTimeout = errors.New("freebuff did not acknowledge the submitted prompt within 45s")
	errFreebuffAcceptanceTimeout = errors.New("freebuff did not create accepted.md for the planner handoff within 45s")
	errFreebuffIdleTimeout       = errors.New("freebuff produced no progress for 10m")
	errFreebuffMaxRuntime        = errors.New("freebuff exceeded the 45m runtime limit")
)

func (r *run) finish(result agents.Result) {
	r.finishOnce.Do(func() {
		if result.Err != nil {
			r.ch.trace("run.finished outcome=error error=%q", result.Err)
		} else {
			r.ch.trace("run.finished outcome=success response_bytes=%d", len(result.Response))
		}
		// Every terminal outcome owns the child process. This is especially
		// important when result.md wins before the interactive TUI exits.
		_ = r.session.cancel()
		result.SessionID = "freebuff:" + r.ch.Dir
		if result.Response == "" {
			if response, err := r.ch.readResult(); err == nil {
				result.Response = strings.TrimSpace(response)
			}
		}
		r.done <- result
		r.cancel()
	})
}
