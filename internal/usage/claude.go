// Package usage reads Claude Code's own session transcripts to report
// real token usage and estimated cost — no separate tracking needed, the
// CLI already writes everything to ~/.claude/projects/**/*.jsonl.
//
// Scope today: Claude Code only. Codex/Gemini/OpenCode/shells have no
// equivalent structured, per-turn usage log this tool can read the same
// way — adding them means teaching this package their own log format,
// which is a separate, provider-specific piece of work (see Openfield's
// internal/orchestrator/usage.go for the shape that would take).
package usage

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Totals accumulates token counts and estimated cost for one bucket
// (a day, a model, or the grand total).
type Totals struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
}

func (t *Totals) add(o Totals) {
	t.InputTokens += o.InputTokens
	t.OutputTokens += o.OutputTokens
	t.CacheReadTokens += o.CacheReadTokens
	t.CacheWriteTokens += o.CacheWriteTokens
	t.CostUSD += o.CostUSD
}

// DayTotal is one day's aggregate, for the weekly breakdown.
type DayTotal struct {
	Date   string // YYYY-MM-DD, local time
	Totals Totals
}

// Report is the result of scanning transcripts.
type Report struct {
	Today Totals
	Week  Totals // rolling 7 days including today
	Days  []DayTotal
	// ByModel breaks the week's totals down per model id — useful since
	// Opus/Sonnet/Haiku have very different per-token prices.
	ByModel map[string]Totals
}

// transcriptLine is the subset of a Claude Code JSONL line this package
// cares about. Unknown/extra fields are ignored by design — decoding into
// a narrow struct means a transcript schema change elsewhere in the file
// never breaks this reader.
type transcriptLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// Scan walks every *.jsonl transcript under ~/.claude/projects (including
// the subagents/ subdirectory each session can have) and builds a Report
// covering the last `days` calendar days (local time), today included.
func Scan(days int) (Report, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Report{}, err
	}
	root := filepath.Join(home, ".claude", "projects")

	if days < 1 {
		days = 7
	}
	now := time.Now()
	todayKey := now.Format("2006-01-02")
	cutoff := now.AddDate(0, 0, -(days - 1))
	cutoff = time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, cutoff.Location())

	byDay := map[string]*Totals{}
	byModel := map[string]*Totals{}

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort: a permission hiccup on one dir shouldn't abort the whole scan
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		scanFile(path, cutoff, byDay, byModel)
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		return Report{}, walkErr
	}

	var rep Report
	rep.ByModel = make(map[string]Totals, len(byModel))
	for model, t := range byModel {
		rep.ByModel[model] = *t
		rep.Week.add(*t)
	}
	if t, ok := byDay[todayKey]; ok {
		rep.Today = *t
	}

	rep.Days = make([]DayTotal, 0, len(byDay))
	for date, t := range byDay {
		if date < cutoff.Format("2006-01-02") {
			continue
		}
		rep.Days = append(rep.Days, DayTotal{Date: date, Totals: *t})
	}
	sort.Slice(rep.Days, func(i, j int) bool { return rep.Days[i].Date < rep.Days[j].Date })

	return rep, nil
}

func scanFile(path string, cutoff time.Time, byDay map[string]*Totals, byModel map[string]*Totals) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // transcript lines can be large (big tool outputs)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var t transcriptLine
		if err := json.Unmarshal(line, &t); err != nil {
			continue
		}
		if t.Type != "assistant" || t.Message.Usage.InputTokens == 0 && t.Message.Usage.OutputTokens == 0 {
			continue
		}
		ts, err := time.Parse(time.RFC3339, t.Timestamp)
		if err != nil {
			continue
		}
		if ts.Before(cutoff) {
			continue
		}

		u := Totals{
			InputTokens:      t.Message.Usage.InputTokens,
			OutputTokens:     t.Message.Usage.OutputTokens,
			CacheReadTokens:  t.Message.Usage.CacheReadInputTokens,
			CacheWriteTokens: t.Message.Usage.CacheCreationInputTokens,
		}
		u.CostUSD = cost(t.Message.Model, u)

		day := ts.Local().Format("2006-01-02")
		if byDay[day] == nil {
			byDay[day] = &Totals{}
		}
		byDay[day].add(u)

		model := t.Message.Model
		if model == "" {
			model = "desconhecido"
		}
		if byModel[model] == nil {
			byModel[model] = &Totals{}
		}
		byModel[model].add(u)
	}
}

// rate holds per-million-token prices in USD. Directional, not
// billing-grade — Anthropic's published list prices change over time and
// this is a small hardcoded table, same trade-off Openfield's own usage
// tracking documents.
type rate struct{ in, out, cacheWrite, cacheRead float64 }

var rateTable = []struct {
	match string
	rate  rate
}{
	{"opus", rate{in: 15, out: 75, cacheWrite: 18.75, cacheRead: 1.5}},
	{"haiku", rate{in: 0.8, out: 4, cacheWrite: 1, cacheRead: 0.08}},
	{"sonnet", rate{in: 3, out: 15, cacheWrite: 3.75, cacheRead: 0.3}},
}

func rateFor(model string) rate {
	m := strings.ToLower(model)
	for _, r := range rateTable {
		if strings.Contains(m, r.match) {
			return r.rate
		}
	}
	return rateTable[2].rate // default: Sonnet-class pricing
}

func cost(model string, u Totals) float64 {
	r := rateFor(model)
	const perMillion = 1_000_000
	return float64(u.InputTokens)*r.in/perMillion +
		float64(u.OutputTokens)*r.out/perMillion +
		float64(u.CacheWriteTokens)*r.cacheWrite/perMillion +
		float64(u.CacheReadTokens)*r.cacheRead/perMillion
}
