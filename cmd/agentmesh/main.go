// Command agentmesh runs a small local engine that lets several agent CLIs
// (claude, codex, gemini, opencode, or plain shells) run side by side and
// talk to each other — send messages, hand off tasks, run commands — right
// from your terminal.
//
// Typical use:
//
//	agentmesh serve &
//	agentmesh spawn coder   claude --cwd ~/myproject
//	agentmesh spawn reviewer claude --cwd ~/myproject
//	agentmesh attach coder            # interactive terminal glued to it
//	agentmesh send reviewer "olhe o PR aberto"
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NatanBack77/agentmesh/internal/mesh"
	"golang.org/x/term"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "spawn":
		err = cmdSpawn(args)
	case "ls", "peers", "list":
		err = cmdList(args)
	case "send", "assign":
		err = cmdSend(args)
	case "handoff":
		err = cmdHandoff(args)
	case "exec":
		err = cmdExec(args)
	case "whoami":
		err = cmdWhoami(args)
	case "kill":
		err = cmdKill(args)
	case "attach":
		err = cmdAttachOrWatch(args, "ATTACH")
	case "watch":
		err = cmdAttachOrWatch(args, "WATCH")
	case "demo":
		err = cmdDemo(args)
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
	fmt.Print(`agentmesh — motor local de comunicação entre agentes

  agentmesh serve [--port N]                    inicia o motor (fica em foreground)
  agentmesh spawn NOME COMANDO [ARGS...] [--cwd DIR]
  agentmesh ls | peers                          lista agentes rodando
  agentmesh send NOME "mensagem"                 manda mensagem (não bloqueia)
  agentmesh handoff NOME "tarefa" [--timeout S]  delega e espera o resultado
  agentmesh exec SHELL "comando"                 roda comando num agente shell e lê a saída
  agentmesh whoami                               identidade do agente atual (usa $AGENTMESH_TERMINAL_ID)
  agentmesh kill NOME                            mata um agente
  agentmesh attach NOME                          terminal interativo anexado (Ctrl+] desanexa)
  agentmesh watch NOME                           acompanha a saída (somente leitura)
  agentmesh demo [--agent claude|codex]          teste automático: sobe o motor,
                                                  spawna 2 agentes de verdade e faz
                                                  um mandar mensagem pro outro sozinho

Variáveis de ambiente: AGENTMESH_URL (default http://127.0.0.1:8990),
AGENTMESH_TERMINAL_ID (identidade do agente que está chamando, herdada
automaticamente em processos que o próprio agentmesh spawnou).
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
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return err
		}
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
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
	portStr, _ := flagValue(args, "--port")
	port := 8990
	if portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil {
			return fmt.Errorf("--port inválido: %w", err)
		}
		port = p
	}

	eng := mesh.New(mesh.Config{Port: port})

	sockPath := mesh.SocketPath()
	ln, err := eng.ServeAttach(sockPath)
	if err != nil {
		return fmt.Errorf("attach socket: %w", err)
	}
	defer ln.Close()
	fmt.Fprintf(os.Stderr, "agentmesh: socket de attach em %s\n", sockPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return eng.Start(ctx, port)
}

func cmdSpawn(args []string) error {
	cwd, args := flagValue(args, "--cwd")
	if len(args) < 2 {
		return fmt.Errorf("uso: agentmesh spawn NOME COMANDO [ARGS...] [--cwd DIR]")
	}
	name, command, rest := args[0], args[1], args[2:]
	var res mesh.SpawnResult
	err := apiRequest("POST", "/spawn", map[string]any{
		"name": name, "command": command, "args": rest, "cwd": cwd,
	}, &res)
	if err != nil {
		return err
	}
	fmt.Printf("spawned %q (%s)\n", res.Name, res.TerminalID)
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
	fmt.Printf("%-14s %-10s %-11s %-6s %s\n", "NOME", "PROVIDER", "STATUS", "PROF.", "CWD")
	for _, v := range views {
		fmt.Printf("%-14s %-10s %-11s %-6d %s\n", v.Name, v.Provider, v.Status, v.ChainDepth, v.CWD)
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
	if len(args) < 1 {
		return fmt.Errorf("uso: agentmesh kill NOME")
	}
	return apiRequest("DELETE", "/agents/"+args[0], nil, nil)
}

func cmdAttachOrWatch(args []string, mode string) error {
	if len(args) < 1 {
		return fmt.Errorf("uso: agentmesh %s NOME", strings.ToLower(mode))
	}
	name := args[0]
	conn, err := net.Dial("unix", mesh.SocketPath())
	if err != nil {
		return fmt.Errorf("não consegui conectar no socket do motor (`agentmesh serve` está rodando?): %w", err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "%s %s\n", mode, name)
	reply := make([]byte, 256)
	n, err := conn.Read(reply)
	if err != nil {
		return err
	}
	line := strings.TrimSpace(string(reply[:n]))
	if line != "OK" {
		return fmt.Errorf("%s", strings.TrimPrefix(line, "ERR "))
	}

	if mode == "ATTACH" {
		fmt.Fprintf(os.Stderr, "\x1b[2manexado a %q — Ctrl+] desanexa\x1b[0m\r\n", name)
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			_, err := io.Copy(os.Stdout, conn)
			return err
		}
		old, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return err
		}
		defer term.Restore(int(os.Stdin.Fd()), old)

		done := make(chan struct{})
		go func() {
			io.Copy(os.Stdout, conn)
			close(done)
		}()

		buf := make([]byte, 1)
		for {
			nr, err := os.Stdin.Read(buf)
			if err != nil {
				break
			}
			if nr > 0 && buf[0] == 0x1d { // Ctrl+]
				break
			}
			if nr > 0 {
				if _, err := conn.Write(buf[:nr]); err != nil {
					break
				}
			}
		}
		conn.Close()
		<-done
		fmt.Fprint(os.Stderr, "\r\n\x1b[2mdesanexado.\x1b[0m\r\n")
		return nil
	}

	// WATCH: read-only.
	fmt.Fprintf(os.Stderr, "\x1b[2macompanhando %q (somente leitura) — Ctrl+C sai\x1b[0m\r\n", name)
	_, err = io.Copy(os.Stdout, conn)
	return err
}

// ensureServer makes sure a motor is reachable at baseURL(), starting one
// in the background (detached, logging to ~/.agentmesh/serve.log) if not.
// This is what lets `agentmesh demo` (and any other command) be a single
// command with nothing to set up first.
func ensureServer() error {
	if reachable() {
		return nil
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

// sendKeys writes raw bytes straight into an agent's PTY — used to click
// through interactive first-run screens (theme picker, trust prompt) that
// exist before the agent ever reaches a detectable idle prompt.
func sendKeys(name, data string) error {
	return apiRequest("POST", "/agents/"+name+"/keys", map[string]string{"data": data}, nil)
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

// watchCapture dials the read-only attach socket for dur and returns
// whatever the agent printed in that window — used to prove a message
// actually landed on the target's screen.
func watchCapture(name string, dur time.Duration) (string, error) {
	conn, err := net.Dial("unix", mesh.SocketPath())
	if err != nil {
		return "", err
	}
	defer conn.Close()
	fmt.Fprintf(conn, "WATCH %s\n", name)
	reply := make([]byte, 256)
	n, err := conn.Read(reply)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(reply[:n]))
	if line != "OK" {
		return "", fmt.Errorf("%s", strings.TrimPrefix(line, "ERR "))
	}
	conn.SetReadDeadline(time.Now().Add(dur))
	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return out.String(), nil
}

func cmdDemo(args []string) error {
	agentCmd, _ := flagValue(args, "--agent")
	if agentCmd == "" {
		agentCmd = "claude"
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
			_ = sendKeys(name, "\r")
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
		fmt.Printf("    dica: `agentmesh attach alpha` (ou beta) pra ver a tela e destravar na mão.\n")
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
		fmt.Println("    alpha terminou o turno. Resposta dele:")
		fmt.Println("    ---")
		for _, l := range strings.Split(strings.TrimSpace(handoffRes.Result), "\n") {
			fmt.Println("    " + l)
		}
		fmt.Println("    ---")
	}

	fmt.Println("6/6 conferindo se a mensagem realmente chegou na tela do beta (até 30s — a detecção de turno pode achar o alpha 'pronto' antes dele de fato rodar o comando)...")
	found := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		seen, _ := watchCapture("beta", 3*time.Second)
		if strings.Contains(seen, "PING-"+token) {
			found = true
			break
		}
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
	fmt.Println("  agentmesh attach alpha    # ou beta — Ctrl+] desanexa")
	fmt.Println("  agentmesh ls")
	fmt.Println("pra encerrar o teste:")
	fmt.Println("  agentmesh kill alpha && agentmesh kill beta")
	return nil
}
