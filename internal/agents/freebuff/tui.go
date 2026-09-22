package freebuff

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/creack/pty"
)

// tuiSession drives the Freebuff TUI over a PTY. The CLI has no headless
// mode, so rly runs it attached to a pseudo-terminal, waits for readiness,
// pastes a short pointer at the channel prompt.md, and dismisses the initial
// "Take over / Exit" instance-lock dialog if it appears.
type tuiSession struct {
	cmd     *exec.Cmd
	ptmx    *os.File
	ready   chan struct{}
	paste   func(string) error
	written bool
}

var (
	errSessionDead   = errors.New("freebuff TUI exited before accepting the task")
	errReadinessGone = errors.New("freebuff TUI exited before becoming ready")
)

// startTUI launches the CLI inside a PTY without seeding a prompt: the
// Freebuff argument parser has no prompt argument (verified against
// cli-args.ts in the freebuff repository), so the TUI opens on its chat
// screen and rly pastes the task pointer afterwards.
func startTUI(command, workspace string) (*tuiSession, error) {
	cmd := exec.Command(command)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "NO_COLOR=", "CI=")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("start %s in pty: %w", command, err)
	}
	s := &tuiSession{cmd: cmd, ptmx: ptmx, ready: make(chan struct{})}
	go s.watch()
	return s, nil
}

// watch consumes raw TUI output. It exists to (a) detect when the chat input
// is ready, (b) dismiss the single-instance dialog, and (c) drain the PTY so
// the child never blocks on a full terminal buffer.
func (s *tuiSession) watch() {
	buf := make([]byte, 16*1024)
	ready := false
	dialogSeen := false
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			if !ready {
				// The chat screen renders a box-drawing input frame once the
				// TUI is accepting keystrokes.
				if strings.ContainsAny(chunk, "│┌└─") {
					ready = true
					close(s.ready)
				}
			}
			if !dialogSeen && strings.Contains(chunk, "Take over") {
				dialogSeen = true
				// The dialog focuses "Take over" first; pressing Escape picks
				// "Exit"... so move right to "Exit" and confirm instead.
				_, _ = s.ptmx.Write([]byte("\x1b[C\r"))
			}
		}
		if err != nil {
			if !ready {
				// Close ready so paste does not block forever on a dead TUI.
				close(s.ready)
			}
			return
		}
	}
}

// pasteWaitReady blocks until the TUI accepts input, then writes one line
// followed by Enter. Writes happen in small chunks because OpenTUI's input
// handling can drop very large pastes.
func (s *tuiSession) pastePrompt(text string) error {
	select {
	case <-s.ready:
	case <-time.After(90 * time.Second):
		return errReadinessGone
	}
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
		if _, err := s.ptmx.Write([]byte(text[i:end])); err != nil {
			return fmt.Errorf("type into freebuff TUI: %w", err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	if _, err := s.ptmx.Write([]byte("\r")); err != nil {
		return fmt.Errorf("submit freebuff task pointer: %w", err)
	}
	return nil
}

// cancel terminates the TUI and its child shell tree.
func (s *tuiSession) cancel() error {
	if s.ptmx != nil {
		// Ctrl-C first so the TUI can shut down cleanly, then close the PTY.
		_, _ = s.ptmx.Write([]byte("\x03"))
		_ = s.ptmx.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	return nil
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
