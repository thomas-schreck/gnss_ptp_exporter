// Package clockclass decides which clockClass a ts2phc-disciplined ptp4l
// grandmaster announces: 6 while ts2phc holds the PHC locked to the GNSS
// PPS, 7 (holdover) for a limited time after the lock was lost, 248
// otherwise. It is a port of the clockClass part of the time-stack-collector
// shell script the helper replaces; the decision is a pure function of the
// inputs gathered by the caller.
package clockclass

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/thomas-schreck/gnss_ptp_exporter/cmd/gnss_ptp_clockclass/internal/gmsettings"
	"github.com/thomas-schreck/gnss_ptp_exporter/internal/ts2phc"
)

// Clock classes (IEEE 1588-2019 Table 4).
const (
	ClassLocked   = 6   // synchronized to a primary reference (GPS)
	ClassHoldover = 7   // was synchronized, in holdover within specification
	ClassFree     = 248 // default, not synchronized
)

// Clock accuracy values (IEEE 1588-2019 Table 5).
const (
	Accuracy100ns   = 0x21
	AccuracyUnknown = 0xFE
)

// Config holds the thresholds; Defaults are the shell script's values.
type Config struct {
	// MaxAge: a servo line counts only while younger (whole seconds).
	MaxAge time.Duration
	// EnterOffsetNs: |offset| to enter class 6; HoldOffsetNs: to keep it.
	EnterOffsetNs int64
	HoldOffsetNs  int64
	// Holdover: how long class 7 is announced after the last lock.
	Holdover time.Duration

	// Values written with every SET.
	OffsetScaledLogVariance uint16
	CurrentUTCOffset        int16
	TimeSource              uint8
}

// Defaults returns the values of the time-stack-collector script.
func Defaults() Config {
	return Config{
		MaxAge:                  75 * time.Second,
		EnterOffsetNs:           100,
		HoldOffsetNs:            1000,
		Holdover:                900 * time.Second,
		OffsetScaledLogVariance: 0xffff,
		CurrentUTCOffset:        37,
		TimeSource:              0x20, // GPS
	}
}

// Leap is the leap second chrony announces.
type Leap struct{ Leap61, Leap59 bool }

// ParseChronyTracking reads the "Leap status" line of `chronyc tracking`.
// Anything but "Insert second" or "Delete second" (including missing
// output) means no leap second, as in the script.
func ParseChronyTracking(out []byte) Leap {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ": ")
		if !ok || !strings.HasPrefix(k, "Leap status") {
			continue
		}
		switch v {
		case "Insert second":
			return Leap{Leap61: true}
		case "Delete second":
			return Leap{Leap59: true}
		}
		return Leap{}
	}
	return Leap{}
}

// NewestServoLine returns the last ts2phc servo line in journal output
// that has servo state s0, s1 or s2 and a frequency, like the script's
// grep -E "offset .* s[012] freq" | tail -1 (any clock).
func NewestServoLine(out []byte) (ts2phc.Sample, bool) {
	var last ts2phc.Sample
	found := false
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		if s, ok := servoLine(string(line)); ok {
			last, found = s, true
		}
	}
	return last, found
}

func servoLine(line string) (ts2phc.Sample, bool) {
	s, ok := ts2phc.ParseLine(line)
	if !ok || s.Servo > 2 || !s.HasFreq {
		return ts2phc.Sample{}, false
	}
	return s, true
}

// State is the lock memory: the ptp4l PID and the Unix time of the last
// locked run. On disk it is one line "<pid> <epoch>", the format of the
// script's /run/time-stack-ptp-class.
type State struct {
	PID       string
	LockEpoch int64
}

// ParseState reads the state like the shell's `read -r m_pid m_lock`:
// first word PID, rest lock time, 0 if that is not a number.
func ParseState(b []byte) State {
	line, _, _ := bytes.Cut(b, []byte{'\n'})
	f := strings.Fields(string(line))
	var st State
	if len(f) > 0 {
		st.PID = f[0]
	}
	if len(f) == 2 && isDigits(f[1]) {
		if v, err := strconv.ParseInt(f[1], 10, 64); err == nil {
			st.LockEpoch = v
		}
	}
	return st
}

// Marshal returns the on-disk form.
func (s State) Marshal() []byte { return []byte(fmt.Sprintf("%s %d\n", s.PID, s.LockEpoch)) }

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Input is everything the decision depends on.
type Input struct {
	Now          time.Time
	PTP4LPID     int // MainPID of ptp4l; the caller does nothing if 0
	Ts2phcActive bool
	// Sample is the newest servo line; HaveSample false if there is none.
	Sample     ts2phc.Sample
	HaveSample bool
	// Current are ptp4l's settings; HaveCurrent false if the GET failed.
	Current     gmsettings.Settings
	HaveCurrent bool
	State       State
	Leap        Leap
}

// Decision is the outcome of one run.
type Decision struct {
	Class  uint8
	Want   gmsettings.Settings
	Locked bool // the run counts as locked: the state file gets the new time
	// NewState is the state to store if Locked.
	NewState State
	// Set is true if ptp4l's settings differ from Want and are known.
	Set bool

	// The inputs as the script saw them, for the log.
	Servo    int
	OffsetNs int64 // absolute value; 999999999 without a servo line
	AgeS     int64 // whole seconds; 9999 without a timestamped line
	LimitNs  int64
}

// Decide computes the clockClass and whether to send a SET.
func Decide(cfg Config, in Input) Decision {
	d := Decision{Servo: 0, OffsetNs: 999999999, AgeS: 9999}
	if in.HaveSample {
		d.Servo = in.Sample.Servo
		d.OffsetNs = in.Sample.OffsetNs
		if d.OffsetNs < 0 {
			d.OffsetNs = -d.OffsetNs
		}
		if !in.Sample.Time.IsZero() {
			d.AgeS = in.Now.Unix() - in.Sample.Time.Unix()
		}
	}
	now := in.Now.Unix()
	d.LimitNs = cfg.EnterOffsetNs
	if in.HaveCurrent && in.Current.ClockClass == ClassLocked {
		d.LimitNs = cfg.HoldOffsetNs
	}
	d.Locked = in.Ts2phcActive && d.Servo == 2 && d.AgeS < int64(cfg.MaxAge/time.Second) && d.OffsetNs <= d.LimitNs
	pid := strconv.Itoa(in.PTP4LPID)
	switch {
	case d.Locked:
		d.Class = ClassLocked
		d.NewState = State{PID: pid, LockEpoch: now}
	case in.State.PID == pid && now-in.State.LockEpoch < int64(cfg.Holdover/time.Second):
		d.Class = ClassHoldover
	default:
		d.Class = ClassFree
	}
	acc, traceable := uint8(AccuracyUnknown), d.Class != ClassFree
	if d.Class == ClassLocked {
		acc = Accuracy100ns
	}
	d.Want = gmsettings.Settings{
		ClockClass:              d.Class,
		ClockAccuracy:           acc,
		OffsetScaledLogVariance: cfg.OffsetScaledLogVariance,
		CurrentUTCOffset:        cfg.CurrentUTCOffset,
		Leap61:                  in.Leap.Leap61,
		Leap59:                  in.Leap.Leap59,
		CurrentUTCOffsetValid:   true,
		PTPTimescale:            true,
		TimeTraceable:           traceable,
		FrequencyTraceable:      traceable,
		TimeSource:              cfg.TimeSource,
	}
	d.Set = in.HaveCurrent && in.Current != d.Want
	return d
}
