# Handoff Notes (IllumioMonitoringDashboard)

## Active Fork
- `/home/ted/Codex/IllumioMonitoringDashboard`

## Scope in This Fork
- All prior rolling/UI-hardening/persistent-state capabilities retained.
- Added webhook notification system for blocked-target anomaly transitions.
- Moved all configuration UI off dashboard into dedicated `/settings` workspace.
- Added network-hosting controls:
  - `bind_address` for server listen address
  - `public_base_url` for generated links (webhooks/drilldowns)
- Added timezone-aware daily buckets/trends:
  - new `timezone` config (IANA, optional)
  - default daily bucketing uses server local timezone when unset
- Added VEN reason enrichment:
  - warning/error drilldown entries now include parsed reason from VEN `conditions`
  - parser uses `conditions[].latest_event.notification_type` (with fallbacks)
- Added separate anomaly settings for non-blocked metrics:
  - `ven_ma_window`, `ven_anomaly_pct`
  - `tampering_ma_window`, `tampering_anomaly_pct`
  - drilldown/report MA overlays and anomaly detection use these independent settings
- Added blocked anomaly baseline modes:
  - `blocked_anomaly_baseline`: `5m` or `daily`
  - `blocked_anomaly_days`: N-day baseline window (for `daily` mode)
  - `blocked_anomaly_min_coverage_pct`: warmup coverage gate for alert suppression
  - Daily mode compares latest 5m value vs N-day average while preserving the 24h(5m) chart
- Added MA/anomaly overlays for:
  - VEN warning charts
  - VEN error charts
  - Tampering charts
- Fixed duplicate blocked anomaly webhook notifications:
  - per-cycle target coalescing before alert evaluation
  - resolved payload now includes threshold/reason fields (no `<nil>` strings)
- Drilldown charts now stay visible at zero values:
  - VEN warning/error, enforcement, tampering, blocked target
- Extended enforcement trends:
  - enforcement drilldowns now support `24h (5m)` plus `Daily` trend toggle
  - daily range selectors expanded to `24h/7d/30d/90d/180d/365d` in drilldown/report views

## Webhook Behavior
- Config keys in `config.json`:
  - `webhook_enabled` (bool)
  - `webhook_url` (string)
  - `webhook_provider` (`generic` | `slack` | `teams`)
  - `webhook_slack_channel`, `webhook_slack_username`, `webhook_slack_icon_emoji`
  - `webhook_teams_title_prefix`
- Event types:
  - `blocked_target_anomaly` with `state=triggered`
  - `blocked_target_anomaly` with `state=resolved`
- Payload includes target, latest 5m, moving average, threshold/window metadata, and drilldown URL.
- Test endpoint:
  - `POST /api/webhook/test`
- UI support:
  - dedicated `/settings` page
  - collapsible settings sections:
    - Traffic And Data Settings
    - Alerting Settings
  - webhook enable toggle/provider/url
  - Slack/Teams provider-specific optional fields
  - test webhook button

## Latest UI Layout
- Dashboard (`/`):
  - no config editor panel
  - blocked traffic panel is full-width
  - `Settings` button links to `/settings`
- Settings (`/settings`):
  - traffic targets/exclusions/history/anomaly defaults
  - separate VEN/tampering anomaly settings
  - timezone setting (`timezone`)
  - network hosting fields (`bind_address`, `public_base_url`)
  - refresh-now action
  - webhook provider fields and test action

## Persisted State
- Shared runtime data directory:
  - default: `$HOME/.illumio-monitoring-dashboard`
  - override: `ILLUMIO_DASH_DATA_DIR`
  - optional config override: `data_dir` in `config.json`
- Files:
  - `rolling_state.json`
  - `blocked_daily_history.json`
  - `ven_daily_history.json`
  - `alert_state.json` (webhook transition state)

## Endpoints
- `/`, `/details`, `/report`, `/trends`
- `/settings`
- `/api/stats`
- `/api/drilldown`
- `/api/export/report.csv`
- `/api/export/drilldown.csv`
- `/api/config/targets`
- `/api/config/alerts`
- `/api/refresh`
- `/api/webhook/test`

## Build/Validation
- `go test ./...`
- `./scripts/rebuild-binaries.sh`

## Resume Checklist (Post-Compact)
1. Use fork: `/home/ted/Codex/IllumioMonitoringDashboard`
2. Run current binary from that folder.
3. If needed, verify settings page has:
  - blocked MA/anomaly
  - VEN MA/anomaly
  - tampering MA/anomaly
  - timezone, bind address, public base URL
4. Trigger **Refresh Now** from `/settings` and validate `/details` charts render even at zero.

## Fallback Forks
- `/home/ted/Codex/IllumioDashboard-rolling-persiststate-v1`
- `/home/ted/Codex/IllumioDashboard-rolling-uihardening-v2`
