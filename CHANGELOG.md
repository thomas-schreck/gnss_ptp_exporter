# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org).

## [Unreleased]

### Added

#### gnss_ptp_clockclass (new, separate program)

- Daemon that sets the clockClass a ts2phc-disciplined ptp4l grandmaster
  announces: 6 while ts2phc's servo is locked (`s2`, fresh line, small
  offset), 7 with accuracy unknown for a limited holdover, 248 otherwise;
  leap second flags from `chronyc tracking`. Port of the clockClass part
  of a shell script, with the same thresholds as defaults.
- Follows ts2phc's journal, evaluates every second, sends `SET
  GRANDMASTER_SETTINGS_NP` only on a change; `--dry-run` computes and logs
  without sending. Lock memory per ptp4l PID in `/run`.
- systemd `Type=notify` unit with watchdog in
  `contrib/gnss_ptp_clockclass/`; documentation in
  `docs/gnss_ptp_clockclass.md`; own release archives
  `gnss_ptp_clockclass-<version>.linux-<arch>.tar.gz`.
- GRANDMASTER_SETTINGS_NP (linuxptp management ID 0xC001) encoder and
  decoder, tested against responses recorded from linuxptp 4.2, fuzzed.

### Changed

- gnss_ptp_exporter: the ptp4l client binds its abstract socket under the
  program name. The exporter stays read-only; a test checks that it cannot
  reach the helper's SET code. Its release archives keep their name and
  layout.

## [0.1.0] - 2026-11-23

### Added

- gpsd collector: fix mode, satellites visible/used per constellation,
  counted once per satellite across signals (`gnssid`, `svid`), C/N0 mean
  and max of used satellites.
- ptp4l collector over the management socket (GET only, read-only socket
  by default): port state, grandmaster role, clockClass, clockAccuracy,
  grandmaster clockClass, steps removed, offset, path delay, gmPresent.
- ts2phc collector from journald or a log file: servo state, offset,
  frequency, age of the newest servo line, per PHC.
- u-blox collector (opt-in): UBX MON-RF jamming, noise, AGC and antenna
  state per RF block; NAV-STATUS spoofing state; polled through gpsd.
- Example systemd unit, Prometheus scrape config and alerting rules.
