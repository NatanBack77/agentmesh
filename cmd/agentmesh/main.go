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
	"os"
	"os/signal"
	"strconv"
	"strings"
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

var _ = time.Second
