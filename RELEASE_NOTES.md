# Release Notes

## v1.0.0 - 2026-03-09

First production-ready public release of **Illumio Monitoring Dashboard**.

### Highlights
- Go-only single-binary dashboard (Linux/Windows/macOS builds).
- Rolling 24h monitoring model:
  - initial 24h baseline fetch
  - incremental 5-minute collection
  - warmup-aware display and transitions
- Persistent local state across restarts (rolling and daily history).
- Dedicated **Settings** workspace with grouped/collapsible configuration sections.
- Dedicated **Trend View / Report** pages with PDF and CSV export.
- Webhook alerting for anomaly trigger/resolution (Generic, Slack, Teams payload modes).

### Monitoring Coverage
- Workload inventory and enforcement mode breakdown:
  - `idle`, `visibility_only`, `selective`, `full`, `unmanaged`
- VEN health monitoring:
  - warning and error counts
  - drilldown with workload/VEN names
  - reason enrichment from VEN conditions/latest event (when returned by API)
- Tampered VENs:
  - deduplicated impacted VEN/workload view
  - trend and drilldown support
- Blocked traffic analytics:
  - source + destination blocked flow aggregation per target
  - label group and label target support (`auto`, `label_group`, `label`)
  - configurable source exclusions

### Trending, Drilldown, and Export
- Drilldowns include line charts with 24h (5m) and daily history where applicable.
- Report/Trend pages include:
  - mode controls (`All`, `24h (5m) only`, `Daily only`)
  - chart length controls (`24h`, `7d`, `30d`, `90d`, `180d`, `365d`)
  - collapsible chart sections and grouped target trends
- Export support:
  - CSV exports for report/drilldown datasets
  - PDF report export with charts

### Anomaly Detection
- Moving-average overlays on monitored trend series.
- Baseline source options:
  - `5m` moving average
  - `daily` N-day baseline
- Independent anomaly settings for:
  - Blocked traffic
  - VEN warnings/errors
  - Tampering
- Configurable baseline lookback window and minimum coverage gate for warmup suppression.
- Per-target overrides for blocked traffic MA/anomaly thresholds.

### Alerting
- Webhook notifications for blocked-target anomaly transitions:
  - `triggered`
  - `resolved`
- Provider formats:
  - Generic JSON
  - Slack incoming webhook format
  - Teams incoming webhook format
- Transition-state tracking to avoid duplicate triggered/resolved spam.
- Test webhook endpoint available from Settings.

### UI/UX
- Dashboard title standardized to **Illumio Monitoring Dashboard**.
- Full configuration moved off main dashboard to improve operational readability.
- Light/dark theme support with improved contrast and readability.
- Data pipeline status indicators for collector stages.

### Hosting and Deployment
- Server bind configuration (`bind_address`) for network hosting.
- External link base configuration (`public_base_url`) for generated links.
- Timezone-aware daily bucketing/charts (`timezone`, optional).
- Shared data directory support:
  - default: `$HOME/.illumio-monitoring-dashboard`
  - override via `ILLUMIO_DASH_DATA_DIR` or `data_dir` in config

### API/Scalability Notes
- Pagination handling for large environments.
- Async blocked traffic query flow with status polling before download.
- High `max_results` usage where applicable for large PCEs.
- Graceful behavior when optional exclusions/targets are missing.

### Included Build Artifacts
- `illumio-dashboard-linux-amd64`
- `illumio-dashboard.exe`
- `illumio-dashboard-mac-intel`
- `illumio-dashboard-mac-arm`

### Upgrade/Compatibility
- Config is backward-compatible with earlier rolling forks; new keys are optional.
- Existing installs can adopt v1.0.0 by replacing binary and preserving data directory.

### Known Limitations
- Some VEN reason/condition fields are API-populated only in certain states and may be empty.
- Provider-specific webhook overrides (for example Slack channel/username/icon) may be ignored by webhook endpoint policy.
- Daily anomaly baselines require sufficient retained history to fully stabilize.
