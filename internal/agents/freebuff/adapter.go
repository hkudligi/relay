package freebuff

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
// plan, route, and trace Freebuff work like any other adapter. Freebuff is
// globally single-session, so the adapter serializes all runs with a
// user-scoped inter-process lock.
type Adapter struct{ Command string }

const (
	freebuffSubmissionTimeout  = 45 * time.Second
	freebuffStartupIdleTimeout = 2 * time.Minute
	freebuffIdleTimeout        = 10 * time.Minute
	freebuffMaxRuntime         = 45 * time.Minute
	// A long wall-clock gap means macOS likely suspended the process while the
	// laptop slept. Do not spend that interval on agent watchdog deadlines.
	freebuffWakeGap = 2 * time.Minute
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
		// Freebuff enforces one active session per account; the adapter's
		// user-scoped session lock also fails fast with ErrSessionConflict.
		SingleSession: true,
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
	lock, err := acquireSessionLock()
	if err != nil {
		return nil, err
	}
	keepLock := false
	defer func() {
		if !keepLock && lock != nil {
			lock.release()
		}
	}()
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
	if session.tmux == "" {
		ch.trace("tui.mode mode=pty warning=%q", session.startupWarning)
	} else {
		ch.trace("tui.mode mode=tmux session=%q", session.tmuxSession)
	}
	if session.visibleMirrorErr != nil {
		ch.trace("visible_terminal.error error=%q", session.visibleMirrorErr)
	} else if session.tmux != "" && runtime.GOOS == "darwin" && os.Getenv("RLY_FREEBUFF_VISIBLE") != "0" {
		ch.trace("visible_terminal.started tmux_session=%q mode=read-only", session.tmuxSession)
	}

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

	keepLock = true
	return newRun(ctx, session, ch, handoffRequired, lock), nil
}

// run streams progress from status.md and completes when result.md appears.
type run struct {
	events          chan agents.Event
	done            chan agents.Result
	session         *tuiSession
	ch              *channel
	handoffRequired bool
	lock            *sessionLock
	cancel          context.CancelFunc
	// cancelOnce tears the process down exactly once; finishOnce delivers the
	// terminal result exactly once. They must stay separate: cancellation can
	// happen before or after the run loop finishes.
	cancelOnce sync.Once
	finishOnce sync.Once
}

func newRun(ctx context.Context, session *tuiSession, ch *channel, handoffRequired bool, lock *sessionLock) *run {
	ctx, cancel := context.WithCancel(ctx)
	r := &run{
		events:          make(chan agents.Event, 32),
		done:            make(chan agents.Result, 1),
		session:         session,
		ch:              ch,
		handoffRequired: handoffRequired,
		lock:            lock,
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
	r.ch.trace("run.loop.started handoff_required=%t file_state=%s", r.handoffRequired, r.ch.fileState())
	r.emit(ctx, agents.Event{
		Kind:    agents.EventProgress,
		Type:    "freebuff.run.started",
		Message: "run " + filepath.Base(r.ch.Dir),
		Data: map[string]any{
			"run_id":      filepath.Base(r.ch.Dir),
			"channel_dir": r.ch.Dir,
		},
		Time: time.Now().UTC(),
	})
	lastActivity := started
	lastHeartbeat := started
	lastWallClock := time.Now().UTC()
	var pendingFileResult string
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
			r.ch.trace("tui.exited error=%q exit_code=%d file_state=%s diagnostic=%q", err, r.session.exitCode(), r.ch.fileState(), clipDiagnostic(r.session.diagnostic()))
			// Flush status lines written before exit, then settle the outcome:
			// result.md wins if the agent wrote it just before exiting; only a
			// missing result.md means the run died mid-task.
			r.emitStatus(ctx, &offset)
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
					r.emit(ctx, agents.Event{Kind: agents.EventResult, Type: "result.md", Message: result.Response, Data: map[string]any{"channel": "response", "path": r.ch.ResultPath()}, Time: time.Now().UTC()})
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
			// A session-ended screen can race with the agent's final result write;
			// let result.md win if it is already complete.
			if response, readErr := r.ch.readResult(); readErr == nil && strings.TrimSpace(response) != "" {
				response = strings.TrimSpace(response)
				r.ch.trace("result.detected source=result.md after_fatal=true bytes=%d", len(response))
				r.emit(ctx, agents.Event{Kind: agents.EventResult, Type: "result.md", Message: response, Data: map[string]any{"channel": "response", "path": r.ch.ResultPath()}, Time: time.Now().UTC()})
				r.finish(agents.Result{ExitCode: 0, Response: response})
				return
			}
			r.ch.trace("tui.fatal error=%q file_state=%s diagnostic=%q", err, r.ch.fileState(), clipDiagnostic(r.session.diagnostic()))
			r.finish(agents.Result{ExitCode: -1, Err: err, Error: err.Error()})
			return
		case <-r.session.activity:
			lastActivity = time.Now()
		case <-ticker.C:
			now := time.Now()
			wallNow := time.Now().UTC()
			if wallGap := wallNow.Sub(lastWallClock); wallGap >= freebuffWakeGap {
				started, lastActivity, lastHeartbeat = resetWatchdogsAfterSleep(started, lastActivity, lastHeartbeat, now, wallGap)
				r.ch.trace("system.wake detected sleep_gap=%s file_state=%s tui_alive=%t", wallGap.Round(time.Second), r.ch.fileState(), r.session.sessionAlive())
				r.emit(ctx, agents.Event{Kind: agents.EventProgress, Type: "system.wake", Message: fmt.Sprintf("resuming after %s sleep", wallGap.Round(time.Second)), Data: map[string]any{"sleep_gap": wallGap.String()}, Time: time.Now().UTC()})
			}
			lastWallClock = wallNow
			if !r.session.sessionAlive() {
				err := fmt.Errorf("freebuff TUI session disappeared; pane=%q", clipDiagnostic(r.session.paneSnapshot()))
				r.ch.trace("tui.session.disappeared error=%q file_state=%s diagnostic=%q", err, r.ch.fileState(), clipDiagnostic(r.session.diagnostic()))
				r.finish(agents.Result{ExitCode: -1, Err: err, Error: err.Error()})
				return
			}
			if r.emitStatus(ctx, &offset) {
				lastActivity = time.Now()
			}
			if r.handoffRequired && !acceptedLogged && r.ch.accepted() {
				r.ch.trace("handoff.accepted path=%q file_state=%s", r.ch.AcceptedPath(), r.ch.fileState())
				acceptedLogged = true
			}
			if !submitted && (!r.handoffRequired || r.ch.accepted()) {
				if recorded, detail := r.ch.nativePromptStatus(started); recorded {
					r.ch.trace("native.prompt.recorded detail=%q", detail)
					submitted = true
				}
			}
			if now.Sub(lastHeartbeat) >= 15*time.Second {
				nativeRecorded, nativeDetail := r.ch.nativePromptStatus(started)
				r.ch.trace("run.heartbeat elapsed=%s submitted=%t native_recorded=%t native_detail=%q accepted=%t file_state=%s tui_alive=%t tui_inactive=%s diagnostic=%q", now.Sub(started).Round(time.Second), submitted, nativeRecorded, nativeDetail, r.ch.accepted(), r.ch.fileState(), r.session.sessionAlive(), r.session.inactiveFor().Round(time.Second), clipDiagnostic(r.session.paneSnapshot()))
				lastHeartbeat = now
			}
			if r.handoffRequired && !r.ch.accepted() && time.Since(started) >= freebuffSubmissionTimeout {
				err := errFreebuffAcceptanceTimeout
				r.ch.trace("run.timeout kind=handoff_acceptance error=%q file_state=%s diagnostic=%q", err, r.ch.fileState(), clipDiagnostic(r.session.paneSnapshot()))
				r.finish(agents.Result{ExitCode: -1, Err: err, Error: err.Error()})
				return
			}
			if !r.handoffRequired && !submitted && time.Since(started) >= freebuffSubmissionTimeout {
				err := errFreebuffSubmissionTimeout
				r.ch.trace("run.timeout kind=prompt_submission error=%q file_state=%s diagnostic=%q", err, r.ch.fileState(), clipDiagnostic(r.session.paneSnapshot()))
				r.finish(agents.Result{ExitCode: -1, Err: err, Error: err.Error()})
				return
			}
			if time.Since(lastActivity) >= freebuffIdleTimeout {
				r.ch.trace("run.timeout kind=idle file_state=%s diagnostic=%q", r.ch.fileState(), clipDiagnostic(r.session.paneSnapshot()))
				r.finish(agents.Result{ExitCode: -1, Err: errFreebuffIdleTimeout, Error: errFreebuffIdleTimeout.Error()})
				return
			}
			if time.Since(started) >= freebuffMaxRuntime {
				r.ch.trace("run.timeout kind=max_runtime file_state=%s diagnostic=%q", r.ch.fileState(), clipDiagnostic(r.session.paneSnapshot()))
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
						r.emit(ctx, agents.Event{Kind: agents.EventResult, Type: "result.md", Message: response, Data: map[string]any{"channel": "response", "path": r.ch.ResultPath()}, Time: time.Now().UTC()})
						r.finish(agents.Result{ExitCode: 0, Response: response})
						return
					}
					pendingFileResult = response
				}
			} else {
				pendingFileResult = ""
			}
		}
	}
}

func resetWatchdogsAfterSleep(started, lastActivity, lastHeartbeat, now time.Time, sleepGap time.Duration) (time.Time, time.Time, time.Time) {
	if sleepGap < freebuffWakeGap {
		return started, lastActivity, lastHeartbeat
	}
	return started.Add(sleepGap), now, now
}

// emitStatus tails new status.md content once per poll and emits it as a
// streamed message event, which the CLI prints and traces like any other
// adapter's progress text.
func (r *run) emitStatus(ctx context.Context, offset *int64) bool {
	data, newOffset, err := r.ch.statusFrom(*offset)
	if err != nil || data == "" {
		return false
	}
	*offset = newOffset
	r.ch.trace("status.read bytes=%d", len(data))
	return r.emit(ctx, agents.Event{Kind: agents.EventProgress, Type: "status.md", Message: strings.TrimRight(data, "\n"), Data: map[string]any{"channel": "status", "path": r.ch.StatusPath()}, Time: time.Now().UTC()})
}

func (r *run) emit(ctx context.Context, event agents.Event) bool {
	select {
	case r.events <- event:
		return true
	case <-ctx.Done():
		return false
	}
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
		if r.lock != nil {
			r.lock.release()
			r.lock = nil
		}
		if result.Err != nil {
			r.ch.trace("run.finished outcome=error error=%q file_state=%s", result.Err, r.ch.fileState())
		} else {
			r.ch.trace("run.finished outcome=success response_bytes=%d file_state=%s", len(result.Response), r.ch.fileState())
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

func clipDiagnostic(value string) string {
	value = strings.TrimSpace(stripANSI(value))
	if len(value) > 1200 {
		return value[len(value)-1200:]
	}
	return value
}
