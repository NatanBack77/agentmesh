// Package mesh is the communication engine: a registry of running agents
// plus the exec/handoff/assign/send primitives that let them talk to each
// other. Adapted from Openfield's internal/orchestrator package, trimmed
// down to just the agent-to-agent communication core (no canvas, no notes,
// no kanban, no workspace scoping — every registered agent is a peer of
// every other agent).
package mesh

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Status represents the current processing state of an agent, as observed
// by output-based turn detection.
type Status int

const (
	StatusUnknown Status = iota
	StatusIdle
	StatusProcessing
	StatusCompleted
	StatusError
)

func (s Status) String() string {
	switch s {
	case StatusIdle:
		return "idle"
	case StatusProcessing:
		return "processing"
	case StatusCompleted:
		return "completed"
	case StatusError:
		return "error"
	default:
		return "unknown"
	}
}

func (s Status) isReady() bool {
	return s == StatusIdle || s == StatusCompleted
}

// Provider identifies which CLI agent runs in a session — used to pick the
// right turn-detection regexes.
type Provider int

const (
	ProviderClaudeCode Provider = iota
	ProviderCodex
	ProviderOpenCode
	ProviderGeminiCLI
	ProviderShell
	ProviderUnknown
)

func (p Provider) String() string {
	switch p {
	case ProviderClaudeCode:
		return "claude"
	case ProviderCodex:
		return "codex"
	case ProviderOpenCode:
		return "opencode"
	case ProviderGeminiCLI:
		return "gemini"
	case ProviderShell:
		return "shell"
	default:
		return "unknown"
	}
}

// ProviderFromCommand infers the Provider from the spawn command string.
func ProviderFromCommand(command string) Provider {
	switch command {
	case "claude":
		return ProviderClaudeCode
	case "codex":
		return ProviderCodex
	case "opencode":
		return ProviderOpenCode
	case "gemini":
		return ProviderGeminiCLI
	case "bash", "zsh", "sh", "fish":
		return ProviderShell
	default:
		return ProviderUnknown
	}
}

// Message is an inbox message queued for delivery to an agent.
type Message struct {
	ID         string
	SenderID   string
	ReceiverID string
	Body       string
	CreatedAt  time.Time
}

// AgentState holds all engine-managed state for one running agent.
type AgentState struct {
	mu sync.Mutex

	TerminalID string
	AgentName  string
	SessionID  string
	Provider   Provider
	CWD        string
	Command    string

	Status           Status
	CreatedAt        time.Time
	LastStatusChange time.Time

	ChainDepth       int
	ParentTerminalID string

	InboxQueue []Message

	stickyReady bool
	dead        chan struct{}

	outputBytes atomic.Int64
}

func (ps *AgentState) AddOutputBytes(n int) {
	if n > 0 {
		ps.outputBytes.Add(int64(n))
	}
}

func newAgentState(terminalID, agentName, sessionID, cwd, command string, provider Provider) *AgentState {
	return &AgentState{
		TerminalID:       terminalID,
		AgentName:        agentName,
		SessionID:        sessionID,
		Provider:         provider,
		CWD:              cwd,
		Command:          command,
		Status:           StatusUnknown,
		CreatedAt:        time.Now(),
		LastStatusChange: time.Now(),
		InboxQueue:       []Message{},
		dead:             make(chan struct{}),
	}
}

// applyStatus transitions the agent to s, enforcing the sticky-ready latch
// (once IDLE/COMPLETED, refuse PROCESSING regression until notifyInputSent
// is called — prevents TUI redraws from flapping the status). Caller must
// hold ps.mu.
func (ps *AgentState) applyStatus(s Status) bool {
	if s == StatusUnknown && ps.Status != StatusUnknown {
		return false
	}
	if ps.stickyReady {
		if s == StatusProcessing || s == StatusUnknown {
			return false
		}
		if ps.Status == StatusCompleted && s == StatusIdle {
			return false
		}
	}
	if s == ps.Status {
		return false
	}
	ps.Status = s
	ps.LastStatusChange = time.Now()
	if s == StatusProcessing {
		ps.stickyReady = false
	} else if s.isReady() {
		ps.stickyReady = true
	}
	return true
}

// notifyInputSent marks the start of a new turn. Caller must hold ps.mu.
func (ps *AgentState) notifyInputSent() {
	ps.stickyReady = false
	ps.Status = StatusProcessing
	ps.LastStatusChange = time.Now()
}

// Registry is the central store of all registered agents.
type Registry struct {
	byID      sync.Map // TerminalID -> *AgentState
	byName    sync.Map // AgentName -> TerminalID
	bySession sync.Map // SessionID -> TerminalID
}

// Register adds a new agent to the registry. Duplicate agent names get a
// short ID suffix ("coder" -> "coder-7c2f") so name-based targeting stays
// unambiguous.
func (r *Registry) Register(terminalID, agentName, sessionID, cwd, command string, provider Provider) (*AgentState, error) {
	if _, exists := r.byID.Load(terminalID); exists {
		return nil, fmt.Errorf("mesh.Register: terminal %q already registered", terminalID)
	}
	if agentName != "" {
		if actual, taken := r.byName.LoadOrStore(agentName, terminalID); taken && actual.(string) != terminalID {
			suffix := terminalID
			if len(suffix) > 4 {
				suffix = suffix[:4]
			}
			agentName = agentName + "-" + suffix
			r.byName.LoadOrStore(agentName, terminalID)
		}
	}
	ps := newAgentState(terminalID, agentName, sessionID, cwd, command, provider)
	r.byID.Store(terminalID, ps)
	r.bySession.Store(sessionID, terminalID)
	return ps, nil
}

func (r *Registry) Unregister(terminalID string) {
	v, loaded := r.byID.LoadAndDelete(terminalID)
	if !loaded {
		return
	}
	ps := v.(*AgentState)
	if ps.AgentName != "" {
		r.byName.Delete(ps.AgentName)
	}
	r.bySession.Delete(ps.SessionID)
}

func (r *Registry) ByID(terminalID string) (*AgentState, bool) {
	v, ok := r.byID.Load(terminalID)
	if !ok {
		return nil, false
	}
	return v.(*AgentState), true
}

func (r *Registry) ByName(name string) (*AgentState, bool) {
	v, ok := r.byName.Load(name)
	if !ok {
		return nil, false
	}
	return r.ByID(v.(string))
}

func (r *Registry) BySession(sessionID string) (*AgentState, bool) {
	v, ok := r.bySession.Load(sessionID)
	if !ok {
		return nil, false
	}
	return r.ByID(v.(string))
}

func (r *Registry) All() []*AgentState {
	var result []*AgentState
	r.byID.Range(func(_, v any) bool {
		result = append(result, v.(*AgentState))
		return true
	})
	return result
}
