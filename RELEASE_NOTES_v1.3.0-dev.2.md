# v1.3.0-dev.2 - Extractor Integration Security Hardening

This prerelease supersedes `v1.3.0-dev.1` and addresses all GitHub Advanced Security findings raised against the initial integrated Blocked Traffic Extractor preview.

## Security changes

- Requires interactive PCE operations to use a server-side saved Extractor profile; request bodies can no longer substitute a network destination or API credentials.
- Restricts non-loopback PCE origins to HTTPS and enforces exact-origin checks for async locations and redirects.
- Requires both a loopback HTTP host and a loopback client address for Extractor routes, preventing a spoofed `Host: localhost` header from bypassing the local-only boundary.
- Constructs SMTP messages from parsed addresses, single-line RFC 2047-encoded subjects, quoted-printable bodies, and base64 attachments to prevent email-header/content injection.
- Routes artifacts, reports, downloads, retention, shared-folder delivery, and SFTP private-key access through root-confined filesystem operations that reject traversal and symlink escape.
- Normalizes SFTP remote directories and rejects traversal, backslashes, control characters, and empty paths.
- Requires shared-folder destinations to exist before saving or delivery and continues to refuse overwrites.

## Validation

- Go unit and integration tests
- Race detector
- Go static analysis
- Frontend JavaScript syntax checks
- Cross-platform Linux, Windows, macOS Intel, and macOS Apple Silicon builds
- Runtime localhost and spoofed-host boundary tests
- GitHub CodeQL analysis

This remains a development prerelease and does not replace the stable Monitoring Dashboard release.
