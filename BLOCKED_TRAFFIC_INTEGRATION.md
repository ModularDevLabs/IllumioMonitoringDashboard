# Blocked Traffic Extractor Development Integration

This branch embeds the Illumio Blocked Traffic Extractor `v1.5.0` in the Monitoring Dashboard executable as a development preview.

## User experience

The Monitoring Dashboard remains at `/`. The integrated extractor is available at `/blocked-traffic/` with its complete workspace:

- Extractor and policy-object discovery
- Blocked-only or all-policy-decision traffic extraction
- Single- and multi-CSV import with exact-row deduplication
- Configurable primary and secondary label dimensions
- Analytics dashboard and month-over-month service trends
- Heatmap explorer
- Configurable executive-summary and image exports
- Saved datasets
- Report templates, schedules, run history, and delivery destinations

Navigation links connect the dashboard, settings, drilldowns, reports, executive view, and extractor workspaces. The shared left-navigation group is labeled **Traffic** because the extractor supports both blocked-only and all-policy-decision scopes; the `/blocked-traffic/...` route prefix remains unchanged for backward compatibility.

The traffic-scope selector defaults to `blocked` for backward compatibility. Selecting `all` sends an empty `policy_decisions` filter to the PCE, which returns allowed, potentially blocked, blocked, and unknown flows. The CSV records the active policy decision, draft policy decision, and selected scope. Profiles and automation templates retain the selection independently. Analytics continue to use the established endpoint/service identity for unique connections, so the same connection appearing under multiple policy decisions contributes its flows without being counted as multiple unique connections.

All-traffic queries can be substantially larger. If a query reports more matches than the PCE's 200,000-row response ceiling, the extractor aborts without writing a partial CSV and instructs the user to choose a smaller chunk interval.

Manual profiles and automation templates can exclude services independently of their service inclusion filter. Exclusions accept discovered PCE service names and explicit protocol/port values such as `TCP:22`, `TCP:1024-2048`, or `UDP:5355`.

Dashboard collection and traffic extraction run concurrently in independent Go workers. They use separate credentials, HTTP clients, contexts, retry state, and progress state, so a dashboard refresh never cancels or replaces an extraction request. Because both functions ultimately reach the same PCE, the extractor also emits periodic per-chunk PCE heartbeats and retains its chunk timeout/retry safeguards to make server-side queueing or throttling visible without pausing dashboard collection.

## Credential boundary

The two functions intentionally keep independent Illumio credentials.

| Function | Credential source | Used for |
|---|---|---|
| Monitoring Dashboard | `config.json` (`pce_url`, `org_id`, `api_key`, `api_secret`) | Continuous VEN, workload, tampering, policy, and dashboard blocked-traffic collection |
| Blocked Traffic Extractor | Saved PCE profiles in the platform user configuration directory | Interactive extraction, policy-object discovery, and scheduled extractor templates |

The embedded extractor does not read, copy, or fall back to dashboard credentials. A user must explicitly create or select an extractor PCE profile even when both functions connect to the same PCE.

Interactive PCE operations accept only a saved profile name. PCE URLs and credentials submitted with an extraction request are ignored, preventing request-time network destination substitution. Saved non-loopback PCE origins must use HTTPS; HTTP remains available only for loopback development endpoints.

On Linux, integrated extractor state is normally stored below `$XDG_CONFIG_HOME/illumio-monitoring-dashboard-extractor` or `~/.config/illumio-monitoring-dashboard-extractor`. Equivalent platform user-configuration locations are used on Windows and macOS. Profile and automation files are created with private permissions where the platform supports them.

The integrated module intentionally does not reuse the standalone Blocked Traffic Extractor's configuration directory. This prevents an independently running extractor and the dashboard development build from executing the same scheduled templates or writing the same state files concurrently.

## Route boundary

All extractor browser and API routes are namespaced:

- Pages: `/blocked-traffic/`, `/blocked-traffic/summary`, `/blocked-traffic/heatmaps`, `/blocked-traffic/executive-summary`, `/blocked-traffic/automation`
- APIs: `/blocked-traffic/api/...`
- Assets: `/blocked-traffic/assets/...`

The module uses same-origin checks for state-changing calls and additionally requires a localhost host name or loopback address. If the dashboard listener is exposed on a network interface, remote users can use dashboard functions but receive `403 Forbidden` for `/blocked-traffic/...`. This intentionally preserves the extractor's localhost-only security boundary for the development preview.

Extractor artifact operations use Go's root-confined filesystem API. Output filenames cannot traverse outside their selected absolute directory, symlink escapes are rejected, existing artifacts are never overwritten, shared-folder destinations must already exist, SFTP remote paths are normalized and traversal-free, and email delivery uses parsed addresses plus encoded headers and body content.

## Source and maintenance

The initial integration is based on `ModularDevLabs/Illumio-Blocked-Traffic-Extractor` tag `v1.5.0`, commit `60ca5b4c5bfbd910ae47d9c9884811d868f070f1`. Its Go code and embedded frontend live under `internal/extractor` so its state and route behavior remain encapsulated.

Future extractor updates should be ported into that package, then have absolute browser routes rebased to `/blocked-traffic` and the integration test suite run before publishing another dashboard development build.

Building this branch from source requires Go 1.25 or newer for root-confined filesystem access. Published binaries remain self-contained.
