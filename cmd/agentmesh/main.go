// Command agentmesh runs a small local engine that lets several agent CLIs
// (claude, codex, gemini, opencode, or plain shells) run side by side and
// talk to each other — send messages, hand off tasks, run commands — right
// from your terminal. Every agent lives in its own tmux session: tmux does
// the actual terminal rendering/sizing/attaching, agentmesh only does the
// orchestration on top of it.
//
// Typical use:
//
//	agentmesh spawn coder    claude   # motor sobe sozinho se precisar
//	agentmesh spawn reviewer claude
//	agentmesh attach coder            # terminal de verdade (é um tmux attach)
//	agentmesh send reviewer "olhe o PR aberto"
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/NatanBack77/agentmesh/internal/mesh"
	"github.com/NatanBack77/agentmesh/internal/tmuxdrv"
	usagepkg "github.com/NatanBack77/agentmesh/internal/usage"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	// Todo comando que fala com o motor sobe um sozinho em background se
	// não encontrar nenhum rodando — só `serve` (que É o motor) e `-h`
	// ficam de fora. É isto que deixa `agentmesh spawn ...` funcionar de
	// primeira sem exigir `agentmesh serve &` manual antes.
	switch cmd {
	case "serve", "-h", "--help", "help", "usage", "cost":
		// "usage"/"cost" read local transcript files directly — no motor needed.
	default:
		if err := ensureServer(); err != nil {
			fmt.Fprintln(os.Stderr, "agentmesh:", err)
			os.Exit(1)
		}
	}

	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "spawn":
		err = cmdSpawn(args)
	case "ls", "peers", "list":
		err = cmdList(args)
	case "send", "assign":
		err = cmdSend(args)
	case "broadcast":
		err = cmdBroadcast(args)
	case "handoff":
		err = cmdHandoff(args)
	case "exec":
		err = cmdExec(args)
	case "whoami":
		err = cmdWhoami(args)
	case "kill":
		err = cmdKill(args)
	case "attach":
		err = cmdAttachOrWatch(args, false)
	case "watch":
		err = cmdAttachOrWatch(args, true)
	case "demo":
		err = cmdDemo(args)
	case "usage", "cost":
		err = cmdUsage(args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "agentmesh: comando desconhecido %q\n\n", cmd)
		usage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentmesh:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`agentmesh — motor local de comunicação entre agentes (roda sobre tmux)

  agentmesh serve [--port N]                    inicia o motor (fica em foreground)
  agentmesh spawn NOME COMANDO [ARGS...] [--cwd DIR] [--role "instrução"]
                                                  [--worktree] [--branch NOME]  (isolamento git, ver abaixo)
  agentmesh ls | peers                          lista agentes rodando (⚠ = esperando confirmação numa tela)
  agentmesh send NOME "mensagem"                 manda mensagem (não bloqueia)
  agentmesh broadcast "mensagem"                 manda pra TODOS os outros agentes
  agentmesh handoff NOME "tarefa" [--timeout S]  delega e espera o resultado
  agentmesh exec SHELL "comando"                 roda comando num agente shell e lê a saída
  agentmesh whoami                               identidade do agente atual (usa $AGENTMESH_TERMINAL_ID)
  agentmesh kill NOME [--remove-worktree] [--delete-branch] [--force]
                                                  mata um agente (por padrão deixa worktree/branch intactos)
  agentmesh attach NOME                          entra no terminal de verdade (tmux attach; Ctrl+B D desanexa)
  agentmesh watch NOME                           acompanha, somente leitura (tmux attach -r)
  agentmesh demo [--agent claude|codex]          teste automático: sobe o motor,
                                                  spawna 2 agentes de verdade e faz
                                                  um mandar mensagem pro outro sozinho
  agentmesh usage [--days N]                     custo/tokens do Claude Code (hoje + últimos N dias, default 7)

Requer tmux instalado. Variáveis de ambiente: AGENTMESH_URL (default
http://127.0.0.1:8990), AGENTMESH_TERMINAL_ID (identidade do agente que
está chamando, herdada automaticamente em processos que o próprio
agentmesh spawnou).
`)
}

func baseURL() string {
	if v := os.Getenv("AGENTMESH_URL"); v != "" {
		return v
	}
	return "http://127.0.0.1:8990"
}

func callerID() string { return os.Getenv("AGENTMESH_TERMINAL_ID") }

func apiRequest(method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, baseURL()+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if id := callerID(); id != "" {
		req.Header.Set("X-AgentMesh-Terminal-ID", id)
	}
	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("não consegui falar com o motor em %s (rodando? `agentmesh serve`): %w", baseURL(), err)
	}
	defer resp.Body.Close()

	// Read the body ONCE: it's an http.Response.Body, a single-use stream —
	// decoding it into `out` first and THEN trying to decode an error out
	// of it again (the old order) always found an already-drained reader
	// on failure, so every spawn/kill/etc error surfaced as a bare "HTTP
	// 400" instead of the actual reason from the server.
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return err
		}
	}
	return nil
}

func flagValue(args []string, name string) (string, []string) {
	out := make([]string, 0, len(args))
	val := ""
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			val = args[i+1]
			i++
			continue
		}
		out = append(out, args[i])
	}
	return val, out
}

func cmdServe(args []string) error {
	if err := tmuxdrv.Available(); err != nil {
		return err
	}
	portStr, _ := flagValue(args, "--port")
	port := 8990
	if portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil {
			return fmt.Errorf("--port inválido: %w", err)
		}
		port = p
	}

	eng := mesh.New(mesh.Config{})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return eng.Start(ctx, port)
}

func cmdSpawn(args []string) error {
	cwd, args := flagValue(args, "--cwd")
	role, args := flagValue(args, "--role")
	branch, args := flagValue(args, "--branch")
	worktree := false
	filtered := args[:0]
	for _, a := range args {
		if a == "--worktree" {
			worktree = true
			continue
		}
		filtered = append(filtered, a)
	}
	args = filtered
	if len(args) < 2 {
		return fmt.Errorf("uso: agentmesh spawn NOME COMANDO [ARGS...] [--cwd DIR] [--role \"instrução\"] [--worktree] [--branch NOME]")
	}
	if branch != "" {
		worktree = true // an explicit --branch implies isolation
	}
	name, command, rest := args[0], args[1], args[2:]

	// Sem --cwd, usa o diretório de onde este comando está sendo rodado —
	// não o diretório do motor (que pode estar em background há muito
	// tempo, ligado num diretório completamente diferente). É isto que
	// permite `cd ~/meu-projeto && agentmesh spawn nome claude` sem apontar
	// nada, e apontar dois agentes pro MESMO diretório é só repetir o
	// mesmo --cwd (ou rodar os dois spawns da mesma pasta) — nenhum dos
	// dois casos precisa de tratamento especial no servidor.
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("não consegui resolver o diretório atual: %w", err)
		}
		cwd = wd
	}
	var res mesh.SpawnResult
	err := apiRequest("POST", "/spawn", map[string]any{
		"name": name, "command": command, "args": rest, "cwd": cwd, "role": role,
		"worktree": worktree, "branch": branch,
	}, &res)
	if err != nil {
		return err
	}
	fmt.Printf("spawned %q (%s)\n", res.Name, res.TerminalID)
	if worktree {
		var v agentView
		if err := apiRequest("GET", "/agents/"+res.TerminalID, nil, &v); err == nil && v.Branch != "" {
			fmt.Printf("  isolado em worktree próprio, branch %q\n", v.Branch)
		}
	}
	return nil
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
	Attention  bool   `json:"attention"`
	Branch     string `json:"branch,omitempty"`
}

func cmdList(args []string) error {
	var views []agentView
	if err := apiRequest("GET", "/agents", nil, &views); err != nil {
		return err
	}
	if len(views) == 0 {
		fmt.Println("(nenhum agente rodando)")
		return nil
	}
	fmt.Printf("%-3s %-14s %-10s %-11s %-6s %-22s %s\n", "", "NOME", "PROVIDER", "STATUS", "PROF.", "BRANCH", "CWD")
	for _, v := range views {
		mark := ""
		if v.Attention {
			mark = "⚠"
		}
		branch := v.Branch
		if branch == "" {
			branch = "-"
		}
		fmt.Printf("%-3s %-14s %-10s %-11s %-6d %-22s %s\n", mark, v.Name, v.Provider, v.Status, v.ChainDepth, branch, v.CWD)
	}
	for _, v := range views {
		if v.Attention {
			fmt.Printf("\n⚠ %q está esperando confirmação numa tela (diálogo) — `agentmesh attach %s`\n", v.Name, v.Name)
		}
	}
	return nil
}

func cmdSend(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("uso: agentmesh send NOME \"mensagem\"")
	}
	var res struct {
		Success      bool   `json:"success"`
		Acknowledged bool   `json:"acknowledged"`
		Error        string `json:"error"`
	}
	err := apiRequest("POST", "/primitives/assign", map[string]string{
		"target_name": args[0], "message": args[1],
	}, &res)
	if err != nil {
		return err
	}
	fmt.Println("entregue.")
	return nil
}

func cmdBroadcast(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("uso: agentmesh broadcast \"mensagem\"")
	}
	var res struct {
		Sent []string `json:"sent"`
	}
	if err := apiRequest("POST", "/primitives/broadcast", map[string]string{"message": args[0]}, &res); err != nil {
		return err
	}
	if len(res.Sent) == 0 {
		fmt.Println("nenhum outro agente rodando pra receber.")
		return nil
	}
	fmt.Printf("entregue pra: %s\n", strings.Join(res.Sent, ", "))
	return nil
}

func cmdHandoff(args []string) error {
	timeoutStr, args := flagValue(args, "--timeout")
	if len(args) < 2 {
		return fmt.Errorf("uso: agentmesh handoff NOME \"tarefa\" [--timeout SEGUNDOS]")
	}
	timeout := 0
	if timeoutStr != "" {
		timeout, _ = strconv.Atoi(timeoutStr)
	}
	var res struct {
		Success bool   `json:"success"`
		Result  string `json:"result"`
		Error   string `json:"error"`
	}
	err := apiRequest("POST", "/primitives/handoff", map[string]any{
		"target_name": args[0], "message": args[1], "timeout_sec": timeout,
	}, &res)
	if err != nil {
		return err
	}
	fmt.Println(res.Result)
	return nil
}

func cmdExec(args []string) error {
	timeoutStr, args := flagValue(args, "--timeout")
	if len(args) < 2 {
		return fmt.Errorf("uso: agentmesh exec SHELL \"comando\" [--timeout SEGUNDOS]")
	}
	timeout := 0
	if timeoutStr != "" {
		timeout, _ = strconv.Atoi(timeoutStr)
	}
	var res struct {
		Result string `json:"result"`
	}
	err := apiRequest("POST", "/primitives/exec", map[string]any{
		"target_name": args[0], "message": args[1], "timeout_sec": timeout,
	}, &res)
	if err != nil {
		return err
	}
	fmt.Println(res.Result)
	return nil
}

func cmdWhoami(args []string) error {
	var v agentView
	if err := apiRequest("GET", "/whoami", nil, &v); err != nil {
		return err
	}
	fmt.Printf("nome: %s\nid: %s\nprovider: %s\nstatus: %s\ncwd: %s\n", v.Name, v.TerminalID, v.Provider, v.Status, v.CWD)
	return nil
}

func cmdKill(args []string) error {
	removeWorktree := false
	deleteBranch := false
	force := false
	filtered := args[:0]
	for _, a := range args {
		switch a {
		case "--remove-worktree":
			removeWorktree = true
		case "--delete-branch":
			deleteBranch = true
		case "--force":
			force = true
		default:
			filtered = append(filtered, a)
		}
	}
	args = filtered
	if len(args) < 1 {
		return fmt.Errorf("uso: agentmesh kill NOME [--remove-worktree] [--delete-branch] [--force]")
	}
	q := url.Values{}
	if removeWorktree {
		q.Set("remove_worktree", "1")
	}
	if deleteBranch {
		q.Set("delete_branch", "1")
	}
	if force {
		q.Set("force", "1")
	}
	path := "/agents/" + args[0]
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	return apiRequest("DELETE", path, nil, nil)
}

// cmdAttachOrWatch resolves the agent's real tmux session name and execs
// straight into `tmux attach`. tmux itself handles raw terminal mode,
// window sizing, resize-on-SIGWINCH, scrollback and detach (Ctrl+B D) — no
// custom protocol needed on agentmesh's side at all.
func cmdAttachOrWatch(args []string, readOnly bool) error {
	verb := "attach"
	if readOnly {
		verb = "watch"
	}
	if len(args) < 1 {
		return fmt.Errorf("uso: agentmesh %s NOME", verb)
	}
	if err := tmuxdrv.Available(); err != nil {
		return err
	}
	name := args[0]
	var v agentView
	if err := apiRequest("GET", "/agents/"+name, nil, &v); err != nil {
		return err
	}

	tmuxArgs := tmuxdrv.AttachArgs(v.TerminalID, readOnly)
	cmd := exec.Command("tmux", tmuxArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// ensureServer makes sure a motor is reachable at baseURL(), starting one
// in the background (detached, logging to ~/.agentmesh/serve.log) if not.
// This is what lets `agentmesh spawn ...` (and every other command) be a
// single command with nothing to set up first.
func ensureServer() error {
	if reachable() {
		return nil
	}
	if err := tmuxdrv.Available(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	logDir := home + "/.agentmesh"
	_ = os.MkdirAll(logDir, 0o755)
	logFile, err := os.OpenFile(logDir+"/serve.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	port := "8990"
	if u, err := url.Parse(baseURL()); err == nil && u.Port() != "" {
		port = u.Port()
	}
	cmd := exec.Command(exe, "serve", "--port", port)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "agentmesh: motor não estava rodando — subi um em background (pid %d, log em %s/serve.log)\n", cmd.Process.Pid, logDir)

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if reachable() {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("motor não respondeu em %s depois de subir", baseURL())
}

func reachable() bool {
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get(baseURL() + "/agents")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// sendKey sends one named tmux key (e.g. "Enter") straight into an agent's
// session — used to click through interactive first-run screens (theme
// picker, trust prompt) that exist before the agent ever reaches a
// detectable idle prompt.
func sendKey(name, key string) error {
	return apiRequest("POST", "/agents/"+name+"/key", map[string]string{"key": key}, nil)
}

// screenOf returns the agent's current visible pane as plain text.
func screenOf(name string) (string, error) {
	var res struct {
		Text string `json:"text"`
	}
	if err := apiRequest("GET", "/agents/"+name+"/screen", nil, &res); err != nil {
		return "", err
	}
	return res.Text, nil
}

// waitStatus polls until the named agent reaches one of the wanted statuses
// or the timeout elapses, returning the status observed at the end.
func waitStatus(name string, timeout time.Duration, wanted ...string) (string, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		var v agentView
		if err := apiRequest("GET", "/agents/"+name, nil, &v); err == nil {
			last = v.Status
			for _, w := range wanted {
				if v.Status == w {
					return v.Status, nil
				}
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return last, fmt.Errorf("timeout esperando %q chegar em %v (ficou em %q)", name, wanted, last)
}

// cmdUsage reports real token usage + estimated cost by reading Claude
// Code's own transcripts directly — no motor involved, works even if
// `agentmesh serve` isn't running, and covers ALL Claude Code usage on
// this machine (not just agents spawned through agentmesh).
func cmdUsage(args []string) error {
	daysStr, args := flagValue(args, "--days")
	oneline := false
	for _, a := range args {
		if a == "--oneline" {
			oneline = true
		}
	}
	days := 7
	if daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d > 0 {
			days = d
		}
	}
	var rep usagepkg.Report
	if oneline {
		// The full scan takes several seconds (it walks every transcript
		// under ~/.claude/projects) — fine for an on-demand report, way
		// too slow to re-run on every tmux status-interval tick (20s) or
		// the bar would visibly stall/flicker. Cache the result for a
		// minute; `agentmesh usage` (no --oneline) always scans fresh.
		r, err := usageCached(days)
		if err != nil {
			return nil // status bar: fail silently, never show a stack trace in the footer
		}
		rep = r
	} else {
		r, err := usagepkg.Scan(days)
		if err != nil {
			return err
		}
		rep = r
	}

	if oneline {
		// Colored progress bars for tmux's status-right (see
		// tmuxdrv.SetStatusBar) — every agentmesh-spawned claude session
		// shows this in its own footer, refreshed by tmux every 20s. tmux
		// re-parses #[...] style tags found INSIDE a #(command) result, so
		// this renders as real color in the status line, not literal text.
		dailyBudget := envFloat("AGENTMESH_DAILY_BUDGET", 50)
		weeklyBudget := envFloat("AGENTMESH_WEEKLY_BUDGET", 300)
		fmt.Printf("💰 hoje %s $%.2f  ·  7d %s $%.2f",
			tmuxBar(rep.Today.CostUSD/dailyBudget*100, 10),
			rep.Today.CostUSD,
			tmuxBar(rep.Week.CostUSD/weeklyBudget*100, 10),
			rep.Week.CostUSD,
		)
		return nil
	}

	dailyBudget := envFloat("AGENTMESH_DAILY_BUDGET", 50)
	weeklyBudget := envFloat("AGENTMESH_WEEKLY_BUDGET", 300)
	printUsageReport(rep, days, dailyBudget, weeklyBudget)
	return nil
}

// usage report is styled like a thermal-printer receipt — one amber "ink"
// accent for money and section rules, grayscale for everything else, red
// reserved for an actual budget breach. No rounded card, no red/yellow/
// green traffic-light bars: a receipt fits the subject (a $ readout) and
// doesn't look like every other lipgloss demo box.
const ledgerWidth = 62

var (
	ledgerInk    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214")) // amber
	ledgerDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	ledgerFaint  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	ledgerBright = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252"))
	ledgerAlert  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
)

// printUsageReport renders the full `agentmesh usage` report as a receipt:
// a torn-edge header/footer, today/week stamped as big totals with a
// sparkline of the underlying daily trend, then a dot-leader ledger for
// the per-day and per-model breakdowns.
func printUsageReport(rep usagepkg.Report, days int, dailyBudget, weeklyBudget float64) {
	dailyCosts := make([]float64, len(rep.Days))
	for i, d := range rep.Days {
		dailyCosts[i] = d.Totals.CostUSD
	}

	fmt.Println(ledgerPerforation())
	fmt.Println(ledgerCenter("A G E N T M E S H", "recibo de uso · claude"))
	fmt.Println(ledgerPerforation())
	fmt.Println()
	fmt.Println(ledgerStamp("HOJE", rep.Today.CostUSD, dailyBudget, fmtTokens(rep.Today), dailyCosts, "/dia"))
	fmt.Println()
	fmt.Println(ledgerStamp(fmt.Sprintf("%d DIAS", days), rep.Week.CostUSD, weeklyBudget, fmtTokens(rep.Week), dailyCosts, "/sem"))

	if len(rep.Days) > 1 {
		fmt.Println()
		fmt.Println(ledgerSection("POR DIA"))
		for _, d := range rep.Days {
			fmt.Println(ledgerLine(d.Date, d.Totals.CostUSD, fmtTokens(d.Totals)))
		}
	}
	if len(rep.ByModel) > 0 {
		fmt.Println()
		fmt.Println(ledgerSection("POR MODELO"))
		models := make([]string, 0, len(rep.ByModel))
		for m := range rep.ByModel {
			models = append(models, m)
		}
		sort.Slice(models, func(i, j int) bool { return rep.ByModel[models[i]].CostUSD > rep.ByModel[models[j]].CostUSD })
		for _, model := range models {
			t := rep.ByModel[model]
			fmt.Println(ledgerLine(model, t.CostUSD, fmtTokens(t)))
		}
	}

	fmt.Println()
	fmt.Println(ledgerPerforation())
	fmt.Println(ledgerFaint.Render("  estimativa direcional · lido de ~/.claude/projects · não é fatura"))
	fmt.Println(ledgerPerforation())
}

// ledgerPerforation draws the receipt's torn top/bottom edge.
func ledgerPerforation() string {
	return ledgerDim.Render(strings.Repeat("∴╌", ledgerWidth/2))
}

// ledgerCenter prints a bold title with a dim subtitle right after it,
// both centered-ish inside the receipt width.
func ledgerCenter(title, subtitle string) string {
	line := title + "   " + subtitle
	pad := max((ledgerWidth-len(line))/2, 0)
	return strings.Repeat(" ", pad) + ledgerInk.Render(title) + "   " + ledgerFaint.Render(subtitle)
}

// ledgerStamp renders one big total ("HOJE" / "N DIAS"): the label, the
// amount in ink, token count, and a sparkline of the daily trend next to
// the % of budget consumed. Only goes red if the budget is actually blown.
func ledgerStamp(label string, cost, budget float64, tokens string, trend []float64, per string) string {
	pct := 0.0
	if budget > 0 {
		pct = cost / budget * 100
	}
	pctStyle := ledgerFaint
	status := ""
	switch {
	case pct >= 100:
		pctStyle, status = ledgerAlert, "  ▲ estourou"
	case pct >= 85:
		pctStyle, status = ledgerInk, "  ▲ quase lá"
	}

	amount := fmt.Sprintf("$%.2f", cost)
	top := fmt.Sprintf("  %s%s%s",
		ledgerBright.Render(label),
		strings.Repeat(" ", max(1, ledgerWidth-len(label)-len(amount)-2)),
		ledgerInk.Render(amount),
	)
	bottom := fmt.Sprintf("  %s   %s %s%s",
		ledgerFaint.Render(tokens+" tokens"),
		ledgerSparkline(trend),
		pctStyle.Render(fmt.Sprintf("%.0f%% de %.0f%s", pct, budget, per)),
		pctStyle.Render(status),
	)
	return top + "\n" + bottom
}

// ledgerSparkline turns a daily cost series into an 8-level block
// sparkline (▁▂▃▄▅▆▇█), scaled to the series' own min/max so a quiet week
// and a spendy week both show visible shape instead of a flat line.
func ledgerSparkline(vals []float64) string {
	if len(vals) == 0 {
		return ledgerDim.Render("▁▁▁▁▁▁▁▁")
	}
	blocks := []rune("▁▂▃▄▅▆▇█")
	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	var b strings.Builder
	for _, v := range vals {
		idx := 0
		if hi > lo {
			idx = int((v - lo) / (hi - lo) * float64(len(blocks)-1))
		}
		b.WriteRune(blocks[idx])
	}
	return ledgerInk.Render(b.String())
}

// ledgerSection prints a small caps section header flanked by dim rules,
// e.g. "· · · · POR DIA · · · · · · · · · · · · · · · · · · · · · · · ·".
func ledgerSection(title string) string {
	label := " " + title + " "
	fill := ledgerWidth - len(label)
	left := fill / 3
	right := fill - left
	return ledgerDim.Render(strings.Repeat("·", left)) +
		ledgerBright.Render(label) +
		ledgerDim.Render(strings.Repeat("·", right))
}

// ledgerLine renders one dot-leader row: "label ..... $amount", the
// classic receipt/invoice alignment trick — no columns to eyeball, the
// dots do the aligning.
func ledgerLine(label string, cost float64, tokens string) string {
	amount := fmt.Sprintf("$%.2f", cost)
	suffix := "  " + amount
	dots := max(ledgerWidth-len(label)-len(suffix)-2, 3)
	return fmt.Sprintf("  %s %s%s  %s",
		ledgerBright.Render(label),
		ledgerDim.Render(strings.Repeat(".", dots)),
		ledgerInk.Render(suffix),
		ledgerFaint.Render(tokens+" tok"),
	)
}

// usageCached wraps usagepkg.Scan with a 60s file cache — the status bar
// (tmux #(), re-run on every status-interval tick) would otherwise pay the
// full multi-second transcript walk over and over for no new information.
func usageCached(days int) (usagepkg.Report, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return usagepkg.Report{}, err
	}
	cachePath := fmt.Sprintf("%s/.agentmesh/usage-cache-%dd.json", home, days)

	if fi, err := os.Stat(cachePath); err == nil && time.Since(fi.ModTime()) < 60*time.Second {
		if b, err := os.ReadFile(cachePath); err == nil {
			var rep usagepkg.Report
			if json.Unmarshal(b, &rep) == nil {
				return rep, nil
			}
		}
	}

	rep, err := usagepkg.Scan(days)
	if err != nil {
		return usagepkg.Report{}, err
	}
	if b, err := json.Marshal(rep); err == nil {
		_ = os.MkdirAll(home+"/.agentmesh", 0o755)
		_ = os.WriteFile(cachePath, b, 0o644)
	}
	return rep, nil
}

func envFloat(name string, def float64) float64 {
	if v := os.Getenv(name); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return def
}

// tmuxBar renders a colored block-progress bar using tmux's inline
// #[fg=...] style tags — tmux re-parses these when they come back through
// a #(command) substitution in status-right, so this prints as an actual
// colored bar in the terminal footer, not literal escape text. pct is
// clamped to [0,100]; color follows the same ok/warning/danger thresholds
// (60/85%) used elsewhere for usage indicators.
func tmuxBar(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := int(pct/100*float64(width) + 0.5)
	color := "colour46" // green
	switch {
	case pct >= 85:
		color = "colour196" // red
	case pct >= 60:
		color = "colour220" // yellow
	}
	var b strings.Builder
	b.WriteString("#[fg=" + color + "]")
	b.WriteString(strings.Repeat("█", filled))
	b.WriteString("#[fg=colour238]")
	b.WriteString(strings.Repeat("░", width-filled))
	b.WriteString("#[default] ")
	fmt.Fprintf(&b, "%.0f%%", pct)
	return b.String()
}

func fmtTokens(t usagepkg.Totals) string {
	total := t.InputTokens + t.OutputTokens + t.CacheReadTokens + t.CacheWriteTokens
	switch {
	case total >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(total)/1_000_000)
	case total >= 1_000:
		return fmt.Sprintf("%.1fk", float64(total)/1_000)
	default:
		return fmt.Sprintf("%d", total)
	}
}

func cmdDemo(args []string) error {
	agentCmd, _ := flagValue(args, "--agent")
	if agentCmd == "" {
		agentCmd = "claude"
	}
	if err := tmuxdrv.Available(); err != nil {
		return err
	}

	fmt.Println("== agentmesh demo ==")
	fmt.Println("1/6 checando o motor...")
	if err := ensureServer(); err != nil {
		return err
	}
	fmt.Println("    motor no ar em", baseURL())

	dirA, err := os.MkdirTemp("", "agentmesh-demo-alpha-")
	if err != nil {
		return err
	}
	dirB, err := os.MkdirTemp("", "agentmesh-demo-beta-")
	if err != nil {
		return err
	}

	fmt.Printf("2/6 subindo dois agentes reais (%s) em diretórios isolados...\n", agentCmd)
	if err := apiRequest("POST", "/spawn", map[string]any{"name": "alpha", "command": agentCmd, "cwd": dirA}, nil); err != nil {
		return fmt.Errorf("spawn alpha: %w", err)
	}
	if err := apiRequest("POST", "/spawn", map[string]any{"name": "beta", "command": agentCmd, "cwd": dirB}, nil); err != nil {
		return fmt.Errorf("spawn beta: %w", err)
	}
	fmt.Println("    alpha e beta no ar")

	fmt.Println("3/6 passando pelas telas de boot (tema/confiança) sozinho...")
	// A few spaced Enters click through whatever first-run screen shows up
	// (theme picker, "trust this folder?", etc.) — every one of those
	// dialogs defaults to a safe option, so a bare Enter just accepts it.
	// STOPS as soon as the agent reports a real status (idle/processing):
	// a stray extra Enter fired blindly AFTER the boot screens are gone can
	// land on the live prompt and submit an empty turn, which is exactly
	// what got a demo run stuck once — so this only sends as many as needed.
	clickThroughBoot := func(name string) {
		for i := 0; i < 6; i++ {
			var v agentView
			if err := apiRequest("GET", "/agents/"+name, nil, &v); err == nil && v.Status != "unknown" {
				return
			}
			time.Sleep(1200 * time.Millisecond)
			_ = sendKey(name, "Enter")
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); clickThroughBoot("alpha") }()
	go func() { defer wg.Done(); clickThroughBoot("beta") }()
	wg.Wait()

	fmt.Println("4/6 esperando os dois ficarem prontos...")
	stA, errA := waitStatus("alpha", 60*time.Second, "idle", "completed")
	stB, errB := waitStatus("beta", 60*time.Second, "idle", "completed")
	fmt.Printf("    alpha: %s   beta: %s\n", stA, stB)
	if errA != nil || errB != nil {
		fmt.Println("    aviso: pelo menos um não ficou pronto a tempo — tentando mesmo assim.")
		fmt.Println("    dica: `agentmesh attach alpha` (ou beta) pra ver a tela e destravar na mão.")
	}

	token := fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	msg := fmt.Sprintf("PING-%s de alpha! Se vc recebeu essa mensagem, é a prova de que o mesh de agentes funciona.", token)
	instruction := fmt.Sprintf(
		"Rode exatamente este comando usando sua ferramenta Bash, sem alterar nada nele, e não faça mais nada além disso: agentmesh send beta %q",
		msg,
	)

	fmt.Println("5/6 mandando o alpha avisar o beta sozinho (via `agentmesh send`, chamado por ele mesmo)...")
	var handoffRes struct {
		Success bool   `json:"success"`
		Result  string `json:"result"`
		Error   string `json:"error"`
	}
	err = apiRequest("POST", "/primitives/handoff", map[string]any{
		"target_name": "alpha", "message": instruction, "timeout_sec": 90,
	}, &handoffRes)
	if err != nil {
		fmt.Println("    handoff falhou:", err)
	} else {
		fmt.Println("    alpha terminou o turno — tela dele no momento:")
		fmt.Println("    ---")
		for _, l := range strings.Split(strings.TrimRight(handoffRes.Result, "\n"), "\n") {
			fmt.Println("    " + l)
		}
		fmt.Println("    ---")
	}

	fmt.Println("6/6 conferindo se a mensagem realmente chegou na tela do beta (até 30s — a detecção de turno pode achar o alpha 'pronto' antes dele de fato rodar o comando)...")
	found := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		seen, err := screenOf("beta")
		if err == nil && strings.Contains(seen, "PING-"+token) {
			found = true
			break
		}
		time.Sleep(1500 * time.Millisecond)
	}
	if found {
		fmt.Println("    ✔ confirmado: a mensagem apareceu no terminal do beta — comunicação agente-a-agente funcionando.")
	} else {
		fmt.Println("    não vi o texto exato ainda. Isso normalmente é a detecção de turno do alpha")
		fmt.Println("    (baseada em regex sobre a tela dele) achando que ele terminou antes da hora —")
		fmt.Println("    o comando pode ainda estar a caminho. Confira na mão:")
		fmt.Println("      agentmesh attach beta")
	}

	fmt.Println()
	fmt.Println("os dois agentes continuam rodando. pra explorar:")
	fmt.Println("  agentmesh attach alpha    # ou beta — Ctrl+B D desanexa")
	fmt.Println("  agentmesh ls")
	fmt.Println("pra encerrar o teste:")
	fmt.Println("  agentmesh kill alpha && agentmesh kill beta")
	return nil
}
