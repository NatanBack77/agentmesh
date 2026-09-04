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

	"github.com/NatanBack77/agentmesh/internal/gitwt"
	"github.com/NatanBack77/agentmesh/internal/tmuxdrv"
	"github.com/google/uuid"
)

// safeBranchName derives a filesystem/branch-safe slug from an agent name,
// falling back to (and always suffixing with) a short piece of its
// terminalID — keeps worktree paths/branches unique even if two spawns
// reuse the same name before the first one is cleaned up.
func safeBranchName(name, terminalID string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	slug := b.String()
	if slug == "" {
		slug = "agent"
	}
	suffix := terminalID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return slug + "-" + suffix
}

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

	bootMu    sync.Mutex
	bootHints map[string]string // terminalID -> first-message to deliver once past boot

	httpSrv *http.Server
	addr    string
	port    int
}

func New(cfg Config) *Engine {
	e := &Engine{
		registry:  &Registry{},
		monitors:  make(map[string]*OutputMonitor),
		bootHints: make(map[string]string),
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
	// Role, when set, is delivered as the agent's first message once it
	// leaves its boot screens — a persona/instruction injected at spawn
	// time, mirroring Openfield's role system. Works for any provider.
	Role string
	// Worktree, when true, isolates this agent: instead of running in CWD
	// directly, it gets its own `git worktree` checkout of CWD's repo, on
	// its own branch — so two agents can work on the same repository at
	// the same time without touching each other's uncommitted changes.
	// CWD must resolve to a git repository when this is set.
	Worktree bool
	// Branch names the worktree's branch. Empty defaults to
	// "agentmesh/<name>". Reused as-is (checked out, not recreated) if it
	// already exists — so re-spawning an agent under the same branch name
	// picks up where a previous run left off instead of erroring.
	Branch string
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

	if err := checkAllowed(cwd); err != nil {
		return SpawnResult{}, err
	}

	terminalID := uuid.New().String()
	provider := ProviderFromCommand(filepath.Base(req.Command))

	var repoRoot, worktreePath, branch string
	if req.Worktree {
		root, err := gitwt.RepoRoot(cwd)
		if err != nil {
			return SpawnResult{}, err
		}
		repoRoot = root
		branch = req.Branch
		if branch == "" {
			branch = "agentmesh/" + safeBranchName(req.Name, terminalID)
		}
		worktreePath = filepath.Join(repoRoot, ".agentmesh", "worktrees", safeBranchName(req.Name, terminalID))
		// repoRoot can be an ancestor of cwd (git repo root above the
		// requested --cwd) — re-check in case that ancestor climbed
		// outside whatever narrower allowlist entry let cwd itself pass.
		if err := checkAllowed(worktreePath); err != nil {
			return SpawnResult{}, err
		}
		if err := gitwt.AddWorktree(repoRoot, worktreePath, branch); err != nil {
			return SpawnResult{}, err
		}
		cwd = worktreePath
	}

	env := []string{
		"AGENTMESH_TERMINAL_ID=" + terminalID,
		"AGENTMESH_NAME=" + req.Name,
		fmt.Sprintf("AGENTMESH_URL=http://127.0.0.1:%d", e.port),
	}

	args := req.Args
	switch provider {
	case ProviderClaudeCode:
		// Claude Code supports handing it system-level context at spawn —
		// use that, it's the most reliable delivery (present from the very
		// first token, never racing the boot screens).
		//
		// --dangerously-skip-permissions: every mesh agent runs fully
		// autonomous by design (see ProviderCodex below for the same
		// call on the other side) — a `send`/`handoff` between agents is
		// meant to complete unattended. Without this, the FIRST Bash/
		// Edit/WebSearch/etc call of a task blocks on a permission menu
		// that nobody is watching, and a blocking handoff just sits there
		// until its timeout (600s default) expires — confirmed live: a
		// codex->claude handoff that needed WebSearch hung ~10min with no
		// visible progress until the caller gave up and killed it. This
		// trades the per-tool confirmation for actual autonomy, which is
		// the whole point of spawning through agentmesh instead of a
		// plain interactive session.
		args = append([]string{"--append-system-prompt", coordinationHint, "--dangerously-skip-permissions"}, args...)
	case ProviderCodex:
		// Codex's equivalent of the above: skip its own approval prompts
		// AND the bwrap sandbox restrictions (see AllowlistPath's sibling
		// concern in cmd/agentmesh's --help — this is the same autonomy
		// trade at the process level instead of the directory level).
		args = append([]string{"--dangerously-bypass-approvals-and-sandbox"}, args...)
	}

	if err := tmuxdrv.NewSession(terminalID, cwd, req.Command, args, env); err != nil {
		if worktreePath != "" {
			_ = gitwt.RemoveWorktree(repoRoot, worktreePath, true)
		}
		return SpawnResult{}, err
	}
	if provider == ProviderClaudeCode {
		// Live cost panel in the session's own footer — see
		// SetStatusBar/`agentmesh usage --oneline`. Best-effort: usage
		// tracking is a nicety, not worth failing a spawn over.
		_ = tmuxdrv.SetStatusBar(terminalID)
	}

	ps, err := e.registry.Register(terminalID, req.Name, cwd, req.Command, provider)
	if err != nil {
		_ = tmuxdrv.KillSession(terminalID)
		if worktreePath != "" {
			_ = gitwt.RemoveWorktree(repoRoot, worktreePath, true)
		}
		return SpawnResult{}, err
	}
	if worktreePath != "" {
		ps.mu.Lock()
		ps.WorktreeRepo = repoRoot
		ps.WorktreePath = worktreePath
		ps.WorktreeBranch = branch
		ps.mu.Unlock()
	}

	// Providers with no --append-system-prompt equivalent (codex, gemini,
	// opencode) get the same coordination hint delivered as their first
	// TYPED message instead, once they clear their boot screens — see
	// handleStatusChange/deliverBootHint. --role text (any provider,
	// including claude and shells) rides along the same one-time delivery.
	//
	// Shell/unknown providers are deliberately EXCLUDED from the hint
	// itself: a plain bash/zsh isn't an agent that reads instructions, it
	// EXECUTES whatever lands in its input as a command — typing the hint
	// there ran it as a shell command and threw a syntax error on the
	// parentheses/backticks in it (reported live). --role still applies:
	// if the caller explicitly asked for one, that's on them.
	var boot strings.Builder
	switch provider {
	case ProviderCodex, ProviderOpenCode, ProviderGeminiCLI:
		boot.WriteString(coordinationHint)
	}
	if req.Role != "" {
		if boot.Len() > 0 {
			boot.WriteString("\n\n")
		}
		boot.WriteString(req.Role)
	}
	if boot.Len() > 0 {
		e.bootMu.Lock()
		e.bootHints[terminalID] = boot.String()
		e.bootMu.Unlock()
	}

	m := newOutputMonitor(terminalID, provider,
		func(s Status, text string) { e.handleStatusChange(ps, s, text) },
		func() { e.handleDead(terminalID) },
		func(on bool) { e.setAttention(ps, on) },
	)
	e.monitorsMu.Lock()
	e.monitors[terminalID] = m
	e.monitorsMu.Unlock()
	m.Start()

	e.writePeersFile()

	return SpawnResult{TerminalID: terminalID, Name: ps.AgentName}, nil
}

// coordinationHint is what every agent is told about how to talk to its
// peers — via Claude Code's system prompt when supported, or as a first
// typed message for everyone else (see Spawn).
const coordinationHint = "Você pode coordenar com outros agentes via a CLI `agentmesh` (já no PATH, e as variáveis AGENTMESH_TERMINAL_ID/AGENTMESH_URL já estão no seu ambiente). Rode `agentmesh peers` pra listar quem mais está rodando, `agentmesh send <peer> \"mensagem\"` pra mandar mensagem sem esperar resposta, `agentmesh handoff <peer> \"tarefa\"` pra delegar e esperar o resultado, `agentmesh exec <peer> \"comando\"` pra rodar comando num peer shell, `agentmesh broadcast \"mensagem\"` pra avisar todo mundo de uma vez. Use `agentmesh whoami` pra saber quem você é."

// setAttention updates NeedsAttention and logs the transition (only when it
// actually flips, so a dialog that stays up doesn't spam the log every
// poll tick).
func (e *Engine) setAttention(ps *AgentState, on bool) {
	ps.mu.Lock()
	changed := ps.NeedsAttention != on
	ps.NeedsAttention = on
	name := ps.AgentName
	ps.mu.Unlock()
	if changed && on {
		log.Printf("[mesh] %q precisa de atenção (diálogo na tela) — `agentmesh attach %s`", name, name)
	}
}

// deliverBootHint sends text as the agent's very first message, once
// (guarded by AgentState.BootHintSent), through the same suppress-guarded
// path as every other delivery.
func (e *Engine) deliverBootHint(ps *AgentState, text string) {
	id := ps.TerminalID
	e.suppress(id, true)
	ps.mu.Lock()
	ps.notifyInputSent()
	ps.mu.Unlock()
	_ = e.deliver(id, text)
	time.AfterFunc(300*time.Millisecond, func() { e.suppress(id, false) })
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
		e.maybeSendBootHint(ps)
	}
}

// maybeSendBootHint fires, at most once per agent, the pending
// coordination-hint/--role message queued in Spawn — on the first time the
// agent is ever seen ready (i.e. it has cleared its boot screens).
func (e *Engine) maybeSendBootHint(ps *AgentState) {
	ps.mu.Lock()
	if ps.BootHintSent {
		ps.mu.Unlock()
		return
	}
	ps.BootHintSent = true
	id := ps.TerminalID
	ps.mu.Unlock()

	e.bootMu.Lock()
	hint, ok := e.bootHints[id]
	delete(e.bootHints, id)
	e.bootMu.Unlock()
	if !ok || hint == "" {
		return
	}
	go e.deliverBootHint(ps, hint)
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
// KillOptions controls what happens to a worktree-isolated agent's git
// state when it's killed. Zero value = leave the worktree and branch
// exactly as they are, so the work is never lost by an accidental kill —
// removal is always something the caller opts into explicitly.
type KillOptions struct {
	RemoveWorktree bool
	DeleteBranch   bool // only meaningful together with RemoveWorktree
	Force          bool // discard uncommitted changes in the worktree
}

func (e *Engine) Kill(nameOrID string, opts KillOptions) error {
	ps, err := e.primitives.resolveTarget(nameOrID)
	if err != nil {
		return err
	}
	ps.mu.Lock()
	id := ps.TerminalID
	repoRoot, worktreePath, branch := ps.WorktreeRepo, ps.WorktreePath, ps.WorktreeBranch
	ps.mu.Unlock()

	e.monitorsMu.Lock()
	if m, ok := e.monitors[id]; ok {
		m.Stop()
		delete(e.monitors, id)
	}
	e.monitorsMu.Unlock()

	e.registry.Unregister(id)
	e.bootMu.Lock()
	delete(e.bootHints, id)
	e.bootMu.Unlock()
	e.writePeersFile()

	// Kill the process BEFORE touching the worktree — an agent still
	// holding open file handles in it could make `git worktree remove`
	// fail or misbehave on some filesystems.
	killErr := tmuxdrv.KillSession(id)

	if worktreePath != "" && opts.RemoveWorktree {
		if err := gitwt.RemoveWorktree(repoRoot, worktreePath, opts.Force); err != nil {
			return fmt.Errorf("agente encerrado, mas não consegui remover o worktree: %w", err)
		}
		if opts.DeleteBranch {
			if err := gitwt.DeleteBranch(repoRoot, branch); err != nil {
				return fmt.Errorf("agente encerrado, worktree removido, mas não consegui apagar a branch %q: %w", branch, err)
			}
		}
	}
	return killErr
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
