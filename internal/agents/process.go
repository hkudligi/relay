package agents

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type normalizer func([]byte) (Event, bool, error)

// Normalizer converts one JSONL record from a vendor CLI into a common event.
type Normalizer func([]byte) (Event, bool, error)

type processRun struct {
	events <-chan Event
	done   <-chan Result
	cancel context.CancelFunc
	once   sync.Once
}

func (r *processRun) Events() <-chan Event { return r.events }
func (r *processRun) Wait() Result         { return <-r.done }
func (r *processRun) Cancel() error {
	r.once.Do(r.cancel)
	return nil
}

func sendEvent(ctx context.Context, events chan<- Event, event Event) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func applyEvent(result *Result, event Event) (sawResult bool) {
	if event.SessionID != "" {
		result.SessionID = event.SessionID
	}
	if event.Kind == EventMessage && event.Message != "" {
		result.Response = event.Message
	}
	if event.Kind == EventError && event.Message != "" {
		result.Err = errors.New(event.Message)
	}
	if event.Kind == EventResult {
		if event.Message != "" {
			result.Response = event.Message
		}
		result.Usage = event.Usage
		return true
	}
	return false
}

func startProcess(parent context.Context, command string, args []string, dir string, normalize normalizer) (Run, error) {
	ctx, cancel := context.WithCancel(parent)
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(nil)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("agent stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("agent stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start %s: %w", command, err)
	}

	events := make(chan Event, 32)
	done := make(chan Result, 1)
	go collectProcess(ctx, cmd, stdout, stderr, normalize, events, done)
	return &processRun{events: events, done: done, cancel: cancel}, nil
}

// StartProcess is the shared subprocess implementation used by adapter packages.
func StartProcess(parent context.Context, command string, args []string, dir string, normalize Normalizer) (Run, error) {
	return startProcess(parent, command, args, dir, normalizer(normalize))
}

// StartProcessInTerminal executes a JSONL-capable adapter in a real terminal
// tab when the host can create one, while preserving rly's structured event
// stream by tailing files written by the terminal-owned process.
func StartProcessInTerminal(parent context.Context, command string, args []string, dir string, normalize Normalizer) (Run, error) {
	if runtime.GOOS != "darwin" {
		return startProcess(parent, command, args, dir, normalizer(normalize))
	}
	if _, err := exec.LookPath("osascript"); err != nil {
		return startProcess(parent, command, args, dir, normalizer(normalize))
	}
	return startTerminalProcess(parent, command, args, dir, normalizer(normalize))
}

func collectProcess(ctx context.Context, cmd *exec.Cmd, stdout, stderr io.Reader, normalize normalizer, events chan<- Event, done chan<- Result) {
	defer close(events)
	defer close(done)
	var stderrBuf boundedBuffer
	stderrDone := make(chan struct{})
	go func() { _, _ = io.Copy(&stderrBuf, stderr); close(stderrDone) }()

	result := Result{ExitCode: -1}
	sawResult := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		event, ok, err := normalize(bytes.Clone(scanner.Bytes()))
		if err != nil {
			result.Err = err
			if !sendEvent(ctx, events, Event{Kind: EventError, Type: "normalize", Message: err.Error(), Time: time.Now().UTC()}) {
				break
			}
			continue
		}
		if !ok {
			continue
		}
		event.Time = time.Now().UTC()
		if event.Kind == EventError && strings.TrimSpace(event.Message) == "" {
			event.Message = "agent reported an error"
		}
		sawResult = applyEvent(&result, event) || sawResult
		if event.Kind == EventError {
			// Provider failures such as quota exhaustion are terminal. Some
			// CLIs (notably agy) report the failure but keep their language
			// server/session alive, so waiting for normal process exit can add
			// a large and needless delay.
			sendEvent(ctx, events, event)
			_ = cmd.Process.Kill()
			break
		}
		if !sendEvent(ctx, events, event) {
			break
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	<-stderrDone
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if scanErr != nil {
		result.Err = fmt.Errorf("read agent output: %w", scanErr)
	}
	if waitErr != nil && result.Err == nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			result.Err = context.Canceled
		} else {
			result.Err = fmt.Errorf("agent exited: %w", waitErr)
		}
	}
	if waitErr == nil && result.Err == nil && !sawResult {
		result.Err = errors.New("agent exited without a terminal result event")
	}
	if result.Err != nil && stderrBuf.String() != "" {
		result.Err = fmt.Errorf("%w: %s", result.Err, strings.TrimSpace(stderrBuf.String()))
	}
	if result.Err != nil {
		result.Error = result.Err.Error()
	}
	done <- result
}

type boundedBuffer struct{ data []byte }

const maxDiagnosticBytes = 64 * 1024

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.data = append(b.data, p...)
	if len(b.data) > maxDiagnosticBytes {
		b.data = b.data[len(b.data)-maxDiagnosticBytes:]
	}
	return n, nil
}
func (b *boundedBuffer) String() string { return string(b.data) }

type terminalRun struct {
	events <-chan Event
	done   <-chan Result
	cancel context.CancelFunc
	once   sync.Once
}

func (r *terminalRun) Events() <-chan Event { return r.events }
func (r *terminalRun) Wait() Result         { return <-r.done }
func (r *terminalRun) Cancel() error {
	r.once.Do(r.cancel)
	return nil
}

func startTerminalProcess(parent context.Context, command string, args []string, dir string, normalize normalizer) (Run, error) {
	ctx, cancel := context.WithCancel(parent)
	lifecycleDir, err := newTerminalLifecycleDir(dir)
	if err != nil {
		cancel()
		return nil, err
	}
	paths := terminalLifecyclePaths{
		dir:     lifecycleDir,
		script:  filepath.Join(lifecycleDir, "run.sh"),
		stdout:  filepath.Join(lifecycleDir, "stdout.jsonl"),
		stderr:  filepath.Join(lifecycleDir, "stderr.log"),
		display: filepath.Join(lifecycleDir, "display.log"),
		pid:     filepath.Join(lifecycleDir, "pid"),
		exit:    filepath.Join(lifecycleDir, "exit"),
		state:   filepath.Join(lifecycleDir, "state.json"),
	}
	started := time.Now().UTC()
	if err := writeTerminalLifecycleState(paths, terminalLifecycleState{
		Version:      1,
		Status:       "starting",
		Command:      command,
		Args:         append([]string(nil), args...),
		Workspace:    dir,
		LifecycleDir: paths.dir,
		Paths:        paths.relativePaths(),
		StartedAt:    started,
		UpdatedAt:    started,
	}); err != nil {
		cancel()
		return nil, err
	}
	if err := writeTerminalScript(paths, command, args, dir); err != nil {
		_ = updateTerminalLifecycleState(paths, "failed", Result{ExitCode: -1, Err: err, Error: err.Error()})
		cancel()
		return nil, err
	}
	if err := openTerminalScript(ctx, paths.script); err != nil {
		_ = updateTerminalLifecycleState(paths, "failed", Result{ExitCode: -1, Err: err, Error: err.Error()})
		cancel()
		return nil, err
	}
	events := make(chan Event, 32)
	done := make(chan Result, 1)
	go collectTerminalProcess(ctx, paths, normalize, events, done)
	return &terminalRun{events: events, done: done, cancel: cancel}, nil
}

type terminalLifecyclePaths struct {
	dir     string
	script  string
	stdout  string
	stderr  string
	display string
	pid     string
	exit    string
	state   string
}

func (p terminalLifecyclePaths) relativePaths() map[string]string {
	return map[string]string{
		"script":  filepath.Base(p.script),
		"stdout":  filepath.Base(p.stdout),
		"stderr":  filepath.Base(p.stderr),
		"display": filepath.Base(p.display),
		"pid":     filepath.Base(p.pid),
		"exit":    filepath.Base(p.exit),
		"state":   filepath.Base(p.state),
	}
}

type terminalLifecycleState struct {
	Version      int               `json:"version"`
	Status       string            `json:"status"`
	Command      string            `json:"command"`
	Args         []string          `json:"args,omitempty"`
	Workspace    string            `json:"workspace"`
	LifecycleDir string            `json:"lifecycle_dir"`
	Paths        map[string]string `json:"paths"`
	SessionID    string            `json:"session_id,omitempty"`
	ExitCode     int               `json:"exit_code,omitempty"`
	Error        string            `json:"error,omitempty"`
	StartedAt    time.Time         `json:"started_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	CompletedAt  *time.Time        `json:"completed_at,omitempty"`
}

func newTerminalLifecycleDir(workspace string) (string, error) {
	base := workspace
	if strings.TrimSpace(base) != "" {
		base = filepath.Join(base, ".rly", "terminal")
		if err := os.MkdirAll(base, 0o755); err != nil {
			return "", fmt.Errorf("create terminal lifecycle root: %w", err)
		}
		return os.MkdirTemp(base, "run-*")
	}
	return os.MkdirTemp("", "rly-terminal-*")
}

func writeTerminalScript(paths terminalLifecyclePaths, command string, args []string, dir string) error {
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set +e\n")
	fmt.Fprintf(&b, "cd %s || exit 127\n", shellQuote(dir))
	fmt.Fprintf(&b, "printf 'rly agent terminal started\\nworkspace: %%s\\ncommand: %%s\\n\\n' %s %s\n", shellQuote(dir), shellQuote(command))
	fmt.Fprintf(&b, ": > %s\n", shellQuote(paths.stdout))
	fmt.Fprintf(&b, ": > %s\n", shellQuote(paths.stderr))
	fmt.Fprintf(&b, ": > %s\n", shellQuote(paths.display))
	b.WriteString("(")
	b.WriteString(shellQuote(command))
	for _, arg := range args {
		b.WriteByte(' ')
		b.WriteString(shellQuote(arg))
	}
	fmt.Fprintf(&b, " > %s 2> %s) &\n", shellQuote(paths.stdout), shellQuote(paths.stderr))
	b.WriteString("child=$!\n")
	fmt.Fprintf(&b, "echo $child > %s\n", shellQuote(paths.pid))
	// Keep structured JSONL files private while showing a concise rendering in
	// the visible Terminal tab.
	fmt.Fprintf(&b, "tail -f %s &\n", shellQuote(paths.display))
	b.WriteString("stdout_viewer=$!\n")
	b.WriteString("wait $child\n")
	b.WriteString("code=$?\n")
	b.WriteString("kill $stdout_viewer 2>/dev/null\n")
	b.WriteString("wait $stdout_viewer 2>/dev/null\n")
	fmt.Fprintf(&b, "echo $code > %s\n", shellQuote(paths.exit))
	b.WriteString("printf '\\nrly agent terminal finished with exit code %s\\n' \"$code\"\n")
	b.WriteString("if [ \"${RLY_KEEP_AGENT_TABS:-1}\" = \"1\" ]; then printf 'Press return to close this tab...'; read _; fi\n")
	if err := os.WriteFile(paths.script, []byte(b.String()), 0o700); err != nil {
		return fmt.Errorf("write terminal script: %w", err)
	}
	return nil
}

func writeTerminalLifecycleState(paths terminalLifecyclePaths, state terminalLifecycleState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode terminal lifecycle state: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(paths.dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("create terminal lifecycle state: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write terminal lifecycle state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close terminal lifecycle state: %w", err)
	}
	if err := os.Rename(tmpName, paths.state); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace terminal lifecycle state: %w", err)
	}
	return nil
}

func updateTerminalLifecycleState(paths terminalLifecyclePaths, status string, result Result) error {
	data, err := os.ReadFile(paths.state)
	if err != nil {
		return err
	}
	var state terminalLifecycleState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("read terminal lifecycle state: %w", err)
	}
	now := time.Now().UTC()
	state.Status = status
	state.UpdatedAt = now
	if status != "running" && status != "starting" {
		state.CompletedAt = &now
	}
	if result.SessionID != "" {
		state.SessionID = result.SessionID
	}
	if result.ExitCode >= 0 {
		state.ExitCode = result.ExitCode
	}
	if result.Error != "" {
		state.Error = result.Error
	} else if result.Err != nil {
		state.Error = result.Err.Error()
	}
	return writeTerminalLifecycleState(paths, state)
}

func openTerminalScript(ctx context.Context, script string) error {
	appleScript := fmt.Sprintf(`tell application "Terminal"
	activate
	if (count of windows) = 0 then
		do script "exec /bin/sh %s" in front window
	else
		tell application "System Events" to tell process "Terminal"
			keystroke "t" using command down
		end tell
		delay 0.75
		do script "exec /bin/sh %s" in front window
	end if
	activate
end tell`, appleScriptQuote(script), appleScriptQuote(script))
	cmd := exec.CommandContext(ctx, "osascript", "-e", appleScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("open Terminal tab: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func collectTerminalProcess(ctx context.Context, paths terminalLifecyclePaths, normalize normalizer, events chan<- Event, done chan<- Result) {
	defer close(events)
	defer close(done)
	_ = updateTerminalLifecycleState(paths, "running", Result{ExitCode: -1})
	if !sendEvent(ctx, events, Event{Kind: EventProgress, Type: "terminal.started", Message: paths.dir, Data: map[string]any{"lifecycle_dir": paths.dir, "state_path": paths.state}, Time: time.Now().UTC()}) {
		err := ctx.Err()
		if err == nil {
			err = context.Canceled
		}
		result := Result{ExitCode: -1, Err: err, Error: err.Error()}
		_ = updateTerminalLifecycleState(paths, "canceled", result)
		done <- result
		return
	}

	result := Result{ExitCode: -1}
	sawResult := false
	var offset int64
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			killTerminalPID(paths.pid)
			result.Err = ctx.Err()
			result.Error = ctx.Err().Error()
			_ = updateTerminalLifecycleState(paths, "canceled", result)
			done <- result
			return
		case <-ticker.C:
			var terminal bool
			var err error
			result, sawResult, offset, terminal, err = readTerminalEvents(ctx, paths, normalize, events, result, sawResult, offset)
			if err != nil {
				if ctx.Err() != nil {
					killTerminalPID(paths.pid)
					result.Err = ctx.Err()
				} else {
					result.Err = err
				}
				result.Error = result.Err.Error()
				_ = updateTerminalLifecycleState(paths, "failed", result)
				done <- result
				return
			}
			if result.Err != nil {
				killTerminalPID(paths.pid)
				result.Error = result.Err.Error()
				_ = updateTerminalLifecycleState(paths, "failed", result)
				done <- result
				return
			}
			if terminal {
				if result.ExitCode == 0 && result.Err == nil && !sawResult {
					result.Err = errors.New("agent exited without a terminal result event")
				}
				if result.Err != nil {
					result.Err = withTerminalStderr(result.Err, paths.stderr)
					result.Error = result.Err.Error()
					_ = appendTerminalDisplay(paths.display, "✗ "+result.Err.Error())
				}
				_ = appendTerminalDisplay(paths.display, fmt.Sprintf("✓ agent finished (exit code %d)", result.ExitCode))
				status := "completed"
				if result.Err != nil {
					status = "failed"
				}
				_ = updateTerminalLifecycleState(paths, status, result)
				sendEvent(ctx, events, Event{Kind: EventProgress, Type: "terminal.completed", Data: map[string]any{"lifecycle_dir": paths.dir, "state_path": paths.state, "exit_code": result.ExitCode}, Time: time.Now().UTC()})
				done <- result
				return
			}
		}
	}
}

func readTerminalEvents(ctx context.Context, paths terminalLifecyclePaths, normalize normalizer, events chan<- Event, result Result, sawResult bool, offset int64) (Result, bool, int64, bool, error) {
	f, err := os.Open(paths.stdout)
	if err == nil {
		defer f.Close()
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return result, sawResult, offset, false, fmt.Errorf("read terminal output: %w", err)
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
		for scanner.Scan() {
			offset += int64(len(scanner.Bytes())) + 1
			event, ok, err := normalize(bytes.Clone(scanner.Bytes()))
			if err != nil {
				if !sendEvent(ctx, events, Event{Kind: EventError, Type: "normalize", Message: err.Error(), Time: time.Now().UTC()}) {
					return result, sawResult, offset, false, ctx.Err()
				}
				continue
			}
			if !ok {
				continue
			}
			event.Time = time.Now().UTC()
			_ = appendTerminalDisplay(paths.display, formatTerminalEvent(event))
			sawResult = applyEvent(&result, event) || sawResult
			if event.Kind == EventError && event.Message != "" {
				sendEvent(ctx, events, event)
				return result, sawResult, offset, false, nil
			}
			if !sendEvent(ctx, events, event) {
				return result, sawResult, offset, false, ctx.Err()
			}
		}
		if err := scanner.Err(); err != nil {
			return result, sawResult, offset, false, fmt.Errorf("read terminal output: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return result, sawResult, offset, false, fmt.Errorf("open terminal output: %w", err)
	}

	exitData, err := os.ReadFile(paths.exit)
	if err != nil {
		if os.IsNotExist(err) {
			return result, sawResult, offset, false, nil
		}
		return result, sawResult, offset, false, fmt.Errorf("read terminal exit status: %w", err)
	}
	var exitCode int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(exitData)), "%d", &exitCode); err != nil {
		return result, sawResult, offset, false, fmt.Errorf("parse terminal exit status: %w", err)
	}
	result.ExitCode = exitCode
	if exitCode != 0 && result.Err == nil {
		result.Err = withTerminalStderr(fmt.Errorf("agent exited with code %d", exitCode), paths.stderr)
	}
	return result, sawResult, offset, true, nil
}

func withTerminalStderr(err error, path string) error {
	if err == nil {
		return nil
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || len(data) == 0 {
		return err
	}
	text := strings.TrimSpace(string(data))
	if len(text) > maxDiagnosticBytes {
		text = text[len(text)-maxDiagnosticBytes:]
	}
	if text == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, text)
}

func formatTerminalEvent(event Event) string {
	switch event.Kind {
	case EventSession:
		return "● session started: " + event.SessionID
	case EventMessage:
		return "\n" + strings.TrimSpace(event.Message)
	case EventError:
		return "✗ " + strings.TrimSpace(event.Message)
	case EventResult:
		return "✓ turn completed"
	case EventProgress:
		if event.Message != "" {
			return "· " + strings.TrimSpace(event.Message)
		}
		return "· " + event.Type
	default:
		return ""
	}
}

func appendTerminalDisplay(path, line string) error {
	if strings.TrimSpace(line) == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, line)
	return err
}

func killTerminalPID(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil || pid <= 0 {
		return
	}
	_ = exec.Command("kill", "-TERM", fmt.Sprintf("%d", pid)).Run()
	pidText := fmt.Sprintf("%d", pid)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := exec.Command("kill", "-0", pidText).Run(); err != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := exec.Command("kill", "-KILL", pidText).Run(); err != nil {
		_ = exec.Command("kill", "-KILL", "-"+pidText).Run()
	}
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func appleScriptQuote(value string) string {
	return strings.ReplaceAll(shellQuote(value), `"`, `\"`)
}

func detect(ctx context.Context, name, command string) Installation {
	installation := Installation{Name: name, Command: command}
	path, err := exec.LookPath(command)
	if err != nil {
		installation.Error = err.Error()
		return installation
	}
	installation.Path = path
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, path, "--version")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			installation.Error = strings.TrimSpace(string(exitErr.Stderr))
		}
		if installation.Error == "" {
			installation.Error = err.Error()
		}
		return installation
	}
	installation.Available = true
	installation.Version = strings.TrimSpace(string(out))
	return installation
}

// Detect resolves a CLI and reads its version without starting an agent session.
func Detect(ctx context.Context, name, command string) Installation {
	return detect(ctx, name, command)
}
