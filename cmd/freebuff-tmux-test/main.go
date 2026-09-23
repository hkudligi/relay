package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultPrompt = "Create .rly/tmux-validation.txt. Ensure .rly exists, write exactly these three lines to the file: TMUX_OK, handoff-received, execution-completed. Verify the file has exactly three lines and report its absolute path."

func main() {
	workspace, prompt := arguments()
	freebuff := os.Getenv("FREEBUFF_BIN")
	if freebuff == "" {
		freebuff = "/opt/homebrew/bin/freebuff"
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		fatal("tmux is required; install it with: brew install tmux")
	}
	if info, err := os.Stat(freebuff); err != nil || info.IsDir() {
		fatal("Freebuff executable not found: " + freebuff)
	}

	session := "rly-freebuff-manual-" + strconv.Itoa(os.Getpid())
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		fatal("resolve workspace: " + err.Error())
	}
	runDir := filepath.Join(workspace, ".rly", "freebuff", "manual-"+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		fatal("create trace directory: " + err.Error())
	}
	tracePath := filepath.Join(runDir, "trace.log")
	trace := func(format string, args ...any) {
		f, err := os.OpenFile(tracePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
	}
	trace("run.created workspace=%q prompt_bytes=%d", workspace, len(prompt))
	if os.Getenv("FREEBUFF_UI") == "terminal" {
		runTerminal(workspace, freebuff, prompt, trace)
		return
	}

	create := exec.Command("tmux", "new-session", "-d", "-s", session, "-c", workspace,
		"env", "TERM=xterm-256color", "NO_COLOR=", "CI=", freebuff, "--cwd", workspace)
	create.Stdout = os.Stdout
	create.Stderr = os.Stderr
	if err := create.Run(); err != nil {
		trace("tmux.create.error error=%q", err)
		fatal("start tmux session: " + err.Error())
	}
	trace("tmux.created session=%q", session)

	fmt.Printf("Starting Freebuff in tmux session: %s\n", session)
	fmt.Println("Waiting 5 seconds before sending Enter...")
	time.Sleep(5 * time.Second)
	if err := sendKeys(session, "Enter"); err != nil {
		trace("input.enter.error phase=activation error=%q", err)
		fatal("send initial Enter: " + err.Error())
	}
	trace("input.enter.sent phase=activation")

	fmt.Println("Sent Enter to Freebuff. Waiting 2 seconds for the chat input...")
	time.Sleep(2 * time.Second)
	if err := sendLiteral(session, prompt); err != nil {
		trace("input.prompt.error error=%q", err)
		fatal("send prompt: " + err.Error())
	}
	trace("input.prompt.sent bytes=%d", len(prompt))
	if err := sendKeys(session, "Enter"); err != nil {
		trace("input.enter.error phase=submit error=%q", err)
		fatal("submit prompt: " + err.Error())
	}
	trace("input.enter.sent phase=submit")

	fmt.Println("Sent prompt: " + prompt)
	fmt.Println("Attaching to the tmux session now.")
	fmt.Println("From another terminal, inspect the screen with:")
	fmt.Printf("tmux capture-pane -p -t %s\n", session)
	fmt.Printf("Trace log: %s\n", tracePath)
	fmt.Println("Detach without stopping Freebuff with: Ctrl-b, then d")

	attach := exec.Command("tmux", "attach-session", "-t", session)
	attach.Stdin = os.Stdin
	attach.Stdout = os.Stdout
	attach.Stderr = os.Stderr
	if err := attach.Run(); err != nil {
		trace("tmux.attach.error error=%q", err)
		fatal("attach tmux session: " + err.Error())
	}
	trace("tmux.attach.exited")
}

func arguments() (string, string) {
	workspace := "."
	prompt := defaultPrompt
	if len(os.Args) > 1 && strings.TrimSpace(os.Args[1]) != "" {
		workspace = os.Args[1]
	}
	if len(os.Args) > 2 && strings.TrimSpace(os.Args[2]) != "" {
		prompt = os.Args[2]
	}
	return workspace, prompt
}

func sendLiteral(session, value string) error {
	return exec.Command("tmux", "send-keys", "-t", session, "-l", value).Run()
}

func sendKeys(session, key string) error {
	return exec.Command("tmux", "send-keys", "-t", session, key).Run()
}

func runTerminal(workspace, freebuff, prompt string, trace func(string, ...any)) {
	command := fmt.Sprintf("cd %s && env TERM=xterm-256color NO_COLOR= CI= %s --cwd %s", shellQuote(workspace), shellQuote(freebuff), shellQuote(workspace))
	script := fmt.Sprintf(`tell application "Terminal"
activate
if (count windows) = 0 then
    do script ""
end if
do script %s in front window
end tell`, appleQuote(command))
	if err := runAppleScript(script); err != nil {
		trace("terminal.tab.error phase=launch error=%q", err)
		fatal("launch Terminal tab: " + err.Error())
	}
	trace("terminal.tab.created")

	fmt.Println("Started Freebuff in a new Terminal tab.")
	fmt.Println("Waiting 5 seconds before sending Enter...")
	time.Sleep(5 * time.Second)
	if err := terminalKeyReturnRetry(trace, "activation"); err != nil {
		trace("terminal.input.error phase=activation error=%q", err)
		fatal("send initial Enter to Terminal: " + err.Error())
	}
	trace("terminal.input.enter.sent phase=activation")

	fmt.Println("Sent Enter. Waiting 2 seconds for the chat input...")
	time.Sleep(2 * time.Second)
	if err := terminalPaste(prompt); err != nil {
		trace("terminal.input.error phase=prompt error=%q", err)
		fatal("paste prompt into Terminal: " + err.Error())
	}
	trace("terminal.input.prompt.sent bytes=%d", len(prompt))
	if err := terminalKeyReturnRetry(trace, "submit"); err != nil {
		trace("terminal.input.error phase=submit error=%q", err)
		fatal("submit prompt in Terminal: " + err.Error())
	}
	trace("terminal.input.enter.sent phase=submit")
	fmt.Println("Prompt submitted in the Terminal tab.")
}

func runAppleScript(script string) error {
	cmd := exec.Command("osascript", "-e", script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func terminalKeyReturn() error {
	return runAppleScript(`tell application "System Events" to tell process "Terminal" to key code 36`)
}

func terminalKeyReturnRetry(trace func(string, ...any), phase string) error {
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := terminalKeyReturn(); err == nil {
			trace("terminal.input.enter.sent phase=%s attempt=%d", phase, attempt)
			return nil
		} else {
			last = err
			trace("terminal.input.retry phase=%s attempt=%d error=%q", phase, attempt, err)
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}
	return last
}

func terminalPaste(value string) error {
	copy := exec.Command("pbcopy")
	copy.Stdin = strings.NewReader(value)
	if err := copy.Run(); err != nil {
		return err
	}
	return runAppleScript(`tell application "System Events" to tell process "Terminal" to keystroke "v" using command down`)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func appleQuote(value string) string {
	return strconv.Quote(value)
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
