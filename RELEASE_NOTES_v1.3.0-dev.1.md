# v1.3.0-dev.1 - Integrated Blocked Traffic Extractor Preview

This prerelease development build embeds the Blocked Traffic Extractor `v1.5.0` inside the Illumio Monitoring Dashboard for evaluation.

## Included

- Complete extractor workspace at `/blocked-traffic/`
- Multi-CSV import, exact-row deduplication, saved datasets, and month-over-month analytics
- Configurable label dimensions, heatmaps, service trends, and executive reporting exports
- Report templates, scheduling, run history, and Slack, Teams, generic webhook, email, and SFTP delivery options
- Navigation between dashboard, settings, drilldowns, trends, executive reporting, and extractor workspaces
- Linux, Windows, macOS Intel, and macOS Apple Silicon development binaries

## Credential and security boundaries

- Monitoring Dashboard collection continues to use the PCE credentials in `config.json`.
- The Blocked Traffic Extractor uses only its separately saved PCE profiles and API credentials.
- Extractor profiles, datasets, templates, destinations, and run history remain in the extractor's dedicated platform user-configuration directory.
- Extractor pages and APIs remain localhost-only, even if the dashboard listener is configured for network access.

## Development status

This is a prerelease preview intended for evaluation and feedback. It does not replace the current stable `v1.2.11` dashboard release.
