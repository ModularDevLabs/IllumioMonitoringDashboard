# Blocked Traffic Extractor Development Integration

This branch embeds the Illumio Blocked Traffic Extractor `v1.5.0` in the Monitoring Dashboard executable as a development preview.

## User experience

The Monitoring Dashboard remains at `/`. The integrated extractor is available at `/blocked-traffic/` with its complete workspace:

- Extractor and policy-object discovery
- Single- and multi-CSV import with exact-row deduplication
- Configurable primary and secondary label dimensions
- Analytics dashboard and month-over-month service trends
- Heatmap explorer
- Configurable executive-summary and image exports
- Saved datasets
- Report templates, schedules, run history, and delivery destinations

Navigation links connect the dashboard, settings, drilldowns, reports, executive view, and extractor workspaces.

## Credential boundary

The two functions intentionally keep independent Illumio credentials.

| Function | Credential source | Used for |
|---|---|---|
| Monitoring Dashboard | `config.json` (`pce_url`, `org_id`, `api_key`, `api_secret`) | Continuous VEN, workload, tampering, policy, and dashboard blocked-traffic collection |
| Blocked Traffic Extractor | Saved PCE profiles in the platform user configuration directory | Interactive extraction, policy-object discovery, and scheduled extractor templates |

The embedded extractor does not read, copy, or fall back to dashboard credentials. A user must explicitly create or select an extractor PCE profile even when both functions connect to the same PCE.

On Linux, integrated extractor state is normally stored below `$XDG_CONFIG_HOME/illumio-monitoring-dashboard-extractor` or `~/.config/illumio-monitoring-dashboard-extractor`. Equivalent platform user-configuration locations are used on Windows and macOS. Profile and automation files are created with private permissions where the platform supports them.

The integrated module intentionally does not reuse the standalone Blocked Traffic Extractor's configuration directory. This prevents an independently running extractor and the dashboard development build from executing the same scheduled templates or writing the same state files concurrently.

## Route boundary

All extractor browser and API routes are namespaced:

- Pages: `/blocked-traffic/`, `/blocked-traffic/summary`, `/blocked-traffic/heatmaps`, `/blocked-traffic/executive-summary`, `/blocked-traffic/automation`
- APIs: `/blocked-traffic/api/...`
- Assets: `/blocked-traffic/assets/...`

The module uses same-origin checks for state-changing calls and additionally requires a localhost host name or loopback address. If the dashboard listener is exposed on a network interface, remote users can use dashboard functions but receive `403 Forbidden` for `/blocked-traffic/...`. This intentionally preserves the extractor's localhost-only security boundary for the development preview.

## Source and maintenance

The initial integration is based on `ModularDevLabs/Illumio-Blocked-Traffic-Extractor` tag `v1.5.0`, commit `60ca5b4c5bfbd910ae47d9c9884811d868f070f1`. Its Go code and embedded frontend live under `internal/extractor` so its state and route behavior remain encapsulated.

Future extractor updates should be ported into that package, then have absolute browser routes rebased to `/blocked-traffic` and the integration test suite run before publishing another dashboard development build.
