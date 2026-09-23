package freebuff

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// tuiSession drives the Freebuff TUI over a PTY. The CLI has no headless
// mode, so rly runs it attached to a pseudo-terminal, waits for readiness,
// pastes a short pointer at the channel prompt.md, and dismisses the initial
// "Take over / Exit" instance-lock dialog if it appears.
type tuiSession struct {
	cmd          *exec.Cmd
	ptmx         *os.File
	ready        chan struct{}
	readyOnce    sync.Once
	waitExit     chan error
	fatal        chan error
	activity     chan struct{}
	mu           sync.Mutex
	transcript   strings.Builder
	written      bool
	cancelOnce   sync.Once
	processGroup bool
	tmux         string
	tmuxSession  string
	readyMode    string
	lastOutput   time.Time
}

var (
	errSessionDead   = errors.New("freebuff TUI exited before accepting the task")
	errReadinessGone = errors.New("freebuff TUI exited before becoming ready")
)

// startTUI launches the CLI inside a PTY without seeding a prompt: the
// Freebuff's argument parser has no prompt argument, so the TUI opens on its
// chat screen and rly pastes the task pointer afterwards.
func startTUI(command, workspace string) (*tuiSession, error) {
	if tmux, err := exec.LookPath("tmux"); err == nil {
		if session, err := startTmuxTUI(tmux, command, workspace); err == nil {
			return session, nil
		}
	}
	return startPTYTUI(command, workspace)
}

func startTmuxTUI(tmux, command, workspace string) (*tuiSession, error) {
	name := fmt.Sprintf("rly-freebuff-%d", time.Now().UnixNano())
	env := append(os.Environ(), "TERM=xterm-256color", "NO_COLOR=", "CI=")
	create := exec.Command(tmux, "new-session", "-d", "-s", name, "-c", filepath.Clean(workspace), command, "--cwd", workspace)
	create.Env = env
	if output, err := create.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("create tmux session: %w: %s", err, strings.TrimSpace(string(output)))
	}
	attach := exec.Command(tmux, "attach-session", "-t", name)
	attach.Env = env
	attach.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	ptmx, err := pty.Start(attach)
	if err != nil {
		_ = exec.Command(tmux, "kill-session", "-t", name).Run()
		return nil, fmt.Errorf("attach tmux session: %w", err)
	}
	s := newTUISession(attach, ptmx, true)
	s.tmux, s.tmuxSession = tmux, name
	s.startWatchers()
	return s, nil
}

func startPTYTUI(command, workspace string) (*tuiSession, error) {
	newCommand := func(withProcessGroup bool) *exec.Cmd {
		cmd := exec.Command(command, "--cwd", workspace)
		cmd.Dir = workspace
		cmd.Env = append(os.Environ(), "TERM=xterm-256color", "NO_COLOR=", "CI=")
		if withProcessGroup {
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		}
		return cmd
	}
	// Freebuff may spawn a node process tree. Put the TUI in its own process
	// group so a timeout cannot leave a locked/orphaned instance behind.
	processGroup := true
	cmd := newCommand(processGroup)
	ptmx, err := pty.Start(cmd)
	if err != nil && errors.Is(err, syscall.EPERM) {
		// Some restricted test/CI sandboxes disallow setpgid. Fall back to
		// killing the direct child rather than refusing to run Freebuff.
		processGroup = false
		cmd = newCommand(false)
		ptmx, err = pty.Start(cmd)
	}
	if err != nil {
		return nil, fmt.Errorf("start %s in pty: %w", command, err)
	}
	s := newTUISession(cmd, ptmx, processGroup)
	s.startWatchers()
	return s, nil
}

func newTUISession(cmd *exec.Cmd, ptmx *os.File, processGroup bool) *tuiSession {
	return &tuiSession{cmd: cmd, ptmx: ptmx, ready: make(chan struct{}), waitExit: make(chan error, 1), fatal: make(chan error, 1), activity: make(chan struct{}, 1), processGroup: processGroup, lastOutput: time.Now()}
}

func (s *tuiSession) startWatchers() {
	go s.watch()
	go func() { s.waitExit <- s.cmd.Wait() }()
	go func() {
		// Freebuff first renders a landing/model-picker screen. Activation can
		// race network/model loading, so retry at increasing intervals instead
		// of assuming one fixed startup delay.
		for _, delay := range []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second} {
			time.Sleep(delay)
			select {
			case <-s.ready:
				return
			default:
			}
			_ = s.sendKeys("\r")
		}
	}()
	go func() {
		// Some Freebuff releases render the chat input without a stable text
		// marker. The known-good manual sequence is: Enter at 5s, prompt at
		// about 7s. Use that as a bounded fallback only while the process is
		// still alive; detected markers always win.
		time.Sleep(7 * time.Second)
		select {
		case <-s.ready:
			return
		default:
		}
		if s.sessionAlive() {
			s.mu.Lock()
			s.readyMode = "timed-fallback"
			s.mu.Unlock()
			s.readyOnce.Do(func() { close(s.ready) })
		}
	}()
}

func (s *tuiSession) readinessMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readyMode == "" {
		return "detected"
	}
	return s.readyMode
}

func (s *tuiSession) inactiveFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastOutput)
}

func (s *tuiSession) sendKeys(value string) error {
	if s.tmux == "" {
		_, err := s.ptmx.Write([]byte(value))
		return err
	}
	cmd := exec.Command(s.tmux, "send-keys", "-t", s.tmuxSession, "-l", value)
	return cmd.Run()
}

func (s *tuiSession) sessionAlive() bool {
	if s.tmux == "" {
		if s.cmd.ProcessState != nil || s.cmd.Process == nil {
			return false
		}
		return s.cmd.Process.Signal(syscall.Signal(0)) == nil
	}
	return exec.Command(s.tmux, "has-session", "-t", s.tmuxSession).Run() == nil
}

func (s *tuiSession) paneSnapshot() string {
	if s.tmux == "" {
		return s.diagnostic()
	}
	output, err := exec.Command(s.tmux, "capture-pane", "-p", "-t", s.tmuxSession).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("tmux capture failed: %v", err)
	}
	value := strings.TrimSpace(stripANSI(string(output)))
	if len(value) > 6000 {
		value = value[len(value)-6000:]
	}
	return value
}

// watch consumes raw TUI output. It exists to (a) detect when the chat input
// is ready, (b) dismiss the single-instance dialog, and (c) drain the PTY so
// the child never blocks on a full terminal buffer.
func (s *tuiSession) watch() {
	buf := make([]byte, 16*1024)
	ready := false
	dialogDismissed := false
	var output strings.Builder
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			s.record(chunk)
			output.WriteString(stripANSI(chunk))
			if output.Len() > 32*1024 {
				trimmed := output.String()
				output.Reset()
				output.WriteString(trimmed[len(trimmed)-16*1024:])
			}
			if strings.Contains(output.String(), "Unhandled rejection") || strings.Contains(output.String(), "EPERM:") || strings.Contains(output.String(), "EACCES:") || strings.Contains(output.String(), "Another freebuff instance") || strings.Contains(output.String(), "Only one CLI per account") {
				s.signalFatal(fmt.Errorf("freebuff failed during startup: %s", s.diagnostic()))
				return
			}
			dialogSeen := strings.Contains(output.String(), "Take over")
			if dialogSeen && !dialogDismissed {
				// The dialog focuses "Take over" first. Select it so an
				// existing stale/session-owned TUI cannot block automation.
				_ = s.sendKeys("\r")
				dialogDismissed = true
			}
			// dialogSeen is intentionally based on the accumulated transcript,
			// so split reads are handled. Once dismissed, its old text must not
			// prevent the subsequent chat prompt from becoming ready.
			if !ready && (dialogDismissed || !dialogSeen) {
				// Require an actual chat input marker. Landing/model-picker
				// panels also contain box-drawing characters.
				// Freebuff also uses ›/❯ for model-picker rows. Do not treat a
				// generic glyph as the chat input; require the input placeholder
				// or the caret-plus-underscore shape used by the chat screen.
				if strings.Contains(output.String(), "› _") || strings.Contains(output.String(), "❯ _") || strings.Contains(output.String(), "Type a message") || strings.Contains(output.String(), "Ask anything") {
					s.mu.Lock()
					s.readyMode = "marker"
					s.mu.Unlock()
					ready = true
					s.readyOnce.Do(func() { close(s.ready) })
				}
			}
			s.signalActivity()
		}
		if err != nil {
			if !ready {
				// Close ready so paste does not block forever on a dead TUI.
				s.readyOnce.Do(func() { close(s.ready) })
			}
			return
		}
	}
}

// pasteWaitReady blocks until the TUI accepts input, then writes one line
// followed by Enter. Writes happen in small chunks because OpenTUI's input
// handling can drop very large pastes.
func (s *tuiSession) pastePrompt(ctx context.Context, text string) error {
	liveness := time.NewTicker(1 * time.Second)
	defer liveness.Stop()
	for {
		select {
		case <-s.ready:
			goto ready
		case err := <-s.waitExit:
			// The run loop still owns the terminal wait result.
			s.waitExit <- err
			return fmt.Errorf("%w: %s", errSessionDead, s.diagnostic())
		case err := <-s.fatal:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-liveness.C:
			if !s.sessionAlive() {
				return fmt.Errorf("%w: %s", errSessionDead, s.diagnostic())
			}
			if s.inactiveFor() >= freebuffStartupIdleTimeout {
				return fmt.Errorf("%w: terminal inactive for %s: %s", errReadinessGone, freebuffStartupIdleTimeout, s.diagnostic())
			}
		}
	}

ready:
	if s.written {
		return errors.New("freebuff task pointer already pasted")
	}
	s.written = true
	return s.writeLines(text)
}

func (s *tuiSession) writeLines(text string) error {
	for i := 0; i < len(text); i += 64 {
		end := i + 64
		if end > len(text) {
			end = len(text)
		}
		if err := s.sendKeys(text[i:end]); err != nil {
			return fmt.Errorf("type into freebuff TUI: %w", err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	// OpenTUI processes text input asynchronously. Send Enter as a separate
	// key event after the final chunk, matching Freebuff's own tmux helper.
	time.Sleep(300 * time.Millisecond)
	if err := s.sendKeys("\r"); err != nil {
		return fmt.Errorf("submit freebuff task pointer: %w", err)
	}
	return nil
}

// cancel terminates the TUI and its child shell tree.
func (s *tuiSession) cancel() error {
	s.cancelOnce.Do(func() {
		if s.tmux != "" {
			_ = exec.Command(s.tmux, "kill-session", "-t", s.tmuxSession).Run()
		}
		if s.ptmx != nil {
			// Ctrl-C first so the TUI can shut down cleanly, then close the PTY.
			_, _ = s.ptmx.Write([]byte("\x03"))
			_ = s.ptmx.Close()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			// Kill the whole process group; Process.Kill alone does not stop
			// child shells/node workers spawned by the TUI.
			if s.processGroup && s.cmd.Process.Pid > 0 {
				_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
			}
			_ = s.cmd.Process.Kill()
		}
	})
	return nil
}

var ansiEscape = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\))`)

func stripANSI(value string) string { return ansiEscape.ReplaceAllString(value, "") }

func (s *tuiSession) record(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastOutput = time.Now()
	s.transcript.WriteString(value)
	if s.transcript.Len() > 32*1024 {
		value := s.transcript.String()
		s.transcript.Reset()
		s.transcript.WriteString(value[len(value)-16*1024:])
	}
}

func (s *tuiSession) diagnostic() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := strings.TrimSpace(stripANSI(s.transcript.String()))
	if len(value) > 4000 {
		value = value[len(value)-4000:]
	}
	if value == "" {
		return "no terminal output captured"
	}
	return value
}

func (s *tuiSession) signalActivity() {
	select {
	case s.activity <- struct{}{}:
	default:
	}
}

func (s *tuiSession) signalFatal(err error) {
	select {
	case s.fatal <- err:
	default:
	}
}

// waitReady exposes readiness for tests and the adapter guard.
func (s *tuiSession) waitReady(ctx context.Context) error {
	select {
	case <-s.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
