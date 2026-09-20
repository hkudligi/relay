package agents

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
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
			events <- Event{Kind: EventError, Type: "normalize", Message: err.Error(), Time: time.Now().UTC()}
			continue
		}
		if !ok {
			continue
		}
		event.Time = time.Now().UTC()
		if event.SessionID != "" {
			result.SessionID = event.SessionID
		}
		if event.Kind == EventMessage && event.Message != "" {
			result.Response = event.Message
		}
		if event.Kind == EventError && event.Message != "" {
			result.Err = errors.New(event.Message)
			// Provider failures such as quota exhaustion are terminal. Some
			// CLIs (notably agy) report the failure but keep their language
			// server/session alive, so waiting for normal process exit can add
			// a large and needless delay.
			events <- event
			_ = cmd.Process.Kill()
			break
		}
		if event.Kind == EventResult {
			sawResult = true
			if event.Message != "" {
				result.Response = event.Message
			}
			result.Usage = event.Usage
			// Preserve an earlier provider error. Some CLIs emit a terminal
			// result after reporting a failed turn (notably on quota errors).
		}
		events <- event
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
