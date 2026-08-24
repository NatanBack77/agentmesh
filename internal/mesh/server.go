package mesh

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// registerRoutes wires the HTTP API. Every route is loopback-only by virtue
// of the listener binding to 127.0.0.1 in Start.
func (e *Engine) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /spawn", e.handleSpawn)
	mux.HandleFunc("GET /agents", e.handleListAgents)
	mux.HandleFunc("GET /agents/{id}", e.handleGetAgent)
	mux.HandleFunc("DELETE /agents/{id}", e.handleKill)
	mux.HandleFunc("GET /peers", e.handlePeers)
	mux.HandleFunc("GET /whoami", e.handleWhoami)

	mux.HandleFunc("POST /primitives/handoff", e.handleHandoff)
	mux.HandleFunc("POST /primitives/exec", e.handleExec)
	mux.HandleFunc("POST /primitives/assign", e.handleAssign)

	mux.HandleFunc("POST /agents/{id}/keys", e.handleRawKeys)
}

// handleRawKeys writes raw bytes straight to an agent's PTY, bypassing every
// turn-status gate assign/handoff have. Used to script past interactive
// first-run screens (theme picker, trust prompt) that block before the
// agent ever reaches an IDLE prompt the normal primitives could target.
func (e *Engine) handleRawKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	sessionID, err := e.ResolveSession(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err := e.WritePTY(sessionID, []byte(req.Data)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

type agentView struct {
	TerminalID string `json:"terminal_id"`
	Name       string `json:"name"`
	Provider   string `json:"provider"`
	Command    string `json:"command"`
	CWD        string `json:"cwd"`
	Status     string `json:"status"`
	ParentID   string `json:"parent_id,omitempty"`
	ChainDepth int    `json:"chain_depth"`
}

func viewOf(ps *AgentState) agentView {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return agentView{
		TerminalID: ps.TerminalID,
		Name:       ps.AgentName,
		Provider:   ps.Provider.String(),
		Command:    ps.Command,
		CWD:        ps.CWD,
		Status:     ps.Status.String(),
		ParentID:   ps.ParentTerminalID,
		ChainDepth: ps.ChainDepth,
	}
}

func (e *Engine) handleSpawn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string   `json:"name"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
		CWD     string   `json:"cwd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	res, err := e.Spawn(SpawnRequest{Name: req.Name, Command: req.Command, Args: req.Args, CWD: req.CWD})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (e *Engine) handleListAgents(w http.ResponseWriter, r *http.Request) {
	all := e.registry.All()
	views := make([]agentView, 0, len(all))
	for _, ps := range all {
		views = append(views, viewOf(ps))
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	writeJSON(w, http.StatusOK, views)
}

func (e *Engine) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	ps, err := e.primitives.resolveTarget(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, viewOf(ps))
}

func (e *Engine) handleKill(w http.ResponseWriter, r *http.Request) {
	if err := e.Kill(r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// handlePeers lists every OTHER registered agent — the flat mesh: every
// agent is a peer of every other, no link-drawing step required.
func (e *Engine) handlePeers(w http.ResponseWriter, r *http.Request) {
	self := r.Header.Get("X-AgentMesh-Terminal-ID")
	all := e.registry.All()
	views := make([]agentView, 0, len(all))
	for _, ps := range all {
		ps.mu.Lock()
		id := ps.TerminalID
		ps.mu.Unlock()
		if id == self {
			continue
		}
		views = append(views, viewOf(ps))
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	writeJSON(w, http.StatusOK, views)
}

func (e *Engine) handleWhoami(w http.ResponseWriter, r *http.Request) {
	self := r.Header.Get("X-AgentMesh-Terminal-ID")
	ps, ok := e.registry.ByID(self)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no X-AgentMesh-Terminal-ID header matched a registered agent"})
		return
	}
	writeJSON(w, http.StatusOK, viewOf(ps))
}

func (e *Engine) handleHandoff(w http.ResponseWriter, r *http.Request) {
	var req handoffRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, handoffResponse{Error: err.Error()})
		return
	}
	callerID := r.Header.Get("X-AgentMesh-Terminal-ID")
	resp, code := e.primitives.DoHandoff(r.Context(), callerID, req)
	writeJSON(w, code, resp)
}

func (e *Engine) handleExec(w http.ResponseWriter, r *http.Request) {
	var req handoffRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, handoffResponse{Error: err.Error()})
		return
	}
	callerID := r.Header.Get("X-AgentMesh-Terminal-ID")
	resp, code := e.primitives.DoExec(r.Context(), callerID, req)
	writeJSON(w, code, resp)
}

func (e *Engine) handleAssign(w http.ResponseWriter, r *http.Request) {
	var req assignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, assignResponse{Error: err.Error()})
		return
	}
	callerID := r.Header.Get("X-AgentMesh-Terminal-ID")
	resp, code := e.primitives.DoAssign(callerID, req)
	writeJSON(w, code, resp)
}

var _ = context.Background // keep context import if handlers grow

// writePeersFile drops a plain-text peer list into every registered agent's
// CWD (.agentmesh/peers.md) so any agent — regardless of provider — can
// discover who else is running just by reading a file, no MCP/tool wiring
// required. Best-effort: spawn still succeeds if a CWD isn't writable.
func (e *Engine) writePeersFile() {
	all := e.registry.All()
	views := make([]agentView, 0, len(all))
	for _, ps := range all {
		views = append(views, viewOf(ps))
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })

	var sb strings.Builder
	sb.WriteString("# Agentes no mesh (agentmesh)\n\n")
	if len(views) == 0 {
		sb.WriteString("Nenhum agente rodando.\n")
	}
	for _, v := range views {
		sb.WriteString("- **" + v.Name + "** (" + v.Provider + ", status " + v.Status + ") — id " + v.TerminalID + "\n")
	}
	sb.WriteString("\nComandos: `agentmesh peers` · `agentmesh send <peer> \"msg\"` · `agentmesh handoff <peer> \"tarefa\"` · `agentmesh exec <peer> \"comando\"` · `agentmesh whoami`\n")
	content := []byte(sb.String())

	for _, ps := range all {
		ps.mu.Lock()
		cwd := ps.CWD
		ps.mu.Unlock()
		dir := filepath.Join(cwd, ".agentmesh")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(dir, "peers.md"), content, 0o644)
	}
}
