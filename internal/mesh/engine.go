package mesh

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/NatanBack77/agentmesh/internal/ptymgr"
	"github.com/google/uuid"
)

// Config tunes the engine's limits.
type Config struct {
	MaxChainDepth int           // default delegation depth cap
	HandoffTTL    time.Duration // default blocking-handoff timeout
	Port          int           // HTTP API port (0 = OS-assigned)
}

func (c Config) maxDepth() int {
	if c.MaxChainDepth <= 0 {
		return 4
	}
	return c.MaxChainDepth
}

func (c Config) handoffTTL() time.Duration {
	if c.HandoffTTL <= 0 {
		return 600 * time.Second
	}
	return c.HandoffTTL
}

// Engine wires the Registry, per-agent OutputMonitors, the PTY manager and
// the HTTP API together. One Engine holds every agent spawned through it.
type Engine struct {
	registry   *Registry
	primitives *Primitives
	pty        *ptymgr.Manager

	monitorsMu sync.Mutex
	monitors   map[string]*OutputMonitor // terminalID -> monitor

	broadcast *broadcaster

	httpSrv *http.Server
	addr    string
	port    int
}

func New(cfg Config) *Engine {
	e := &Engine{
		registry: &Registry{},
		monitors: make(map[string]*OutputMonitor),
	}
	e.broadcast = newBroadcaster()
	e.pty = ptymgr.NewManager(e.onData, e.onExit)
	e.primitives = NewPrimitives(e.registry, e.writeSession, cfg.maxDepth(), cfg.handoffTTL())
	e.primitives.onInputDelivered = e.resetMonitorBuffer
	e.primitives.onFlow = func(src, tgt, kind string) {
		log.Printf("[mesh] %s --%s--> %s", shortID(src), kind, shortID(tgt))
	}
	return e
}

func shortID(id string) string {
	if id == "" {
		return "(user)"
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (e *Engine) writeSession(sessionID string, data []byte) error {
	return e.pty.Write(sessionID, data)
}

func (e *Engine) resetMonitorBuffer(terminalID string) {
	e.monitorsMu.Lock()
	m, ok := e.monitors[terminalID]
	e.monitorsMu.Unlock()
	if ok {
		m.ResetBuffer()
	}
}

// onData is the PTY manager's output hook: fan-out to the turn-status
// monitor, the in-flight handoff capture buffer, and any attach/watch
// subscribers.
func (e *Engine) onData(sessionID string, data []byte) {
	ps, ok := e.registry.BySession(sessionID)
	if !ok {
		return
	}
	ps.AddOutputBytes(len(data))
	e.broadcast.publish(sessionID, data)

	e.monitorsMu.Lock()
	m, ok := e.monitors[ps.TerminalID]
	e.monitorsMu.Unlock()
	if ok {
		m.Feed(data)
	}
	e.primitives.CaptureOutput(ps.TerminalID, data)
}

func (e *Engine) onExit(sessionID string) {
	ps, ok := e.registry.BySession(sessionID)
	if !ok {
		return
	}
	ps.mu.Lock()
	ps.applyStatus(StatusError)
	close(ps.dead)
	ps.mu.Unlock()
	e.primitives.NotifyDead(ps.TerminalID)
	log.Printf("[mesh] agent %q (%s) exited", ps.AgentName, shortID(ps.TerminalID))
}

// SpawnRequest describes an agent to spawn.
type SpawnRequest struct {
	Name    string
	Command string
	Args    []string
	CWD     string
}

// SpawnResult is returned to the caller after a successful spawn.
type SpawnResult struct {
	TerminalID string `json:"terminal_id"`
	SessionID  string `json:"session_id"`
	Name       string `json:"name"`
}

// Spawn starts a new agent process under a fresh PTY, registers it, and
// wires its turn-detection monitor. cwd defaults to the engine's own
// working directory when empty.
func (e *Engine) Spawn(req SpawnRequest) (SpawnResult, error) {
	if req.Command == "" {
		return SpawnResult{}, fmt.Errorf("command is required")
	}
	cwd := req.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cwd, _ = filepath.Abs(cwd)

	terminalID := uuid.New().String()
	provider := ProviderFromCommand(filepath.Base(req.Command))

	env := []string{
		"AGENTMESH_TERMINAL_ID=" + terminalID,
		"AGENTMESH_NAME=" + req.Name,
		fmt.Sprintf("AGENTMESH_URL=http://127.0.0.1:%d", e.port),
	}

	args := req.Args
	if provider == ProviderClaudeCode {
		hint := "Você pode coordenar com outros agentes via a CLI `agentmesh` (já no PATH, e as variáveis AGENTMESH_TERMINAL_ID/AGENTMESH_URL já estão no seu ambiente). Rode `agentmesh peers` pra listar quem mais está rodando, `agentmesh send <peer> \"mensagem\"` pra mandar mensagem sem esperar resposta, `agentmesh handoff <peer> \"tarefa\"` pra delegar e esperar o resultado, `agentmesh exec <peer> \"comando\"` pra rodar comando num peer shell. Use `agentmesh whoami` pra saber quem você é."
		args = append([]string{"--append-system-prompt", hint}, args...)
	}

	sess, err := e.pty.Spawn("", req.Command, args, cwd, env...)
	if err != nil {
		return SpawnResult{}, err
	}

	ps, err := e.registry.Register(terminalID, req.Name, sess.ID, cwd, req.Command, provider)
	if err != nil {
		_ = e.pty.Kill(sess.ID)
		return SpawnResult{}, err
	}

	m := newOutputMonitor(provider, func(s Status) {
		e.handleStatusChange(ps, s)
	})
	e.monitorsMu.Lock()
	e.monitors[terminalID] = m
	e.monitorsMu.Unlock()

	e.writePeersFile()

	return SpawnResult{TerminalID: terminalID, SessionID: sess.ID, Name: ps.AgentName}, nil
}

func (e *Engine) handleStatusChange(ps *AgentState, s Status) {
	ps.mu.Lock()
	changed := ps.applyStatus(s)
	ready := ps.Status.isReady()
	id := ps.TerminalID
	ps.mu.Unlock()

	if !changed {
		return
	}
	if ready {
		e.primitives.NotifyReady(id, "")
		drainInbox(ps, e.writeSession, func() { e.resetMonitorBuffer(id) })
	}
}

// Kill terminates an agent by name or ID and unregisters it.
func (e *Engine) Kill(nameOrID string) error {
	ps, err := e.primitives.resolveTarget(nameOrID)
	if err != nil {
		return err
	}
	ps.mu.Lock()
	sid, id := ps.SessionID, ps.TerminalID
	ps.mu.Unlock()

	e.monitorsMu.Lock()
	if m, ok := e.monitors[id]; ok {
		m.Stop()
		delete(e.monitors, id)
	}
	e.monitorsMu.Unlock()

	e.registry.Unregister(id)
	e.writePeersFile()
	return e.pty.Kill(sid)
}

// Registry exposes the read-only agent list.
func (e *Engine) Registry() *Registry { return e.registry }

// Primitives exposes the communication primitives (for the HTTP layer).
func (e *Engine) Primitives() *Primitives { return e.primitives }

// SessionForAttach returns the PTY session ID and manager for a name/ID —
// used by the attach/watch side channel.
func (e *Engine) ResolveSession(nameOrID string) (sessionID string, err error) {
	ps, err := e.primitives.resolveTarget(nameOrID)
	if err != nil {
		return "", err
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.SessionID, nil
}

func (e *Engine) WritePTY(sessionID string, data []byte) error { return e.pty.Write(sessionID, data) }
func (e *Engine) ResizePTY(sessionID string, cols, rows uint16) error {
	return e.pty.Resize(sessionID, cols, rows)
}
func (e *Engine) Subscribe(sessionID string) (<-chan []byte, func()) {
	return e.broadcast.subscribe(sessionID)
}

// Start brings up the HTTP API on cfg.Port (0 = OS-assigned) and blocks
// until ctx is cancelled.
func (e *Engine) Start(ctx context.Context, port int) error {
	mux := http.NewServeMux()
	e.registerRoutes(mux)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	e.port = ln.Addr().(*net.TCPAddr).Port
	e.addr = ln.Addr().String()

	srv := &http.Server{Handler: mux}
	e.httpSrv = srv

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	log.Printf("[mesh] HTTP API on http://%s", e.addr)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// Port returns the bound HTTP API port (valid after Start has begun listening).
func (e *Engine) Port() int { return e.port }
