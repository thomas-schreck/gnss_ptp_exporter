package clockclass

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/thomas-schreck/gnss_ptp_exporter/cmd/gnss_ptp_clockclass/internal/gmsettings"
	"github.com/thomas-schreck/gnss_ptp_exporter/internal/ts2phc"
)

// Servo holds the newest ts2phc servo line seen on the journal stream.
type Servo struct {
	mu   sync.Mutex
	s    ts2phc.Sample
	have bool
}

// Feed takes one journal line; servo lines (s0-s2 with frequency, as in
// NewestServoLine) replace the stored sample.
func (v *Servo) Feed(line string) {
	s, ok := servoLine(line)
	if !ok {
		return
	}
	v.mu.Lock()
	v.s, v.have = s, true
	v.mu.Unlock()
}

// Latest returns the newest servo sample.
func (v *Servo) Latest() (ts2phc.Sample, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.s, v.have
}

// Opener starts a journal stream (journalctl --follow) and returns its
// output and a function that waits for the process to end.
type Opener func(ctx context.Context) (io.ReadCloser, func() error, error)

// Follow feeds the stream from open into v until ctx ends. When the stream
// breaks it reconnects after a backoff that starts at minBackoff, doubles
// up to maxBackoff and starts over after a stream that lasted a minute.
// The stored sample keeps its journal timestamp, so a broken stream makes
// it stale, never fresh.
func Follow(ctx context.Context, open Opener, v *Servo, log *slog.Logger, minBackoff, maxBackoff time.Duration) {
	backoff := minBackoff
	for ctx.Err() == nil {
		start := time.Now()
		err := follow(ctx, open, v)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = minBackoff
		}
		log.Warn("ts2phc journal stream ended, reconnecting", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

func follow(ctx context.Context, open Opener, v *Servo) error {
	r, wait, err := open(ctx)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 64<<10)
	for sc.Scan() {
		v.Feed(sc.Text())
	}
	serr := sc.Err()
	_ = r.Close()
	werr := wait()
	if serr != nil {
		return serr
	}
	if werr != nil {
		return werr
	}
	return errors.New("end of stream")
}

// Units is the systemd state of ptp4l and ts2phc.
type Units struct {
	PTP4LPID     int // MainPID, 0 if ptp4l is not running
	Ts2phcActive bool
}

// Env connects the daemon to the system; tests replace it.
type Env struct {
	Now   func() time.Time
	Units func(ctx context.Context) (Units, error)
	// Get reads ptp4l's GRANDMASTER_SETTINGS_NP (read-only socket).
	Get func(ctx context.Context) (gmsettings.Settings, error)
	// Set writes them (read-write socket); nil in dry-run mode.
	Set  func(ctx context.Context, s gmsettings.Settings) (gmsettings.Settings, error)
	Leap func(ctx context.Context) (Leap, error)
	// LoadState reads the lock memory (an fs.ErrNotExist error if there
	// is none yet); SaveState is nil in dry-run mode.
	LoadState func() (State, error)
	SaveState func(State) error
	// Notify sends an sd_notify message; may be nil.
	Notify func(state string)
	Log    *slog.Logger
}

// Options are the daemon's timing parameters.
type Options struct {
	Interval      time.Duration // evaluation period
	LeapInterval  time.Duration // how often chrony is asked
	StateInterval time.Duration // how often a lasting lock is written to the state file
	StartupGrace  time.Duration // no SET before the first servo line or this long after start
	SetRetry      time.Duration // pause after a failed SET
	Watchdog      bool          // send WATCHDOG=1 after every evaluation
}

// DefaultOptions returns the defaults of the command.
func DefaultOptions() Options {
	return Options{Interval: time.Second, LeapInterval: time.Minute, StateInterval: 30 * time.Second,
		StartupGrace: 10 * time.Second, SetRetry: 10 * time.Second}
}

// Daemon re-evaluates the clockClass every Interval.
type Daemon struct {
	cfg   Config
	opt   Options
	env   Env
	servo *Servo

	start      time.Time
	pid        int
	state      State
	savedState State
	savedAt    time.Time
	leap       Leap
	leapAt     time.Time
	graceOver  bool
	nextSet    time.Time
	last       string // last logged decision
	lastLogged time.Time
	warned     map[string]warning
}

type warning struct {
	msg string
	at  time.Time
}

// NewDaemon returns a daemon reading servo lines from servo.
func NewDaemon(cfg Config, opt Options, env Env, servo *Servo) *Daemon {
	return &Daemon{cfg: cfg, opt: opt, env: env, servo: servo, warned: map[string]warning{}}
}

// DryRun reports whether the daemon only computes.
func (d *Daemon) DryRun() bool { return d.env.Set == nil }

// Run evaluates until ctx ends. It reports READY=1 after the first
// evaluation and, with Options.Watchdog, WATCHDOG=1 after every one, so
// systemd's watchdog fires when the loop hangs. It never changes ptp4l on
// exit.
func (d *Daemon) Run(ctx context.Context) error {
	d.begin()
	t := time.NewTicker(d.opt.Interval)
	defer t.Stop()
	for first := true; ; first = false {
		d.Tick(ctx)
		if ctx.Err() != nil {
			d.notify("STOPPING=1")
			return nil
		}
		if first {
			d.notify("READY=1")
		}
		if d.opt.Watchdog {
			d.notify("WATCHDOG=1")
		}
		select {
		case <-ctx.Done():
			d.notify("STOPPING=1")
			return nil
		case <-t.C:
		}
	}
}

// begin loads the lock memory and starts the startup grace period.
func (d *Daemon) begin() {
	d.start = d.env.Now()
	st, err := d.env.LoadState()
	switch {
	case err == nil:
		d.state, d.savedState = st, st
		d.env.Log.Info("lock memory loaded", "pid", st.PID, "last_lock", st.LockEpoch)
	case !errors.Is(err, fs.ErrNotExist): // no file after boot is normal
		d.warn("state", "reading state file failed", err)
	}
}

func (d *Daemon) notify(s string) {
	if d.env.Notify != nil {
		d.env.Notify(s)
	}
}

// warn logs a warning, but the same message for key at most once a minute.
func (d *Daemon) warn(key, msg string, err error) {
	now := d.env.Now()
	text := msg + ": " + err.Error()
	if w, ok := d.warned[key]; ok && w.msg == text && now.Sub(w.at) < time.Minute {
		return
	}
	d.warned[key] = warning{msg: text, at: now}
	d.env.Log.Warn(msg, "err", err)
}

// Tick runs one evaluation.
func (d *Daemon) Tick(ctx context.Context) {
	now := d.env.Now()
	u, err := d.env.Units(ctx)
	if err != nil {
		d.warn("units", "reading unit state failed", err)
		return
	}
	delete(d.warned, "units")
	if u.PTP4LPID == 0 {
		if d.pid != 0 {
			d.env.Log.Info("ptp4l stopped")
		}
		d.pid = 0
		return
	}
	if u.PTP4LPID != d.pid {
		d.env.Log.Info("ptp4l running", "pid", u.PTP4LPID, "lock_memory_pid", d.state.PID)
		d.pid = u.PTP4LPID
	}

	// GET every evaluation, not only after a ptp4l restart: ptp4l's own
	// ExecStartPost may reset the settings seconds after the start.
	cur, err := d.env.Get(ctx)
	if err != nil {
		d.warn("get", "reading GRANDMASTER_SETTINGS_NP failed", err)
	} else {
		delete(d.warned, "get")
	}
	if d.leapAt.IsZero() || now.Sub(d.leapAt) >= d.opt.LeapInterval {
		l, lerr := d.env.Leap(ctx)
		if lerr != nil {
			// As the script: no answer from chrony means no leap second.
			d.warn("leap", "reading chrony leap status failed", lerr)
		}
		d.leap, d.leapAt = l, now
	}
	sample, have := d.servo.Latest()
	dec := Decide(d.cfg, Input{
		Now: now, PTP4LPID: u.PTP4LPID, Ts2phcActive: u.Ts2phcActive,
		Sample: sample, HaveSample: have,
		Current: cur, HaveCurrent: err == nil,
		State: d.state, Leap: d.leap,
	})
	if dec.Locked {
		d.state = dec.NewState
		d.saveState(now)
	}
	if !d.graceOver && (have || now.Sub(d.start) >= d.opt.StartupGrace) {
		d.graceOver = true
	}
	d.logDecision(now, dec, cur, err == nil)
	if !dec.Set || !d.graceOver || d.DryRun() || now.Before(d.nextSet) {
		return
	}
	got, err := d.env.Set(ctx, dec.Want)
	if err != nil {
		d.nextSet = now.Add(d.opt.SetRetry)
		d.env.Log.Warn("SET GRANDMASTER_SETTINGS_NP failed", "from", cur.ClockClass, "to", dec.Class, "err", err)
		return
	}
	d.env.Log.Info(fmt.Sprintf("PTP clockClass %d -> %d", cur.ClockClass, dec.Class), decisionAttrs(dec)...)
	if got != dec.Want {
		d.env.Log.Warn("ptp4l answered the SET with other settings", "want", dec.Want.PMCArgs(), "got", got.PMCArgs())
	}
}

func (d *Daemon) saveState(now time.Time) {
	if d.env.SaveState == nil {
		return
	}
	if d.state.PID == d.savedState.PID && now.Sub(d.savedAt) < d.opt.StateInterval {
		return
	}
	if err := d.env.SaveState(d.state); err != nil {
		d.warn("save", "writing state file failed", err)
		return
	}
	delete(d.warned, "save")
	d.savedState, d.savedAt = d.state, now
}

func decisionAttrs(dec Decision) []any {
	return []any{"accuracy", fmt.Sprintf("0x%02X", dec.Want.ClockAccuracy), "ts2phc_servo", dec.Servo,
		"offset_ns", dec.OffsetNs, "age_s", dec.AgeS, "leap61", dec.Want.Leap61, "leap59", dec.Want.Leap59}
}

// logDecision logs at debug every evaluation; in dry-run mode at info on
// every change and once a minute.
func (d *Daemon) logDecision(now time.Time, dec Decision, cur gmsettings.Settings, haveCur bool) {
	current := "unknown"
	if haveCur {
		current = strconv.Itoa(int(cur.ClockClass))
	}
	key := fmt.Sprintf("%d/%t/%t/%s", dec.Class, dec.Set && d.graceOver, dec.Locked, current)
	attrs := append([]any{"current", current, "want", dec.Class, "set", dec.Set && d.graceOver,
		"locked", dec.Locked, "limit_ns", dec.LimitNs}, decisionAttrs(dec)...)
	if !d.DryRun() || (key == d.last && now.Sub(d.lastLogged) < time.Minute) {
		d.env.Log.Debug("decision", attrs...)
		return
	}
	d.last, d.lastLogged = key, now
	msg := "decision (dry run)"
	if dec.Set && d.graceOver {
		msg = "would SET GRANDMASTER_SETTINGS_NP " + dec.Want.PMCArgs() + " (dry run)"
	}
	d.env.Log.Info(msg, attrs...)
}
