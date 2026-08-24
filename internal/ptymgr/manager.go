// Package ptymgr spawns, tracks and kills PTY-backed agent processes.
//
// Adapted from Openfield's internal/pty package (github.com/NatanBack77/Openfield),
// stripped of the desktop-app-specific bits (crashlog, Wails event emission)
// and the fixed command whitelist — this is a personal terminal tool, not a
// multi-tenant desktop app, so any command in PATH is allowed to spawn.
package ptymgr

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
)

// Session represents one spawned PTY-backed process.
type Session struct {
	ID    string
	PTY   *os.File
	Cmd   *exec.Cmd
	OutCh chan []byte
	Done  chan struct{}
}

// Manager spawns, tracks and kills PTY-backed processes.
type Manager struct {
	sessions map[string]*Session
	mu       sync.RWMutex
	// onData is invoked for every output chunk produced by a session.
	onData func(sessionID string, data []byte)
	// onExit is invoked once, after the process has exited.
	onExit func(sessionID string)
}

func NewManager(onData func(sessionID string, data []byte), onExit func(sessionID string)) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		onData:   onData,
		onExit:   onExit,
	}
}

// guard recovers a panic on a background goroutine so one crashed agent
// session never takes the whole engine process down with it.
func guard(label string) {
	if r := recover(); r != nil {
		log.Printf("agentmesh: recovered panic in %s: %v", label, r)
	}
}

// Spawn creates a new PTY and starts command in cwd. If id is empty, a new
// UUID is generated. extraEnv entries are appended on top of os.Environ().
func (m *Manager) Spawn(id, command string, args []string, cwd string, extraEnv ...string) (*Session, error) {
	if err := m.validateCWD(cwd); err != nil {
		return nil, err
	}
	if id == "" {
		id = uuid.New().String()
	}

	if len(args) == 0 {
		switch filepath.Base(command) {
		case "bash", "zsh", "fish", "sh":
			args = []string{"-l"}
		}
	}

	execPath := command
	if filepath.Base(command) == command {
		if resolved, err := exec.LookPath(command); err == nil {
			execPath = resolved
		} else {
			return nil, fmt.Errorf("%q não encontrado no PATH: %w", command, err)
		}
	}

	cmd := exec.Command(execPath, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	cmd.Env = append(cmd.Env, extraEnv...)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("pty.Start(%q): %w", command, err)
	}

	s := &Session{
		ID:    id,
		PTY:   ptmx,
		Cmd:   cmd,
		OutCh: make(chan []byte, 256),
		Done:  make(chan struct{}),
	}

	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	go m.readLoop(s)
	return s, nil
}

// readLoop reads process output until EOF, batching in 16ms windows so a
// chatty agent doesn't flood downstream consumers (turn detector, attach
// subscribers) with thousands of tiny writes per second.
func (m *Manager) readLoop(s *Session) {
	defer guard("readLoop " + s.ID)

	rawCh := make(chan []byte, 512)

	go func() {
		defer guard("pty raw reader " + s.ID)
		defer close(rawCh)
		buf := make([]byte, 4096)
		for {
			n, err := s.PTY.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				select {
				case rawCh <- data:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()

	defer func() {
		close(s.Done)
		if m.onData != nil {
			m.onData(s.ID, []byte("\r\n\x1b[2m[agentmesh: process exited]\x1b[0m\r\n"))
		}
		m.mu.Lock()
		delete(m.sessions, s.ID)
		m.mu.Unlock()
		if m.onExit != nil {
			m.onExit(s.ID)
		}
	}()

	const batchWindow = 16 * time.Millisecond
	timer := time.NewTimer(batchWindow)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	armed := false

	batch := make([]byte, 0, 32*1024)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		data := make([]byte, len(batch))
		copy(data, batch)
		batch = batch[:0]
		select {
		case s.OutCh <- data:
		default:
		}
		if m.onData != nil {
			m.onData(s.ID, data)
		}
	}

	disarm := func() {
		if armed && !timer.Stop() {
			<-timer.C
		}
		armed = false
	}

	for {
		select {
		case data, ok := <-rawCh:
			if !ok {
				disarm()
				flush()
				return
			}
			batch = append(batch, data...)
			if len(batch) >= 32*1024 {
				disarm()
				flush()
				continue
			}
			if !armed {
				timer.Reset(batchWindow)
				armed = true
			}
		case <-timer.C:
			armed = false
			flush()
		}
	}
}

// Write sends bytes to the process stdin.
func (m *Manager) Write(id string, data []byte) error {
	m.mu.RLock()
	s, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	_, err := s.PTY.Write(data)
	return err
}

// Resize updates the PTY window size.
func (m *Manager) Resize(id string, cols, rows uint16) error {
	m.mu.RLock()
	s, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	return pty.Setsize(s.PTY, &pty.Winsize{Cols: cols, Rows: rows})
}

// Kill terminates the process and removes the session.
func (m *Manager) Kill(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	if !ok {
		return nil
	}
	s.PTY.Close()
	if s.Cmd.Process != nil {
		return s.Cmd.Process.Kill()
	}
	return nil
}

func (m *Manager) validateCWD(cwd string) error {
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("diretório não encontrado: %q", cwd)
		}
		return fmt.Errorf("diretório inválido %q: %v", cwd, err)
	}
	forbidden := []string{"/etc", "/sys", "/proc", "/boot", "/dev"}
	for _, f := range forbidden {
		if strings.HasPrefix(resolved, f) {
			return fmt.Errorf("diretório protegido pelo sistema: %s", f)
		}
	}
	return nil
}
