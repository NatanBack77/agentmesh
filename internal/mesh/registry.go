// Package mesh is the communication engine: a registry of running agents
// plus the exec/handoff/assign/send primitives that let them talk to each
// other. Every agent lives in its own tmux session (see internal/tmuxdrv) —
// tmux owns terminal rendering, sizing and attaching; this package only
// owns identity, turn status and message routing.
package mesh

import (
	"fmt"
	"sync"
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
// TerminalID doubles as the tmux session name — there is no separate PTY
// session identifier in this design.
type AgentState struct {
	mu sync.Mutex

	TerminalID string
	AgentName  string
	Provider   Provider
	CWD        string
	Command    string

	// Worktree isolation (empty fields = not using one). RepoRoot is the
	// shared repo; WorktreePath (== CWD when set) and WorktreeBranch are
	// this agent's own checkout — see internal/gitwt.
	WorktreeRepo   string
	WorktreePath   string
	WorktreeBranch string

	Status           Status
	CreatedAt        time.Time
	LastStatusChange time.Time

	ChainDepth       int
	ParentTerminalID string

	// NeedsAttention is true while a known blocking dialog (permission
	// menu, trust prompt, ...) is showing on the agent's screen — surfaced
	// in `agentmesh ls` so you know to `attach` and press something.
	NeedsAttention bool

	// BootHintSent guards the one-time delivery of the peers/coordination
	// hint (and --role text, if any) once the agent leaves its boot
	// screens for the first time. See Engine.deliverBootHint.
	BootHintSent bool

	InboxQueue []Message

	stickyReady bool
	dead        chan struct{}
}

func newAgentState(terminalID, agentName, cwd, command string, provider Provider) *AgentState {
	return &AgentState{
		TerminalID:       terminalID,
		AgentName:        agentName,
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
// is called — prevents redraw flapping). Caller must hold ps.mu.
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
	byID   sync.Map // TerminalID -> *AgentState
	byName sync.Map // AgentName -> TerminalID
}

// Register adds a new agent to the registry. Duplicate agent names get a
// short ID suffix ("coder" -> "coder-7c2f") so name-based targeting stays
// unambiguous.
func (r *Registry) Register(terminalID, agentName, cwd, command string, provider Provider) (*AgentState, error) {
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
	ps := newAgentState(terminalID, agentName, cwd, command, provider)
	r.byID.Store(terminalID, ps)
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

func (r *Registry) All() []*AgentState {
	var result []*AgentState
	r.byID.Range(func(_, v any) bool {
		result = append(result, v.(*AgentState))
		return true
	})
	return result
}
