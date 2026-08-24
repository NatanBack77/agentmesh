package mesh

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/NatanBack77/agentmesh/internal/tmuxdrv"
	"github.com/google/uuid"
)

// Config tunes the engine's limits.
type Config struct {
	MaxChainDepth int           // default delegation depth cap
	HandoffTTL    time.Duration // default blocking-handoff timeout
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

// Engine wires the Registry, per-agent OutputMonitors and the HTTP API
// together. Every agent it spawns lives in its own tmux session — Engine
// itself holds no PTYs.
type Engine struct {
	registry   *Registry
	primitives *Primitives

	monitorsMu sync.Mutex
	monitors   map[string]*OutputMonitor // terminalID -> monitor

	httpSrv *http.Server
	addr    string
	port    int
}

func New(cfg Config) *Engine {
	e := &Engine{
		registry: &Registry{},
		monitors: make(map[string]*OutputMonitor),
	}
	e.primitives = NewPrimitives(e.registry, e.deliver, e.suppress, cfg.maxDepth(), cfg.handoffTTL())
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

func (e *Engine) monitorFor(terminalID string) *OutputMonitor {
	e.monitorsMu.Lock()
	defer e.monitorsMu.Unlock()
	return e.monitors[terminalID]
}

// suppress arms/disarms ready-detection for one agent's OutputMonitor. The
// caller (primitives.go) is responsible for turning it on BEFORE touching
// the agent's status — see the comment on Primitives.suppress for why the
// ordering matters.
func (e *Engine) suppress(terminalID string, on bool) {
	if m := e.monitorFor(terminalID); m != nil {
		m.suppressReady.Store(on)
	}
}

// deliver types message into the agent's tmux session and presses Enter.
// Settle time scales with message length so a busy TUI still ingesting the
// paste doesn't eat the Enter as a plain newline; the second Enter is a
// no-op safety net if the first one already submitted.
func (e *Engine) deliver(terminalID, message string) error {
	text := strings.TrimRight(message, "\r\n")
	if text != "" {
		if err := tmuxdrv.SendLiteral(terminalID, text); err != nil {
			return err
		}
	}
	settle := 150*time.Millisecond + time.Duration(len(text)/20)*time.Millisecond
	settle = min(settle, 1200*time.Millisecond)
	time.Sleep(settle)
	if err := tmuxdrv.SendKey(terminalID, "Enter"); err != nil {
		return err
	}
	time.Sleep(400 * time.Millisecond)
	return tmuxdrv.SendKey(terminalID, "Enter")
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
	Name       string `json:"name"`
}

// Spawn starts a new agent process inside a fresh tmux session, registers
// it, and starts its turn-detection poller. cwd defaults to the engine's
// own working directory when empty (the CLI always fills this in with the
// caller's cwd before it gets here — see cmd/agentmesh).
func (e *Engine) Spawn(req SpawnRequest) (SpawnResult, error) {
	if err := tmuxdrv.Available(); err != nil {
		return SpawnResult{}, err
	}
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

	if err := tmuxdrv.NewSession(terminalID, cwd, req.Command, args, env); err != nil {
		return SpawnResult{}, err
	}

	ps, err := e.registry.Register(terminalID, req.Name, cwd, req.Command, provider)
	if err != nil {
		_ = tmuxdrv.KillSession(terminalID)
		return SpawnResult{}, err
	}

	m := newOutputMonitor(terminalID, provider,
		func(s Status, text string) { e.handleStatusChange(ps, s, text) },
		func() { e.handleDead(terminalID) },
	)
	e.monitorsMu.Lock()
	e.monitors[terminalID] = m
	e.monitorsMu.Unlock()
	m.Start()

	e.writePeersFile()

	return SpawnResult{TerminalID: terminalID, Name: ps.AgentName}, nil
}

func (e *Engine) handleStatusChange(ps *AgentState, s Status, text string) {
	ps.mu.Lock()
	changed := ps.applyStatus(s)
	ready := ps.Status.isReady()
	id := ps.TerminalID
	ps.mu.Unlock()

	if !changed {
		return
	}
	if ready {
		e.primitives.NotifyReady(id, text)
		drainInbox(ps, e.deliver, e.suppress, nil)
	}
}

// handleDead runs when an OutputMonitor finds its tmux session gone —
// the agent process exited (or was killed by something other than our own
// Kill, which already stops the monitor before tearing the session down).
func (e *Engine) handleDead(terminalID string) {
	ps, ok := e.registry.ByID(terminalID)
	if !ok {
		return
	}
	ps.mu.Lock()
	ps.applyStatus(StatusError)
	select {
	case <-ps.dead:
	default:
		close(ps.dead)
	}
	name := ps.AgentName
	ps.mu.Unlock()

	e.primitives.NotifyDead(terminalID)

	e.monitorsMu.Lock()
	if m, ok := e.monitors[terminalID]; ok {
		m.Stop()
		delete(e.monitors, terminalID)
	}
	e.monitorsMu.Unlock()

	log.Printf("[mesh] agent %q (%s) saiu — sessão tmux encerrada", name, shortID(terminalID))
}

// Kill terminates an agent's tmux session and unregisters it.
func (e *Engine) Kill(nameOrID string) error {
	ps, err := e.primitives.resolveTarget(nameOrID)
	if err != nil {
		return err
	}
	ps.mu.Lock()
	id := ps.TerminalID
	ps.mu.Unlock()

	e.monitorsMu.Lock()
	if m, ok := e.monitors[id]; ok {
		m.Stop()
		delete(e.monitors, id)
	}
	e.monitorsMu.Unlock()

	e.registry.Unregister(id)
	e.writePeersFile()
	return tmuxdrv.KillSession(id)
}

// Registry exposes the read-only agent list.
func (e *Engine) Registry() *Registry { return e.registry }

// Primitives exposes the communication primitives (for the HTTP layer).
func (e *Engine) Primitives() *Primitives { return e.primitives }

// Start brings up the HTTP API on port (0 = OS-assigned) and blocks until
// ctx is cancelled.
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
