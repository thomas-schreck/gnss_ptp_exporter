package clockclass

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thomas-schreck/gnss_ptp_exporter/cmd/gnss_ptp_clockclass/internal/gmsettings"
)

// sim is a simulated host: systemd units, ptp4l's settings, chrony and the
// state file.
type sim struct {
	t      *testing.T
	now    time.Time
	units  Units
	cur    gmsettings.Settings // ptp4l's current settings
	getErr error
	setErr error
	sets   []gmsettings.Settings
	leap   Leap
	state  *State
	saves  int
	logs   bytes.Buffer
	servo  Servo
	d      *Daemon
}

const t0 = 1795461000

func newSim(t *testing.T, dry bool, state *State) *sim {
	s := &sim{t: t, now: time.Unix(t0, 0), units: Units{PTP4LPID: 1013, Ts2phcActive: true},
		cur: settings(ClassFree), state: state}
	env := Env{
		Now:   func() time.Time { return s.now },
		Units: func(context.Context) (Units, error) { return s.units, nil },
		Get: func(context.Context) (gmsettings.Settings, error) {
			return s.cur, s.getErr
		},
		Leap: func(context.Context) (Leap, error) { return s.leap, nil },
		LoadState: func() (State, error) {
			if s.state == nil {
				return State{}, fs.ErrNotExist
			}
			return *s.state, nil
		},
		Log: slog.New(slog.NewTextHandler(&s.logs, nil)),
	}
	if !dry {
		env.Set = func(_ context.Context, w gmsettings.Settings) (gmsettings.Settings, error) {
			if s.setErr != nil {
				return gmsettings.Settings{}, s.setErr
			}
			s.sets = append(s.sets, w)
			s.cur = w
			return w, nil
		}
		env.SaveState = func(st State) error {
			s.state = &st
			s.saves++
			return nil
		}
	}
	s.d = NewDaemon(Defaults(), DefaultOptions(), env, &s.servo)
	s.d.begin()
	return s
}

// line feeds a ts2phc servo line stamped with the current time.
func (s *sim) line(offset int64, servo int) {
	s.servo.Feed(fmt.Sprintf("%d.000040 timeserver ts2phc[4327]: [1.839] /dev/ptp0 offset %d s%d freq +140",
		s.now.Unix(), offset, servo))
}

// step advances the clock by one second, optionally feeding a line, and
// evaluates.
func (s *sim) step(feed func()) {
	s.now = s.now.Add(time.Second)
	if feed != nil {
		feed()
	}
	s.d.Tick(context.Background())
}

func (s *sim) class() uint8 { return s.cur.ClockClass }

func (s *sim) expectSets(n int, class uint8) {
	s.t.Helper()
	if len(s.sets) != n {
		s.t.Fatalf("t+%ds: %d SETs, want %d (%+v)\n%s", s.now.Unix()-t0, len(s.sets), n, s.sets, s.logs.String())
	}
	if s.class() != class {
		s.t.Fatalf("t+%ds: class %d, want %d", s.now.Unix()-t0, s.class(), class)
	}
	if n > 0 && s.sets[n-1] != settings(s.sets[n-1].ClockClass) {
		s.t.Fatalf("SET %s", s.sets[n-1].PMCArgs())
	}
}

func TestDaemonSequence(t *testing.T) {
	s := newSim(t, false, nil)
	locked := func() { s.line(3, 2) }

	// ptp4l started with 248; ts2phc reports s2 while still settling.
	s.step(nil)
	s.step(func() { s.line(-8291, 2) })
	s.expectSets(0, 248)

	// Lock: 6 at once.
	s.step(locked)
	s.expectSets(1, 6)
	lockedAt := s.now.Unix()
	if s.state == nil || *s.state != (State{"1013", lockedAt}) {
		t.Fatalf("state %+v", s.state)
	}
	for range 60 {
		s.step(locked)
	}
	s.expectSets(1, 6)
	// The state file is written every 30 s while locked, not every second.
	if s.saves != 3 || s.state.LockEpoch != lockedAt+60 {
		t.Fatalf("%d saves, state %+v", s.saves, s.state)
	}

	// The journal stream breaks: no more lines. The last line ages out
	// after 75 s, then holdover 7.
	lastLine := s.now.Unix()
	for s.now.Unix() < lastLine+74 {
		s.step(nil)
	}
	s.expectSets(1, 6)
	s.step(nil)
	s.expectSets(2, 7)
	lastLock := lastLine + 74

	// Holdover ends 900 s after the last locked evaluation.
	for s.now.Unix() < lastLock+899 {
		s.step(nil)
	}
	s.expectSets(2, 7)
	s.step(nil)
	s.expectSets(3, 248)

	// Lock again.
	s.step(locked)
	s.expectSets(4, 6)

	// ptp4l restarts: new PID, settings back to 248. The old lock does not
	// count for the new PID, so no holdover while ts2phc is in s1.
	s.units.PTP4LPID = 2000
	s.cur = settings(248)
	s.step(func() { s.line(-112, 1) })
	s.expectSets(4, 248)
	s.step(locked)
	s.expectSets(5, 6)
	if s.state.PID != "2000" {
		t.Errorf("state %+v", s.state)
	}
	// ptp4l's ExecStartPost resets to 248 after our SET: the next GET sees
	// it and the helper sets 6 again.
	s.cur = settings(248)
	s.step(locked)
	s.expectSets(6, 6)

	// Lock lost with the new PID: holdover 7.
	s.step(func() { s.line(5000, 2) })
	s.expectSets(7, 7)

	// ptp4l stops: nothing is sent.
	s.units.PTP4LPID = 0
	s.step(nil)
	s.expectSets(7, 7)
}

func TestDaemonLeapAndUnchanged(t *testing.T) {
	s := newSim(t, false, nil)
	s.step(func() { s.line(1, 2) })
	s.expectSets(1, 6)
	s.leap = Leap{Leap61: true}
	// chrony is read once a minute.
	for range 59 {
		s.step(func() { s.line(1, 2) })
	}
	s.expectSets(1, 6)
	s.step(func() { s.line(1, 2) })
	if len(s.sets) != 2 || !s.sets[1].Leap61 || s.sets[1].ClockClass != 6 {
		t.Fatalf("%+v", s.sets)
	}
}

func TestDaemonRestartContinues(t *testing.T) {
	// The helper restarts (update) while ptp4l announces 6; its state file
	// says the lock was 10 s ago. Within the startup grace no SET happens
	// before the journal delivers, then the lock is confirmed: no change.
	s := newSim(t, false, &State{"1013", t0 - 10})
	s.cur = settings(6)
	for range 3 {
		s.step(nil)
	}
	s.expectSets(0, 6)
	s.step(func() { s.line(2, 2) })
	s.expectSets(0, 6)

	// Without any servo line the grace ends and holdover applies.
	s = newSim(t, false, &State{"1013", t0 - 10})
	s.cur = settings(6)
	for range 9 {
		s.step(nil)
	}
	s.expectSets(0, 6)
	s.step(nil)
	s.expectSets(1, 7)
}

func TestDaemonDryRun(t *testing.T) {
	s := newSim(t, true, &State{"1013", t0 - 10})
	s.step(func() { s.line(3, 2) })
	for range 5 {
		s.step(func() { s.line(3, 2) })
	}
	if s.saves != 0 || len(s.sets) != 0 || s.cur.ClockClass != 248 {
		t.Fatalf("dry run changed something: %d saves, %+v", s.saves, s.sets)
	}
	logs := s.logs.String()
	if strings.Count(logs, "would SET GRANDMASTER_SETTINGS_NP clockClass 6 clockAccuracy 0x21") != 1 {
		t.Errorf("logs:\n%s", logs)
	}
	// Decisions are logged on change only.
	s.step(func() { s.line(5000, 2) })
	if !strings.Contains(s.logs.String(), "would SET GRANDMASTER_SETTINGS_NP clockClass 7 clockAccuracy 0xFE") {
		t.Errorf("logs:\n%s", s.logs.String())
	}
}

func TestDaemonErrors(t *testing.T) {
	s := newSim(t, false, nil)
	s.getErr = errors.New("ptp4l does not answer")
	s.step(func() { s.line(3, 2) })
	s.step(func() { s.line(3, 2) })
	s.expectSets(0, 248)
	if strings.Count(s.logs.String(), "ptp4l does not answer") != 1 {
		t.Errorf("warning not rate-limited:\n%s", s.logs.String())
	}
	s.getErr = nil
	s.setErr = errors.New("permission denied")
	s.step(func() { s.line(3, 2) })
	s.expectSets(0, 248)
	// The next attempt waits SetRetry (10 s).
	for range 9 {
		s.step(func() { s.line(3, 2) })
	}
	if strings.Count(s.logs.String(), "permission denied") != 1 {
		t.Errorf("retried too early:\n%s", s.logs.String())
	}
	s.setErr = nil
	s.step(func() { s.line(3, 2) })
	s.expectSets(1, 6)
}

type notes struct {
	mu sync.Mutex
	l  []string
}

func (n *notes) add(s string) { n.mu.Lock(); n.l = append(n.l, s); n.mu.Unlock() }
func (n *notes) count(s string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, x := range n.l {
		if x == s {
			c++
		}
	}
	return c
}

func runDaemon(t *testing.T, units func(context.Context) (Units, error)) *notes {
	t.Helper()
	n := &notes{}
	env := Env{
		Now:       time.Now,
		Units:     units,
		Get:       func(context.Context) (gmsettings.Settings, error) { return settings(248), nil },
		Leap:      func(context.Context) (Leap, error) { return Leap{}, nil },
		LoadState: func() (State, error) { return State{}, fs.ErrNotExist },
		Notify:    n.add,
		Log:       slog.New(slog.DiscardHandler),
	}
	opt := DefaultOptions()
	opt.Interval, opt.Watchdog = 10*time.Millisecond, true
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := NewDaemon(Defaults(), opt, env, &Servo{}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWatchdogOnlyWhileLoopRuns(t *testing.T) {
	n := runDaemon(t, func(context.Context) (Units, error) { return Units{PTP4LPID: 1, Ts2phcActive: true}, nil })
	if n.count("READY=1") != 1 || n.count("WATCHDOG=1") < 5 || n.count("STOPPING=1") != 1 {
		t.Errorf("notifications %v", n.l)
	}

	// An evaluation that hangs (here: systemctl never answers) stops the
	// pings, so systemd's watchdog restarts the service.
	n = runDaemon(t, func(ctx context.Context) (Units, error) {
		<-ctx.Done()
		return Units{}, ctx.Err()
	})
	if n.count("READY=1") != 0 || n.count("WATCHDOG=1") != 0 {
		t.Errorf("notifications from a hanging loop: %v", n.l)
	}
}

// pipeOpener hands out io.Pipes; the test writes lines and breaks them.
type pipeOpener struct {
	opens atomic.Int32
	w     chan *io.PipeWriter
}

func (p *pipeOpener) open(ctx context.Context) (io.ReadCloser, func() error, error) {
	p.opens.Add(1)
	r, w := io.Pipe()
	go func() { <-ctx.Done(); _ = w.Close() }()
	p.w <- w
	return r, func() error { return nil }, nil
}

func TestFollowReconnects(t *testing.T) {
	p := &pipeOpener{w: make(chan *io.PipeWriter, 4)}
	v := &Servo{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Follow(ctx, p.open, v, slog.New(slog.DiscardHandler), 10*time.Millisecond, 20*time.Millisecond)
		close(done)
	}()
	w := <-p.w
	fmt.Fprintln(w, "1795461779.000045 timeserver ts2phc[4327]: [1.8] /dev/ptp0 offset 2 s2 freq +146")
	_ = w.CloseWithError(errors.New("journalctl died"))
	// The sample survives the broken stream with its old timestamp.
	w = <-p.w
	if s, ok := v.Latest(); !ok || s.Time.Unix() != 1795461779 {
		t.Fatalf("%+v %v", s, ok)
	}
	fmt.Fprintln(w, "1795461790.000045 timeserver ts2phc[4327]: [1.8] /dev/ptp0 offset -7 s1 freq +146")
	fmt.Fprintln(w, "1795461790.500000 timeserver ts2phc[4327]: something else")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if s, _ := v.Latest(); s.Servo == 1 && s.OffsetNs == -7 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second stream not read")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if p.opens.Load() != 2 {
		t.Errorf("%d opens", p.opens.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Follow did not stop")
	}
}

func TestFollowOpenError(t *testing.T) {
	var opens atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	Follow(ctx, func(context.Context) (io.ReadCloser, func() error, error) {
		opens.Add(1)
		return nil, nil, errors.New("no journalctl")
	}, &Servo{}, slog.New(slog.DiscardHandler), 10*time.Millisecond, 40*time.Millisecond)
	// Backoff 10, 20, 40, 40 ms: a handful of attempts, not a busy loop.
	if n := opens.Load(); n < 2 || n > 6 {
		t.Errorf("%d attempts", n)
	}
}
