package mesh

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/NatanBack77/agentmesh/internal/usage"
)

// Plan-quota bars ("how much of my subscription's rate-limit window is
// used") are a different question from the per-model $ costs in costs.go —
// answered here per provider, from whichever source that provider's own
// CLI treats as ground truth:
//
//   - Claude Code: Anthropic's account API (usage.CachedQuota — the SAME
//     endpoint and OAuth token claude.ai's own Configurações → Uso page
//     uses). Requires an OAuth login (Pro/Max); a bare API key has no
//     subscription quota to report.
//   - Codex: no network call needed at all — every rollout file already
//     has the exact primary/secondary rate-limit snapshot Codex's own
//     `/status` command reads, written locally after every turn. The most
//     recently modified rollout file's last snapshot is the account's
//     current status.
//
// This is account-wide, not per spawned agent: every "claude" agent in the
// mesh shares one Anthropic login, every "codex" agent shares one OpenAI
// login, so one bar per provider is the correct granularity — a per-agent
// bar would imply a quota split that doesn't exist.
type dashboardProviderQuota struct {
	Provider        string    `json:"provider"`
	Available       bool      `json:"available"`
	Error           string    `json:"error,omitempty"`
	PlanType        string    `json:"plan_type,omitempty"`
	SessionPct      float64   `json:"session_pct"`
	SessionResetsAt time.Time `json:"session_resets_at"`
	WeekPct         float64   `json:"week_pct"`
	WeekResetsAt    time.Time `json:"week_resets_at"`
}

type dashboardQuotas struct {
	GeneratedAt time.Time                `json:"generated_at"`
	Providers   []dashboardProviderQuota `json:"providers"`
}

// quotaSnapshot returns the cached quota report, recomputing at most once
// every 30s. Claude's own fetch already sits behind usage.CachedQuota's
// 120s shared file cache, so this cache mainly protects the Codex
// filesystem walk from running on every 300ms SSE tick.
func (e *Engine) quotaSnapshot() dashboardQuotas {
	e.quotaMu.Lock()
	defer e.quotaMu.Unlock()
	if e.quotaCache != nil && time.Since(e.quotaAt) < 30*time.Second {
		return *e.quotaCache
	}
	q := dashboardQuotas{
		GeneratedAt: time.Now(),
		Providers:   []dashboardProviderQuota{claudeQuota(), codexQuota()},
	}
	e.quotaCache = &q
	e.quotaAt = time.Now()
	return q
}

func claudeQuota() dashboardProviderQuota {
	q, err := usage.CachedQuota()
	if err != nil {
		return dashboardProviderQuota{Provider: "claude", Available: false, Error: err.Error()}
	}
	return dashboardProviderQuota{
		Provider: "claude", Available: true,
		SessionPct: q.SessionPct, SessionResetsAt: q.SessionResetsAt,
		WeekPct: q.WeekPct, WeekResetsAt: q.WeekResetsAt,
	}
}

type codexRateLimitLine struct {
	Type    string `json:"type"`
	Payload struct {
		Type       string `json:"type"`
		RateLimits struct {
			Primary struct {
				UsedPercent float64 `json:"used_percent"`
				ResetsAt    int64   `json:"resets_at"`
			} `json:"primary"`
			Secondary struct {
				UsedPercent float64 `json:"used_percent"`
				ResetsAt    int64   `json:"resets_at"`
			} `json:"secondary"`
			PlanType string `json:"plan_type"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

// codexQuota finds the most recently modified Codex rollout file (rate
// limits are account-wide, so whichever session touched them last has the
// current picture) and reads the last rate_limits snapshot it logged.
func codexQuota() dashboardProviderQuota {
	home, err := os.UserHomeDir()
	if err != nil {
		return dashboardProviderQuota{Provider: "codex", Available: false, Error: err.Error()}
	}
	root := filepath.Join(home, ".codex", "sessions")

	var latestPath string
	var latestMod time.Time
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(latestMod) {
			latestMod = info.ModTime()
			latestPath = path
		}
		return nil
	})
	if latestPath == "" {
		return dashboardProviderQuota{Provider: "codex", Available: false, Error: "nenhum registro local de sessão do codex encontrado"}
	}

	f, err := os.Open(latestPath)
	if err != nil {
		return dashboardProviderQuota{Provider: "codex", Available: false, Error: err.Error()}
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var last *codexRateLimitLine
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec codexRateLimitLine
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Type == "event_msg" && rec.Payload.Type == "token_count" && rec.Payload.RateLimits.Primary.ResetsAt > 0 {
			cp := rec
			last = &cp
		}
	}
	if last == nil {
		return dashboardProviderQuota{Provider: "codex", Available: false, Error: "sessão mais recente ainda não reportou rate limits"}
	}
	return dashboardProviderQuota{
		Provider: "codex", Available: true, PlanType: last.Payload.RateLimits.PlanType,
		SessionPct:      last.Payload.RateLimits.Primary.UsedPercent,
		SessionResetsAt: time.Unix(last.Payload.RateLimits.Primary.ResetsAt, 0),
		WeekPct:         last.Payload.RateLimits.Secondary.UsedPercent,
		WeekResetsAt:    time.Unix(last.Payload.RateLimits.Secondary.ResetsAt, 0),
	}
}
