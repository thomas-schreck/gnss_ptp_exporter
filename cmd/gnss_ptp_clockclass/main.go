// Command gnss_ptp_clockclass sets the clockClass a ts2phc-disciplined
// ptp4l grandmaster announces: 6 while ts2phc holds the PHC locked to the
// GNSS PPS, 7 for a limited holdover after the lock was lost, 248
// otherwise. It follows ts2phc's journal, re-evaluates every second and
// sends GRANDMASTER_SETTINGS_NP to ptp4l only when the settings change.
//
// Unlike gnss_ptp_exporter it writes to ptp4l; see docs/gnss_ptp_clockclass.md.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thomas-schreck/gnss_ptp_exporter/cmd/gnss_ptp_clockclass/internal/clockclass"
	"github.com/thomas-schreck/gnss_ptp_exporter/cmd/gnss_ptp_clockclass/internal/gmsettings"
	"github.com/thomas-schreck/gnss_ptp_exporter/cmd/gnss_ptp_clockclass/internal/sdnotify"
	"github.com/thomas-schreck/gnss_ptp_exporter/internal/ptp4l"
)

const name = "gnss_ptp_clockclass"

// logOutput is where logs go (journald via stderr under systemd).
var logOutput io.Writer = os.Stderr

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

type config struct {
	logLevel string
	dryRun   bool

	ptp4lSocket   string
	ptp4lROSocket string
	ptp4lDomain   uint
	ptp4lTimeout  time.Duration
	ptp4lUnit     string

	ts2phcUnit    string
	ts2phcBacklog int
	journalctl    string
	systemctl     string
	chronyc       string
	stateFile     string

	interval     time.Duration
	leapInterval time.Duration
	startupGrace time.Duration

	maxAge     time.Duration
	lockOffset int64
	holdOffset int64
	holdover   time.Duration
	utcOffset  int
	timeSource uint

	showVersion bool
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	var c config
	def := clockclass.Defaults()
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.logLevel, "log.level", "info", "Log level: debug, info, warn, error.")
	fs.BoolVar(&c.dryRun, "dry-run", false, "Compute and log the decisions, never send a SET and never write the state file.")

	fs.StringVar(&c.ptp4lSocket, "ptp4l.socket", "/var/run/ptp4l", "ptp4l's read-write management socket (uds_address), used only for SET.")
	fs.StringVar(&c.ptp4lROSocket, "ptp4l.ro-socket", "/var/run/ptp4lro", "ptp4l's read-only socket (uds_ro_address), used for GET.")
	fs.UintVar(&c.ptp4lDomain, "ptp4l.domain", 0, "PTP domain number of the ptp4l instance.")
	fs.DurationVar(&c.ptp4lTimeout, "ptp4l.timeout", 2*time.Second, "Timeout of one ptp4l request.")
	fs.StringVar(&c.ptp4lUnit, "ptp4l.unit", "ptp4l.service", "systemd unit of ptp4l; its MainPID keys the lock memory.")

	fs.StringVar(&c.ts2phcUnit, "ts2phc.unit", "ts2phc.service", "systemd unit of ts2phc, followed in the journal.")
	fs.IntVar(&c.ts2phcBacklog, "ts2phc.backlog", 50, "Journal lines read when the stream (re)connects.")
	fs.StringVar(&c.journalctl, "journalctl", "journalctl", "journalctl binary.")
	fs.StringVar(&c.systemctl, "systemctl", "systemctl", "systemctl binary.")
	fs.StringVar(&c.chronyc, "chronyc", "chronyc", "chronyc binary (leap second status from `chronyc tracking`).")
	fs.StringVar(&c.stateFile, "state-file", "/run/gnss_ptp_clockclass/state", "Lock memory \"<ptp4l PID> <epoch of last lock>\".")

	fs.DurationVar(&c.interval, "interval", time.Second, "Evaluation period.")
	fs.DurationVar(&c.leapInterval, "leap-interval", time.Minute, "How often chrony's leap status is read.")
	fs.DurationVar(&c.startupGrace, "startup-grace", 10*time.Second, "No SET after start until the first servo line arrives or this time has passed.")

	fs.DurationVar(&c.maxAge, "max-age", def.MaxAge, "A ts2phc servo line counts as fresh while younger (whole seconds).")
	fs.Int64Var(&c.lockOffset, "lock-offset", def.EnterOffsetNs, "Max |offset| in ns to enter class 6.")
	fs.Int64Var(&c.holdOffset, "hold-offset", def.HoldOffsetNs, "Max |offset| in ns to stay in class 6.")
	fs.DurationVar(&c.holdover, "holdover", def.Holdover, "How long class 7 is announced after the last lock.")
	fs.IntVar(&c.utcOffset, "utc-offset", int(def.CurrentUTCOffset), "currentUtcOffset (TAI-UTC) to announce.")
	fs.UintVar(&c.timeSource, "time-source", uint(def.TimeSource), "timeSource to announce (0x20 GPS).")

	fs.BoolVar(&c.showVersion, "version", false, "Print the version and exit.")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	switch {
	case fs.NArg() > 0:
		return c, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	case c.ptp4lDomain > 255:
		return c, errors.New("--ptp4l.domain must be 0..255")
	case c.timeSource > 255:
		return c, errors.New("--time-source must be 0..255")
	case c.utcOffset < -32768 || c.utcOffset > 32767:
		return c, errors.New("--utc-offset out of range")
	case c.interval < 100*time.Millisecond:
		return c, errors.New("--interval must be at least 100ms")
	case c.maxAge < time.Second || c.holdover < 0 || c.leapInterval <= 0 || c.ptp4lTimeout <= 0:
		return c, errors.New("--max-age must be at least 1s, --holdover not negative, --leap-interval and --ptp4l.timeout positive")
	case c.lockOffset < 0 || c.holdOffset < c.lockOffset:
		return c, errors.New("need 0 <= --lock-offset <= --hold-offset")
	case c.ts2phcBacklog < 1:
		return c, errors.New("--ts2phc.backlog must be positive")
	}
	return c, nil
}

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "devel"
}

func main() { os.Exit(realMain(os.Args[1:])) }

func realMain(args []string) int {
	cfg, err := parseFlags(args, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if cfg.showVersion {
		fmt.Println(name, buildVersion())
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func run(ctx context.Context, cfg config) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.logLevel)); err != nil {
		return fmt.Errorf("--log.level: %w", err)
	}
	log := slog.New(slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: level}))

	n := sdnotify.FromEnv()
	opt := clockclass.DefaultOptions()
	opt.Interval, opt.LeapInterval, opt.StartupGrace = cfg.interval, cfg.leapInterval, cfg.startupGrace
	if wd, ok := sdnotify.Watchdog(); ok {
		opt.Watchdog = true
		if cfg.interval > wd/2 {
			log.Warn("--interval is longer than half of WatchdogSec", "interval", cfg.interval, "watchdog", wd)
		}
	}
	env := newEnv(cfg, log)
	env.Notify = func(s string) {
		if err := n.Notify(s); err != nil {
			log.Warn("sd_notify failed", "err", err)
		}
	}
	servo := &clockclass.Servo{}
	go clockclass.Follow(ctx, journalOpener(cfg, log), servo, log, time.Second, 30*time.Second)
	log.Info("starting", "version", buildVersion(), "dry_run", cfg.dryRun)
	return clockclass.NewDaemon(daemonConfig(cfg), opt, env, servo).Run(ctx)
}

func daemonConfig(cfg config) clockclass.Config {
	c := clockclass.Defaults()
	c.MaxAge, c.Holdover = cfg.maxAge, cfg.holdover
	c.EnterOffsetNs, c.HoldOffsetNs = cfg.lockOffset, cfg.holdOffset
	c.CurrentUTCOffset = int16(cfg.utcOffset) //nolint:gosec // range checked in parseFlags
	c.TimeSource = uint8(cfg.timeSource)      //nolint:gosec // range checked in parseFlags
	return c
}

// command runs a fixed binary without a shell, with C locale and a
// timeout.
func command(ctx context.Context, bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // fixed binary from a flag, no shell
	cmd.Env = []string{"LC_ALL=C", "SYSTEMD_COLORS=0", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	return cmd.Output()
}

func newEnv(cfg config, log *slog.Logger) clockclass.Env {
	domain := uint8(cfg.ptp4lDomain) //nolint:gosec // range checked in parseFlags
	env := clockclass.Env{
		Now: time.Now,
		Log: log,
		Units: func(ctx context.Context) (clockclass.Units, error) {
			out, err := command(ctx, cfg.systemctl, "show", "--property=MainPID", "--property=ActiveState",
				"--", cfg.ptp4lUnit, cfg.ts2phcUnit)
			if err != nil {
				return clockclass.Units{}, fmt.Errorf("systemctl show: %w", err)
			}
			return parseUnits(out)
		},
		Get: func(context.Context) (gmsettings.Settings, error) {
			c, err := ptp4l.Dial(cfg.ptp4lROSocket, domain, time.Now().Add(cfg.ptp4lTimeout))
			if err != nil {
				return gmsettings.Settings{}, err
			}
			defer func() { _ = c.Close() }()
			return gmsettings.Get(c)
		},
		Leap: func(ctx context.Context) (clockclass.Leap, error) {
			out, err := command(ctx, cfg.chronyc, "tracking")
			if err != nil {
				return clockclass.Leap{}, fmt.Errorf("chronyc tracking: %w", err)
			}
			return clockclass.ParseChronyTracking(out), nil
		},
		LoadState: func() (clockclass.State, error) {
			b, err := os.ReadFile(cfg.stateFile) //nolint:gosec // path from a flag set by the administrator
			if err != nil {
				return clockclass.State{}, err
			}
			return clockclass.ParseState(b), nil
		},
	}
	if cfg.dryRun {
		return env
	}
	env.Set = func(_ context.Context, s gmsettings.Settings) (gmsettings.Settings, error) {
		return gmsettings.Set(cfg.ptp4lSocket, domain, s, cfg.ptp4lTimeout)
	}
	env.SaveState = func(s clockclass.State) error { return writeAtomic(cfg.stateFile, s.Marshal()) }
	return env
}

// parseUnits reads `systemctl show -p MainPID -p ActiveState ptp4l ts2phc`:
// one block per unit, in argument order, separated by an empty line.
func parseUnits(out []byte) (clockclass.Units, error) {
	blocks := bytes.Split(bytes.TrimSpace(out), []byte("\n\n"))
	if len(blocks) != 2 {
		return clockclass.Units{}, fmt.Errorf("systemctl show: %d unit blocks, want 2", len(blocks))
	}
	props := func(b []byte) map[string]string {
		m := map[string]string{}
		for _, l := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				m[k] = v
			}
		}
		return m
	}
	p, t := props(blocks[0]), props(blocks[1])
	pid, err := strconv.Atoi(p["MainPID"])
	if err != nil {
		return clockclass.Units{}, fmt.Errorf("systemctl show: MainPID %q", p["MainPID"])
	}
	if p["ActiveState"] != "active" {
		pid = 0
	}
	return clockclass.Units{PTP4LPID: pid, Ts2phcActive: t["ActiveState"] == "active"}, nil
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }() //nolint:gosec // temp file next to the configured state file
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path) //nolint:gosec // path from a flag set by the administrator
}

// journalOpener follows ts2phc's journal with journalctl, without shell.
func journalOpener(cfg config, log *slog.Logger) clockclass.Opener {
	return func(ctx context.Context) (io.ReadCloser, func() error, error) {
		cmd := exec.CommandContext(ctx, cfg.journalctl, //nolint:gosec // fixed binary from a flag, no shell
			"--unit="+cfg.ts2phcUnit, "--follow", "--lines="+strconv.Itoa(cfg.ts2phcBacklog),
			"--output=short-unix", "--no-pager", "--quiet")
		cmd.Env = []string{"LC_ALL=C", "SYSTEMD_COLORS=0"}
		cmd.Stderr = logWriter{log}
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		return out, cmd.Wait, nil
	}
}

// logWriter logs journalctl's error output.
type logWriter struct{ log *slog.Logger }

func (w logWriter) Write(b []byte) (int, error) {
	if s := strings.TrimSpace(string(b)); s != "" {
		w.log.Warn("journalctl", "stderr", s)
	}
	return len(b), nil
}
