package mesh

import (
	"encoding/json"
	"net/http"
	"time"
)

// handleScreenEvents streams one agent's tmux pane over SSE so the
// dashboard can show it updating live instead of a one-shot snapshot.
//
// It costs nothing extra on the tmux side: every agent already has an
// OutputMonitor polling `tmux capture-pane` every 300ms for turn detection
// (turntimer.go), so this reads that same cached buffer (OutputMonitor.
// LastText) on a matching cadence and only writes to the client when the
// text actually changed — no second capture-pane process per agent, no
// redundant bytes over the wire for an unchanged screen. Mirrors
// handleDashboardEvents' shutdown/heartbeat shape for consistency.
func (e *Engine) handleScreenEvents(w http.ResponseWriter, r *http.Request) {
	ps, err := e.primitives.resolveTarget(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	mon := e.monitorFor(ps.TerminalID)
	if mon == nil {
		http.Error(w, "agent has no active output monitor", http.StatusNotFound)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
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
	lastText := ""

	write := func(text string) bool {
		payload, err := json.Marshal(map[string]string{"text": text})
		if err != nil {
			return true // transient marshal failure: keep the connection open
		}
		if _, err := w.Write([]byte("event: screen\ndata: ")); err != nil {
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

	lastText = mon.LastText()
	if !write(lastText) {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			text := mon.LastText()
			if text == lastText {
				continue
			}
			lastText = text
			if !write(text) {
				return
			}
		case <-heartbeat.C:
			if !write(lastText) {
				return
			}
		}
	}
}
