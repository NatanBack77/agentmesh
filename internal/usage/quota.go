package usage

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Quota is Anthropic's own view of how much of your plan's rate-limit
// windows you've used — the SAME numbers claude.ai's Configurações → Uso
// page shows (session %, weekly %, reset times). This is a completely
// different question from Scan's token-cost estimate: Scan answers "what
// would this have cost via the API," read entirely from local transcripts;
// Quota answers "how much of my subscription's quota is left," which only
// Anthropic's own account API knows. Only available after an OAuth login
// (Pro/Max subscription) — API-key auth has no such quota to report.
type Quota struct {
	SessionPct      float64
	SessionResetsAt time.Time
	WeekPct         float64
	WeekResetsAt    time.Time
}

type oauthUsageResponse struct {
	FiveHour struct {
		Utilization float64   `json:"utilization"`
		ResetsAt    time.Time `json:"resets_at"`
	} `json:"five_hour"`
	SevenDay struct {
		Utilization float64   `json:"utilization"`
		ResetsAt    time.Time `json:"resets_at"`
	} `json:"seven_day"`
}

// FetchQuota calls the same endpoint claude.ai's own account page calls,
// authenticated with the OAuth token Claude Code already stored locally
// after `claude auth login` on a Pro/Max subscription — no separate login
// or credential of agentmesh's own needed. Best-effort by design: no
// credentials file, API-key auth (no claudeAiOauth block), being offline,
// or the endpoint's own aggressive rate limiting should all just be an
// error the caller falls back from, never a crash.
func FetchQuota() (Quota, error) {
	token, err := oauthAccessToken()
	if err != nil {
		return Quota{}, err
	}

	req, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		return Quota{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "agentmesh")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Quota{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Quota{}, errors.New("api/oauth/usage: status " + resp.Status)
	}

	var body oauthUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Quota{}, err
	}
	return Quota{
		SessionPct:      body.FiveHour.Utilization,
		SessionResetsAt: body.FiveHour.ResetsAt,
		WeekPct:         body.SevenDay.Utilization,
		WeekResetsAt:    body.SevenDay.ResetsAt,
	}, nil
}

// oauthAccessToken reads the same credentials file the `claude` CLI itself
// writes and reads (~/.claude/.credentials.json) — agentmesh doesn't do
// its own login flow, it borrows the token Claude Code already has.
func oauthAccessToken() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return "", err
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(b, &creds); err != nil {
		return "", err
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		return "", errors.New("sem login OAuth salvo (claude.ai) — provavelmente autenticado via API key, que não tem quota de assinatura")
	}
	return creds.ClaudeAiOauth.AccessToken, nil
}
