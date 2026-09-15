package mesh

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Cost tracking reads each CLI's own local transcript/session store instead
// of estimating — every provider persists per-turn token usage somewhere on
// disk, so the numbers here are measured, not guessed:
//
//   - Claude Code: ~/.claude/projects/<encoded-cwd>/<session>.jsonl — one
//     line per turn, "message.usage" carries exact input/output/cache
//     tokens plus the model. costUSD is present but null on subscription
//     plans, so cost is computed here from Anthropic's published per-token
//     rates and prompt-cache multipliers (1.25x/2x write, 0.1x read —
//     0.025x read for the Fable/Mythos 5.1 tier).
//   - Codex: ~/.codex/sessions/<date>/rollout-*.jsonl — "turn_context"
//     events carry the active model, "token_count" events carry per-turn
//     usage deltas (last_token_usage). Cost computed from OpenAI's
//     published per-token rates; cached input is billed at the reduced
//     cached-input rate, not the base input rate.
//   - opencode: ~/.local/share/opencode/opencode.db (sqlite) — opencode
//     already computes and stores a "cost" field per assistant message
//     using its own pricing database, so that figure is used as-is rather
//     than re-derived.
//   - Gemini CLI: no local session/usage log was found on this machine —
//     reported explicitly as unavailable rather than estimated.
//
// All of this is best-effort against whatever's on disk for the current
// OS user, machine-wide (not scoped to agentmesh's own spawned agents) —
// it answers "what did I actually spend today/this week", which is the
// only version of this question these tools let us answer precisely.

type tokenPricing struct {
	Input        float64 // USD per token
	CacheWrite5m float64
	CacheWrite1h float64
	CacheRead    float64
	Output       float64
}

func claudePrice(inputUSD, outputUSD, cacheReadMultiplier float64) tokenPricing {
	in := inputUSD / 1e6
	return tokenPricing{
		Input:        in,
		CacheWrite5m: in * 1.25,
		CacheWrite1h: in * 2.0,
		CacheRead:    in * cacheReadMultiplier,
		Output:       outputUSD / 1e6,
	}
}

func openAIPrice(inputUSD, cachedInputUSD, outputUSD float64) tokenPricing {
	return tokenPricing{
		Input:     inputUSD / 1e6,
		CacheRead: cachedInputUSD / 1e6,
		Output:    outputUSD / 1e6,
	}
}

// Pricing cached 2026-09-08 from platform.claude.com and
// developers.openai.com/api/docs/pricing. Anthropic cache-read multiplier
// is 0.1x base input for every tier except Fable/Mythos 5.1, which use
// 0.025x.
var claudePricing = map[string]tokenPricing{
	"claude-opus-5":     claudePrice(5.00, 25.00, 0.1),
	"claude-opus-4-8":   claudePrice(5.00, 25.00, 0.1),
	"claude-opus-4-7":   claudePrice(5.00, 25.00, 0.1),
	"claude-opus-4-6":   claudePrice(5.00, 25.00, 0.1),
	"claude-sonnet-5":   claudePrice(2.00, 10.00, 0.1),
	"claude-sonnet-4-6": claudePrice(3.00, 15.00, 0.1),
	"claude-haiku-4-5":  claudePrice(1.00, 5.00, 0.1),
	"claude-fable-5-1":  claudePrice(10.00, 50.00, 0.025),
	"claude-fable-5":    claudePrice(10.00, 50.00, 0.025),
	"claude-mythos-5-1": claudePrice(10.00, 50.00, 0.025),
	"claude-mythos-5":   claudePrice(10.00, 50.00, 0.025),
}

var openAIPricing = map[string]tokenPricing{
	"gpt-5.5":       openAIPrice(5.00, 0.50, 30.00),
	"gpt-5":         openAIPrice(1.25, 0.125, 10.00),
	"gpt-5-mini":    openAIPrice(0.25, 0.025, 2.00),
	"gpt-5-nano":    openAIPrice(0.05, 0.005, 0.40),
	"gpt-5.3-codex": openAIPrice(1.75, 0.175, 14.00),
}

type modelUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
}

type costKey struct{ Provider, Model string }
type projectKey struct{ Provider, Project string }

type dashboardCostRow struct {
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// dashboardCostProjectRow is the same cost data grouped by working
// directory instead of model — the closest proxy these CLIs let us measure
// for "cost per task": each one already tags its own transcript lines with
// the exact --cwd a turn ran in (Claude's "cwd" field, Codex's
// turn_context.cwd, opencode's message.path.cwd), no path decoding needed.
type dashboardCostProjectRow struct {
	Provider string  `json:"provider"`
	Project  string  `json:"project"`
	CostUSD  float64 `json:"cost_usd"`
}

type dashboardCosts struct {
	GeneratedAt    time.Time                 `json:"generated_at"`
	Day24hUSD      float64                   `json:"day_24h_usd"`
	Week7dUSD      float64                   `json:"week_7d_usd"`
	Day24hRows     []dashboardCostRow        `json:"day_24h_rows"`
	Week7dRows     []dashboardCostRow        `json:"week_7d_rows"`
	Day24hProjects []dashboardCostProjectRow `json:"day_24h_projects"`
	Week7dProjects []dashboardCostProjectRow `json:"week_7d_projects"`
	UnpricedModels []string                  `json:"unpriced_models,omitempty"`
	NoLocalData    []string                  `json:"no_local_data,omitempty"`
	KnownDirs      []string                  `json:"known_dirs,omitempty"`
}

// costsSnapshot returns the cached cost report, refreshing it from disk at
// most once every 4s. Refreshing only tails the bytes appended to each
// transcript since the last read (see tailClaudeUsage/tailCodexUsage), so a
// short interval keeps token/cost figures close to real time without
// rescanning gigabytes of JSONL history on every SSE tick.
func (e *Engine) costsSnapshot() dashboardCosts {
	e.costsMu.Lock()
	defer e.costsMu.Unlock()
	if e.costsCache != nil && time.Since(e.costsAt) < 4*time.Second {
		return *e.costsCache
	}
	c := e.computeCosts(time.Now())
	e.costsCache = &c
	e.costsAt = time.Now()
	return c
}

// tailFileLines reads the bytes appended to path since offset and returns
// the complete lines found in them, plus the offset to resume from next
// time. A line still being written (no trailing newline yet) is left
// unread and picked up on the next call. If the file is now shorter than
// offset (rotated/truncated), it is re-read from the start.
func tailFileLines(path string, offset int64) (lines []string, newOffset int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	size := info.Size()
	if size < offset {
		offset = 0
	}
	if size == offset {
		return nil, offset, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return nil, offset, err
	}
	lastNL := bytes.LastIndexByte(buf, '\n')
	if lastNL < 0 {
		return nil, offset, nil
	}
	for part := range bytes.SplitSeq(buf[:lastNL], []byte("\n")) {
		if len(part) == 0 {
			continue
		}
		lines = append(lines, string(part))
	}
	return lines, offset + int64(lastNL) + 1, nil
}

type claudeUsageEvent struct {
	Model, Project                string
	Ts                            time.Time
	In, Out, WriteEph5m, WriteEph1h, Read int64
}

// tailClaudeUsage appends newly-written assistant turns from every Claude
// Code transcript to e.claudeEvents, reading only the bytes each file grew
// by since the last call.
func (e *Engine) tailClaudeUsage(home string) {
	root := filepath.Join(home, ".claude", "projects")
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		lines, newOff, err := tailFileLines(path, e.claudeFileOffsets[path])
		if err != nil {
			return nil
		}
		e.claudeFileOffsets[path] = newOff
		for _, line := range lines {
			if !strings.Contains(line, `"assistant"`) {
				continue
			}
			var rec claudeUsageLine
			if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Type != "assistant" {
				continue
			}
			ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
			if err != nil {
				continue
			}
			u := rec.Message.Usage
			w5, w1 := u.CacheCreation.Ephemeral5m, u.CacheCreation.Ephemeral1h
			if w5 == 0 && w1 == 0 && u.CacheCreationInputTokens > 0 {
				w5 = u.CacheCreationInputTokens
			}
			e.claudeEvents = append(e.claudeEvents, claudeUsageEvent{
				Model: rec.Message.Model, Project: rec.Cwd, Ts: ts,
				In: u.InputTokens, Out: u.OutputTokens,
				WriteEph5m: w5, WriteEph1h: w1, Read: u.CacheReadInputTokens,
			})
		}
		return nil
	})
}

type codexUsageEvent struct {
	Model, Project    string
	Ts                time.Time
	In, CachedIn, Out int64
}

// tailCodexUsage appends newly-written token_count events from every Codex
// rollout file to e.codexEvents, reading only the bytes each file grew by
// since the last call. The active model/project for a file is remembered
// across calls (turn_context events, which set it, may be far behind the
// newly appended token_count events in an earlier tail).
func (e *Engine) tailCodexUsage(home string) {
	root := filepath.Join(home, ".codex", "sessions")
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		lines, newOff, err := tailFileLines(path, e.codexFileOffsets[path])
		if err != nil {
			return nil
		}
		e.codexFileOffsets[path] = newOff
		model, project := e.codexFileModel[path], e.codexFileProject[path]
		for _, line := range lines {
			var rec codexRolloutLine
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				continue
			}
			switch {
			case rec.Type == "turn_context":
				if rec.Payload.Model != "" {
					model = rec.Payload.Model
				}
				if rec.Payload.Cwd != "" {
					project = rec.Payload.Cwd
				}
			case rec.Type == "event_msg" && rec.Payload.Type == "token_count":
				ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
				if err != nil {
					continue
				}
				u := rec.Payload.Info.LastTokenUsage
				if u.InputTokens == 0 && u.OutputTokens == 0 {
					continue
				}
				e.codexEvents = append(e.codexEvents, codexUsageEvent{Model: model, Project: project, Ts: ts, In: u.InputTokens, CachedIn: u.CachedInputTokens, Out: u.OutputTokens})
			}
		}
		e.codexFileModel[path] = model
		e.codexFileProject[path] = project
		return nil
	})
}

func (e *Engine) computeCosts(now time.Time) dashboardCosts {
	dayCutoff := now.Add(-24 * time.Hour)
	weekCutoff := now.Add(-7 * 24 * time.Hour)

	day := map[costKey]*modelUsage{}
	week := map[costKey]*modelUsage{}
	dayProj := map[projectKey]float64{}
	weekProj := map[projectKey]float64{}
	knownDirs := map[string]bool{}
	var unpriced []string
	seenUnpriced := map[string]bool{}

	addUsage := func(m map[costKey]*modelUsage, provider, model string, in, out, cacheRead, cacheWrite int64, cost float64) {
		k := costKey{provider, model}
		u := m[k]
		if u == nil {
			u = &modelUsage{}
			m[k] = u
		}
		u.InputTokens += in
		u.OutputTokens += out
		u.CacheReadTokens += cacheRead
		u.CacheWriteTokens += cacheWrite
		u.CostUSD += cost
	}

	// record handles providers priced from a shared table (cost derived
	// here); recordCost handles providers that already carry a computed
	// cost (Claude/opencode). Both feed the same day/week/project maps.
	record := func(provider, model, project string, ts time.Time, in, out, cacheRead, cacheWrite int64, pricing map[string]tokenPricing) {
		if ts.Before(weekCutoff) {
			return
		}
		p, ok := pricing[model]
		var cost float64
		if ok {
			cost = float64(in)*p.Input + float64(out)*p.Output + float64(cacheRead)*p.CacheRead
		} else if model != "" && !seenUnpriced[model] {
			seenUnpriced[model] = true
			unpriced = append(unpriced, model)
		}
		if project != "" {
			knownDirs[project] = true
			weekProj[projectKey{provider, project}] += cost
			if !ts.Before(dayCutoff) {
				dayProj[projectKey{provider, project}] += cost
			}
		}
		addUsage(week, provider, model, in, out, cacheRead, cacheWrite, cost)
		if !ts.Before(dayCutoff) {
			addUsage(day, provider, model, in, out, cacheRead, cacheWrite, cost)
		}
	}
	recordCost := func(provider, model, project string, ts time.Time, in, out, cacheRead, cacheWrite int64, cost float64) {
		if ts.Before(weekCutoff) {
			return
		}
		if project != "" {
			knownDirs[project] = true
			weekProj[projectKey{provider, project}] += cost
			if !ts.Before(dayCutoff) {
				dayProj[projectKey{provider, project}] += cost
			}
		}
		addUsage(week, provider, model, in, out, cacheRead, cacheWrite, cost)
		if !ts.Before(dayCutoff) {
			addUsage(day, provider, model, in, out, cacheRead, cacheWrite, cost)
		}
	}

	home, err := os.UserHomeDir()
	noLocalData := []string{"gemini"}
	if err == nil {
		e.tailClaudeUsage(home)
		e.tailCodexUsage(home)

		// Drop events that fell out of the 7-day window so the in-memory
		// history stays bounded instead of growing forever across the
		// engine's lifetime.
		keptClaude := e.claudeEvents[:0]
		for _, ev := range e.claudeEvents {
			if !ev.Ts.Before(weekCutoff) {
				keptClaude = append(keptClaude, ev)
			}
		}
		e.claudeEvents = keptClaude
		keptCodex := e.codexEvents[:0]
		for _, ev := range e.codexEvents {
			if !ev.Ts.Before(weekCutoff) {
				keptCodex = append(keptCodex, ev)
			}
		}
		e.codexEvents = keptCodex

		for _, ev := range e.claudeEvents {
			p, ok := claudePricing[ev.Model]
			var cost float64
			if ok {
				cost = float64(ev.In)*p.Input + float64(ev.Out)*p.Output + float64(ev.Read)*p.CacheRead +
					float64(ev.WriteEph5m)*p.CacheWrite5m + float64(ev.WriteEph1h)*p.CacheWrite1h
			} else if ev.Model != "" && !seenUnpriced[ev.Model] {
				seenUnpriced[ev.Model] = true
				unpriced = append(unpriced, ev.Model)
			}
			recordCost("claude", ev.Model, ev.Project, ev.Ts, ev.In, ev.Out, ev.Read, ev.WriteEph5m+ev.WriteEph1h, cost)
		}
		for _, ev := range e.codexEvents {
			nonCached := max(ev.In-ev.CachedIn, 0)
			record("codex", ev.Model, ev.Project, ev.Ts, nonCached, ev.Out, ev.CachedIn, 0, openAIPricing)
		}
		scanOpenCodeUsage(home, weekCutoff, func(model, project string, ts time.Time, in, out, cacheRead, cacheWrite int64, cost float64) {
			recordCost("opencode", model, project, ts, in, out, cacheRead, cacheWrite, cost)
		})
	}

	if dirs, configured, err := loadAllowlist(); err == nil && configured {
		for _, d := range dirs {
			knownDirs[d] = true
		}
	}

	sort.Strings(unpriced)

	toRows := func(m map[costKey]*modelUsage) ([]dashboardCostRow, float64) {
		rows := make([]dashboardCostRow, 0, len(m))
		var total float64
		for k, u := range m {
			rows = append(rows, dashboardCostRow{
				Provider: k.Provider, Model: k.Model,
				InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
				CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
				CostUSD: u.CostUSD,
			})
			total += u.CostUSD
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].CostUSD > rows[j].CostUSD })
		return rows, total
	}
	toProjectRows := func(m map[projectKey]float64) []dashboardCostProjectRow {
		rows := make([]dashboardCostProjectRow, 0, len(m))
		for k, cost := range m {
			rows = append(rows, dashboardCostProjectRow{Provider: k.Provider, Project: k.Project, CostUSD: cost})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].CostUSD > rows[j].CostUSD })
		return rows
	}

	dayRows, dayTotal := toRows(day)
	weekRows, weekTotal := toRows(week)

	dirs := make([]string, 0, len(knownDirs))
	for d := range knownDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	return dashboardCosts{
		GeneratedAt:    now,
		Day24hUSD:      dayTotal,
		Week7dUSD:      weekTotal,
		Day24hRows:     dayRows,
		Week7dRows:     weekRows,
		Day24hProjects: toProjectRows(dayProj),
		Week7dProjects: toProjectRows(weekProj),
		UnpricedModels: unpriced,
		NoLocalData:    noLocalData,
		KnownDirs:      dirs,
	}
}

type claudeUsageLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
	Message   struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheCreation            struct {
				Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
				Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
}

// codexRolloutLine covers two different envelope shapes used in the same
// file: "turn_context" is a top-level record with the model directly under
// "payload"; "token_count" instead arrives wrapped in a top-level
// "event_msg" record, with its own kind and usage nested one level deeper
// under payload.type/payload.info.
type codexRolloutLine struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type  string `json:"type"`
		Model string `json:"model"`
		Cwd   string `json:"cwd"`
		Info  struct {
			LastTokenUsage struct {
				InputTokens       int64 `json:"input_tokens"`
				CachedInputTokens int64 `json:"cached_input_tokens"`
				OutputTokens      int64 `json:"output_tokens"`
			} `json:"last_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}


type opencodeMessageData struct {
	Role string  `json:"role"`
	Cost float64 `json:"cost"`
	Path struct {
		Cwd string `json:"cwd"`
	} `json:"path"`
	Tokens struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Time       struct {
		Completed int64 `json:"completed"`
	} `json:"time"`
}

// scanOpenCodeUsage reads opencode's own sqlite store via the sqlite3 CLI
// (best-effort: silently returns nothing if the db or the CLI is missing)
// and trusts the "cost" field opencode already computed per message, rather
// than re-deriving it from a separate pricing table.
func scanOpenCodeUsage(home string, since time.Time, emit func(model, project string, ts time.Time, in, out, cacheRead, cacheWrite int64, costUSD float64)) {
	db := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	if _, err := os.Stat(db); err != nil {
		return
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return
	}
	sinceMs := since.UnixMilli()
	query := "SELECT data FROM message WHERE time_updated >= " + strconv.FormatInt(sinceMs, 10) + " AND data LIKE '%\"cost\"%';"
	cmd := exec.Command("sqlite3", "-json", db, query)
	out, err := cmd.Output()
	if err != nil {
		return
	}
	var rows []struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return
	}
	for _, row := range rows {
		var d opencodeMessageData
		if err := json.Unmarshal([]byte(row.Data), &d); err != nil || d.Role != "assistant" {
			continue
		}
		ts := time.UnixMilli(d.Time.Completed)
		provider := d.ProviderID
		if provider == "" {
			provider = "opencode"
		}
		model := d.ModelID
		if provider != "" {
			model = provider + "/" + model
		}
		emit(model, d.Path.Cwd, ts, d.Tokens.Input, d.Tokens.Output+d.Tokens.Reasoning, d.Tokens.Cache.Read, d.Tokens.Cache.Write, d.Cost)
	}
}
