package mesh

// turntimer.go — turn detection driven by tmux, not by raw PTY bytes.
//
// Every OutputMonitor polls `tmux capture-pane` for its session on a short
// tick. Because tmux does the full terminal emulation internally, what we
// get back is exactly the rendered screen as a human would read it — no
// ANSI stripping, no guessing at line-wrap boundaries, none of the
// garbled-text bugs a raw-byte approach was prone to.
//
// Status is derived the same way as before: per-provider regexes over the
// visible screen, gated by quiescence (the screen must stop changing for a
// short window before a READY status is trusted) — a screen that's still
// being redrawn, or that happens to show typed-but-not-yet-submitted text
// sitting next to a prompt glyph, must never be mistaken for "done".

import (
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NatanBack77/agentmesh/internal/tmuxdrv"
)

const (
	pollInterval    = 300 * time.Millisecond
	quiescenceDelay = 700 * time.Millisecond
)

type statusPatterns struct {
	idle       *regexp.Regexp
	processing *regexp.Regexp
	completed  *regexp.Regexp
	// dialog matches a blocking first-run screen (trust prompt, theme
	// picker, permission menu) that ALSO happens to contain a prompt-like
	// "❯ " glyph next to a numbered option — which would otherwise be
	// indistinguishable from the real idle input prompt by the idle
	// pattern alone. When dialog matches, the screen is reported UNKNOWN
	// (not idle) regardless of what else matches, so a caller waiting for
	// "ready" keeps waiting — and the boot-dismiss loop in `agentmesh
	// demo`/spawn keeps sending Enter — until the actual dialog is gone.
	dialog *regexp.Regexp
}

func providerPatterns(p Provider) statusPatterns {
	switch p {
	case ProviderClaudeCode:
		return statusPatterns{
			idle:       regexp.MustCompile(`[>❯][\s\xa0]`),
			processing: regexp.MustCompile(`(?m)^\s*[✶✢✽✻✳·*].*…`),
			completed:  regexp.MustCompile(`[✶✢✽✻✳][\s\xa0]+\w.*\d+s`),
			dialog:     regexp.MustCompile(`(?m)^\s*❯?\s*\d+\.\s|Enter to confirm|Esc to cancel|trust this folder|Security guide`),
		}
	case ProviderCodex:
		return statusPatterns{
			idle:       regexp.MustCompile(`^\s*[>$]\s`),
			processing: regexp.MustCompile(`(?i)thinking|processing|\.\.\.|generating`),
			completed:  regexp.MustCompile(`(?i)done|completed|finished`),
		}
	case ProviderOpenCode:
		return statusPatterns{
			idle:       regexp.MustCompile(`ctrl\+p\s+commands`),
			processing: regexp.MustCompile(`\besc interrupt\b`),
			completed:  regexp.MustCompile(`▣\s+\S+\s+·\s+.+?\s+·\s+(?:\d+m\s+)?\d+(?:\.\d+)?s`),
		}
	case ProviderGeminiCLI:
		return statusPatterns{
			idle:       regexp.MustCompile(`^\s*[>❯$]\s`),
			processing: regexp.MustCompile(`[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]`),
			completed:  regexp.MustCompile(`(?i)done|completed|\(\d+s\)`),
		}
	case ProviderShell:
		return statusPatterns{idle: regexp.MustCompile(`[$%#>❯]\s`)}
	default:
		return statusPatterns{
			idle:       regexp.MustCompile(`[$%#>❯]\s`),
			processing: regexp.MustCompile(`\.\.\.|processing|thinking`),
		}
	}
}

// OutputMonitor polls one tmux session's visible pane and derives its
// status. One is created per spawned agent.
type OutputMonitor struct {
	sessionName string
	patterns    statusPatterns

	// onStatusChange is called (outside any lock) with the detected status
	// and the pane text that produced it — the caller uses that text as the
	// handoff result, so no separate output-accumulation buffer is needed.
	onStatusChange func(Status, string)
	// onDead is called once if the tmux session stops existing.
	onDead func()

	// suppressReady is set true by deliver() for the duration of typing a
	// message + a short grace period after Enter. Without it, a message
	// still sitting typed-but-unsubmitted next to the idle prompt glyph
	// would quiesce and get mistaken for "turn finished".
	suppressReady atomic.Bool

	mu         sync.Mutex
	lastText   string
	lastChange time.Time

	stopOnce sync.Once
	stopCh   chan struct{}
}

func newOutputMonitor(sessionName string, p Provider, onChange func(Status, string), onDead func()) *OutputMonitor {
	return &OutputMonitor{
		sessionName:    sessionName,
		patterns:       providerPatterns(p),
		onStatusChange: onChange,
		onDead:         onDead,
		lastChange:     time.Now(),
		stopCh:         make(chan struct{}),
	}
}

// Start begins polling in the background.
func (m *OutputMonitor) Start() { go m.loop() }

// Stop ends polling. Safe to call more than once.
func (m *OutputMonitor) Stop() {
	m.stopOnce.Do(func() { close(m.stopCh) })
}

func (m *OutputMonitor) loop() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.tick()
		}
	}
}

func (m *OutputMonitor) tick() {
	text, err := tmuxdrv.CapturePane(m.sessionName)
	if err != nil {
		// The session is gone — the agent's process exited (or was killed
		// outside our own Kill path) and tmux tore the session down with it.
		if m.onDead != nil {
			m.onDead()
		}
		return
	}

	m.mu.Lock()
	changed := text != m.lastText
	if changed {
		m.lastText = text
		m.lastChange = time.Now()
	}
	quiet := time.Since(m.lastChange)
	m.mu.Unlock()

	if changed {
		// Mid-redraw: only PROCESSING is trusted here. A frame captured
		// while the screen is actively changing can be a partial redraw, or
		// — critically — our own message sitting typed-but-not-submitted
		// right next to a prompt glyph that would otherwise match "idle".
		if s := m.detectStatus(text); s == StatusProcessing {
			m.onStatusChange(s, text)
		}
		return
	}

	if quiet < quiescenceDelay || m.suppressReady.Load() {
		return
	}
	if s := m.detectStatus(text); s != StatusUnknown {
		m.onStatusChange(s, text)
	}
}

func (m *OutputMonitor) detectStatus(text string) Status {
	if m.patterns.dialog != nil && m.patterns.dialog.MatchString(text) {
		return StatusUnknown
	}
	if m.patterns.processing != nil && m.patterns.processing.MatchString(text) {
		return StatusProcessing
	}
	if m.patterns.completed != nil && m.patterns.completed.MatchString(text) {
		return StatusCompleted
	}
	if m.patterns.idle != nil && m.patterns.idle.MatchString(text) {
		return StatusIdle
	}
	return StatusUnknown
}
