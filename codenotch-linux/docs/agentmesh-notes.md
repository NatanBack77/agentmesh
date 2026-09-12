# AgentMesh Patterns To Reuse

Observed in the local AgentMesh repository:

- Dashboard state is one backend snapshot, not scattered UI reads.
- Live updates use event streams (`/dashboard/events`) and periodic snapshots.
- Theme is applied early on `<html data-theme>` with a persisted localStorage
  preference and system fallback.
- Claude quota is cached on disk for 120 seconds to avoid multiplying endpoint
  calls across active sessions.
- Codex quota prefers local rollout files because Codex already writes exact
  rate-limit snapshots after turns.
- Status is inferred from terminal/screen/transcript behavior; provider CLIs do
  not expose a stable official "working" API.

Implications for this Linux MVP:

- Keep a single `state::Snapshot` backend contract.
- Keep provider polling decoupled from UI hover/repaint.
- Use stale cached readings honestly instead of inventing fresh values.
- Let `app/ui/*` own presentation only; provider parsing belongs in Rust.
- Preserve the theme-toggle shape already prepared in
  the sibling UI-base workspace: `data-theme`, `data-theme-switching`,
  `prefers-color-scheme`, and `localStorage["meshnotch.theme"]`.
