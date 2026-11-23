package ts2phc

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thomas-schreck/gnss_ptp_exporter/internal/promtest"
)

// journal_short_unix.txt: the s2 lines are recorded from linuxptp 4.2 on
// a Raspberry Pi 5 (journalctl -o short-unix, host name replaced); the
// start-up, s0 and s1 lines are written in the same format.

func TestParseLine(t *testing.T) {
	cases := []struct {
		line string
		want Sample
		ok   bool
	}{
		{"1795453716.000032 host ts2phc[4327]: [359198.839] /dev/ptp0 offset         -1 s2 freq    +139",
			Sample{Clock: "/dev/ptp0", OffsetNs: -1, Servo: 2, FreqPPB: 139, HasFreq: true, Monotonic: 359198.839,
				Time: time.Unix(1795453716, 32000)}, true},
		{"[359205.839] /dev/ptp0 offset          7 s2 freq    +146",
			Sample{Clock: "/dev/ptp0", OffsetNs: 7, Servo: 2, FreqPPB: 146, HasFreq: true, Monotonic: 359205.839}, true},
		{"ts2phc[12.5]: eth1 offset -8291 s0 freq -0",
			Sample{Clock: "eth1", OffsetNs: -8291, Servo: 0, HasFreq: true, Monotonic: 12.5}, true},
		{"/dev/ptp1 offset 3 s3", Sample{Clock: "/dev/ptp1", OffsetNs: 3, Servo: 3}, true},
		{"1795453501.000020 host ts2phc[4327]: [359183.839] nmea delay: 131071997 ns", Sample{}, false},
		{"ts2phc[1.0]: UTC-TAI offset not set in system! Trying to revert to leapfile", Sample{}, false},
		{"offset 5 s2 freq +1", Sample{}, false},
		{"", Sample{}, false},
	}
	for _, c := range cases {
		got, ok := ParseLine(c.line)
		if ok != c.ok || !got.Time.Equal(c.want.Time) {
			t.Errorf("ParseLine(%q) = %+v, %v; want %+v, %v", c.line, got, ok, c.want, c.ok)
			continue
		}
		got.Time, c.want.Time = time.Time{}, time.Time{}
		if got != c.want {
			t.Errorf("ParseLine(%q) = %+v; want %+v", c.line, got, c.want)
		}
	}
}

func TestLatestPerClock(t *testing.T) {
	b, err := os.ReadFile("testdata/stdout.log")
	if err != nil {
		t.Fatal(err)
	}
	got := Latest(b)
	if len(got) != 2 || got[0].Clock != "/dev/ptp0" || got[0].OffsetNs != 7 || got[1].Clock != "eth2" || got[1].Servo != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestAge(t *testing.T) {
	now := time.Unix(1000, 0)
	mono := func() (float64, bool) { return 500, true }
	noMono := func() (float64, bool) { return 0, false }
	if a, ok := Age(Sample{Time: time.Unix(990, 0), Monotonic: 1}, now, time.Time{}, mono); !ok || a != 10*time.Second {
		t.Errorf("journal time: %v %v", a, ok)
	}
	if a, ok := Age(Sample{Monotonic: 497.5}, now, time.Unix(1, 0), mono); !ok || a != 2500*time.Millisecond {
		t.Errorf("monotonic: %v %v", a, ok)
	}
	if a, ok := Age(Sample{Monotonic: 497.5}, now, time.Unix(996, 0), noMono); !ok || a != 4*time.Second {
		t.Errorf("mtime fallback: %v %v", a, ok)
	}
	if _, ok := Age(Sample{}, now, time.Time{}, noMono); ok {
		t.Error("age without any time source")
	}
}

type fakeSource struct {
	out   []byte
	mtime time.Time
	err   error
}

func (f fakeSource) Read(context.Context) ([]byte, time.Time, error) { return f.out, f.mtime, f.err }

func TestCollector(t *testing.T) {
	b, err := os.ReadFile("testdata/journal_short_unix.txt")
	if err != nil {
		t.Fatal(err)
	}
	c := New(fakeSource{out: b}, time.Minute, time.Second, slog.New(slog.DiscardHandler))
	c.now = func() time.Time { return time.Unix(1795453725, 40000) }
	got := promtest.Gather(t, c)
	promtest.Expect(t, got, map[string]float64{
		"ts2phc_up":                             1,
		`ts2phc_servo_state{clock="/dev/ptp0"}`: 2,
		`ts2phc_master_offset_nanoseconds{clock="/dev/ptp0"}`: 7,
		`ts2phc_frequency_ppb{clock="/dev/ptp0"}`:             146,
		`ts2phc_log_age_seconds{clock="/dev/ptp0"}`:           2,
	})

	c.now = func() time.Time { return time.Unix(1795453723+61, 40000) }
	got = promtest.Gather(t, c)
	promtest.Expect(t, got, map[string]float64{"ts2phc_up": 0, `ts2phc_log_age_seconds{clock="/dev/ptp0"}`: 61})

	c = New(fakeSource{err: errors.New("no journal")}, time.Minute, time.Second, slog.New(slog.DiscardHandler))
	got = promtest.Gather(t, c)
	promtest.Expect(t, got, map[string]float64{"ts2phc_up": 0})
	promtest.Absent(t, got, `ts2phc_servo_state{clock="/dev/ptp0"}`)

	c = New(fakeSource{out: []byte("nothing here\n")}, time.Minute, time.Second, slog.New(slog.DiscardHandler))
	promtest.Expect(t, promtest.Gather(t, c), map[string]float64{"ts2phc_up": 0})
}

func TestJournalRead(t *testing.T) {
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	script := filepath.Join(dir, "journalctl")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + args + "\ncat testdata/journal_short_unix.txt\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	j := Journal{Journalctl: script, Unit: "ts2phc.service; rm -rf /", Lines: 50}
	out, _, err := j.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(Latest(out)) != 1 {
		t.Errorf("no samples from journal output")
	}
	a, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	want := "--unit=ts2phc.service; rm -rf /\n--lines=50\n--output=short-unix\n--no-pager\n--quiet\n"
	if string(a) != want {
		t.Errorf("journalctl args:\n%s\nwant:\n%s", a, want)
	}

	j.Journalctl = filepath.Join(dir, "missing")
	if _, _, err := j.Read(context.Background()); err == nil {
		t.Error("missing journalctl accepted")
	}
}

func TestFileRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ts2phc.log")
	long := strings.Repeat("x", 200) + "\n"
	if err := os.WriteFile(p, []byte(long+"ts2phc[1.0]: /dev/ptp0 offset 4 s2 freq +1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, mtime, err := File{Path: p, TailBytes: 100}.Read(context.Background())
	if err != nil || mtime.IsZero() || len(out) != 100 {
		t.Fatalf("read %d bytes, mtime %v, err %v", len(out), mtime, err)
	}
	if s := Latest(out); len(s) != 1 || s[0].OffsetNs != 4 {
		t.Errorf("samples %+v", s)
	}
	if _, _, err := (File{Path: p + ".missing", TailBytes: 1}).Read(context.Background()); err == nil {
		t.Error("missing file accepted")
	}
}

func FuzzParseLine(f *testing.F) {
	b, err := os.ReadFile("testdata/journal_short_unix.txt")
	if err != nil {
		f.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		f.Add(l)
	}
	f.Add("99999999999999999999.1 h ts2phc[1]: [1.5] x offset 99999999999999999999 s9 freq +1e9")
	f.Fuzz(func(t *testing.T, line string) {
		s, ok := ParseLine(line)
		if !ok {
			return
		}
		if s.Clock == "" || s.Servo < 0 || s.Servo > 9 {
			t.Fatalf("bad sample %+v", s)
		}
		c := New(fakeSource{out: []byte(line)}, time.Minute, time.Second, slog.New(slog.DiscardHandler))
		_ = promtest.Gather(t, c)
	})
}
