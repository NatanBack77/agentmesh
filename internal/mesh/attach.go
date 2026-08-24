package mesh

// attach.go — a tiny raw byte-duplex side channel over a Unix domain socket
// so `agentmesh attach <name>` can give you a real interactive terminal
// glued to an already-running agent's PTY, without going through HTTP
// framing. Protocol: client connects, writes "ATTACH <name>\n" (or "WATCH
// <name>\n" for read-only), server replies "OK\n" or "ERR <msg>\n", then it's
// raw bytes both ways until either side closes.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

// SocketPath returns the default attach-socket path, honoring $AGENTMESH_SOCK.
func SocketPath() string {
	if v := os.Getenv("AGENTMESH_SOCK"); v != "" {
		return v
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir, _ = os.UserHomeDir()
		dir = dir + "/.agentmesh"
	}
	_ = os.MkdirAll(dir, 0o700)
	return dir + "/agentmesh.sock"
}

// ServeAttach listens on the Unix socket at path and services ATTACH/WATCH
// requests until ctx-independent shutdown (caller closes the listener).
func (e *Engine) ServeAttach(path string) (net.Listener, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go e.handleAttachConn(conn)
		}
	}()
	return ln, nil
}

func (e *Engine) handleAttachConn(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		return
	}
	line = strings.TrimSpace(line)
	parts := strings.SplitN(line, " ", 2)
	if len(parts) != 2 {
		fmt.Fprint(conn, "ERR malformed request\n")
		return
	}
	mode, name := parts[0], parts[1]

	sessionID, err := e.ResolveSession(name)
	if err != nil {
		fmt.Fprintf(conn, "ERR %s\n", err.Error())
		return
	}
	fmt.Fprint(conn, "OK\n")

	ch, cancel := e.Subscribe(sessionID)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for data := range ch {
			if _, err := conn.Write(data); err != nil {
				return
			}
		}
	}()

	if mode == "ATTACH" {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				_ = e.WritePTY(sessionID, buf[:n])
			}
			if err != nil {
				if err != io.EOF {
					_ = err
				}
				break
			}
		}
	} else {
		// WATCH: read-only, block until the connection closes.
		io.Copy(io.Discard, r)
	}
	cancel()
	<-done
}
