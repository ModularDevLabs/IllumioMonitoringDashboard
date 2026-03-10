# Illumio PCE Health Dashboard (Go) - Rolling Window Experiment

A standalone Go dashboard for Illumio PCE health and security metrics.

This fork is an experiment for rolling-window collection behavior:

- First successful cycle: pulls a 24-hour baseline.
- Subsequent cycles (every 5 minutes): pull only the last 5 minutes and keep a rolling view.

It serves a web UI on port `18443` by default, with configurable bind/public URL settings for network hosting.

## Features

- VEN health visibility:
  - Warning-state workloads
  - Error-state workloads
  - VEN health is collected from paginated `/vens?health=warning` and `/vens?health=error` API calls
  - No fixed page cap; pagination continues until no more rows, with a per-query timeout safeguard
  - VEN warning/error trend charts now include moving-average overlays and anomaly detection (using configured MA window and anomaly threshold)
- Workload inventory:
  - Total workloads
  - Enforcement mode breakdown for managed workloads (`idle`, `visibility_only`, `selective`, `full`)
  - Unmanaged workload count
- Tampering monitoring:
  - Unique tampered VEN/workload names with `agent.tampering` events (last 24h)
  - Deduped names for drilldown
  - Tampering trend charts include moving-average overlays and anomaly detection (24h/5m series)
- Blocked traffic analytics:
  - Configurable targets (labels and/or label groups)
  - Configurable source exclusions (default `LG-SCANNERS`)
  - Async traffic flow query support
  - Counts combine blocked source + blocked destination queries per target
  - Uses `max_results: 200000` for blocked traffic queries
  - Partial-success handling (green + warning badge)
  - Configurable moving-average/anomaly detection for blocked 5m trends:
    - `blocked_ma_window` (default `12`)
    - `blocked_anomaly_pct` (default `50`)
    - `blocked_anomaly_baseline`: `5m` or `daily`
    - `blocked_anomaly_days` (daily baseline window, default `7`)
    - `blocked_anomaly_min_coverage_pct` (warmup suppression threshold, default `70`)
  - Dashboard target tiles flag anomalous targets when latest 5m value exceeds moving average threshold
- Webhook notifications:
  - Optional webhook for anomaly trigger/resolved events with provider formats:
    - `generic` (raw JSON event payload)
    - `slack` (Incoming Webhook `text` payload)
    - `teams` (MessageCard payload)
  - Configurable in UI or `config.json`
  - Test webhook button in `/settings`
  - Dedicated UI page: `/settings`
- Drilldown pages:
  - VEN warnings/errors
  - Each enforcement mode (including unmanaged workloads) with 24h trend lines
  - Tampering deduped workloads
- UI target management:
  - Add/remove/edit blocked-traffic targets in `/settings`
  - Save targets directly to `config.json`
  - Trigger immediate refresh from UI
- Reporting exports:
  - CSV export (`/api/export/report.csv`) for Excel
  - Print-friendly report page (`/report`) with summary + trend charts for PDF export
  - Trend view (`/trends`) reuses report layout and auto-refreshes every 5 minutes
  - Blocked traffic trends grouped by target in collapsible sections
  - Enforcement mode trends grouped in a collapsible section
- Theme:
  - Light/dark mode toggle in dashboard, drilldown, and report views
  - Shared UI helpers embedded from `/static/ui-common.js`
- Durable cross-version state:
  - Rolling 24h in-memory series is now persisted to disk (`rolling_state.json`)
  - Daily history remains persisted (`blocked_daily_history.json`, `ven_daily_history.json`)
  - State files are stored in a shared data directory so new fork/binary versions can reuse history
  - Atomic JSON writes and schema versioning are used for safe upgrades

## Binaries

Cross-platform binaries are produced in the project root:

- `illumio-dashboard-linux-amd64`
- `illumio-dashboard.exe`
- `illumio-dashboard-mac-intel`
- `illumio-dashboard-mac-arm`

## Quick Start

1. Run your platform binary.
2. Enter PCE URL, API key, API secret, and optional org ID.
3. Save config when prompted.
4. Open `http://localhost:18443`.
5. Go to `/settings` to configure traffic targets, retention, and optional alerting.

## Network Hosting Walkthrough

1. Set `bind_address` to a network listener, for example `0.0.0.0:18443`.
2. Set `public_base_url` to a reachable host URL used in generated links, for example `https://illumio-dashboard.internal`.
3. Restart the binary so listener changes apply.
4. Open firewall/security-group access for the listen port.
5. Optionally place behind a reverse proxy with TLS termination.

### HTTPS Example (Reverse Proxy)

Use this pattern when you want users to browse `https://illumiodashboard.local`:

1. Dashboard app:
- `bind_address`: `127.0.0.1:18443`
- `public_base_url`: `https://illumiodashboard.local`

2. Name resolution:
- DNS record or hosts file maps `illumiodashboard.local` to the host IP.

3. Reverse proxy terminates TLS on `:443` and forwards to `127.0.0.1:18443`.

Nginx example:

```nginx
server {
    listen 443 ssl;
    server_name illumiodashboard.local;

    ssl_certificate     /etc/ssl/certs/illumiodashboard.crt;
    ssl_certificate_key /etc/ssl/private/illumiodashboard.key;

    location / {
        proxy_pass http://127.0.0.1:18443;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
}
```

Caddy example:

```caddy
illumiodashboard.local {
    reverse_proxy 127.0.0.1:18443
}
```

## Rollback

- Stable baseline directory: `/home/ted/Codex/IllumioDashboard`
- Experimental rolling directory: `/home/ted/Codex/IllumioDashboard-rolling`
- UI hardening fork: `/home/ted/Codex/IllumioDashboard-rolling-uihardening`
- UI hardening v2 fork: `/home/ted/Codex/IllumioDashboard-rolling-uihardening-v2`

To roll back, run binaries from the stable directory and ignore/remove this fork.

## Configuration

Configuration is stored in `config.json`.
Runtime state is stored in a shared data directory:
- default: `$HOME/.illumio-monitoring-dashboard`
- override: `ILLUMIO_DASH_DATA_DIR`
- optional config override: `data_dir` in `config.json`

### Required fields

```json
{
  "pce_url": "https://your-pce:8443",
  "api_key": "api_key_id",
  "api_secret": "api_secret",
  "org_id": "1",
  "timezone": "America/Chicago",
  "bind_address": "0.0.0.0:18443",
  "public_base_url": "https://illumio-dashboard.internal",
  "data_dir": "/path/to/shared/state",
  "history_days": 365,
  "blocked_ma_window": 12,
  "blocked_anomaly_pct": 50,
  "blocked_anomaly_baseline": "daily",
  "blocked_anomaly_days": 7,
  "blocked_anomaly_min_coverage_pct": 70,
  "ven_ma_window": 12,
  "ven_anomaly_pct": 50,
  "ven_anomaly_baseline": "5m",
  "ven_anomaly_days": 7,
  "ven_anomaly_min_coverage_pct": 70,
  "tampering_ma_window": 12,
  "tampering_anomaly_pct": 50,
  "tampering_anomaly_baseline": "daily",
  "tampering_anomaly_days": 7,
  "tampering_anomaly_min_coverage_pct": 70,
  "tampering_daily_anomaly_pct": 50,
  "webhook_enabled": false,
  "webhook_provider": "generic",
  "webhook_url": "https://hooks.example.com/..."
}
```

### Configuration Reference

| Key | Purpose | Default | Notes |
|---|---|---|---|
| `pce_url` | Illumio PCE base URL | none | Required |
| `api_key` | PCE API key ID | none | Required |
| `api_secret` | PCE API secret | none | Required |
| `org_id` | PCE org ID | `1` | String in config |
| `timezone` | Timezone used for daily buckets/charts | server local time | IANA zone (for example `America/Chicago`); empty uses server local |
| `bind_address` | HTTP listen address | `:18443` | Restart required after change |
| `public_base_url` | External URL used in generated links/webhooks | `http://localhost:18443` | No trailing slash needed |
| `data_dir` | Shared state directory | `$HOME/.illumio-monitoring-dashboard` | Override via env `ILLUMIO_DASH_DATA_DIR` |
| `history_days` | Retention days for daily history files | `365` | Range `1..3650` |
| `blocked_ma_window` | Global 5m moving-average window points | `12` | Range `2..288` |
| `blocked_anomaly_pct` | Global blocked anomaly threshold percent | `50` | Range `1..10000` |
| `blocked_anomaly_baseline` | Baseline source for blocked anomaly detection | `5m` | `5m` compares latest 5m to 5m MA; `daily` compares latest 5m to N-day baseline |
| `blocked_anomaly_days` | Daily baseline lookback days (when baseline=`daily`) | `7` | Range `1..3650`; averages last N daily totals, then compares latest 5m value vs that daily baseline |
| `blocked_anomaly_min_coverage_pct` | Minimum daily baseline coverage before anomaly alerts are allowed | `70` | Range `1..100`; lower coverage stays in warmup/suppressed state |
| `ven_ma_window` | VEN warning/error MA window points | blocked MA fallback | Range `2..288` |
| `ven_anomaly_pct` | VEN warning/error anomaly threshold percent | blocked threshold fallback | Range `1..10000` |
| `ven_anomaly_baseline` | VEN anomaly baseline source | `5m` | `5m` compares latest 5m to 5m MA; `daily` compares latest 5m to N-day baseline |
| `ven_anomaly_days` | VEN daily baseline lookback days (when baseline=`daily`) | blocked days fallback | Range `1..3650` |
| `ven_anomaly_min_coverage_pct` | VEN minimum daily baseline coverage before anomaly checks | blocked min coverage fallback | Range `1..100` |
| `tampering_ma_window` | Tampering MA window points | blocked MA fallback | Range `2..288` |
| `tampering_anomaly_pct` | Tampering anomaly threshold percent | blocked threshold fallback | Range `1..10000` |
| `tampering_anomaly_baseline` | Tampering anomaly baseline source | `daily` | `5m` compares latest 5m to 5m MA; `daily` compares latest 5m to N-day baseline |
| `tampering_anomaly_days` | Tampering daily baseline lookback days (when baseline=`daily`) | blocked days fallback | Range `1..3650` |
| `tampering_anomaly_min_coverage_pct` | Tampering minimum daily baseline coverage before anomaly checks | blocked min coverage fallback | Range `1..100` |
| `tampering_daily_anomaly_pct` | Tampering threshold when baseline=`daily` | tampering anomaly fallback | Range `1..10000` |
| `traffic_targets[]` | Blocked traffic targets | built-in defaults | Each item has `name`, `kind`, optional per-target MA/anomaly overrides |
| `traffic_source_exclusions[]` | Source exclusions for blocked queries | `LG-SCANNERS` (auto) | Each item has `name`, `kind` |
| `webhook_enabled` | Enable webhook alert sends | `false` | Requires valid `webhook_url` |
| `webhook_url` | Webhook endpoint | empty | Used for alert transitions + test webhook |
| `webhook_provider` | Payload format | `generic` | `generic`, `slack`, `teams` |
| `webhook_slack_channel` | Optional Slack channel override | empty | Some endpoints ignore override |
| `webhook_slack_username` | Optional Slack username override | empty | Some endpoints ignore override |
| `webhook_slack_icon_emoji` | Optional Slack emoji override | empty | Some endpoints ignore override |
| `webhook_teams_title_prefix` | Optional Teams title prefix | empty | Added to MessageCard title |

### Optional traffic target configuration

`traffic_targets` defines blocked-traffic targets shown in UI and chart.

```json
{
  "traffic_targets": [
    { "name": "LG-E-PROD-ENVS", "kind": "label_group", "blocked_ma_window": 12, "blocked_anomaly_pct": 50 },
    { "name": "LG-E-NONPROD-ENVS", "kind": "label_group" },
    { "name": "E-WEB", "kind": "label" },
    { "name": "SOME-NAME", "kind": "auto" }
  ]
}
```

Optional source exclusions:

```json
{
  "traffic_source_exclusions": [
    { "name": "LG-SCANNERS", "kind": "auto" }
  ]
}
```

`kind` values:

- `label_group`: resolve only as label group
- `label`: resolve only as label
- `auto`: try label first, then label group

Optional per-target blocked anomaly overrides:
- `blocked_ma_window`: MA window points for this target only (2-288)
- `blocked_anomaly_pct`: anomaly threshold percent for this target only (1-10000)
- If omitted, global blocked anomaly settings are used.

If `traffic_targets` is omitted, defaults are used:

- `LG-E-PROD-ENVS`
- `LG-E-NONPROD-ENVS`

`history_days` controls how many days of daily blocked totals are retained on disk.

- default: `365`
- valid range: `1` to `3650`

## UI Guide

### Data Pipeline Status

- Green dot: successful data retrieval
- Red dot: data retrieval failed
- Green with `!`: partial success (for blocked traffic, some targets succeeded and some failed)

### Drilldowns

Click these cards/badges to open detailed lists:

- VEN Warnings
- VEN Errors
- Enforcement mode blocks (Idle/Visibility/Selective/Full)
- Tampered VENs (deduped VEN/workload names)
- Any blocked traffic target tile (5-minute line trend for that environment/target)
- VEN warning/error drilldowns also show a 24h trend line (while keeping workload name lists)
  - VEN warning/error drilldowns include toggle:
    - `24h (5m)` recent trend
    - `Daily` retained trend (bounded by `history_days`)
  - Enforcement mode drilldowns include toggle:
    - `24h (5m)` recent trend
    - `Daily` retained trend (bounded by `history_days`)
  - Blocked target drilldowns include `24h (5m)` and `Daily` trend toggle

### Trend View / Report

- `GET /trends` is a live auto-refreshing summary+trend page
- Report/trend pages include:
  - VEN trend charts (`24h (5m)` and `Daily`)
  - Blocked trend charts per target in collapsible groups
  - Enforcement mode trend charts in a collapsible group
  - Click any chart to open a larger expanded view
  - Y-axis auto-scales to rounded place-value bands (for example 2800-2900 for values around 2808) instead of always starting at zero
  - Refresh metadata (next refresh + data staleness)
  - Day-range filters (`24h`, `7d`, `30d`, `90d`, `180d`, `365d`) for daily trend lines

### Traffic And Data Settings

Use `/settings` to manage traffic/data controls:

1. Add target rows
2. Choose target `kind` (`auto`, `label_group`, `label`)
3. Save targets (writes `config.json`)
4. Click **Refresh Now** to apply immediately
5. Set daily blocked history retention days (saved to `config.json`)

### Hosting Settings

Use `/settings` to manage network exposure controls:
- Set `Server Bind Address` (`:18443`, `0.0.0.0:18443`, etc.)
- Set `Public Base URL` used in webhook/drilldown links
- Set optional `Timezone` for daily bucket boundaries and daily chart labels (`America/Chicago`, etc.)
- Restart service after bind-address changes

### Alerting Settings

Use `/settings` to manage webhook alerting:
- Enable/disable webhook
- Choose provider (`generic`, `slack`, `teams`)
- Set webhook URL
- Optional Slack fields: channel, username, icon emoji
- Optional Teams field: title prefix
- Send test webhook

## API Endpoints

- `GET /api/stats`:
  - Dashboard aggregate stats JSON
- `GET /api/drilldown?metric=<metric>`:
  - Drilldown list for a metric
  - metrics: `ven_warning`, `ven_error`, `mode_idle`, `mode_visibility_only`, `mode_selective`, `mode_full`, `mode_unmanaged`, `tampering`
- `GET /api/export/drilldown.csv?metric=<metric>[&target=<target>]`:
  - Export drilldown list + trend points (`24h (5m)` and `Daily` when available) to CSV
- `GET /api/config/targets`:
  - Current configured traffic targets
  - Current configured traffic source exclusions
  - Current `history_days`
  - Current blocked moving-average and anomaly settings
  - Current `timezone`
  - Current `bind_address` and `public_base_url`
- `PUT /api/config/targets`:
  - Save traffic/data settings
  - body: `{ "traffic_targets": [{"name":"...","kind":"..."}], "traffic_source_exclusions": [{"name":"LG-SCANNERS","kind":"auto"}], "history_days": 365, "blocked_ma_window": 12, "blocked_anomaly_pct": 50, "blocked_anomaly_baseline": "daily", "blocked_anomaly_days": 7, "blocked_anomaly_min_pct": 70, "ven_ma_window": 12, "ven_anomaly_pct": 50, "ven_anomaly_baseline": "5m", "ven_anomaly_days": 7, "ven_anomaly_min_pct": 70, "tampering_ma_window": 12, "tampering_anomaly_pct": 50, "tampering_anomaly_baseline": "daily", "tampering_anomaly_days": 7, "tampering_anomaly_min_pct": 70, "tampering_daily_anomaly_pct": 50, "timezone": "America/Chicago", "bind_address": "0.0.0.0:18443", "public_base_url": "https://illumio-dashboard.internal" }`
- `POST /api/refresh`:
  - Trigger immediate collection cycle
- `GET /api/config/alerts`:
  - Read alerting/webhook settings
- `PUT /api/config/alerts`:
  - Save alerting/webhook settings
- `POST /api/webhook/test`:
  - Sends a test webhook event using current config
- `GET /api/export/report.csv`:
  - Export current summary, lists, and trend data to CSV (Excel-compatible)
- `GET /report`:
  - Render printable report view with summary and trend charts
- `GET /trends`:
  - Live summary+trend view (same layout as report) with auto-refresh every 5 minutes

## Build and Test

### Rebuild all binaries

```bash
./scripts/rebuild-binaries.sh
```

### Standard tests

```bash
go test ./...
go test -race ./...
```

### Live integration test

Uses credentials from `config.json` and calls PCE APIs directly:

```bash
go test -run TestLiveIntegrationFromConfig -v -count=1
```

## Operational Notes

- Collector interval: 5 minutes
- Rolling mode in this fork:
  - Startup performs a 24-hour baseline fetch.
  - Blocked traffic uses one 24-hour baseline query per target.
  - New targets added later also get a one-time 24-hour baseline on first successful scan.
  - Then each refresh fetches only a 5-minute window and updates rolling counters.
  - During warmup (<24h since target baseline), UI shows dual blocked columns:
    - `Past 24h Baseline`
    - `Past Xm (5m agg)` where `X` grows over time
  - After warmup reaches 24h, each target consolidates to a single rolling 24h value.
  - If a blocked query appears to hit max results cap, target warning indicates possible truncation.
  - Rolling buckets are persisted in `rolling_state.json` (schema-versioned) for restart continuity.
  - Daily blocked totals are stored in `blocked_daily_history.json` using one record per target per completed day in configured timezone (or server local time by default).
  - VEN daily maxima are stored in `ven_daily_history.json`.
  - On startup, legacy local state files are auto-migrated into the shared data directory if destination files are absent.
  - Retention is pruned based on `history_days`.
- HTTP basic auth is used for PCE API calls
- `config.json` is written with file mode `0600`
- For async traffic queries, result count is read from job status and falls back to results download endpoints if needed

## Troubleshooting

- `socket: operation not permitted`:
  - Environment/network policy is blocking outbound PCE access
- Blocked target shows warning:
  - Target name may not exist in current PCE policy scope
  - Verify target `name` and `kind`
- Empty drilldown list:
  - Metric may be healthy (no matching entities in latest cycle)

## Project Layout

- `main.go`: backend, API collectors, config APIs, drilldown API
- `index.html`: dashboard UI and target editor
- `details.html`: drilldown UI
- `report.html`: printable report UI (PDF export target)
- `static/ui-common.js`: shared UI helpers (theme, staleness/refresh text, metric tooltip mappings)
- `integration_live_test.go`: live API integration test
- `scripts/rebuild-binaries.sh`: multi-platform build script
- `config.json`: runtime configuration
- `blocked_daily_history.json`: persisted daily blocked totals per target
- `ven_daily_history.json`: persisted daily VEN warning/error max values
