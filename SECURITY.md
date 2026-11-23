# Security policy

gnss_ptp_exporter parses data from gpsd (JSON over TCP), from the GNSS
receiver (UBX frames relayed by gpsd), from ptp4l (management messages over
a Unix socket) and from ts2phc's log. Bugs that let such input crash or hang
the exporter, exhaust its memory, or make it send anything other than the
documented read-only requests are security issues, as is anything that
exposes data beyond the documented metrics over HTTP.

## Reporting a vulnerability

Please **do not open a public issue.** Report privately via GitHub:
**Security → Report a vulnerability** in this repository (private
vulnerability reporting).

Include the affected version or commit, a minimal reproducer (for example
the input line or datagram) and the observed behavior. You will get an
answer within 14 days. Fixes are developed in a private advisory and
published together with the advisory once a release is available. Reporters
are credited unless they prefer otherwise.

## Supported versions

Only the latest release and `main` receive security fixes.

## Design

- **Read only.** The ptp4l client builds GET requests only (tested) and is
  meant for ptp4l's read-only socket, which refuses everything else. The
  u-blox collector sends only empty poll frames for MON-RF and NAV-STATUS.
  Nothing is written to disk.
- **No privileges.** The example systemd unit runs as a dynamic user with
  no capabilities and a system call filter.
- **Bounded input.** gpsd lines are limited to 1 MiB, UBX payloads to
  4 KiB, ptp4l datagrams to 1500 bytes, the ts2phc file tail to 64 KiB.
  Every network operation has a timeout.
- **No shell.** `journalctl` is executed directly with fixed arguments.

## How it is tested

- Unit tests with recorded fixtures for every collector; `go test -race` in
  CI.
- Go native fuzz tests for every parser (gpsd reports, UBX scanner,
  MON-RF, NAV-STATUS, ts2phc lines, ptp4l responses): a few seconds in
  every CI run, five minutes per target nightly (`Fuzz` workflow).
- golangci-lint (including gosec), govulncheck and CodeQL on every change.

## Supply chain

- Dependabot alerts and security updates are on; weekly update PRs for Go
  modules and GitHub Actions go through CI.
- Workflows pin actions by commit SHA, use least-privilege `permissions:`
  and check out without persisted credentials.
- Only the maintainer can create `v*` tags (repository ruleset). Release
  archives carry build provenance attestations and an SPDX SBOM:

  ```sh
  gh attestation verify gnss_ptp_exporter-<version>.linux-arm64.tar.gz -R thomas-schreck/gnss_ptp_exporter
  ```
- OpenSSF Scorecard runs weekly and on every push to `main`.
