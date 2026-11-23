# gnss_ptp_clockclass

A small daemon for a ts2phc-disciplined **ptp4l grandmaster**: it sets the
clockClass ptp4l announces from the state of ts2phc's servo, so that PTP
clients only trust the grandmaster while its PHC is really locked to the
GNSS PPS.

It is a separate program from
[gnss_ptp_exporter](../README.md) and ships in its own release archive.
The exporter only measures; this tool **writes to ptp4l** (one management
TLV, see [Permissions](#permissions)).

## Why

ptp4l announces whatever clockClass its configuration or the last
`pmc SET GRANDMASTER_SETTINGS_NP` says. A static clockClass 6 ("locked to
a primary reference") is wrong while the PHC is still settling after a
boot, when the receiver loses its fix, or when ts2phc stops. With ts2phc
there is no built-in coupling between the servo state and the announced
quality; this tool provides it.

## Logic

Every second (`--interval`) the tool evaluates:

| Class | clockAccuracy | time-/frequencyTraceable | Condition |
|---|---|---|---|
| **6** | `0x21` (100 ns) | 1 | ts2phc unit active, newest servo line `s2`, younger than `--max-age` (75 s), \|offset\| ≤ `--lock-offset` (100 ns); once in 6, \|offset\| ≤ `--hold-offset` (1000 ns) keeps it |
| **7** | `0xFE` (unknown) | 1 | not locked, but locked before since ptp4l (same PID) started, at most `--holdover` (15 min) ago |
| **248** | `0xFE` | 0 | otherwise, including before the first lock after a ptp4l start |

The higher hold threshold avoids flapping on a single noisy sample; the
lower entry threshold keeps 6 away from a PHC that reports `s2` within
seconds of a start while still several µs off. Holdover announces accuracy
"unknown" because a receiver without fix may send no PPS at all and the
PHC then free-runs with unknown drift.

Inputs:

- **ts2phc servo** — the tool follows the unit's journal
  (`journalctl --unit=ts2phc.service --follow --output=short-unix`, started
  directly, no shell) and keeps the newest line like
  `/dev/ptp0 offset -4 s2 freq +143` (states `s0`–`s2` with a frequency;
  the age comes from the journal timestamp). If the stream breaks it
  reconnects with backoff (1 s … 30 s); without new lines the last one
  simply ages out, so a broken stream leads to holdover or 248, never to 6.
- **Unit state** — `systemctl show --property=MainPID --property=ActiveState
  ptp4l.service ts2phc.service`. Nothing is sent while ptp4l is not active.
- **Current settings** — `GET GRANDMASTER_SETTINGS_NP` on ptp4l's
  **read-only** socket every evaluation. This also catches a reset by
  ptp4l's own start-up (for example an `ExecStartPost` that sets 248).
- **Leap second** — `chronyc tracking`, `Leap status: Insert second` sets
  leap61, `Delete second` leap59; read every `--leap-interval` (60 s).
  If chronyc fails, both flags are 0.

A SET is sent only when any announced field differs from ptp4l's current
value. Every SET writes the complete settings:

```
clockClass <6|7|248> clockAccuracy <0x21|0xFE> offsetScaledLogVariance 0xffff
currentUtcOffset 37 leap61 <0|1> leap59 <0|1> currentUtcOffsetValid 1
ptpTimescale 1 timeTraceable <1|0> frequencyTraceable <1|0> timeSource 0x20
```

### Lock memory

The time of the last locked evaluation is kept in memory and in the state
file (`--state-file`, default `/run/gnss_ptp_clockclass/state`, one line
`<ptp4l PID> <Unix time of last lock>`, written at most every 30 s while
locked). A restart of the tool continues the holdover where it stopped; a
new ptp4l PID makes the old lock irrelevant. The format is the one of the
`/run/time-stack-ptp-class` file of the shell script this tool replaced,
so `--state-file=/run/time-stack-ptp-class` takes over its memory.

### Start and stop

After a start the tool sends no SET until the journal delivered the first
servo line or `--startup-grace` (10 s) passed, so a restart during an
update never flips a locked grandmaster to holdover. Stopping changes
nothing in ptp4l: clients keep the current class.

## Dry run

`--dry-run` gathers the same inputs and logs every changed decision
(`would SET GRANDMASTER_SETTINGS_NP …`), but never opens the read-write
socket and never writes the state file (it still reads it). It needs only
read access: the read-only socket (mode 0666), the journal (group
`systemd-journal` or `adm`) and `chronyc`.

```sh
gnss_ptp_clockclass --dry-run --state-file=/run/time-stack-ptp-class
```

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--dry-run` | `false` | compute and log, never SET, never write the state file |
| `--log.level` | `info` | `debug` logs every evaluation |
| `--ptp4l.socket` | `/var/run/ptp4l` | read-write socket (`uds_address`), SET only |
| `--ptp4l.ro-socket` | `/var/run/ptp4lro` | read-only socket (`uds_ro_address`), GET |
| `--ptp4l.domain` | `0` | PTP domain number |
| `--ptp4l.timeout` | `2s` | timeout of one request |
| `--ptp4l.unit` | `ptp4l.service` | unit whose MainPID keys the lock memory |
| `--ts2phc.unit` | `ts2phc.service` | unit whose journal is followed |
| `--ts2phc.backlog` | `50` | journal lines read when the stream (re)connects |
| `--journalctl`, `--systemctl`, `--chronyc` | from `PATH` | binaries |
| `--state-file` | `/run/gnss_ptp_clockclass/state` | lock memory |
| `--interval` | `1s` | evaluation period |
| `--leap-interval` | `1m` | chrony query period |
| `--startup-grace` | `10s` | no SET before the first servo line or this long after start |
| `--max-age` | `75s` | a servo line counts while younger (whole seconds) |
| `--lock-offset` | `100` | ns, enter class 6 |
| `--hold-offset` | `1000` | ns, stay in class 6 |
| `--holdover` | `15m` | class 7 after the last lock |
| `--utc-offset` | `37` | currentUtcOffset (TAI−UTC) |
| `--time-source` | `0x20` | timeSource (GPS) |
| `--version` | | print the version and exit |

The defaults are those of the shell script the tool replaced (it ran every
30 s, hence the 75 s line age). With the one-second loop a much smaller
`--max-age` (for example `5s`) reacts faster to a lost PPS.

## systemd

[`contrib/gnss_ptp_clockclass/gnss_ptp_clockclass.service`](../contrib/gnss_ptp_clockclass/gnss_ptp_clockclass.service)
runs it as `Type=notify`: `READY=1` after the first evaluation,
`WATCHDOG=1` after every evaluation (`WatchdogSec=10s`), so systemd
restarts a hanging loop; `Restart=always`. A SIGTERM ends the process
cleanly without touching ptp4l.

Install from a release archive (or `go install
github.com/thomas-schreck/gnss_ptp_exporter/cmd/gnss_ptp_clockclass@latest`):

```sh
install -m 0755 gnss_ptp_clockclass /usr/local/bin/
install -m 0644 contrib/gnss_ptp_clockclass/gnss_ptp_clockclass.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now gnss_ptp_clockclass.service
```

Run only one instance that sets the clockClass: stop any other script or
timer doing the same before enabling the service.

## Permissions

The tool sends exactly one kind of write request: `SET
GRANDMASTER_SETTINGS_NP` (linuxptp management ID `0xC001`) on ptp4l's
read-write socket. ptp4l refuses SET on the read-only socket, and linuxptp
creates the read-write socket as `root:root` mode `0660`, so the service
runs as **root without capabilities** (`CapabilityBoundingSet=`; root
owns the socket and the journal, no capability is needed). The read-write
socket would allow any other management SET or COMMAND too; the code
builds no other (see [SECURITY.md](../SECURITY.md)).

GRANDMASTER_SETTINGS_NP is linuxptp-specific and not in
[facebook/time](https://github.com/facebook/time); this repository encodes
it itself after `struct grandmaster_settings_np` in linuxptp's `tlv.h`,
tested against responses recorded from linuxptp 4.2.

## Logs

Info: start, ptp4l (re)start, and every change as
`PTP clockClass 248 -> 6 accuracy=0x21 ts2phc_servo=2 offset_ns=3 age_s=0 …`.
Warnings (rate-limited): failed GET, SET, chrony, unit or journal reads.
`--log.level=debug` logs every evaluation.
