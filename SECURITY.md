# Security policy

This policy covers both programs of this repository: the exporter
`gnss_ptp_exporter` and the clockClass helper `gnss_ptp_clockclass`.

gnss_ptp_exporter parses data from gpsd (JSON over TCP), from the GNSS
receiver (UBX frames relayed by gpsd), from ptp4l (management messages over
a Unix socket) and from ts2phc's log. Bugs that let such input crash or hang
the exporter, exhaust its memory, or make it send anything other than the
documented read-only requests are security issues, as is anything that
exposes data beyond the documented metrics over HTTP.

## Reporting a vulnerability

Please **do not open a public issue.** Report privately via GitHub:
**Security → Report a vulnerability** in this repository (private
vulnerability reporting). Direct link:
<https://github.com/thomas-schreck/gnss_ptp_exporter/security/advisories/new>.

Include the affected version or commit, a minimal reproducer (for example
the input line or datagram) and the observed behavior. You will get an
answer within 14 days. Fixes are developed in a private advisory and
published together with the advisory once a release is available. Reporters
are credited unless they prefer otherwise.

## Supported versions

Only the latest release and `main` receive security fixes.

## Design

### gnss_ptp_exporter

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

### gnss_ptp_clockclass

The helper is the one part of this repository that **writes**: it changes
the clockClass ptp4l announces, so a bug in it can make PTP clients trust
or distrust the grandmaster wrongly. Making it send anything other than the
documented request, or set a class its documented logic does not yield,
is a security issue.

- **One write request.** It sends exactly one kind of write: `SET
  GRANDMASTER_SETTINGS_NP` (linuxptp management ID 0xC001) on ptp4l's
  read-write socket (`/var/run/ptp4l`). That socket would accept any
  management SET or COMMAND; the code builds no other (the SET builder
  takes only these settings, and a test checks that no other package
  uses SET or COMMAND). GETs go to the read-only socket.
- **Only on change.** A SET is sent when ptp4l's current settings (GET on
  every evaluation) differ from the computed ones; `--dry-run` never opens
  the read-write socket and never writes the state file.
- **Privileges.** linuxptp creates the read-write socket `root:root 0660`,
  so the example unit runs as root, but with an empty capability bounding
  set, `NoNewPrivileges`, `ProtectSystem=strict` and a system call filter.
  It writes only its state file under `/run/gnss_ptp_clockclass/`.
- **No shell.** `journalctl`, `systemctl` and `chronyc` are executed
  directly with fixed arguments and a timeout.
- **Isolation from the exporter.** The SET code lives in
  `cmd/gnss_ptp_clockclass/internal/`, which Go's internal package rule
  keeps the exporter from importing; a test checks the exporter's
  dependency list.

## How it is tested

- Unit tests with recorded fixtures for every collector; `go test -race` in
  CI.
- Go native fuzz tests for every parser (gpsd reports, UBX scanner,
  MON-RF, NAV-STATUS, ts2phc lines, ptp4l responses,
  GRANDMASTER_SETTINGS_NP, chrony output, state file): a few seconds in
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
