# Release Notes

## v1.2.8 - 2026-04-02

### Added
- Separate webhook channel support for blocked daily reconcile summaries.
  - New optional settings for `daily_summary_webhook_*` (provider/url/enabled plus Slack/Teams overrides).
  - Summary payload is blocked-traffic-only by design (does not include tampering metrics).
  - Triggered when previous-day blocked reconciliation completes for all configured targets.
- Daily summary manual send endpoint:
  - `POST /api/webhook/daily-summary/send`
  - Sends previous-day blocked reconcile summary on demand.

### Changed
- Webhook test now validates all enabled webhook configurations (anomaly + daily summary).
- Daily summary webhook message now includes per-target blocked totals by target name.
- Alerting Settings action row moved to the top of the section for clearer save/test/send flow.

## v1.2.7

Stable release focused on VEN status observability and suspended VEN workflow parity.

### Added
- New `Suspended VENs` card on the main dashboard, inline with VEN warnings/errors/tampering.
- New drilldown metric: `ven_suspended`.
  - Includes workload/VEN name list.
  - Includes `24h (5m)` and `Daily` trend modes, aligned with warning/error drilldowns.

### Changed
- Suspended VEN detection now uses VEN **status** semantics (not health semantics).
- Suspended VEN drilldown entries display VEN/workload name only (no forced reason suffix when API reason text is absent).

## v1.2.5-branch (feature/experiments-2026-03-19)

Experimental feature branch updates (not merged to stable main):

### Adaptive API Rate Control
- Added global API token-bucket limiter for all PCE API requests:
  - default cap: `450 RPM`
  - configurable via `config.json` key: `api_max_rpm`
- Added adaptive HTTP `429` handling:
  - parses and respects `Retry-After` when returned by PCE
  - temporarily reduces active RPM budget under throttling
  - gradually recovers budget back toward configured max
- Added cycle-level API usage telemetry:
  - `[API-RATE]` logs include per-cycle request totals, average RPM, and current/target budget

### Cycle-Aware Query Scheduling
- Added cycle deadline pressure checks for blocked traffic collection so high-volume environments remain stable within 5-minute collector windows.
- Added graceful degradation tiers under budget pressure:
  - Tier 1: blocked totals remain primary and continue refreshing
  - Tier 2: blocked port/proto enrichment is deferred first
  - Tier 3: blocked hostname enrichment is deferred next
  - late-cycle fallback can switch to count-only blocked query path when necessary

### Tampering Daily Chart Consistency (rc8)
- Fixed tampering daily data feed to use deduped **today-so-far** workload set counts from current-day 5m buckets.
- Preserved daily **max-only** behavior while correcting the source metric so daily tampering no longer reflects stale/inflated historical rolling artifacts.

### Policy Drilldown Range Controls
- Added daily range selector support in policy drilldowns:
  - `7d`, `30d`, `90d`, `180d`, `365d`
- Applies to:
  - `policy_rulesets`
  - `policy_rules`
  - `policy_ruleset`

### Tampering Metric Alignment (rc9)
- Updated tampering 24h(5m) trend points to use deduped unique impacted workload/VEN counts per 5-minute bucket.
- Updated daily tampering value semantics to represent unique impacted workload/VEN counts for the calendar day (not sum of 5m points).
- Added tampering reconcile method-version marker so prior day keys are replayed once when methodology changes, ensuring stored historical daily values are re-baselined under the current method.

### Blocked Target Scope
- Added `traffic_targets[].kind = "all"` support:
  - runs environment-wide blocked query with blank source/destination filters
  - target name is optional; defaults to `ALL-BLOCKED-TRAFFIC`
- Added comma-separated multi-label target support in `traffic_targets[].name`:
  - example: `A-Daily, E-Production`
  - matcher applies all listed labels together for target scoping.

### Blocked Tile Warmup Consistency
- Warmup current view now shows `baseline + incremental` so tiles no longer show `0` while recent warmup increment is non-zero.

### Blocked Hostname Metrics (Configurable)
- Added optional per-host blocked flow aggregation (inbound/outbound counts):
  - `blocked_host_metrics_enabled`
  - `blocked_host_retention_mode`:
    - `rolling_24h_only`
    - `rolling_24h_plus_daily`
    - `daily_only` (new): store only daily host rollups; no 24h 5m host snapshots
- Added SQLite storage for hostname metrics:
  - `blocked_hosts_5m` (rolling 24h snapshots)
  - `blocked_hosts_daily` (daily retained snapshots when configured)
- Added blocked target drilldown hostname table:
  - columns: `hostname`, `outbound`, `inbound`, `total`
  - uses rolling 24h or daily rollups based on retention mode

### Blocked Hostname + Exclusion Fixes
- Fixed blocked host drilldown first-run behavior:
  - when rolling 24h host snapshots are still empty, drilldown now runs a live 24h host fallback query so hostname data is available immediately.
- Fixed source-exclusion clearing:
  - empty `traffic_source_exclusions` now persists as empty (no automatic `LG-SCANNERS` fallback reinjection).
- Improved blocked hostname direction fidelity:
  - host aggregation now prefers workload `hostname` over generic workload `name` to reduce host-key collisions that could skew inbound/outbound totals.

### UI Polish
- Added `Collapse/Expand` control for `Blocked Ports (Daily Aggregate)` in blocked-target drilldown.
- Standardized top banner title text across main/supporting pages to reduce visual context switching.

### Policy Growth Metrics (Opt-In)
- Added optional daily policy-growth tracking:
  - `rules_metrics_enabled` (default `false`)
  - collects total `rulesets` and total `rules` once per day
  - persists in daily history with retention governed by `history_days`
  - stores per-ruleset daily rule counts for drilldown trends
- Added Trend View policy section:
  - `Policy Rulesets (Daily)`
  - `Policy Rules (Daily)`
- Added Executive View policy coverage (when enabled):
  - protection KPIs for total rulesets/rules
  - policy metrics status KPI
  - `Policy Growth Trend (Daily)` chart
- Added manual policy refresh control:
  - Settings button: `Refresh Policy Metrics`
  - API: `POST /api/refresh/policy-metrics`
  - forces immediate rulesets/rules snapshot update (no wait for next 5m cycle)
- Added drilldown metrics for policy growth:
  - `policy_rulesets`
  - `policy_rules`
  - `policy_ruleset` (targeted ruleset trend)
- Drilldown enhancement:
  - `policy_rulesets` drilldown now lists rulesets with current total rules
  - each ruleset row is clickable to open `policy_ruleset` daily trend

## v1.2.4 - 2026-03-18

Stable release focused on tampering-data correctness, reconciliation reliability, and operator controls.

### Tampering Data Correctness
- Tampering queries now strictly filter events to the requested time window before dedupe/counting.
- Tampering event counts are deduped by stable event signature before 5m/24h trend aggregation.
- Added startup tampering-history reconciliation with persisted per-day completion markers for prior stored day keys.

### Tampering Reconciliation Controls
- Added manual tampering-history reconciliation API:
  - `POST /api/reconcile/tampering-history`
- Added tampering reconcile status API:
  - `GET /api/reconcile/tampering-history/status`
- Added Settings UI control:
  - `Reconcile Tampering History` button with status line.

### Tampering Pipeline Stability
- Added tampering slice pagination safety guards to prevent runaway `skip` loops:
  - repeated-page signature detection
  - stagnation detection when pages add no new events
  - per-slice timeout guard for non-progressing pagination

## v1.2.2 - 2026-03-18

Stabilization release focused on blocked-traffic correctness, reconciliation controls, and operator usability.

### Highlights
- Improved blocked 24h rolling accuracy by deduplicating repeated 5-minute flow snapshots using flow signatures and last-detected progression.
- Added configurable dedupe backend for blocked rolling calculations:
  - `sqlite` (default; lower process memory footprint, restart-safe)
  - `memory` (in-process)
- Added blocked-history reconcile controls and visibility:
  - startup-triggered reconcile with persisted completion marker
  - incremental startup reconcile when target set changes
  - manual trigger endpoint and settings UI trigger
  - reconcile status endpoint

### Blocked Traffic Accuracy And Reconciliation
- Corrected combined blocked count path to prefer async job `result_count` (with row-count fallback).
- Added previous-day authoritative blocked reconciliation via 24h target snapshots.
- Added full historical blocked reconciliation endpoint:
  - `POST /api/reconcile/blocked-history`
- Added reconcile status endpoint:
  - `GET /api/reconcile/blocked-history/status`
- Full reconcile now uses org-scoped API paths consistently.
- Full reconcile exclusion resolution is now best-effort (unresolved exclusion labels/groups are skipped without warning spam).

### Runtime/Operations
- Process logging now writes to `illumiomonitoringdashboard.log` in the runtime directory (and stdout).
- Added verbose blocked/reconcile diagnostic logging for discrepancy triage.
- Startup reconcile marker persisted per target identity; already-reconciled targets are skipped on subsequent restarts.

### UI/UX
- Dashboard layout now uses more available horizontal space on larger screens.
- Dashboard refresh metadata countdown now remains accurate after page reload (aligned to snapshot cadence).
- Increased chart point hover targets across drilldown/report/executive views for easier interaction.
- Preserved trend/report section expand/collapse state across view and range changes (carried forward in stable release).

### Configuration And Docs
- Added `blocked_rolling_dedupe_backend` configuration and settings UI control.
- Updated configuration examples/reference and API examples in README for new dedupe backend and reconcile usage.

## v1.2.1 - 2026-03-12

Patch release focused on Trend View usability.

### Fixes
- Trend/Report section expansion state now persists when changing:
  - `View` mode (`Show: All`, `24h (5m) Only`, `Daily Only`)
  - `Chart Length` (`7d/30d/90d/180d/365d`)
- Previously expanded/collapsed groups no longer reset on rerender after mode/range changes.
- Applies to:
  - `Anomaly Outcomes`
  - `VEN Status Trends`
  - `Enforcement Mode Trends`
  - per-target blocked traffic trend groups

### Notes
- If a section has no charts for the selected mode, it remains closed for that render and reuses the saved state when charts are available again.

## v1.1.1 - 2026-03-11

Patch release focused on security hardening and deployment guidance.

### Security Fixes
- Added trusted-origin protection for all mutable API routes:
  - `PUT /api/config/targets`
  - `PUT /api/config/alerts`
  - `POST /api/refresh`
  - `POST /api/webhook/test`
- State-changing requests now validate browser `Origin`/`Referer` against trusted origins derived from:
  - `public_base_url`
  - current request host/protocol
  - forwarded host/protocol headers when behind proxy
- Cross-origin mutable requests are rejected with `403` and logged with security context.

### UI Hardening
- Hardened dynamic UI rendering against HTML injection by replacing unsafe `innerHTML` usage in key surfaces:
  - dashboard blocked target tile rendering
  - settings target editor rows
  - blocked-port drilldown table rows

### Testing
- Added unit tests for origin normalization and trusted-origin allow/block behavior.

### Documentation
- Clarified deployment modes:
  - convenience/internal direct-bind mode
  - secure production mode (localhost bind + authenticated HTTPS reverse proxy)
- Documented origin-check behavior for state-changing API routes.

## v1.1.0 - 2026-03-11

Minor feature release focused on executive reporting, anomaly observability, and API efficiency improvements.

### Highlights
- New **Executive View** capabilities:
  - period selector (`7d/30d/90d`)
  - business value ranking by blocked outcomes
  - narrative summary generator (copy/download)
  - board-ready print mode enhancements
- New **Anomaly Outcomes** section in Trend/Report view:
  - collapsible section with active and transition counts
  - recent transition context from persisted history
- New dashboard **SLO Confidence** badge in pipeline status strip.
- New persisted anomaly history across all anomaly types:
  - blocked targets
  - VEN warnings
  - VEN errors
  - tampering
- New anomaly history API:
  - `GET /api/anomalies/history?days=<n>&limit=<n>`

### Performance and Collector Changes
- Added per-target blocked query pacing/staggering to smooth 5-minute burst load.
- Added query result reuse for blocked count + port/proto extraction paths.
- Daily blocked history now accumulates from 5-minute deltas (counts and ports), reducing separate daily snapshot pressure.

### UI/UX
- Trend/report anomaly outcomes integrated with collapsible behavior to match existing grouped sections.
- Executive page now supports period-aware KPI framing and period-aware board links.

### Data/Storage
- Added `anomaly_history.jsonl` in shared data directory for durable transition history.
- Retention follows `history_days`, keeping storage bounded.

### Compatibility
- Backward-compatible with existing persisted state and config.
- No required config migration.

## v1.0.1 - 2026-03-09

Patch release focused on chart UX and daily-history correctness.

### Fixes
- Fixed chart zoom modal behavior:
  - resolved canvas reuse errors (`Canvas is already in use`)
  - resolved Chart.js scriptable resolver recursion errors in zoom view
  - drilldown/report charts now expand reliably when clicked
- Added `Trend View` shortcut button to drilldown top navigation.
- Fixed blocked daily trend continuity:
  - daily trend now includes a live "today-so-far" point from rolling 5m data
  - prevents daily chart from appearing stuck at prior day while current day is in progress
- Fixed DST-related daily snapshot gap:
  - changed daily snapshot windowing to calendar-day math (`AddDate(0,0,-1)`)
  - prevents missing/misaligned daily points around DST transitions (for example March 8, 2026 in US timezones)

### Operational Notes
- No config migration required.
- Existing persisted history files remain compatible.

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
