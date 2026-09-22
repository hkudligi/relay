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
// coding agent: release 0.0.183 accepts no prompt argument and exposes no
// print, output-format, or stream-json flags. This adapter therefore runs the
// TUI on a pseudo-terminal and exchanges all task data through temporary
// markdown files in the workspace (prompt.md, status.md, result.md) so rly can
// plan, route, and trace Freebuff work like any other adapter.
type Adapter struct{ Command string }

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
	if err := ch.writePrompt(r.Prompt); err != nil {
		return nil, fmt.Errorf("write freebuff prompt: %w", err)
	}

	session, err := startTUI(a.Command, workspace)
	if err != nil {
		return nil, err
	}

	// The task pointer pasted into the TUI stays short: the full brief lives
	// in prompt.md where the agent reads it with file tools. A one-line paste
	// avoids OpenTUI large-paste truncation and keeps TUI history readable.
	pointer := fmt.Sprintf(
		"Read the file %s and complete the task it describes exactly as written, including the reporting protocol.",
		ch.PromptPath(),
	)
	if err := session.pastePrompt(pointer); err != nil {
		_ = session.cancel()
		return nil, err
	}

	return newRun(ctx, session, ch), nil
}

// run streams progress from status.md and completes when result.md appears.
type run struct {
	events  chan agents.Event
	done    chan agents.Result
	session *tuiSession
	ch      *channel
	cancel  context.CancelFunc
	// cancelOnce tears the process down exactly once; finishOnce delivers the
	// terminal result exactly once. They must stay separate: cancellation can
	// happen before or after the run loop finishes.
	cancelOnce sync.Once
	finishOnce sync.Once
}

func newRun(ctx context.Context, session *tuiSession, ch *channel) *run {
	ctx, cancel := context.WithCancel(ctx)
	r := &run{
		events:  make(chan agents.Event, 32),
		done:    make(chan agents.Result, 1),
		session: session,
		ch:      ch,
		cancel:  cancel,
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

	waitExit := make(chan error, 1)
	go func() { waitExit <- r.session.cmd.Wait() }()

	var offset int64
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = r.session.cancel()
			r.finish(agents.Result{ExitCode: -1, Err: ctx.Err(), Error: ctx.Err().Error()})
			return
		case err := <-waitExit:
			// Flush status lines written before exit, then settle the outcome:
			// result.md wins if the agent wrote it just before exiting; only a
			// missing result.md means the run died mid-task.
			r.emitStatus(&offset)
			result := agents.Result{ExitCode: 0}
			if response, readErr := r.ch.readResult(); readErr == nil {
				result.Response = strings.TrimSpace(response)
				r.events <- agents.Event{Kind: agents.EventResult, Type: "result.md", Message: result.Response}
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
		case <-ticker.C:
			r.emitStatus(&offset)
		}

		if response, err := r.ch.readResult(); err == nil {
			r.events <- agents.Event{Kind: agents.EventResult, Type: "result.md", Message: strings.TrimSpace(response)}
			r.finish(agents.Result{ExitCode: 0, Response: strings.TrimSpace(response)})
			return
		}
	}
}

// emitStatus tails new status.md content once per poll and emits it as a
// streamed message event, which the CLI prints and traces like any other
// adapter's progress text.
func (r *run) emitStatus(offset *int64) {
	data, newOffset, err := r.ch.statusFrom(*offset)
	if err != nil || data == "" {
		return
	}
	*offset = newOffset
	r.events <- agents.Event{Kind: agents.EventMessage, Type: "status.md", Message: strings.TrimRight(data, "\n")}
}

var errNoResult = errors.New("freebuff exited without writing result.md")

func (r *run) finish(result agents.Result) {
	r.finishOnce.Do(func() {
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
