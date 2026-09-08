package mesh

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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

type dashboardCostRow struct {
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

type dashboardCosts struct {
	GeneratedAt    time.Time          `json:"generated_at"`
	Day24hUSD      float64            `json:"day_24h_usd"`
	Week7dUSD      float64            `json:"week_7d_usd"`
	Day24hRows     []dashboardCostRow `json:"day_24h_rows"`
	Week7dRows     []dashboardCostRow `json:"week_7d_rows"`
	UnpricedModels []string           `json:"unpriced_models,omitempty"`
	NoLocalData    []string           `json:"no_local_data,omitempty"`
}

// costsSnapshot returns the cached cost report, recomputing it from disk at
// most once every 20s — scanning every local transcript on every 300ms SSE
// tick would turn the dashboard into a filesystem stress test.
func (e *Engine) costsSnapshot() dashboardCosts {
	e.costsMu.Lock()
	defer e.costsMu.Unlock()
	if e.costsCache != nil && time.Since(e.costsAt) < 20*time.Second {
		return *e.costsCache
	}
	c := computeCosts(time.Now())
	e.costsCache = &c
	e.costsAt = time.Now()
	return c
}

func computeCosts(now time.Time) dashboardCosts {
	dayCutoff := now.Add(-24 * time.Hour)
	weekCutoff := now.Add(-7 * 24 * time.Hour)

	day := map[costKey]*modelUsage{}
	week := map[costKey]*modelUsage{}
	var unpriced []string
	seenUnpriced := map[string]bool{}

	record := func(provider, model string, ts time.Time, in, out, cacheRead, cacheWrite int64, pricing map[string]tokenPricing) {
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
		add := func(m map[costKey]*modelUsage) {
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
		add(week)
		if !ts.Before(dayCutoff) {
			add(day)
		}
	}
	recordCost := func(provider, model string, ts time.Time, in, out, cacheRead, cacheWrite int64, cost float64) {
		if ts.Before(weekCutoff) {
			return
		}
		add := func(m map[costKey]*modelUsage) {
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
		add(week)
		if !ts.Before(dayCutoff) {
			add(day)
		}
	}

	home, err := os.UserHomeDir()
	noLocalData := []string{"gemini"}
	if err == nil {
		scanClaudeUsage(home, weekCutoff, func(model string, ts time.Time, in, out, cacheRead5m, cacheRead1h, cacheReadTok int64) {
			p, ok := claudePricing[model]
			var cost float64
			if ok {
				cost = float64(in)*p.Input + float64(out)*p.Output + float64(cacheReadTok)*p.CacheRead +
					float64(cacheRead5m)*p.CacheWrite5m + float64(cacheRead1h)*p.CacheWrite1h
			} else if model != "" && !seenUnpriced[model] {
				seenUnpriced[model] = true
				unpriced = append(unpriced, model)
			}
			recordCost("claude", model, ts, in, out, cacheReadTok, cacheRead5m+cacheRead1h, cost)
		})
		scanCodexUsage(home, weekCutoff, func(model string, ts time.Time, in, cachedIn, out int64) {
			nonCached := max(in-cachedIn, 0)
			record("codex", model, ts, nonCached, out, cachedIn, 0, openAIPricing)
		})
		scanOpenCodeUsage(home, weekCutoff, func(model string, ts time.Time, in, out, cacheRead, cacheWrite int64, cost float64) {
			recordCost("opencode", model, ts, in, out, cacheRead, cacheWrite, cost)
		})
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

	dayRows, dayTotal := toRows(day)
	weekRows, weekTotal := toRows(week)

	return dashboardCosts{
		GeneratedAt:    now,
		Day24hUSD:      dayTotal,
		Week7dUSD:      weekTotal,
		Day24hRows:     dayRows,
		Week7dRows:     weekRows,
		UnpricedModels: unpriced,
		NoLocalData:    noLocalData,
	}
}

type claudeUsageLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
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

// scanClaudeUsage walks every Claude Code project transcript and reports
// each assistant turn's token usage split into 5m-cache-write,
// 1h-cache-write, and cache-read buckets (Anthropic prices each
// differently).
func scanClaudeUsage(home string, since time.Time, emit func(model string, ts time.Time, in, out, cacheWrite5m, cacheWrite1h, cacheRead int64)) {
	root := filepath.Join(home, ".claude", "projects")
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(since) {
			return nil // file untouched since the cutoff can't hold newer lines
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 || !bytes.Contains(line, []byte(`"assistant"`)) {
				continue
			}
			var rec claudeUsageLine
			if err := json.Unmarshal(line, &rec); err != nil || rec.Type != "assistant" {
				continue
			}
			ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
			if err != nil || ts.Before(since) {
				continue
			}
			u := rec.Message.Usage
			w5, w1 := u.CacheCreation.Ephemeral5m, u.CacheCreation.Ephemeral1h
			if w5 == 0 && w1 == 0 && u.CacheCreationInputTokens > 0 {
				w5 = u.CacheCreationInputTokens // older transcripts omit the split; default TTL is 5m
			}
			emit(rec.Message.Model, ts, u.InputTokens, u.OutputTokens, w5, w1, u.CacheReadInputTokens)
		}
		return nil
	})
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
		Info  struct {
			LastTokenUsage struct {
				InputTokens       int64 `json:"input_tokens"`
				CachedInputTokens int64 `json:"cached_input_tokens"`
				OutputTokens      int64 `json:"output_tokens"`
			} `json:"last_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

// scanCodexUsage walks every Codex rollout file, tracking the active model
// via turn_context events and summing the per-turn usage deltas reported by
// token_count events.
func scanCodexUsage(home string, since time.Time, emit func(model string, ts time.Time, in, cachedIn, out int64)) {
	root := filepath.Join(home, ".codex", "sessions")
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(since) {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		model := ""
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var rec codexRolloutLine
			if err := json.Unmarshal(line, &rec); err != nil {
				continue
			}
			switch {
			case rec.Type == "turn_context":
				if rec.Payload.Model != "" {
					model = rec.Payload.Model
				}
			case rec.Type == "event_msg" && rec.Payload.Type == "token_count":
				ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
				if err != nil || ts.Before(since) {
					continue
				}
				u := rec.Payload.Info.LastTokenUsage
				if u.InputTokens == 0 && u.OutputTokens == 0 {
					continue
				}
				emit(model, ts, u.InputTokens, u.CachedInputTokens, u.OutputTokens)
			}
		}
		return nil
	})
}

type opencodeMessageData struct {
	Role   string `json:"role"`
	Cost   float64 `json:"cost"`
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
func scanOpenCodeUsage(home string, since time.Time, emit func(model string, ts time.Time, in, out, cacheRead, cacheWrite int64, costUSD float64)) {
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
		emit(model, ts, d.Tokens.Input, d.Tokens.Output+d.Tokens.Reasoning, d.Tokens.Cache.Read, d.Tokens.Cache.Write, d.Cost)
	}
}
