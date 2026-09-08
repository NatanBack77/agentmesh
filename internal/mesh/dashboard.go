package mesh

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

type dashboardSnapshot struct {
	GeneratedAt time.Time               `json:"generated_at"`
	Summary     dashboardSummary        `json:"summary"`
	Agents      []dashboardAgent        `json:"agents"`
	Providers   []dashboardProvider     `json:"providers"`
	Signals     []dashboardMarketSignal `json:"signals"`
	Costs       dashboardCosts          `json:"costs"`
	Quotas      dashboardQuotas         `json:"quotas"`
}

type dashboardSummary struct {
	TotalAgents       int     `json:"total_agents"`
	ReadyAgents       int     `json:"ready_agents"`
	ProcessingAgents  int     `json:"processing_agents"`
	ErrorAgents       int     `json:"error_agents"`
	NeedsAttention    int     `json:"needs_attention"`
	QueuedMessages    int     `json:"queued_messages"`
	ActiveDelegations int     `json:"active_delegations"`
	WorktreeAgents    int     `json:"worktree_agents"`
	MeshReadiness     float64 `json:"mesh_readiness"`
}

type dashboardAgent struct {
	TerminalID       string    `json:"terminal_id"`
	Name             string    `json:"name"`
	Provider         string    `json:"provider"`
	Command          string    `json:"command"`
	CWD              string    `json:"cwd"`
	Status           string    `json:"status"`
	Attention        bool      `json:"attention"`
	ChainDepth       int       `json:"chain_depth"`
	ParentID         string    `json:"parent_id,omitempty"`
	Branch           string    `json:"branch,omitempty"`
	InboxCount       int       `json:"inbox_count"`
	UptimeSeconds    int64     `json:"uptime_seconds"`
	StatusAgeSeconds int64     `json:"status_age_seconds"`
	CreatedAt        time.Time `json:"created_at"`
	LastStatusChange time.Time `json:"last_status_change"`
}

type dashboardProvider struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Ready int    `json:"ready"`
}

type dashboardMarketSignal struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Note  string `json:"note"`
}

func (e *Engine) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardHTML))
}

func (e *Engine) handleDashboardData(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, e.dashboardSnapshot())
}

// handleDashboardEvents streams dashboardSnapshot() over SSE. It polls the
// snapshot at a short internal interval and writes an event only when the
// serialized payload changed since the last write, plus an unconditional
// heartbeat every few seconds so proxies/clients see the connection is
// alive even during quiet periods. The loop exits — and releases every
// ticker — as soon as the client disconnects (r.Context().Done()) or a
// write fails, so it never leaks a goroutine per connection.
func (e *Engine) handleDashboardEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable nginx response buffering, if present
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	const (
		pollInterval      = 300 * time.Millisecond
		heartbeatInterval = 15 * time.Second
	)

	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	ctx := r.Context()
	var lastSum [sha256.Size]byte

	write := func(force bool) bool {
		snapshot := e.dashboardSnapshot()
		stable := snapshot
		stable.GeneratedAt = time.Time{}
		stable.Costs.GeneratedAt = time.Time{}
		stable.Quotas.GeneratedAt = time.Time{}
		for i := range stable.Agents {
			stable.Agents[i].UptimeSeconds = 0
			stable.Agents[i].StatusAgeSeconds = 0
		}
		stablePayload, err := json.Marshal(stable)
		if err != nil {
			return true // transient marshal failure: keep the connection open
		}
		sum := sha256.Sum256(stablePayload)
		if !force && sum == lastSum {
			return true
		}
		lastSum = sum
		payload, err := json.Marshal(snapshot)
		if err != nil {
			return true
		}
		if _, err := w.Write([]byte("event: snapshot\ndata: ")); err != nil {
			return false
		}
		if _, err := w.Write(payload); err != nil {
			return false
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !write(true) {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			if !write(false) {
				return
			}
		case <-heartbeat.C:
			if !write(true) {
				return
			}
		}
	}
}

func (e *Engine) dashboardSnapshot() dashboardSnapshot {
	now := time.Now()
	all := e.registry.All()
	agents := make([]dashboardAgent, 0, len(all))
	providerTotals := map[string]*dashboardProvider{}
	summary := dashboardSummary{TotalAgents: len(all)}

	for _, ps := range all {
		ps.mu.Lock()
		agent := dashboardAgent{
			TerminalID:       ps.TerminalID,
			Name:             ps.AgentName,
			Provider:         ps.Provider.String(),
			Command:          ps.Command,
			CWD:              ps.CWD,
			Status:           ps.Status.String(),
			Attention:        ps.NeedsAttention,
			ChainDepth:       ps.ChainDepth,
			ParentID:         ps.ParentTerminalID,
			Branch:           ps.WorktreeBranch,
			InboxCount:       len(ps.InboxQueue),
			UptimeSeconds:    int64(now.Sub(ps.CreatedAt).Seconds()),
			StatusAgeSeconds: int64(now.Sub(ps.LastStatusChange).Seconds()),
			CreatedAt:        ps.CreatedAt,
			LastStatusChange: ps.LastStatusChange,
		}
		status := ps.Status
		ps.mu.Unlock()

		agents = append(agents, agent)
		summary.QueuedMessages += agent.InboxCount
		if agent.Attention {
			summary.NeedsAttention++
		}
		if agent.ParentID != "" {
			summary.ActiveDelegations++
		}
		if agent.Branch != "" {
			summary.WorktreeAgents++
		}
		switch status {
		case StatusIdle, StatusCompleted:
			summary.ReadyAgents++
		case StatusProcessing:
			summary.ProcessingAgents++
		case StatusError:
			summary.ErrorAgents++
		}

		pt := providerTotals[agent.Provider]
		if pt == nil {
			pt = &dashboardProvider{Name: agent.Provider}
			providerTotals[agent.Provider] = pt
		}
		pt.Count++
		if status.isReady() {
			pt.Ready++
		}
	}

	if summary.TotalAgents > 0 {
		summary.MeshReadiness = float64(summary.ReadyAgents) / float64(summary.TotalAgents)
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })

	providers := make([]dashboardProvider, 0, len(providerTotals))
	for _, p := range providerTotals {
		providers = append(providers, *p)
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Name < providers[j].Name })

	return dashboardSnapshot{
		GeneratedAt: now,
		Summary:     summary,
		Agents:      agents,
		Providers:   providers,
		Signals:     dashboardSignals(summary),
		Costs:       e.costsSnapshot(),
		Quotas:      e.quotaSnapshot(),
	}
}

func dashboardSignals(s dashboardSummary) []dashboardMarketSignal {
	return []dashboardMarketSignal{
		{Label: "Controle local", Value: "Loopback + tmux", Note: "AgentMesh opera em 127.0.0.1 e usa sessoes tmux reais em vez de depender de SaaS."},
		{Label: "Primitivas", Value: "send, broadcast, handoff, exec", Note: "O diferencial e coordenar agentes de terminal existentes com bloqueio, fila e deteccao de ciclo."},
		{Label: "Saude atual", Value: percent(s.MeshReadiness), Note: "Percentual de agentes prontos no snapshot local do motor."},
		{Label: "Fila", Value: intLabel(s.QueuedMessages), Note: "Mensagens aguardando agentes ocupados ficarem prontos."},
	}
}

func percent(v float64) string {
	b, _ := json.Marshal(v * 100)
	return string(b) + "%"
}

func intLabel(v int) string {
	b, _ := json.Marshal(v)
	return string(b)
}

const dashboardHTML = `<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>AgentMesh · painel do mesh</title>
  <style>
    :root {
      color-scheme: dark;
      --bg: #0c0f0d;
      --bg-grid: #10130f;
      --panel: #141805;
      --panel-2: #1a1f18;
      --line: #262b23;
      --line-strong: #3b4235;
      --text: #eae6d9;
      --muted: #9ba398;
      --faint: #666e63;
      --accent: #c8e06a;
      --accent-ink: #10130a;
      --amber: #e0a558;
      --red: #e2685c;
      --violet: #9c8fe0;
      --ink: #070907;
      --radius: 3px;
      --focus: #d7f28a;
    }
    * { box-sizing: border-box; }
    ::selection { background: var(--accent); color: var(--accent-ink); }
    html { color-scheme: dark; }
    body {
      margin: 0;
      min-height: 100vh;
      background:
        radial-gradient(1200px 480px at 12% -10%, rgba(200, 224, 106, .05), transparent 60%),
        repeating-linear-gradient(0deg, rgba(255,255,255,.014) 0px, rgba(255,255,255,.014) 1px, transparent 1px, transparent 22px),
        repeating-linear-gradient(90deg, rgba(255,255,255,.014) 0px, rgba(255,255,255,.014) 1px, transparent 1px, transparent 22px),
        var(--bg);
      color: var(--text);
      font: 13.5px/1.5 "IBM Plex Mono", "Berkeley Mono", ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace;
      font-variant-numeric: tabular-nums;
    }
    button, input, select { font: inherit; color: inherit; }
    a { color: var(--accent); text-decoration: none; }
    a:hover, a:focus-visible { text-decoration: underline; }
    :focus-visible {
      outline: 2px solid var(--focus);
      outline-offset: 2px;
      border-radius: 2px;
    }
    .sr-only {
      position: absolute; width: 1px; height: 1px; padding: 0; margin: -1px;
      overflow: hidden; clip: rect(0,0,0,0); white-space: nowrap; border: 0;
    }
    .shell {
      width: min(1400px, calc(100vw - 32px));
      margin: 0 auto;
      padding: 16px 0 48px;
    }

    /* header ---------------------------------------------------------- */
    .topbar {
      display: flex;
      align-items: center;
      gap: 18px;
      border: 1px solid var(--line);
      background: linear-gradient(180deg, #171c14, #12160f);
      padding: 10px 14px;
      border-radius: var(--radius);
      flex-wrap: wrap;
    }
    .brand { display: flex; align-items: center; gap: 10px; }
    .brand svg { flex: none; }
    .brand-text h1 {
      margin: 0;
      font-size: 15.5px;
      letter-spacing: .01em;
      font-weight: 700;
    }
    .brand-text .tagline { color: var(--faint); font-size: 11px; margin-top: 1px; }
    .divider-v { width: 1px; align-self: stretch; background: var(--line); margin: -2px 0; }
    nav.tabs {
      display: flex;
      gap: 2px;
      flex: 1;
      min-width: 200px;
    }
    nav.tabs [role="tab"] {
      appearance: none;
      background: transparent;
      border: 1px solid transparent;
      color: var(--muted);
      padding: 8px 12px;
      border-radius: var(--radius);
      cursor: pointer;
      font-size: 12.5px;
      letter-spacing: .01em;
      min-height: 34px;
    }
    nav.tabs [role="tab"]:hover { color: var(--text); }
    nav.tabs [role="tab"][aria-selected="true"] {
      color: var(--accent-ink);
      background: var(--accent);
      font-weight: 700;
    }
    .top-meta {
      display: flex;
      align-items: center;
      gap: 8px;
      flex-wrap: wrap;
      justify-content: flex-end;
    }
    .host {
      color: var(--faint);
      font-size: 11.5px;
      border: 1px solid var(--line);
      border-radius: var(--radius);
      padding: 0 8px;
      height: 30px;
      display: inline-flex;
      align-items: center;
    }
    .chip {
      border: 1px solid var(--line);
      background: rgba(0,0,0,.25);
      height: 30px;
      border-radius: var(--radius);
      display: inline-flex;
      align-items: center;
      gap: 7px;
      padding: 0 10px;
      white-space: nowrap;
      font-size: 12px;
    }
    .chip .dot { width: 7px; height: 7px; }
    .chip.live { border-color: color-mix(in srgb, var(--accent) 55%, var(--line)); color: var(--accent); }
    .chip.fallback { border-color: color-mix(in srgb, var(--amber) 55%, var(--line)); color: var(--amber); }
    .chip.error { border-color: color-mix(in srgb, var(--red) 55%, var(--line)); color: var(--red); }
    .stamp { color: var(--faint); font-size: 11px; }
    .btn {
      appearance: none;
      border: 1px solid var(--line-strong);
      background: var(--panel-2, #1a1f18);
      color: var(--text);
      height: 34px;
      min-width: 44px;
      border-radius: var(--radius);
      display: inline-flex;
      align-items: center;
      gap: 8px;
      padding: 0 12px;
      cursor: pointer;
      font-size: 12.5px;
    }
    .btn:hover { border-color: var(--faint); }
    .btn.primary {
      border-color: var(--accent);
      background: var(--accent);
      color: var(--accent-ink);
      font-weight: 700;
    }
    .btn.primary:hover { filter: brightness(1.06); }
    .btn.ghost { background: transparent; }
    .iconbtn { width: 34px; padding: 0; justify-content: center; }

    /* views ------------------------------------------------------------ */
    [role="tabpanel"][hidden] { display: none; }

    .metrics {
      display: grid;
      grid-template-columns: repeat(4, minmax(0, 1fr));
      gap: 10px;
      margin-top: 12px;
    }
    .metric, .panel {
      border: 1px solid var(--line);
      background: #131711;
      border-radius: var(--radius);
    }
    .metric {
      padding: 13px 14px;
      min-height: 92px;
      position: relative;
      overflow: hidden;
    }
    .metric-head {
      display: flex;
      align-items: center;
      justify-content: space-between;
      color: var(--muted);
    }
    .metric-head svg { opacity: .8; }
    .label {
      color: var(--muted);
      text-transform: uppercase;
      font-size: 10.5px;
      letter-spacing: .09em;
    }
    .value {
      margin-top: 10px;
      font-size: 27px;
      line-height: 1;
      font-weight: 700;
      letter-spacing: -.01em;
    }
    .metric.tone-ok .value { color: var(--accent); }
    .metric.tone-warn .value { color: var(--amber); }
    .metric.tone-bad .value { color: var(--red); }
    .note { margin-top: 8px; color: var(--faint); font-size: 11.5px; }
    .bar {
      height: 6px;
      border: 1px solid var(--line);
      background: var(--ink);
      border-radius: 999px;
      overflow: hidden;
      margin-top: 10px;
    }
    .bar > i {
      display: block;
      height: 100%;
      width: 0%;
      background: linear-gradient(90deg, var(--accent), var(--violet));
      transition: width .4s ease;
    }

    .layout {
      display: grid;
      grid-template-columns: minmax(0, 1fr) 372px;
      gap: 12px;
      margin-top: 12px;
      align-items: start;
    }
    .panel { padding: 14px; }
    .panel > header, .panel-head {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 10px;
      margin-bottom: 12px;
      padding-bottom: 10px;
      border-bottom: 1px solid var(--line);
      flex-wrap: wrap;
    }
    .panel h2 {
      margin: 0;
      font-size: 12.5px;
      text-transform: uppercase;
      letter-spacing: .07em;
      color: var(--text);
    }
    .panel .hint { color: var(--faint); font-size: 11px; }

    .toolrow { display: flex; gap: 8px; flex-wrap: wrap; }
    .search {
      display: flex;
      align-items: center;
      gap: 7px;
      border: 1px solid var(--line);
      background: var(--ink);
      border-radius: var(--radius);
      padding: 0 10px;
      height: 32px;
      min-width: 220px;
      color: var(--faint);
    }
    .search input {
      background: transparent;
      border: 0;
      color: var(--text);
      width: 100%;
      height: 100%;
    }
    .search input:focus-visible { outline: none; }
    select.statusfilter {
      border: 1px solid var(--line);
      background: var(--ink);
      color: var(--text);
      border-radius: var(--radius);
      height: 32px;
      padding: 0 8px;
    }

    table.agents {
      width: 100%;
      border-collapse: collapse;
      font-size: 12.5px;
    }
    table.agents th {
      text-align: left;
      color: var(--faint);
      font-weight: 500;
      text-transform: uppercase;
      font-size: 10.5px;
      letter-spacing: .06em;
      padding: 0 10px 8px;
      border-bottom: 1px solid var(--line);
    }
    table.agents td {
      padding: 10px;
      border-bottom: 1px solid var(--line);
      vertical-align: middle;
    }
    table.agents tbody tr { cursor: pointer; }
    table.agents tbody tr:hover, table.agents tbody tr:focus-within { background: rgba(200,224,106,.04); }
    table.agents tbody tr[aria-selected="true"] { background: rgba(200,224,106,.08); }
    .rowbtn {
      all: unset;
      display: block;
      width: 100%;
      cursor: pointer;
    }
    .agentname { font-weight: 700; }
    .agentpath {
      color: var(--faint);
      font-size: 11px;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      max-width: 30ch;
      display: block;
    }
    .status-pill {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      font-size: 12px;
    }
    .dot {
      width: 8px;
      height: 8px;
      border-radius: 50%;
      background: var(--faint);
      flex: none;
    }
    .dot.idle, .dot.completed { background: var(--accent); }
    .dot.processing { background: var(--amber); }
    .dot.error { background: var(--red); }
    .badge-attn {
      color: var(--amber);
      font-weight: 700;
      display: inline-flex;
      align-items: center;
      gap: 5px;
    }
    .muted-cell { color: var(--faint); }

    .drawer {
      margin-top: 10px;
      border: 1px solid var(--line-strong);
      border-radius: var(--radius);
      background: #10140d;
      padding: 12px;
    }
    .drawer-grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
      gap: 10px 16px;
      margin-bottom: 10px;
    }
    .drawer-grid div span { display: block; color: var(--faint); font-size: 10.5px; text-transform: uppercase; letter-spacing: .05em; }
    .drawer-grid div strong { font-size: 12.5px; }
    .drawer .actions { display: flex; gap: 8px; flex-wrap: wrap; }
    .screen {
      margin-top: 10px;
      background: var(--ink);
      border: 1px solid var(--line);
      border-radius: var(--radius);
      padding: 10px;
      max-height: 220px;
      overflow: auto;
      white-space: pre-wrap;
      font-size: 11.5px;
      color: var(--muted);
    }
    .keysend { display: flex; gap: 6px; margin-top: 8px; }
    .keysend input {
      border: 1px solid var(--line);
      background: var(--ink);
      color: var(--text);
      border-radius: var(--radius);
      height: 32px;
      padding: 0 8px;
      flex: 1;
      min-width: 0;
    }

    .empty {
      min-height: 160px;
      display: grid;
      place-items: center;
      text-align: center;
      border: 1px dashed var(--line-strong);
      border-radius: var(--radius);
      padding: 28px 18px;
      gap: 6px;
    }
    .empty h3 { margin: 8px 0 2px; font-size: 14px; }
    .empty p { margin: 0; color: var(--muted); font-size: 12px; max-width: 44ch; }
    .empty .cmdrow { display: flex; gap: 8px; margin-top: 14px; flex-wrap: wrap; justify-content: center; }
    code.cmd {
      display: inline-flex;
      align-items: center;
      gap: 8px;
      background: var(--ink);
      border: 1px solid var(--line);
      border-radius: var(--radius);
      padding: 6px 10px;
      font-size: 12px;
      color: var(--accent);
    }
    code.cmd button { all: unset; cursor: pointer; color: var(--faint); }
    code.cmd button:hover { color: var(--text); }

    /* delegation graph ---------------------------------------------------- */
    .topology { position: relative; min-height: 190px; }
    .graph-scroll {
      overflow-x: auto;
      overflow-y: hidden;
      padding-bottom: 4px;
      margin: -2px;
      padding: 2px;
    }
    .graph-canvas { position: relative; }
    .graph-canvas svg { position: absolute; inset: 0; overflow: visible; }
    .gnode {
      position: absolute;
      display: flex;
      align-items: center;
      gap: 7px;
      border: 1px solid var(--line-strong);
      background: #171c14;
      border-radius: var(--radius);
      padding: 0 10px;
      font-size: 11.5px;
      cursor: default;
    }
    .gnode:hover, .gnode:focus-visible { border-color: var(--faint); z-index: 2; }
    .gnode .dot { flex: none; }
    .gnode-body { min-width: 0; }
    .gnode-body strong {
      display: block;
      font-size: 12px;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .gnode-body span { color: var(--faint); font-size: 10px; }
    .gnode.idle, .gnode.completed { border-color: color-mix(in srgb, var(--accent) 55%, var(--line-strong)); }
    .gnode.processing { border-color: color-mix(in srgb, var(--amber) 55%, var(--line-strong)); }
    .gnode.error { border-color: color-mix(in srgb, var(--red) 55%, var(--line-strong)); }
    .gnode.attn { box-shadow: 0 0 0 1px var(--amber); }
    .topo-summary {
      display: flex;
      justify-content: space-between;
      gap: 10px;
      color: var(--faint);
      font-size: 11px;
      margin-top: 10px;
      padding-top: 10px;
      border-top: 1px solid var(--line);
      flex-wrap: wrap;
    }

    .facts { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; margin-top: 12px; }
    .fact {
      border: 1px solid var(--line);
      border-radius: var(--radius);
      padding: 9px 10px;
      background: #10140d;
      font-size: 11.5px;
    }
    .fact .label { display: block; margin-bottom: 3px; }

    .quota-grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
      gap: 12px;
    }
    .quota-block {
      border: 1px solid var(--line);
      border-radius: var(--radius);
      padding: 14px;
      background: #10140d;
    }
    .quota-provider {
      font-weight: 700;
      text-transform: uppercase;
      font-size: 11.5px;
      letter-spacing: .06em;
      color: var(--text);
      margin-bottom: 12px;
    }
    .quota-row {
      display: flex;
      justify-content: space-between;
      align-items: baseline;
      margin-bottom: 6px;
      gap: 8px;
    }
    .quota-row .label { text-transform: none; font-size: 12.5px; color: var(--text); letter-spacing: 0; }
    .quota-pct { font-size: 12.5px; color: var(--muted); white-space: nowrap; }
    .quota-block + .quota-block { margin-top: 0; }
    .quota-block .bar { margin-top: 0; }

    .activity { display: grid; gap: 0; margin-top: 12px; max-height: 220px; overflow: auto; }
    .event-row {
      display: grid;
      grid-template-columns: 58px minmax(0, 1fr);
      gap: 8px;
      border-bottom: 1px solid var(--line);
      padding: 8px 0;
      color: var(--muted);
      font-size: 11.5px;
    }
    .event-row:last-child { border-bottom: 0; }
    .event-row time { color: var(--faint); }
    .event-row strong { color: var(--text); font-weight: 600; }

    dialog#spawn-dialog {
      border: 1px solid var(--line-strong);
      background: #12160f;
      color: var(--text);
      border-radius: var(--radius);
      padding: 0;
      width: min(440px, calc(100vw - 40px));
    }
    dialog#spawn-dialog::backdrop { background: rgba(4,5,4,.72); }
    .dialog-inner { padding: 18px; }
    .dialog-inner h2 { margin: 0 0 4px; font-size: 15px; }
    .dialog-inner p.hint { margin: 0 0 16px; color: var(--faint); font-size: 12px; }
    .field { margin-bottom: 12px; }
    .field label { display: block; font-size: 11px; text-transform: uppercase; letter-spacing: .05em; color: var(--muted); margin-bottom: 5px; }
    .field input, .field select {
      width: 100%;
      border: 1px solid var(--line);
      background: var(--ink);
      color: var(--text);
      border-radius: var(--radius);
      height: 36px;
      padding: 0 10px;
    }
    .dialog-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 16px; }
    .form-error { color: var(--red); font-size: 12px; margin-top: 8px; min-height: 1em; }

    @media (max-width: 1040px) {
      .layout { grid-template-columns: 1fr; }
      .metrics { grid-template-columns: repeat(2, minmax(0, 1fr)); }
    }
    @media (max-width: 720px) {
      .shell { padding-top: 12px; }
      .metrics { grid-template-columns: 1fr; }
      .topbar { flex-direction: column; align-items: stretch; }
      .top-meta { justify-content: flex-start; }
      table.agents thead { display: none; }
      table.agents, table.agents tbody, table.agents tr, table.agents td { display: block; width: 100%; }
      table.agents tr { border-bottom: 1px solid var(--line); padding: 8px 0; }
      table.agents td { border: 0; padding: 3px 0; }
    }
    @media (prefers-reduced-motion: reduce) {
      * { animation-duration: .001ms !important; transition-duration: .001ms !important; }
    }
  </style>
</head>
<body>
  <div class="shell">
    <header class="topbar">
      <div class="brand">
        <svg width="26" height="26" viewBox="0 0 26 26" fill="none" aria-hidden="true">
          <circle cx="13" cy="5" r="3" fill="#c8e06a"/>
          <circle cx="5" cy="19" r="3" fill="#9c8fe0"/>
          <circle cx="21" cy="19" r="3" fill="#e0a558"/>
          <path d="M13 8v6M13 14 6 17M13 14l7 3" stroke="#48503f" stroke-width="1.4"/>
        </svg>
        <div class="brand-text">
          <h1>AgentMesh</h1>
          <div class="tagline">Local agent runtime</div>
        </div>
      </div>
      <div class="divider-v"></div>
      <nav class="tabs" role="tablist" aria-label="Seções do painel">
        <button role="tab" id="tab-overview" aria-controls="panel-overview" aria-selected="true">Visão geral</button>
        <button role="tab" id="tab-agents" aria-controls="panel-agents" aria-selected="false" tabindex="-1">Agentes</button>
        <button role="tab" id="tab-coord" aria-controls="panel-coord" aria-selected="false" tabindex="-1">Coordenação</button>
        <button role="tab" id="tab-costs" aria-controls="panel-costs" aria-selected="false" tabindex="-1">Custos</button>
      </nav>
      <div class="top-meta">
        <span class="host">127.0.0.1</span>
        <span class="chip" id="conn" role="status"><i class="dot"></i><span id="conn-text">aguardando dados</span></span>
        <span class="stamp" id="stamp"></span>
        <button class="btn primary" id="open-spawn"><span aria-hidden="true">+</span> Novo agente</button>
      </div>
    </header>

    <div id="live-announcer" class="sr-only" role="status" aria-live="polite"></div>

    <section id="panel-overview" role="tabpanel" aria-labelledby="tab-overview">
      <div class="metrics" id="metrics"></div>

      <div class="layout">
        <div>
          <section class="panel" aria-labelledby="agents-heading">
            <header>
              <h2 id="agents-heading">Agentes</h2>
              <span class="hint" id="agents-count"></span>
            </header>
            <div id="agents-body"></div>
          </section>
        </div>

        <aside>
          <section class="panel" aria-labelledby="topo-heading">
            <header>
              <h2 id="topo-heading">Mapa de delegações</h2>
              <span class="hint">setas = parent_id</span>
            </header>
            <div class="topology" id="topology"></div>
          </section>

          <section class="panel" style="margin-top:12px" aria-labelledby="events-heading">
            <header>
              <h2 id="events-heading">Eventos locais</h2>
              <span class="hint">nesta sessão</span>
            </header>
            <div class="activity" id="activity"></div>
          </section>
        </aside>
      </div>
    </section>

    <section id="panel-agents" role="tabpanel" aria-labelledby="tab-agents" hidden>
      <section class="panel" style="margin-top:12px" aria-labelledby="agents2-heading">
        <header>
          <h2 id="agents2-heading">Todos os agentes</h2>
          <div class="toolrow">
            <label class="search"><span aria-hidden="true">⌕</span>
              <input type="search" id="agent-search" placeholder="Buscar por nome ou diretório…" aria-label="Buscar agentes por nome ou diretório">
            </label>
            <select class="statusfilter" id="status-filter" aria-label="Filtrar por status">
              <option value="">Todos os status</option>
              <option value="idle">idle</option>
              <option value="processing">processing</option>
              <option value="completed">completed</option>
              <option value="error">error</option>
            </select>
          </div>
        </header>
        <div id="agents-body-2"></div>
      </section>
    </section>

    <section id="panel-coord" role="tabpanel" aria-labelledby="tab-coord" hidden>
      <div class="layout" style="grid-template-columns: 1fr 1fr;">
        <section class="panel" aria-labelledby="topo2-heading">
          <header>
            <h2 id="topo2-heading">Topologia de delegação</h2>
            <span class="hint" id="topo2-summary"></span>
          </header>
          <div class="topology" id="topology-2"></div>
        </section>
        <section class="panel" aria-labelledby="signals-heading">
          <header>
            <h2 id="signals-heading">Sinais e primitivas</h2>
          </header>
          <div class="facts" id="signals"></div>
        </section>
      </div>
    </section>

    <section id="panel-costs" role="tabpanel" aria-labelledby="tab-costs" hidden>
      <section class="panel" aria-labelledby="quota-heading">
        <header>
          <h2 id="quota-heading">Limites de uso do plano</h2>
          <span class="hint">sessão (5h) e semana — direto da conta, mesmos números do provider</span>
        </header>
        <div id="quota-body"></div>
      </section>

      <div class="metrics" id="cost-metrics" style="grid-template-columns: repeat(2, minmax(0, 1fr)); margin-top:12px;"></div>
      <section class="panel" style="margin-top:12px" aria-labelledby="costs-heading">
        <header>
          <h2 id="costs-heading">Custo por modelo</h2>
          <span class="hint">últimos 7 dias · ordenado por custo</span>
        </header>
        <div id="cost-body"></div>
        <p class="note" id="cost-caveat" style="margin-top:10px"></p>
      </section>
    </section>
  </div>

  <dialog id="spawn-dialog">
    <form method="dialog" class="dialog-inner" id="spawn-form">
      <h2>Novo agente</h2>
      <p class="hint">Sobe um processo dentro de uma sessão tmux própria via <code>POST /spawn</code>.</p>
      <div class="field">
        <label for="spawn-name">Nome</label>
        <input id="spawn-name" name="name" required placeholder="ex.: review" autocomplete="off">
      </div>
      <div class="field">
        <label for="spawn-command">Comando / provider</label>
        <select id="spawn-command" name="command">
          <option value="claude">claude</option>
          <option value="codex">codex</option>
          <option value="gemini">gemini</option>
          <option value="opencode">opencode</option>
          <option value="bash">shell (bash)</option>
        </select>
      </div>
      <div class="field">
        <label for="spawn-cwd">Diretório de trabalho</label>
        <input id="spawn-cwd" name="cwd" placeholder="padrão: diretório do motor" autocomplete="off">
      </div>
      <p class="form-error" id="spawn-error" role="alert"></p>
      <div class="dialog-actions">
        <button type="button" class="btn ghost" id="spawn-cancel">Cancelar</button>
        <button type="submit" class="btn primary" id="spawn-submit">Spawnar</button>
      </div>
    </form>
  </dialog>

  <script>
    const $ = (id) => document.getElementById(id);
    const announcer = $("live-announcer");
    let fallbackTimer = 0;
    let lastStreamAt = 0;
    let lastMode = "";
    const recentEvents = [];
    let currentAgents = [];
    let selectedId = "";
    let query = "";
    let statusFilter = "";

    const fmtTime = (seconds) => {
      if (seconds < 60) return seconds + "s";
      if (seconds < 3600) return Math.floor(seconds / 60) + "m";
      return Math.floor(seconds / 3600) + "h " + Math.floor((seconds % 3600) / 60) + "m";
    };
    const safe = (value) => String(value ?? "").replace(/[&<>"']/g, (c) => ({ "&":"&amp;", "<":"&lt;", ">":"&gt;", "\"":"&quot;", "'":"&#39;" }[c]));

    /* ---- tabs (roving-tabindex pattern) -------------------------------- */
    const tabs = [$("tab-overview"), $("tab-agents"), $("tab-coord"), $("tab-costs")];
    const panels = { "tab-overview": $("panel-overview"), "tab-agents": $("panel-agents"), "tab-coord": $("panel-coord"), "tab-costs": $("panel-costs") };
    function selectTab(tab) {
      tabs.forEach((t) => {
        const active = t === tab;
        t.setAttribute("aria-selected", String(active));
        t.tabIndex = active ? 0 : -1;
        panels[t.id].hidden = !active;
      });
      tab.focus();
      if (tab.id === "tab-coord") renderTopology(currentAgents, "topology-2", "topo2-summary");
    }
    tabs.forEach((tab, i) => {
      tab.addEventListener("click", () => selectTab(tab));
      tab.addEventListener("keydown", (e) => {
        if (e.key === "ArrowRight") selectTab(tabs[(i + 1) % tabs.length]);
        if (e.key === "ArrowLeft") selectTab(tabs[(i - 1 + tabs.length) % tabs.length]);
      });
    });

    /* ---- data loop ------------------------------------------------------ */
    async function loadDashboard() {
      const res = await fetch("/dashboard/data", { cache: "no-store" });
      const data = await res.json();
      render(data, "fallback");
    }

    function setConnectionState(mode) {
      const chip = $("conn");
      chip.className = "chip " + mode;
      const text = { live: "SSE · ao vivo", fallback: "polling · 2,5s", error: "sem conexão" }[mode] || "aguardando dados";
      $("conn-text").textContent = text;
      if (mode !== lastMode) {
        announcer.textContent = "Conexão: " + text;
        lastMode = mode;
      }
    }

    function startFallbackPolling() {
      if (fallbackTimer) return;
      fallbackTimer = window.setInterval(() => {
        if (Date.now() - lastStreamAt > 5000) {
          loadDashboard().catch(() => setConnectionState("error"));
        }
      }, 2500);
    }

    function connectStream() {
      if (!("EventSource" in window)) {
        startFallbackPolling();
        loadDashboard().catch(console.error);
        return false;
      }
      const events = new EventSource("/dashboard/events");
      const handle = (event) => {
        lastStreamAt = Date.now();
        render(JSON.parse(event.data), "live");
      };
      events.addEventListener("snapshot", handle);
      events.onmessage = handle;
      events.onerror = () => {
        setConnectionState("error");
        startFallbackPolling();
      };
      return true;
    }

    /* ---- render ----------------------------------------------------------- */
    function render(data, mode) {
      const s = data.summary;
      setConnectionState(mode || "fallback");
      $("stamp").textContent = "atualizado " + new Date(data.generated_at).toLocaleTimeString();
      recordSnapshot(data, mode || "fallback");
      currentAgents = data.agents;

      $("metrics").innerHTML = [
        metric("Agentes", s.total_agents, s.processing_agents + " processando · " + s.error_agents + " erro", null, "ok"),
        metric("Prontidão", Math.round(s.mesh_readiness * 100) + "%", s.ready_agents + " de " + s.total_agents + " disponíveis", s.mesh_readiness * 100, "ok"),
        metric("Fila", s.queued_messages, "mensagens aguardando entrega", null, s.queued_messages > 0 ? "warn" : "ok"),
        metric("Atenção", s.needs_attention, "intervenção humana", null, s.needs_attention > 0 ? "bad" : "ok")
      ].join("");

      renderAgentsTable("agents-body", data.agents, { withDrawer: true });
      renderAgentsTable("agents-body-2", filterAgents(data.agents), { withDrawer: true });
      $("agents-count").textContent = data.agents.length + " no total";
      renderTopology(data.agents, "topology", null);
      if (!$("panel-coord").hidden) renderTopology(data.agents, "topology-2", "topo2-summary");
      renderSignals(data.signals);
      renderCosts(data.costs);
      renderQuota(data.quotas);
      renderActivity();
    }

    function filterAgents(agents) {
      return agents.filter((a) => {
        if (statusFilter && a.status !== statusFilter) return false;
        if (!query) return true;
        const hay = (a.name + " " + a.cwd).toLowerCase();
        return hay.includes(query);
      });
    }

    function metric(label, value, note, bar, tone) {
      const progress = Number.isFinite(bar) ? '<div class="bar"><i style="width:' + Math.max(0, Math.min(100, bar)) + '%"></i></div>' : "";
      return '<article class="metric tone-' + tone + '"><div class="metric-head"><span class="label">' + safe(label) + '</span></div>' +
        '<div class="value">' + safe(value) + '</div>' + progress + '<div class="note">' + safe(note) + '</div></article>';
    }

    function renderAgentsTable(targetId, agents, opts) {
      const el = $(targetId);
      if (!agents.length) {
        el.innerHTML = emptyState();
        wireEmptyState(el);
        return;
      }
      el.innerHTML =
        '<table class="agents"><caption class="sr-only">Lista de agentes registrados no mesh</caption><thead><tr>' +
          '<th scope="col">Agente / diretório</th><th scope="col">Status</th><th scope="col">Provider</th>' +
          '<th scope="col">Uptime</th><th scope="col">Fila</th><th scope="col">Atenção</th>' +
        '</tr></thead><tbody>' +
        agents.map((a) => agentRow(a)).join("") +
        '</tbody></table>';
      el.querySelectorAll("tr[data-id]").forEach((tr) => {
        const btn = tr.querySelector(".rowbtn");
        btn.addEventListener("click", () => {
          selectedId = selectedId === tr.dataset.id ? "" : tr.dataset.id;
          renderAgentsTable(targetId, agents, opts);
        });
      });
      if (opts && opts.withDrawer && selectedId) {
        const agent = agents.find((a) => a.terminal_id === selectedId);
        if (agent) {
          const host = el.querySelector('tr[data-id="' + cssEscape(selectedId) + '"]');
          if (host) host.insertAdjacentHTML("afterend", '<tr><td colspan="6">' + drawer(agent) + '</td></tr>');
          wireDrawer(agent);
        }
      }
    }

    function cssEscape(v) { return window.CSS && CSS.escape ? CSS.escape(v) : v; }

    function agentRow(a) {
      const selected = a.terminal_id === selectedId;
      return '<tr data-id="' + safe(a.terminal_id) + '" aria-selected="' + selected + '">' +
        '<td><button class="rowbtn" aria-expanded="' + selected + '">' +
          '<span class="agentname">' + safe(a.name || a.terminal_id.slice(0, 8)) + '</span>' +
          '<span class="agentpath">' + safe(a.cwd) + '</span></button></td>' +
        '<td><span class="status-pill"><i class="dot ' + safe(a.status) + '" aria-hidden="true"></i>' + safe(a.status) + '</span></td>' +
        '<td class="muted-cell">' + safe(a.provider) + '</td>' +
        '<td>' + fmtTime(a.uptime_seconds) + '</td>' +
        '<td>' + safe(a.inbox_count) + '</td>' +
        '<td>' + (a.attention ? '<span class="badge-attn">⚠ Permissão</span>' : '<span class="muted-cell">—</span>') + '</td>' +
      '</tr>';
    }

    function drawer(a) {
      return '<div class="drawer">' +
        '<div class="drawer-grid">' +
          '<div><span>Pai</span><strong>' + (a.parent_id ? safe(a.parent_id.slice(0, 8)) : '—') + '</strong></div>' +
          '<div><span>Profundidade</span><strong>' + safe(a.chain_depth) + '</strong></div>' +
          '<div><span>Branch</span><strong>' + safe(a.branch || '—') + '</strong></div>' +
          '<div><span>Criado</span><strong>' + new Date(a.created_at).toLocaleTimeString() + '</strong></div>' +
        '</div>' +
        '<div class="actions">' +
          '<button class="btn" id="act-screen" data-id="' + safe(a.terminal_id) + '"><span aria-hidden="true">▤</span> Ver tela</button>' +
          '<button class="btn" id="act-kill" data-id="' + safe(a.terminal_id) + '"><span aria-hidden="true">✕</span> Encerrar</button>' +
        '</div>' +
        '<pre class="screen" id="screen-out" hidden></pre>' +
        '<form class="keysend" id="keysend-form">' +
          '<label class="sr-only" for="keysend-input">Tecla ou texto para enviar ao agente ' + safe(a.name) + '</label>' +
          '<input id="keysend-input" placeholder="Enter, C-c, Escape…">' +
          '<button class="btn" type="submit">Enviar tecla</button>' +
        '</form>' +
      '</div>';
    }

    function wireDrawer(agent) {
      const screenBtn = $("act-screen");
      const killBtn = $("act-kill");
      const keyForm = $("keysend-form");
      if (screenBtn) screenBtn.addEventListener("click", async () => {
        const out = $("screen-out");
        out.hidden = false;
        out.textContent = "carregando…";
        try {
          const res = await fetch("/agents/" + encodeURIComponent(agent.terminal_id) + "/screen");
          const data = await res.json();
          out.textContent = data.text || "(tela vazia)";
        } catch (err) {
          out.textContent = "erro ao ler tela: " + err;
        }
      });
      if (killBtn) killBtn.addEventListener("click", async () => {
        if (!window.confirm('Encerrar o agente "' + agent.name + '"?')) return;
        await fetch("/agents/" + encodeURIComponent(agent.terminal_id), { method: "DELETE" });
        selectedId = "";
        loadDashboard().catch(console.error);
      });
      if (keyForm) keyForm.addEventListener("submit", async (e) => {
        e.preventDefault();
        const input = $("keysend-input");
        if (!input.value) return;
        await fetch("/agents/" + encodeURIComponent(agent.terminal_id) + "/key", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ key: input.value })
        });
        input.value = "";
      });
    }

    function emptyState() {
      return '<div class="empty">' +
        '<div aria-hidden="true">⌁</div>' +
        '<h3>Nenhum agente em execução</h3>' +
        '<p>Suba o motor e adicione o primeiro agente pra ver dados aqui.</p>' +
        '<div class="cmdrow">' +
          copyCmd("agentmesh serve") +
          copyCmd("agentmesh spawn nome claude") +
        '</div>' +
      '</div>';
    }
    function copyCmd(cmd) {
      return '<code class="cmd">' + safe(cmd) + '<button type="button" class="copy-cmd" data-cmd="' + safe(cmd) + '" aria-label="Copiar comando ' + safe(cmd) + '">⧉</button></code>';
    }
    function wireEmptyState(el) {
      el.querySelectorAll(".copy-cmd").forEach((btn) => {
        btn.addEventListener("click", () => navigator.clipboard?.writeText(btn.dataset.cmd));
      });
    }

    // Lays the mesh out as an actual tree graph: x comes from subtree width
    // (post-order), y from depth, so siblings fan out under their real
    // parent instead of being grouped by depth across unrelated branches.
    const NODE_W = 136, NODE_H = 46, COL_GAP = 20, ROW_GAP = 46;

    function renderTopology(agents, targetId, summaryId) {
      const el = $(targetId);
      if (!agents.length) {
        el.innerHTML = '<div class="empty">Mesh sem peers registrados.</div>';
        if (summaryId) $(summaryId).textContent = "";
        return;
      }

      const byId = new Map(agents.map((a) => [a.terminal_id, a]));
      const childrenOf = new Map();
      agents.forEach((a) => {
        const parent = a.parent_id && byId.has(a.parent_id) ? a.parent_id : null;
        if (!childrenOf.has(parent)) childrenOf.set(parent, []);
        childrenOf.get(parent).push(a);
      });
      const roots = childrenOf.get(null) || [];

      const positions = new Map();
      const visited = new Set();
      let cursor = 0;
      let maxDepth = 0;
      function layout(agent, depth) {
        if (visited.has(agent.terminal_id)) return cursor; // cycle guard
        visited.add(agent.terminal_id);
        maxDepth = Math.max(maxDepth, depth);
        const kids = childrenOf.get(agent.terminal_id) || [];
        let x;
        if (!kids.length) {
          x = cursor;
          cursor++;
        } else {
          const xs = kids.map((k) => layout(k, depth + 1));
          x = (xs[0] + xs[xs.length - 1]) / 2;
        }
        positions.set(agent.terminal_id, { x, depth, agent });
        return x;
      }
      roots.forEach((r) => layout(r, 0));

      const colW = NODE_W + COL_GAP, rowH = NODE_H + ROW_GAP;
      const width = Math.max(cursor * colW, NODE_W + colW / 2);
      const height = (maxDepth + 1) * rowH;
      const markerId = "arrow-" + targetId;

      let edges = '<defs><marker id="' + markerId + '" markerWidth="7" markerHeight="7" refX="6" refY="3.5" orient="auto">' +
        '<path d="M0,0 L7,3.5 L0,7 z" fill="#5b6350"/></marker></defs>';
      let nodes = '';
      positions.forEach(({ x, depth, agent: a }) => {
        const cx = x * colW + colW / 2;
        const top = depth * rowH + 6;
        nodes += '<div class="gnode ' + safe(a.status) + (a.attention ? ' attn' : '') + '" tabindex="0" ' +
          'style="left:' + (cx - NODE_W / 2) + 'px;top:' + top + 'px;width:' + NODE_W + 'px;height:' + NODE_H + 'px" ' +
          'title="' + safe(a.cwd) + '" aria-label="' + safe(a.name || a.terminal_id.slice(0, 8)) + ', status ' + safe(a.status) + ', fila ' + safe(a.inbox_count) + '">' +
          '<i class="dot ' + safe(a.status) + '" aria-hidden="true"></i>' +
          '<div class="gnode-body"><strong>' + safe(a.name || a.terminal_id.slice(0, 8)) + '</strong>' +
          '<span>depth ' + safe(a.chain_depth) + ' · fila ' + safe(a.inbox_count) + '</span></div></div>';

        if (a.parent_id && positions.has(a.parent_id)) {
          const p = positions.get(a.parent_id);
          const px = p.x * colW + colW / 2, py = p.depth * rowH + 6 + NODE_H;
          const cyTop = top;
          const midY = (py + cyTop) / 2;
          edges += '<path d="M' + px + ',' + py + ' C ' + px + ',' + midY + ' ' + cx + ',' + midY + ' ' + cx + ',' + cyTop +
            '" fill="none" stroke="#5b6350" stroke-width="1.4" marker-end="url(#' + markerId + ')"/>';
        }
      });

      el.innerHTML = '<div class="graph-scroll"><div class="graph-canvas" style="width:' + width + 'px;height:' + height + 'px">' +
        '<svg width="' + width + '" height="' + height + '" aria-hidden="true">' + edges + '</svg>' +
        nodes +
      '</div></div>';

      const delegations = agents.filter((a) => a.parent_id).length;
      const worktrees = agents.filter((a) => a.branch).length;
      if (summaryId) $(summaryId).textContent = delegations + " delegações · " + worktrees + " worktrees";
      const topoHint = el.closest("section")?.querySelector(".hint");
      if (topoHint && targetId === "topology") topoHint.textContent = "setas = parent_id";
    }

    function renderSignals(signals) {
      $("signals").innerHTML = signals.map((sig) =>
        '<div class="fact"><span class="label">' + safe(sig.label) + '</span><strong>' + safe(sig.value) + '</strong><div class="note">' + safe(sig.note) + '</div></div>').join("");
    }

    function fmtCountdown(iso) {
      const ms = new Date(iso).getTime() - Date.now();
      if (!Number.isFinite(ms)) return "";
      if (ms <= 0) return "agora";
      const totalMin = Math.round(ms / 60000);
      const days = Math.floor(totalMin / 1440);
      const hours = Math.floor((totalMin % 1440) / 60);
      const mins = totalMin % 60;
      if (days > 0) return "em " + days + "d " + hours + "h";
      if (hours > 0) return "em " + hours + "h " + mins + "min";
      return "em " + mins + "min";
    }

    function quotaBlock(p) {
      if (!p.available) {
        return '<div class="quota-block"><div class="quota-provider">' + safe(p.provider) + '</div>' +
          '<p class="note">sem dado local/de conta: ' + safe(p.error) + '</p></div>';
      }
      const plan = p.plan_type ? " · plano " + safe(p.plan_type) : "";
      const sessionPct = Math.max(0, Math.min(100, p.session_pct));
      const weekPct = Math.max(0, Math.min(100, p.week_pct));
      return '<div class="quota-block">' +
        '<div class="quota-provider">' + safe(p.provider) + plan + '</div>' +
        '<div class="quota-row"><span class="label">Sessão atual (5h)</span><span class="quota-pct">' + Math.round(sessionPct) + '% usado</span></div>' +
        '<div class="bar"><i style="width:' + sessionPct + '%"></i></div>' +
        '<div class="note" style="margin-top:6px">reinicia ' + fmtCountdown(p.session_resets_at) + '</div>' +
        '<div class="quota-row" style="margin-top:14px"><span class="label">Limite semanal</span><span class="quota-pct">' + Math.round(weekPct) + '% usado</span></div>' +
        '<div class="bar"><i style="width:' + weekPct + '%"></i></div>' +
        '<div class="note" style="margin-top:6px">reinicia ' + fmtCountdown(p.week_resets_at) + '</div>' +
      '</div>';
    }

    function renderQuota(quotas) {
      const providers = (quotas && quotas.providers) || [];
      $("quota-body").innerHTML = providers.length
        ? '<div class="quota-grid">' + providers.map(quotaBlock).join("") + '</div>'
        : '<p class="note">Sem provider com quota de assinatura detectável nesta máquina.</p>';
    }

    function fmtUSD(v) {
      return "$" + (v < 1 ? v.toFixed(4) : v.toFixed(2));
    }

    function renderCosts(costs) {
      if (!costs) return;
      $("cost-metrics").innerHTML = [
        metric("Últimas 24h", fmtUSD(costs.day_24h_usd || 0), "gasto medido nos registros locais de cada CLI", null, "ok"),
        metric("Últimos 7 dias", fmtUSD(costs.week_7d_usd || 0), "soma por provider/modelo na tabela abaixo", null, "ok")
      ].join("");

      const rows = costs.week_7d_rows || [];
      if (!rows.length) {
        $("cost-body").innerHTML = '<div class="empty">Nenhum uso registrado localmente nos últimos 7 dias.</div>';
      } else {
        $("cost-body").innerHTML =
          '<table class="agents"><caption class="sr-only">Custo por provider e modelo, últimos 7 dias</caption><thead><tr>' +
            '<th scope="col">Provider / modelo</th><th scope="col">Entrada</th><th scope="col">Saída</th>' +
            '<th scope="col">Cache leitura</th><th scope="col">Cache escrita</th><th scope="col">Custo (7d)</th>' +
          '</tr></thead><tbody>' +
          rows.map((r) =>
            '<tr><td><span class="agentname">' + safe(r.provider) + '</span><span class="agentpath">' + safe(r.model) + '</span></td>' +
            '<td>' + safe(r.input_tokens.toLocaleString("pt-BR")) + '</td>' +
            '<td>' + safe(r.output_tokens.toLocaleString("pt-BR")) + '</td>' +
            '<td>' + safe(r.cache_read_tokens.toLocaleString("pt-BR")) + '</td>' +
            '<td>' + safe(r.cache_write_tokens.toLocaleString("pt-BR")) + '</td>' +
            '<td><strong>' + fmtUSD(r.cost_usd) + '</strong></td></tr>').join("") +
          '</tbody></table>';
      }

      const notes = [];
      if (costs.no_local_data && costs.no_local_data.length) {
        notes.push("sem dados locais de uso: " + costs.no_local_data.join(", "));
      }
      if (costs.unpriced_models && costs.unpriced_models.length) {
        notes.push("modelos sem tabela de preço (tokens contados, custo não somado): " + costs.unpriced_models.join(", "));
      }
      notes.push("calculado a partir dos registros locais de cada CLI (~/.claude/projects, ~/.codex/sessions, opencode.db) — todo uso na máquina, não só deste mesh.");
      $("cost-caveat").textContent = notes.join(" · ");
    }

    function recordSnapshot(data, mode) {
      const latest = recentEvents[0];
      const signature = [data.summary.total_agents, data.summary.ready_agents, data.summary.processing_agents,
        data.summary.error_agents, data.summary.queued_messages, data.summary.needs_attention].join(":");
      if (latest && latest.signature === signature) return;
      recentEvents.unshift({
        signature, mode,
        at: new Date(data.generated_at).toLocaleTimeString(),
        text: data.summary.total_agents + " agentes · " + data.summary.ready_agents + " prontos · " + data.summary.queued_messages + " na fila"
      });
      recentEvents.splice(8);
    }

    function renderActivity() {
      $("activity").innerHTML = recentEvents.length
        ? recentEvents.map((item) => '<div class="event-row"><time>' + safe(item.at) + '</time><span><strong>' + safe(item.mode) + '</strong> · ' + safe(item.text) + '</span></div>').join("")
        : '<p class="note">Sem eventos ainda nesta sessão.</p>';
    }

    /* ---- search / filter ------------------------------------------------- */
    $("agent-search").addEventListener("input", (e) => {
      query = e.target.value.trim().toLowerCase();
      renderAgentsTable("agents-body-2", filterAgents(currentAgents), { withDrawer: true });
    });
    $("status-filter").addEventListener("change", (e) => {
      statusFilter = e.target.value;
      renderAgentsTable("agents-body-2", filterAgents(currentAgents), { withDrawer: true });
    });

    /* ---- spawn dialog ------------------------------------------------------ */
    const dialog = $("spawn-dialog");
    $("open-spawn").addEventListener("click", () => { $("spawn-error").textContent = ""; dialog.showModal(); });
    $("spawn-cancel").addEventListener("click", () => dialog.close());
    $("spawn-form").addEventListener("submit", async (e) => {
      e.preventDefault();
      const name = $("spawn-name").value.trim();
      const command = $("spawn-command").value;
      const cwd = $("spawn-cwd").value.trim();
      const submit = $("spawn-submit");
      submit.disabled = true;
      try {
        const res = await fetch("/spawn", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ name, command, cwd })
        });
        if (!res.ok) {
          const body = await res.json().catch(() => ({}));
          throw new Error(body.error || ("HTTP " + res.status));
        }
        dialog.close();
        $("spawn-form").reset();
        loadDashboard().catch(console.error);
      } catch (err) {
        $("spawn-error").textContent = String(err.message || err);
      } finally {
        submit.disabled = false;
      }
    });

    if (!connectStream()) {
      loadDashboard().catch(() => setConnectionState("error"));
    }
  </script>
</body>
</html>`
