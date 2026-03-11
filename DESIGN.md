# Design Document: Illumio Monitoring Dashboard

## 1. Objective
Provide a single-binary, cross-platform Illumio monitoring tool with a local web UI that continuously surfaces operational and security posture metrics with drilldowns and trend visualizations.

## 2. Scope and Key Metrics
- VEN health counts and names:
  - Warning (`/vens?health=warning`)
  - Error (`/vens?health=error`)
- Workload inventory:
  - Total workloads
  - Managed enforcement modes (`idle`, `visibility_only`, `selective`, `full`)
  - Unmanaged workloads
- Tampering visibility:
  - Deduped VEN/workload names for `agent.tampering` events
- Blocked traffic analytics:
  - Configurable targets (labels and/or label groups)
  - Configurable source exclusions
  - Configurable moving average window + anomaly threshold for blocked 5m trends
  - Source+destination blocked query aggregation
  - Rolling 24h behavior with warmup handling
- VEN/tampering analytics:
  - Configurable moving average window + anomaly threshold for VEN warning/error trends
  - Configurable moving average window + anomaly threshold for tampering trends
- Optional webhook alerting:
  - Triggered/resolved notifications for blocked-target anomalies
  - Provider-specific formatting (`generic`, `slack`, `teams`)
- Executive workflow:
  - `/executive` with outcomes vs risk signals
  - period selector (`7d/30d/90d`)
  - business-value ranking
  - narrative summary generator and board-ready print mode

## 3. Architecture
### Backend (Go)
- Single executable with embedded UI assets via `//go:embed`.
- `net/http` server serves:
  - dashboard pages
  - drilldown/report/trend pages
  - JSON APIs and CSV export endpoints
- Background collector goroutine runs every 5 minutes and refreshes in-memory state.
- Persistent storage:
  - `config.json` (local app directory)
  - Shared runtime data directory (default `$HOME/.illumio-monitoring-dashboard`, override via `ILLUMIO_DASH_DATA_DIR` or `config.data_dir`)
  - `rolling_state.json` (schema-versioned rolling bucket state)
  - `blocked_daily_history.json`
  - `ven_daily_history.json`
  - `alert_state.json` (anomaly alert transition state)
  - `anomaly_history.jsonl` (persisted anomaly transitions for blocked targets, VEN warnings/errors, tampering)
- Host/runtime network settings via `config.json`:
  - `timezone` (IANA timezone for daily buckets/trends; default server local timezone)
  - `bind_address` (listener address, default `:18443`)
  - `public_base_url` (absolute base URL used in generated links/webhook payloads, default `http://localhost:18443`)
  - Deployment modes:
    - direct listener mode for convenience/internal use
    - secure mode via reverse proxy with TLS/auth where app binds localhost-only (`127.0.0.1:18443`) and proxy is the only exposed endpoint

### Frontend (HTML/JS)
- Bootstrap + Chart.js.
- Shared UI utility layer in embedded `/static/ui-common.js` for:
  - theme toggling
  - staleness/next-refresh text
  - metric tooltip helpers

## 4. Data Collection Model
- First cycle/baseline:
  - 24h blocked traffic baseline per target.
- Subsequent cycles:
  - last 5 minutes for rolling updates.
- Rolling cache stores per-cycle buckets for:
  - VEN warning/error counts
  - enforcement mode counts
  - tampering dedup inputs
  - blocked counts per target
- Rolling cache survives restarts via persisted `rolling_state.json`.
- Startup migration copies legacy local state files into shared data dir when destination is missing.
- Daily retention:
  - `history_days` controls keep window for daily trend snapshots.

## 5. UX Model
- Dashboard (`/`):
  - high-level cards + blocked summary tiles + main blocked chart
- Settings (`/settings`):
  - Traffic And Data Settings (targets/exclusions/retention/anomaly defaults)
  - Separate anomaly settings for:
    - Blocked traffic
    - VEN warning/error
    - Tampering
  - Hosting Settings (bind/public URL)
  - Alerting Settings (webhook provider/config/test)
- Drilldown (`/details`):
  - names list + trend chart for selected metric
  - CSV/PDF export actions
- Report (`/report`) and Trend View (`/trends`):
  - shared page layout
  - summary cards + trend charts
  - blocked charts grouped in collapsible sections by target
  - enforcement mode charts grouped in collapsible section
  - anomaly outcomes grouped in collapsible section
  - day-range controls for daily trends (`24h`, `7d`, `30d`, `90d`, `180d`, `365d`)
  - next-refresh/staleness indicators
- Dashboard (`/`):
  - pipeline status row includes SLO confidence badge

## 6. API Surface (UI-facing)
- `GET /api/stats`
- `GET /api/drilldown?metric=<metric>[&target=<target>]`
- `GET /api/export/drilldown.csv?metric=<metric>[&target=<target>]`
- `GET /api/export/report.csv`
- `GET /api/config/targets`
- `PUT /api/config/targets`
- `GET /api/config/alerts`
- `PUT /api/config/alerts`
- `POST /api/refresh`
- `GET /api/debug/ven-status`
- `POST /api/webhook/test`
- `GET /api/anomalies/history?days=<n>&limit=<n>`

## 7. Reliability/Scale Considerations
- Large-PCE pagination support for workloads, VENs, labels, and label groups.
- Label/label-group lookup diagnostics include scanned object counts.
- Async blocked traffic querying with timeout/retry/fallback result extraction.
- Per-target blocked query pacing/staggering to reduce request bursts every 5-minute cycle.
- Query-result reuse path for blocked count + blocked ports in daily/history aggregation flows.
- Partial-success status surfaced in UI rather than full pipeline failure.

## 8. Current Status
- Implemented and functional in active repo:
  - `/home/ted/Codex/IllumioMonitoringDashboard`
- Includes UI hardening, network hosting controls, persisted rolling/daily history, anomaly history, webhook notifications, executive reporting, and trend/report anomaly outcomes.
