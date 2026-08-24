package mesh

// turntimer.go — output-based turn detection: regex over a rolling 8KB
// buffer + a 200ms quiescence timer decides when an agent went from
// PROCESSING back to IDLE/COMPLETED, purely by watching its PTY output.
// Works with any CLI tool, no cooperation required from the agent itself.

import (
	"bytes"
	"regexp"
	"sync"
	"time"
)

const (
	quiescenceDelay  = 200 * time.Millisecond
	rollingBufferMax = 8192
)

type statusPatterns struct {
	idle       *regexp.Regexp
	processing *regexp.Regexp
	completed  *regexp.Regexp
}

func providerPatterns(p Provider) statusPatterns {
	switch p {
	case ProviderClaudeCode:
		return statusPatterns{
			idle:       regexp.MustCompile(`[>❯][\s\xa0]`),
			processing: regexp.MustCompile(`(?m)^\s*[✶✢✽✻✳·*].*…`),
			completed:  regexp.MustCompile(`[✶✢✽✻✳][\s\xa0]+\w.*\d+s`),
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
		return statusPatterns{
			idle: regexp.MustCompile(`[$%#>❯]\s`),
		}
	default:
		return statusPatterns{
			idle:       regexp.MustCompile(`[$%#>❯]\s`),
			processing: regexp.MustCompile(`\.\.\.|processing|thinking`),
		}
	}
}

// OutputMonitor tracks the rolling output buffer and quiescence timer for a
// single agent session and derives its status.
type OutputMonitor struct {
	mu             sync.Mutex
	buf            bytes.Buffer
	bursting       bool
	timer          *time.Timer
	armedAt        time.Time
	patterns       statusPatterns
	onStatusChange func(detected Status)
}

func newOutputMonitor(p Provider, onChange func(Status)) *OutputMonitor {
	return &OutputMonitor{patterns: providerPatterns(p), onStatusChange: onChange}
}

// Feed appends data to the rolling buffer and schedules detection. Called
// on the PTY output hot path — must not block.
func (m *OutputMonitor) Feed(data []byte) {
	m.mu.Lock()
	m.buf.Write(data)
	if m.buf.Len() > rollingBufferMax {
		m.buf.Next(m.buf.Len() - rollingBufferMax)
	}
	wasBursting := m.bursting
	m.bursting = true

	var snap []byte
	if !wasBursting {
		snap = m.snapshotLocked()
	}

	m.armedAt = time.Now()
	if m.timer == nil {
		m.timer = time.AfterFunc(quiescenceDelay, m.onQuiescent)
	} else {
		m.timer.Stop()
		m.timer.Reset(quiescenceDelay)
	}
	m.mu.Unlock()

	if snap != nil {
		m.detect(snap, true)
	}
}

func (m *OutputMonitor) snapshotLocked() []byte {
	snap := make([]byte, m.buf.Len())
	copy(snap, m.buf.Bytes())
	return snap
}

func (m *OutputMonitor) onQuiescent() {
	m.mu.Lock()
	if time.Since(m.armedAt) < quiescenceDelay {
		m.mu.Unlock()
		return
	}
	m.bursting = false
	snap := m.snapshotLocked()
	m.mu.Unlock()
	m.detect(snap, false)
}

// ResetBuffer clears the rolling window — called right after input is
// delivered, so the previous turn's idle prompt (and the echo of what we
// just typed) can't be mistaken for the new turn already being done.
func (m *OutputMonitor) ResetBuffer() {
	m.mu.Lock()
	m.buf.Reset()
	m.mu.Unlock()
}

// risingEdge suppresses ready statuses on a mid-burst detection: that frame
// can be the echo of input just written, and reporting ready from it would
// complete a handoff prematurely. Ready only comes from a quiescent screen.
func (m *OutputMonitor) detect(buf []byte, risingEdge bool) {
	s := m.detectStatus(buf)
	if s == StatusUnknown {
		return
	}
	if risingEdge && s.isReady() {
		return
	}
	m.onStatusChange(s)
}

func (m *OutputMonitor) detectStatus(buf []byte) Status {
	text := stripANSI(string(buf))
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

func (m *OutputMonitor) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.armedAt = time.Now()
	if m.timer != nil {
		m.timer.Stop()
	}
}
