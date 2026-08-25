// Package tmuxdrv drives tmux as the terminal backend for agentmesh: every
// agent lives in its own tmux session, so terminal rendering, resizing,
// scrollback and attaching are all handled by tmux itself — mature, correct,
// and already installed on almost every Linux box — instead of agentmesh
// reimplementing a PTY manager and its own attach protocol.
package tmuxdrv

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Available checks that the tmux binary exists in PATH.
func Available() error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return errors.New("tmux não encontrado no PATH — instale com o gerenciador de pacotes do seu sistema (ex: sudo apt install tmux / sudo dnf install tmux / brew install tmux)")
	}
	return nil
}

func run(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// NewSession starts command+args as a detached tmux session named
// sessionName, in cwd, with the given extra "KEY=value" environment
// entries. The command is wrapped with the `env` utility (present on
// every Unix system) instead of a shell string, so argv stays an argv —
// no quoting hazard even when an arg has spaces or quotes in it (e.g.
// claude's --append-system-prompt "long text with spaces").
func NewSession(sessionName, cwd, command string, args []string, env []string) error {
	inner := append([]string{"env"}, env...)
	inner = append(inner, command)
	inner = append(inner, args...)

	tmuxArgs := append([]string{
		"new-session", "-d", "-s", sessionName, "-c", cwd,
		"-x", "220", "-y", "50", "--",
	}, inner...)

	_, err := run(tmuxArgs...)
	return err
}

// HasSession reports whether a tmux session with that name currently exists.
func HasSession(sessionName string) bool {
	return exec.Command("tmux", "has-session", "-t", sessionName).Run() == nil
}

// KillSession terminates a session (and the process tree in it). Not
// finding the session is not an error — it's already gone.
func KillSession(sessionName string) error {
	out, err := run("kill-session", "-t", sessionName)
	if err != nil && strings.Contains(out, "session not found") {
		return nil
	}
	return err
}

// CapturePane returns the CURRENTLY VISIBLE pane content as plain text.
// Because tmux does the full terminal emulation internally, this is
// clean, correctly wrapped text exactly as a human would see it — not raw
// ANSI bytes that need to be stripped and re-guessed at.
func CapturePane(sessionName string) (string, error) {
	out, err := exec.Command("tmux", "capture-pane", "-p", "-t", sessionName).Output()
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane -t %s: %w", sessionName, err)
	}
	return string(out), nil
}

// SendLiteral types text into the session verbatim (no key-name
// interpretation) — this is how a message body gets delivered.
func SendLiteral(sessionName, text string) error {
	_, err := run("send-keys", "-t", sessionName, "-l", "--", text)
	return err
}

// SendKey sends one named key (Enter, C-c, Escape, Up, ...) — this is how
// a confirm/submit keystroke, or a boot-screen dismissal, gets delivered.
func SendKey(sessionName, key string) error {
	_, err := run("send-keys", "-t", sessionName, key)
	return err
}

// SetStatusBar points a session's status-right at `agentmesh usage
// --oneline`, refreshed periodically — this is the "custo ao vivo, no
// rodapé do terminal" panel: tmux already re-runs a `#(...)` command on its
// own schedule, so no extra process or polling loop is needed on
// agentmesh's side. Best-effort: a session someone customized by hand
// isn't worth failing the spawn over.
func SetStatusBar(sessionName string) error {
	if _, err := run("set-option", "-t", sessionName, "status-interval", "20"); err != nil {
		return err
	}
	if _, err := run("set-option", "-t", sessionName, "status-right-length", "90"); err != nil {
		return err
	}
	_, err := run("set-option", "-t", sessionName, "status-right", "#(agentmesh usage --oneline 2>/dev/null)")
	return err
}

// AttachArgs returns the argv (without the leading "tmux") to exec for an
// interactive attach. readOnly uses tmux's own read-only client mode
// (`-r`) — no custom protocol needed, tmux enforces it natively.
func AttachArgs(sessionName string, readOnly bool) []string {
	if readOnly {
		return []string{"attach-session", "-r", "-t", sessionName}
	}
	return []string{"attach-session", "-t", sessionName}
}
