package clockclass

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thomas-schreck/gnss_ptp_exporter/cmd/gnss_ptp_clockclass/internal/gmsettings"
	"github.com/thomas-schreck/gnss_ptp_exporter/internal/ts2phc"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseChronyTracking(t *testing.T) {
	for file, want := range map[string]Leap{
		"chrony_normal.txt":          {},
		"chrony_insert.txt":          {Leap61: true},
		"chrony_delete.txt":          {Leap59: true},
		"chrony_unsynchronised.txt":  {},
		"../../../../../go.mod":      {}, // anything else
		"chrony_normal.txt.missing:": {},
	} {
		b, _ := os.ReadFile(filepath.Join("testdata", file))
		if got := ParseChronyTracking(b); got != want {
			t.Errorf("%s: %+v", file, got)
		}
	}
}

func TestNewestServoLine(t *testing.T) {
	for file, want := range map[string]struct {
		servo  int
		offset int64
		unix   int64
	}{
		"journal_locked.txt":  {2, 2, 1795461779},
		"journal_startup.txt": {2, -8291, 1795453503},
		"journal_s1.txt":      {1, -112, 1795453503},
		// s3 and lines without freq are skipped, as the script's grep does.
		"journal_s3.txt":      {2, -4, 1795461778},
		"journal_stopped.txt": {2, 2, 1795461779},
	} {
		s, ok := NewestServoLine(read(t, file))
		if !ok || s.Servo != want.servo || s.OffsetNs != want.offset || s.Time.Unix() != want.unix {
			t.Errorf("%s: %+v %v", file, s, ok)
		}
	}
	if _, ok := NewestServoLine([]byte("-- No entries --\n")); ok {
		t.Error("found a line in nothing")
	}
}

func TestState(t *testing.T) {
	for in, want := range map[string]State{
		"1013 1795461388\n":   {"1013", 1795461388},
		"1013\t1795461388":    {"1013", 1795461388},
		"  1013   17954 \n":   {"1013", 17954},
		"1013 abc\n":          {"1013", 0},
		"1013 1 2\n":          {"1013", 0},
		"":                    {},
		"1013 -5\n":           {"1013", 0},
		"1013 1795461388\nx2": {"1013", 1795461388},
	} {
		if got := ParseState([]byte(in)); got != want {
			t.Errorf("%q: %+v", in, got)
		}
	}
	if s := string(State{"1013", 42}.Marshal()); s != "1013 42\n" {
		t.Errorf("%q", s)
	}
}

// settings returns what the script would SET for class.
func settings(class uint8) gmsettings.Settings {
	s := gmsettings.Settings{ClockClass: class, ClockAccuracy: AccuracyUnknown, OffsetScaledLogVariance: 0xffff,
		CurrentUTCOffset: 37, CurrentUTCOffsetValid: true, PTPTimescale: true, TimeSource: 0x20}
	if class == ClassLocked {
		s.ClockAccuracy = Accuracy100ns
	}
	if class != ClassFree {
		s.TimeTraceable, s.FrequencyTraceable = true, true
	}
	return s
}

func TestSettingsMatchScript(t *testing.T) {
	// SET GRANDMASTER_SETTINGS_NP clockClass $want clockAccuracy $acc
	// offsetScaledLogVariance 0xffff currentUtcOffset 37 leap61 0 leap59 0
	// currentUtcOffsetValid 1 ptpTimescale 1 timeTraceable $tt
	// frequencyTraceable $tt timeSource 0x20
	for class, want := range map[uint8]string{
		6:   "clockClass 6 clockAccuracy 0x21 offsetScaledLogVariance 0xffff currentUtcOffset 37 leap61 0 leap59 0 currentUtcOffsetValid 1 ptpTimescale 1 timeTraceable 1 frequencyTraceable 1 timeSource 0x20",
		7:   "clockClass 7 clockAccuracy 0xFE offsetScaledLogVariance 0xffff currentUtcOffset 37 leap61 0 leap59 0 currentUtcOffsetValid 1 ptpTimescale 1 timeTraceable 1 frequencyTraceable 1 timeSource 0x20",
		248: "clockClass 248 clockAccuracy 0xFE offsetScaledLogVariance 0xffff currentUtcOffset 37 leap61 0 leap59 0 currentUtcOffsetValid 1 ptpTimescale 1 timeTraceable 0 frequencyTraceable 0 timeSource 0x20",
	} {
		if got := settings(class).PMCArgs(); got != want {
			t.Errorf("%d:\n got %s\nwant %s", class, got, want)
		}
	}
}

func TestDecide(t *testing.T) {
	const pid = 1013
	locked, _ := NewestServoLine(read(t, "journal_locked.txt"))
	settling, _ := NewestServoLine(read(t, "journal_startup.txt"))
	s1, _ := NewestServoLine(read(t, "journal_s1.txt"))
	at := func(s ts2phc.Sample, age int64) (ts2phc.Sample, time.Time) {
		return s, s.Time.Add(time.Duration(age)*time.Second + 300*time.Millisecond)
	}
	offset := func(ns int64) ts2phc.Sample { s := locked; s.OffsetNs = ns; return s }

	type tc struct {
		name    string
		sample  ts2phc.Sample
		have    bool
		age     int64
		active  bool
		current uint8 // 0: GET failed
		state   State // LockEpoch relative to now
		leap    Leap
		class   uint8
		locked  bool
		set     bool
	}
	for _, c := range []tc{
		{name: "locked, no change", sample: locked, have: true, age: 1, active: true, current: 6, class: 6, locked: true},
		{name: "first lock", sample: locked, have: true, age: 1, active: true, current: 248, class: 6, locked: true, set: true},
		{name: "settling after start", sample: settling, have: true, age: 1, active: true, current: 248, class: 248, set: false},
		{name: "s1", sample: s1, have: true, age: 1, active: true, current: 248, class: 248},
		{name: "s1 after lock", sample: s1, have: true, age: 1, active: true, current: 6, state: State{"1013", -30}, class: 7, set: true},
		{name: "enter needs 100 ns", sample: offset(-101), have: true, age: 1, active: true, current: 248, class: 248},
		{name: "enter at 100 ns", sample: offset(100), have: true, age: 1, active: true, current: 248, class: 6, locked: true, set: true},
		{name: "hold up to 1000 ns", sample: offset(-1000), have: true, age: 1, active: true, current: 6, class: 6, locked: true},
		{name: "lose above 1000 ns", sample: offset(1001), have: true, age: 1, active: true, current: 6, state: State{"1013", -1}, class: 7, set: true},
		{name: "holdover keeps 500 ns out of 6", sample: offset(500), have: true, age: 1, active: true, current: 7, state: State{"1013", -60}, class: 7},
		{name: "line 74 s old", sample: locked, have: true, age: 74, active: true, current: 6, class: 6, locked: true},
		{name: "line 75 s old", sample: locked, have: true, age: 75, active: true, current: 6, state: State{"1013", -75}, class: 7, set: true},
		{name: "no line", active: true, current: 6, state: State{"1013", -100}, class: 7, set: true},
		{name: "ts2phc stopped", sample: locked, have: true, age: 1, current: 6, state: State{"1013", -1}, class: 7, set: true},
		{name: "holdover 899 s", age: 1, active: true, current: 7, state: State{"1013", -899}, class: 7},
		{name: "holdover expired", age: 1, active: true, current: 7, state: State{"1013", -900}, class: 248, set: true},
		{name: "ptp4l restarted", sample: s1, have: true, age: 1, active: true, current: 248, state: State{"999", -10}, class: 248},
		{name: "GET failed", sample: locked, have: true, age: 1, active: true, current: 0, class: 6, locked: true},
		{name: "GET failed uses 100 ns", sample: offset(500), have: true, age: 1, active: true, current: 0, class: 248},
		{name: "leap second", sample: locked, have: true, age: 1, active: true, current: 6, leap: Leap{Leap61: true}, class: 6, locked: true, set: true},
	} {
		sample, now := c.sample, time.Unix(1795461779, 0)
		if c.have {
			sample, now = at(c.sample, c.age)
		}
		st := c.state
		if st.PID != "" {
			st.LockEpoch += now.Unix()
		}
		in := Input{Now: now, PTP4LPID: pid, Ts2phcActive: c.active, Sample: sample, HaveSample: c.have,
			HaveCurrent: c.current != 0, State: st, Leap: c.leap}
		if c.current != 0 {
			in.Current = settings(c.current)
		}
		d := Decide(Defaults(), in)
		if d.Class != c.class || d.Locked != c.locked || d.Set != c.set {
			t.Errorf("%s: class %d locked %v set %v; want %d %v %v (%+v)", c.name, d.Class, d.Locked, d.Set, c.class, c.locked, c.set, d)
		}
		want := settings(c.class)
		want.Leap61, want.Leap59 = c.leap.Leap61, c.leap.Leap59
		if d.Want != want {
			t.Errorf("%s: want %s", c.name, d.Want.PMCArgs())
		}
		if c.locked && d.NewState != (State{"1013", now.Unix()}) {
			t.Errorf("%s: new state %+v", c.name, d.NewState)
		}
	}
}

func TestDecideComparesAllFields(t *testing.T) {
	// The script compared clockClass, clockAccuracy, leap61, leap59 and
	// frequencyTraceable; the helper compares everything it writes, so a
	// drifted timeTraceable or currentUtcOffset is repaired too.
	locked, _ := NewestServoLine(read(t, "journal_locked.txt"))
	cur := settings(6)
	cur.CurrentUTCOffset = 36
	d := Decide(Defaults(), Input{Now: locked.Time.Add(time.Second), PTP4LPID: 1, Ts2phcActive: true,
		Sample: locked, HaveSample: true, Current: cur, HaveCurrent: true})
	if !d.Set || d.Want.CurrentUTCOffset != 37 {
		t.Errorf("%+v", d)
	}
}

func FuzzInputs(f *testing.F) {
	for _, name := range []string{"journal_locked.txt", "journal_s3.txt", "chrony_insert.txt"} {
		b, _ := os.ReadFile(filepath.Join("testdata", name))
		f.Add(b)
	}
	f.Add([]byte("1013 1795461388\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		l := ParseChronyTracking(b)
		if l.Leap61 && l.Leap59 {
			t.Fatal("both leap flags")
		}
		if s, ok := NewestServoLine(b); ok && (s.Servo > 2 || !s.HasFreq) {
			t.Fatalf("%+v", s)
		}
		st := ParseState(b)
		if st.LockEpoch < 0 || strings.ContainsAny(st.PID, " \t\n") {
			t.Fatalf("%+v", st)
		}
	})
}
