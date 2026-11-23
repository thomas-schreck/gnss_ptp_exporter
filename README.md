# gnss_ptp_exporter

[![CI](https://github.com/thomas-schreck/gnss_ptp_exporter/actions/workflows/ci.yml/badge.svg)](https://github.com/thomas-schreck/gnss_ptp_exporter/actions/workflows/ci.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/thomas-schreck/gnss_ptp_exporter/badge)](https://scorecard.dev/viewer/?uri=github.com/thomas-schreck/gnss_ptp_exporter)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/17046/badge)](https://www.bestpractices.dev/projects/17046)

A Prometheus exporter for GNSS-disciplined time servers built from
**GNSS receiver → gpsd → ts2phc → ptp4l**, the common Raspberry Pi
Stratum-1 / PTP grandmaster stack (for example a Pi 5 with an Intel i226
NIC and a u-blox ZED-F9T). One static binary covers the whole chain:

| Collector | Source | Default |
|---|---|---|
| `gpsd` | gpsd JSON over TCP (`?WATCH`), streaming | on |
| `ptp4l` | linuxptp management messages over the Unix socket (GET only) | on |
| `ts2phc` | ts2phc's log (journald or a file); ts2phc has no query interface | on |
| `ublox` | UBX `MON-RF` and `NAV-STATUS`, polled through gpsd's raw passthrough | off |

The exporter **only measures**. It never sends a PTP management SET or
COMMAND, never changes gpsd or receiver configuration, and writes nothing
to disk. chrony is out of scope; use
[chrony_exporter](https://github.com/SuperQ/chrony_exporter) next to it.

**Setting the clockClass** is the job of a separate program in this
repository, [`gnss_ptp_clockclass`](docs/gnss_ptp_clockclass.md): it
announces clockClass 6, 7 or 248 from ts2phc's servo state and is the only
part that writes to ptp4l. It has its own release archives.

Every collector works on its own: if a component is missing or down, its
`*_up` gauge is 0 and the other collectors keep reporting.

## Install

Download an archive from the
[releases](https://github.com/thomas-schreck/gnss_ptp_exporter/releases)
(linux amd64, arm64 and armv7), or build it:

```sh
go install github.com/thomas-schreck/gnss_ptp_exporter@latest
```

Verify a release download:

```sh
sha256sum -c SHA256SUMS --ignore-missing
gh attestation verify gnss_ptp_exporter-<version>.linux-arm64.tar.gz -R thomas-schreck/gnss_ptp_exporter
```

A hardened systemd unit is in
[`contrib/systemd/gnss_ptp_exporter.service`](contrib/systemd/gnss_ptp_exporter.service).
It runs as a dynamic user without capabilities; the `systemd-journal`
group is only needed for the ts2phc collector's journald source.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--web.listen-address` | `:10063` | HTTP listen address |
| `--web.telemetry-path` | `/metrics` | metrics path |
| `--log.level` | `info` | `debug`, `info`, `warn`, `error` |
| `--collector.gpsd` | `true` | gpsd collector |
| `--gpsd.address` | `localhost:2947` | gpsd host:port (also used by `ublox`) |
| `--gpsd.max-age` | `10s` | older TPV/SKY reports count as missing |
| `--collector.ptp4l` | `true` | ptp4l collector |
| `--ptp4l.socket` | `/var/run/ptp4lro` | management socket; the read-only `uds_ro_address` is enough |
| `--ptp4l.domain` | `0` | PTP domain number |
| `--ptp4l.timeout` | `2s` | timeout for all ptp4l queries of one scrape |
| `--collector.ts2phc` | `true` | ts2phc collector |
| `--ts2phc.source` | `journald` | `journald` or `file` |
| `--ts2phc.unit` | `ts2phc.service` | systemd unit (journald) |
| `--ts2phc.journalctl` | `journalctl` | journalctl binary (journald) |
| `--ts2phc.lines` | `100` | journal lines read per scrape (journald) |
| `--ts2phc.file` | | log file ts2phc writes with `-m` (file) |
| `--ts2phc.max-age` | `1m` | `ts2phc_up` is 0 when the newest servo line is older |
| `--collector.ublox` | `false` | u-blox collector |
| `--ublox.device` | | gpsd device path; empty: the first device whose driver is u-blox |
| `--ublox.interval` | `1m` | poll interval |
| `--ublox.timeout` | `5s` | timeout of one poll |
| `--version` | | print the version and exit |

**Port 10063** was free in the Prometheus
[default port allocations](https://github.com/prometheus/prometheus/wiki/Default-port-allocations)
when this exporter was written (gpsd-exporter uses 9978, chrony_exporter
9123).

## Metrics

### gpsd

| Metric | Labels | Meaning |
|---|---|---|
| `gpsd_up` | | 1 if connected and a TPV report arrived within `--gpsd.max-age` |
| `gpsd_fix_mode` | | TPV mode: 0 unknown, 1 no fix, 2 2D, 3 3D; 0 while `gpsd_up` is 0 |
| `gpsd_satellites_visible` | | satellites in the latest SKY report |
| `gpsd_satellites_used` | | satellites used in the solution |
| `gpsd_satellites_visible_by_gnss` | `gnss` | by constellation, plus `gnss="all"` |
| `gpsd_satellites_used_by_gnss` | `gnss` | by constellation, plus `gnss="all"` |
| `gpsd_snr_used_mean_dbhz` | `gnss` | mean C/N0 of used satellites (best signal each), dB-Hz |
| `gpsd_snr_used_max_dbhz` | `gnss` | max C/N0 of used satellites, dB-Hz |
| `gpsd_sky_age_seconds` | | age of the last SKY report with satellites |

`gnss` is one of `GPS`, `SBAS`, `Galileo`, `BeiDou`, `IMES`, `QZSS`,
`GLONASS`, `NavIC`, `other`.

**Satellites, not signals.** Multi-band receivers report one SKY entry per
*signal* (L1 C/A, L2C, E5b, …). gpsd with a ZED-F9T alternates between a
per-signal list (e.g. 69 entries, `uSat` 35) and a per-satellite list (36
entries, `uSat` 22) every second. The exporter ignores `nSat`/`uSat` and
merges entries by `(gnssid, svid)`: a satellite counts once, is used if any
of its signals is used, and contributes its best signal to the SNR
statistics. Both report shapes therefore give the same, stable numbers.

### ptp4l

| Metric | Labels | Meaning |
|---|---|---|
| `ptp4l_up` | | 1 if ptp4l answered all queries |
| `ptp4l_port_state` | `port` | IEEE 1588 port state: 1 INITIALIZING, 2 FAULTY, 3 DISABLED, 4 LISTENING, 5 PRE_MASTER, 6 MASTER, 7 PASSIVE, 8 UNCALIBRATED, 9 SLAVE |
| `ptp4l_grandmaster_role` | | 1 if the grandmaster identity is this clock and a port is MASTER; 0 while `ptp4l_up` is 0 |
| `ptp4l_clock_class` | | clockClass of the local clock (`DEFAULT_DATA_SET`), what a grandmaster announces |
| `ptp4l_clock_accuracy` | | clockAccuracy enumeration (33 = 0x21 = 100 ns, 254 = unknown) |
| `ptp4l_grandmaster_clock_class` | | clockClass of the current grandmaster (`PARENT_DATA_SET`) |
| `ptp4l_steps_removed` | | boundary clocks to the grandmaster |
| `ptp4l_offset_from_master_nanoseconds` | | `CURRENT_DATA_SET` offset (slaves) |
| `ptp4l_mean_path_delay_nanoseconds` | | `CURRENT_DATA_SET` path delay (slaves) |
| `ptp4l_gm_present` | | `TIME_STATUS_NP` gmPresent (synchronized to a remote grandmaster) |
| `ptp4l_clock_info` | `clock_identity`, `grandmaster_identity`, `domain` | always 1 |

Queries per scrape: `DEFAULT_DATA_SET`, `PARENT_DATA_SET`,
`CURRENT_DATA_SET`, `TIME_STATUS_NP` and `PORT_DATA_SET` for each port.
They are all GETs and work on ptp4l's read-only socket
(`uds_ro_address`, default `/var/run/ptp4lro`, mode 0666), which refuses
anything else. `PORT_PROPERTIES_NP` is not used because linuxptp 4.2 answers it
with an error on the read-only socket. The client binds an abstract Unix socket, so it
needs no writable directory and works with ptp4l running under
`PrivateTmp=yes`.

### ts2phc

| Metric | Labels | Meaning |
|---|---|---|
| `ts2phc_up` | | 1 if the newest servo line is not older than `--ts2phc.max-age` |
| `ts2phc_servo_state` | `clock` | 0 unlocked, 1 jump, 2 locked, 3 locked stable |
| `ts2phc_master_offset_nanoseconds` | `clock` | PHC offset from the PPS edge |
| `ts2phc_frequency_ppb` | `clock` | frequency adjustment of the PHC |
| `ts2phc_log_age_seconds` | `clock` | age of the newest servo line |

`clock` is the PHC as ts2phc logs it (`/dev/ptp0`, `eth1`, …). The parsed
line looks like `/dev/ptp0 offset -1 s2 freq +139`. With
`--ts2phc.source=journald` the age comes from the journal timestamp; with
`--ts2phc.source=file` from ts2phc's `[monotonic]` line prefix, or the
file's modification time if lines have none.

### u-blox (`--collector.ublox`)

| Metric | Labels | Meaning |
|---|---|---|
| `gnss_ubx_poll_success` | | 1 if the last poll returned both MON-RF and NAV-STATUS |
| `gnss_ubx_last_poll_timestamp_seconds` | | time of the last poll |
| `gnss_rf_jamming_indicator` | `block` | CW jamming indicator, 0 (none) … 255 (strong) |
| `gnss_rf_jamming_state` | `block` | 0 unknown/disabled, 1 ok, 2 warning, 3 critical |
| `gnss_rf_noise_per_ms` | `block` | noise level |
| `gnss_rf_agc_ratio` | `block` | AGC monitor, agcCnt/8191 |
| `gnss_rf_antenna_status` | `block` | 0 INIT, 1 DONTKNOW, 2 OK, 3 SHORT, 4 OPEN |
| `gnss_rf_antenna_power` | `block` | 0 OFF, 1 ON, 2 DONTKNOW |
| `gnss_spoofing_state` | | NAV-STATUS spoofDetState: 0 unknown/off, 1 none, 2 indicated, 3 multiple |

`block` 0 is usually L1, 1 is L2/L5. The collector opens its own gpsd
connection, asks gpsd for raw data (`?WATCH={"raw":2}`) and sends the two
empty poll frames with `?DEVICE={"hexdata":...}`, as gpsd's `ubxtool` does.
Polls run in the background (default every minute), so a scrape never
waits for the serial line.

### Exporter

`gnss_ptp_exporter_build_info{version,goversion}`, plus the standard Go and
process metrics.

## Prometheus and alerts

[`examples/prometheus.yml`](examples/prometheus.yml) is a scrape job;
[`examples/gnss_ptp_alerts.yml`](examples/gnss_ptp_alerts.yml) has alerting
rules for every component (no fix, servo not locked, PHC offset, ptp4l not
grandmaster, clockClass degraded, spoofing, jamming, antenna faults).

## Limitations

- **Linux only.** The ptp4l client uses an abstract Unix socket, which
  needs the exporter in ptp4l's network namespace (no `PrivateNetwork=yes`).
- **u-blox generation specific.** MON-RF exists from u-blox F9 (protocol 27)
  on; M8 and older receivers answer nothing and `gnss_ubx_poll_success`
  stays 0. Jamming state and antenna supervision only carry information if
  the receiver has them configured (e.g. `CFG-ITFM`, `CFG-HW-ANT_*`); some
  timing firmware (ZED-F9T TIM 2.2x) reports jamming state 0 throughout.
  Spoofing detection needs a firmware that supports it.
- **gpsd must accept `?DEVICE` writes** for the u-blox collector; with
  gpsd's read-only mode (`-b`) the polls fail.
- **ts2phc has no query interface.** The collector depends on ts2phc's servo
  lines, which it logs at info level (to stdout with `-m`, to syslog unless
  `-q`); a log level below 6 (`-l`) hides them. The journald source needs
  read access to the journal.
- **No control.** The exporter does not set the PTP clockClass from the
  ts2phc servo state. That needs a separate, explicitly enabled component.

### Differences from the shell collector it replaces

The metric names follow the `time-stack-collector` textfile script this
exporter grew out of, so dashboards keep working. Semantics changed where
the script was wrong or ambiguous:

- `gpsd_satellites_used`/`_visible` count satellites, not signals (the
  script took the maximum of `uSat`/`nSat`, i.e. signals on a ZED-F9T), and
  `gpsd_satellites_used_by_gnss` counts each satellite once.
- `ptp4l_port_state` uses the IEEE 1588 numbering (MASTER = 6, SLAVE = 9),
  has a `port` label, and is absent when ptp4l does not answer (the script
  used MASTER = 5 and -1 for unknown).
- `ts2phc_*` gauges carry a `clock` label; when there is no servo line only
  `ts2phc_up` (0) is exported instead of placeholder values.
- `ptp4l_up` and `ts2phc_up` mean "answers" / "logs fresh servo lines",
  not "systemd unit active".
- `gpsd_fix_mode` follows gpsd: 1 means *no fix*, 0 unknown.

## Development

```sh
go test ./...                 # unit tests with recorded fixtures
go test -race ./...           # CI runs this
scripts/fuzz.sh 30s           # every fuzz target for 30 s
golangci-lint run             # v2, config in .golangci.yml
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

See [CONTRIBUTING.md](CONTRIBUTING.md) and [SECURITY.md](SECURITY.md).

## License

MIT, see [LICENSE](LICENSE).
