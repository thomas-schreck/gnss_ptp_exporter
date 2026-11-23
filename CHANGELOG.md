# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org).

## [Unreleased]

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
