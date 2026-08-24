package mesh

// primitives.go — the four ways one agent talks to another:
//   exec     — run a command in a SHELL agent and read its output back
//   handoff  — blocking delegation: deliver a message, wait for the target
//              to finish its turn, return what it produced
//   assign   — fire-and-forget: deliver now if idle, queue if busy
//   send     — same delivery semantics as assign, targets by exact ID
//
// Turn completion is detected purely from PTY output (turntimer.go) — no
// cooperation required from the agent process itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

var (
	ErrTerminalNotFound   = errors.New("mesh: agent not found")
	ErrChainDepthExceeded = errors.New("mesh: max delegation depth exceeded")
	ErrHandoffTimeout     = errors.New("mesh: handoff timed out")
	ErrTargetDead         = errors.New("mesh: target agent died")
	ErrCycleDetected      = errors.New("mesh: delegation cycle detected")
	ErrReceiverBusy       = errors.New("mesh: receiver already has a pending handoff")
	ErrSelfMessage        = errors.New("mesh: cannot message yourself")
)

const handoffOutputCap = 64 * 1024

type handoffState struct {
	callerID   string
	targetID   string
	chainDepth int
	doneCh     chan struct{}
	closeOnce  sync.Once
	result     string
	err        error
	armed      atomic.Bool
	outMu      sync.Mutex
	output     []byte
}

func (h *handoffState) complete(result string, err error) {
	h.closeOnce.Do(func() {
		h.result = result
		h.err = err
		close(h.doneCh)
	})
}

func (h *handoffState) appendOutput(data []byte) {
	h.outMu.Lock()
	defer h.outMu.Unlock()
	h.output = append(h.output, data...)
	if over := len(h.output) - handoffOutputCap; over > 0 {
		h.output = h.output[over:]
	}
}

func (h *handoffState) snapshotOutput() string {
	h.outMu.Lock()
	raw := string(h.output)
	h.outMu.Unlock()
	return stripANSI(raw)
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]`)

func stripANSI(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	return strings.ReplaceAll(s, "\r", "")
}

type handoffStore struct {
	mu sync.Mutex
	m  map[string]*handoffState
}

func newHandoffStore() *handoffStore { return &handoffStore{m: make(map[string]*handoffState)} }

func (s *handoffStore) tryAcquire(targetID string, h *handoffState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.m[targetID]; exists {
		return ErrReceiverBusy
	}
	s.m[targetID] = h
	return nil
}

func (s *handoffStore) release(targetID string) {
	s.mu.Lock()
	delete(s.m, targetID)
	s.mu.Unlock()
}

func (s *handoffStore) get(targetID string) (*handoffState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.m[targetID]
	return h, ok
}

func (s *handoffStore) wouldDeadlock(callerID, targetID string) bool {
	if callerID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	visited := map[string]bool{}
	current := targetID
	for !visited[current] {
		visited[current] = true
		next := ""
		for _, hs := range s.m {
			if hs.callerID == current {
				next = hs.targetID
				break
			}
		}
		if next == "" {
			return false
		}
		if next == callerID {
			return true
		}
		current = next
	}
	return false
}

// Primitives implements handoff/assign/send/exec over a Registry.
type Primitives struct {
	registry *Registry
	writePTY func(sessionID string, data []byte) error
	handoffs *handoffStore

	maxDepth   int
	handoffTTL time.Duration

	onInputDelivered func(terminalID string)
	onFlow           func(sourceID, targetID, kind string)
}

func NewPrimitives(registry *Registry, writePTY func(string, []byte) error, maxDepth int, handoffTTL time.Duration) *Primitives {
	return &Primitives{
		registry:   registry,
		writePTY:   writePTY,
		handoffs:   newHandoffStore(),
		maxDepth:   maxDepth,
		handoffTTL: handoffTTL,
	}
}

func (h *Primitives) inputDelivered(id string) {
	if h.onInputDelivered != nil {
		h.onInputDelivered(id)
	}
}

func (h *Primitives) notifyFlow(src, tgt, kind string) {
	if h.onFlow != nil && src != "" && tgt != "" {
		h.onFlow(src, tgt, kind)
	}
}

// resolveTarget resolves a name or ID to an AgentState. Every registered
// agent is a peer of every other — there is no per-workspace scoping in
// this standalone build.
func (h *Primitives) resolveTarget(nameOrID string) (*AgentState, error) {
	if ps, ok := h.registry.ByID(nameOrID); ok {
		return ps, nil
	}
	if ps, ok := h.registry.ByName(nameOrID); ok {
		return ps, nil
	}
	all := h.registry.All()
	lower := strings.ToLower(nameOrID)
	var partial []*AgentState
	names := make([]string, 0, len(all))
	for _, ps := range all {
		name := strings.ToLower(ps.AgentName)
		if name == lower {
			return ps, nil
		}
		if lower != "" && strings.Contains(name, lower) {
			partial = append(partial, ps)
		}
		names = append(names, ps.AgentName)
	}
	if len(partial) == 1 {
		return partial[0], nil
	}
	sort.Strings(names)
	if len(partial) > 1 {
		ambiguous := make([]string, len(partial))
		for i, ps := range partial {
			ambiguous[i] = ps.AgentName
		}
		sort.Strings(ambiguous)
		return nil, fmt.Errorf("%w: %q is ambiguous — matches: %s", ErrTerminalNotFound, nameOrID, strings.Join(ambiguous, ", "))
	}
	return nil, fmt.Errorf("%w: %q — registered agents: %s", ErrTerminalNotFound, nameOrID, strings.Join(names, ", "))
}

func (h *Primitives) validateChain(callerID, targetID string, callerDepth int) error {
	if callerDepth+1 > h.maxDepth {
		return fmt.Errorf("%w: depth %d > max %d", ErrChainDepthExceeded, callerDepth+1, h.maxDepth)
	}
	visited := map[string]bool{callerID: true}
	current := callerID
	for {
		ps, ok := h.registry.ByID(current)
		if !ok {
			break
		}
		parent := ps.ParentTerminalID
		if parent == "" {
			break
		}
		if visited[parent] || parent == targetID {
			return fmt.Errorf("%w: %q is already in the delegation chain", ErrCycleDetected, targetID)
		}
		visited[parent] = true
		current = parent
	}
	if targetID == callerID {
		return fmt.Errorf("%w: caller and target are the same agent", ErrCycleDetected)
	}
	return nil
}

type handoffRequest struct {
	TargetName string `json:"target_name"`
	Message    string `json:"message"`
	TimeoutSec int    `json:"timeout_sec"`
}

type handoffResponse struct {
	Success    bool   `json:"success"`
	TerminalID string `json:"terminal_id,omitempty"`
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
}

// DoHandoff blocks until target becomes ready after receiving message, then
// returns what it produced.
func (h *Primitives) DoHandoff(ctx context.Context, callerID string, req handoffRequest) (handoffResponse, int) {
	target, err := h.resolveTarget(req.TargetName)
	if err != nil {
		return handoffResponse{Success: false, Error: err.Error()}, http.StatusNotFound
	}

	var callerDepth int
	if callerID != "" {
		if caller, ok := h.registry.ByID(callerID); ok {
			caller.mu.Lock()
			callerDepth = caller.ChainDepth
			caller.mu.Unlock()
		}
	}

	target.mu.Lock()
	targetID := target.TerminalID
	targetSID := target.SessionID
	target.mu.Unlock()

	if err := h.validateChain(callerID, targetID, callerDepth); err != nil {
		return handoffResponse{Success: false, TerminalID: targetID, Error: err.Error()}, http.StatusConflict
	}
	if h.handoffs.wouldDeadlock(callerID, targetID) {
		return handoffResponse{
			Success: false, TerminalID: targetID,
			Error: fmt.Sprintf("%v: target %q is already waiting on you", ErrCycleDetected, req.TargetName),
		}, http.StatusConflict
	}

	hs := &handoffState{callerID: callerID, targetID: targetID, chainDepth: callerDepth + 1, doneCh: make(chan struct{})}
	if err := h.handoffs.tryAcquire(targetID, hs); err != nil {
		return handoffResponse{Success: false, TerminalID: targetID, Error: err.Error()}, http.StatusConflict
	}
	defer h.handoffs.release(targetID)

	ttl := h.handoffTTL
	if req.TimeoutSec > 0 {
		ttl = time.Duration(req.TimeoutSec) * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, ttl)
	defer cancel()

	if err := waitTargetReady(waitCtx, target); err != nil {
		return handoffResponse{Success: false, TerminalID: targetID, Error: err.Error()}, http.StatusGatewayTimeout
	}

	target.mu.Lock()
	target.ChainDepth = callerDepth + 1
	target.ParentTerminalID = callerID
	target.notifyInputSent()
	target.mu.Unlock()

	hs.armed.Store(true)

	if err := deliverToPTY(h.writePTY, targetSID, req.Message); err != nil {
		return handoffResponse{Success: false, TerminalID: targetID, Error: fmt.Sprintf("write PTY: %v", err)}, http.StatusInternalServerError
	}
	h.inputDelivered(targetID)
	h.notifyFlow(callerID, targetID, "handoff")

	select {
	case <-hs.doneCh:
		if hs.err != nil {
			return handoffResponse{Success: false, TerminalID: targetID, Error: hs.err.Error()}, http.StatusGatewayTimeout
		}
		return handoffResponse{Success: true, TerminalID: targetID, Result: hs.result}, http.StatusOK
	case <-target.dead:
		return handoffResponse{Success: false, TerminalID: targetID, Error: ErrTargetDead.Error()}, http.StatusBadGateway
	case <-waitCtx.Done():
		hs.complete("", fmt.Errorf("%w after %s", ErrHandoffTimeout, ttl))
		target.mu.Lock()
		st, since := target.Status, time.Since(target.LastStatusChange).Round(time.Second)
		target.mu.Unlock()
		return handoffResponse{
			Success: false, TerminalID: targetID,
			Error: fmt.Sprintf("%v after %s (status %s for %s)", ErrHandoffTimeout, ttl, st, since),
		}, http.StatusGatewayTimeout
	}
}

// DoExec is handoff specialized for shell targets.
func (h *Primitives) DoExec(ctx context.Context, callerID string, req handoffRequest) (handoffResponse, int) {
	target, err := h.resolveTarget(req.TargetName)
	if err != nil {
		return handoffResponse{Success: false, Error: err.Error()}, http.StatusNotFound
	}
	target.mu.Lock()
	provider := target.Provider
	target.mu.Unlock()
	if provider != ProviderShell {
		return handoffResponse{
			Success: false,
			Error:   fmt.Sprintf("target %q is not a shell (provider %s) — exec only runs on shells; use handoff for agents", req.TargetName, provider),
		}, http.StatusUnprocessableEntity
	}
	return h.DoHandoff(ctx, callerID, req)
}

func waitTargetReady(ctx context.Context, target *AgentState) error {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		target.mu.Lock()
		ready := target.Status.isReady()
		target.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-target.dead:
			return ErrTargetDead
		case <-ctx.Done():
			target.mu.Lock()
			st, since := target.Status, time.Since(target.LastStatusChange).Round(time.Second)
			target.mu.Unlock()
			return fmt.Errorf("%w: target never became ready (status %s for %s)", ErrHandoffTimeout, st, since)
		case <-tick.C:
		}
	}
}

// NotifyReady unblocks any waiting ARMED handoff for terminalID.
func (h *Primitives) NotifyReady(terminalID, fallback string) {
	hs, ok := h.handoffs.get(terminalID)
	if !ok || !hs.armed.Load() {
		return
	}
	out := hs.snapshotOutput()
	if out == "" {
		out = fallback
	}
	hs.complete(out, nil)
}

// CaptureOutput feeds target output into its in-flight handoff buffer.
func (h *Primitives) CaptureOutput(terminalID string, data []byte) {
	hs, ok := h.handoffs.get(terminalID)
	if !ok || !hs.armed.Load() {
		return
	}
	hs.appendOutput(data)
}

// NotifyDead unblocks any waiting handoff with ErrTargetDead.
func (h *Primitives) NotifyDead(terminalID string) {
	if hs, ok := h.handoffs.get(terminalID); ok {
		hs.complete("", ErrTargetDead)
	}
}

type assignRequest struct {
	TargetName string `json:"target_name"`
	Message    string `json:"message"`
}

type assignResponse struct {
	Success      bool   `json:"success"`
	TerminalID   string `json:"terminal_id,omitempty"`
	Acknowledged bool   `json:"acknowledged"`
	Error        string `json:"error,omitempty"`
}

// DoAssign delivers a message without blocking: now if idle, queued if busy.
func (h *Primitives) DoAssign(callerID string, req assignRequest) (assignResponse, int) {
	target, err := h.resolveTarget(req.TargetName)
	if err != nil {
		return assignResponse{Success: false, Error: err.Error()}, http.StatusNotFound
	}

	target.mu.Lock()
	targetID := target.TerminalID
	targetSID := target.SessionID
	targetStatus := target.Status
	if callerID != "" {
		target.ParentTerminalID = callerID
	}
	target.mu.Unlock()

	if targetStatus.isReady() {
		target.mu.Lock()
		target.notifyInputSent()
		target.mu.Unlock()
		if err := deliverToPTY(h.writePTY, targetSID, req.Message); err != nil {
			return assignResponse{Success: false, TerminalID: targetID, Error: fmt.Sprintf("write PTY: %v", err)}, http.StatusInternalServerError
		}
		h.inputDelivered(targetID)
		h.notifyFlow(callerID, targetID, "message")
		return assignResponse{Success: true, TerminalID: targetID, Acknowledged: true}, http.StatusOK
	}

	enqueueInbox(target, callerID, req.Message)
	h.notifyFlow(callerID, targetID, "message")
	return assignResponse{Success: true, TerminalID: targetID, Acknowledged: true}, http.StatusAccepted
}

// deliverToPTY writes text then a real Enter keypress (TUI agents don't
// submit on plain "\n"). Settle time scales with message length so a busy
// TUI still ingesting the paste doesn't eat the Enter as a newline; the
// second Enter is a no-op safety net if the first already submitted.
func deliverToPTY(writePTY func(string, []byte) error, sessionID, message string) error {
	text := strings.TrimRight(message, "\r\n")
	if err := writePTY(sessionID, []byte(text)); err != nil {
		return err
	}
	settle := 150*time.Millisecond + time.Duration(len(text)/20)*time.Millisecond
	settle = min(settle, 1200*time.Millisecond)
	time.Sleep(settle)
	if err := writePTY(sessionID, []byte("\r")); err != nil {
		return err
	}
	time.Sleep(400 * time.Millisecond)
	return writePTY(sessionID, []byte("\r"))
}

func enqueueInbox(ps *AgentState, senderID, body string) {
	msg := Message{ID: uuid.New().String(), SenderID: senderID, ReceiverID: ps.TerminalID, Body: body, CreatedAt: time.Now()}
	ps.mu.Lock()
	ps.InboxQueue = append(ps.InboxQueue, msg)
	ps.mu.Unlock()
}

// drainInbox delivers all queued inbox messages once the target goes ready.
func drainInbox(ps *AgentState, writePTY func(string, []byte) error, onDelivered func()) {
	ps.mu.Lock()
	if len(ps.InboxQueue) == 0 {
		ps.mu.Unlock()
		return
	}
	msgs := ps.InboxQueue
	ps.InboxQueue = []Message{}
	sessionID := ps.SessionID
	ps.notifyInputSent()
	ps.mu.Unlock()

	for _, m := range msgs {
		_ = deliverToPTY(writePTY, sessionID, m.Body)
	}
	if onDelivered != nil {
		onDelivered()
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
