// Package ts2phc extracts the servo state of linuxptp's ts2phc from its log
// output. ts2phc has no query interface, so its log is the only source.
package ts2phc

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Sample is one servo line, e.g.
//
//	ts2phc[359198.839]: /dev/ptp0 offset         -1 s2 freq    +139
type Sample struct {
	Clock    string  // PHC device or interface, e.g. /dev/ptp0 or eth1
	OffsetNs int64   // offset of the PHC from the PPS source, ns
	Servo    int     // servo state: 0 unlocked, 1 jump/acquiring, 2 locked, 3 locked stable
	FreqPPB  float64 // frequency adjustment of the PHC, ppb
	HasFreq  bool
	// Monotonic is the CLOCK_MONOTONIC time in the "[123.456]"
	// prefix, in seconds; 0 if the line has none.
	Monotonic float64
	// Time is the wall-clock time of the line when the source knows it
	// (journald); zero otherwise.
	Time time.Time
}

var (
	servoRe = regexp.MustCompile(`(?:^|\s)(\S+)\s+offset\s+(-?\d+)\s+s(\d)(?:\s+freq\s+([+-]?\d+(?:\.\d+)?))?`)
	monoRe  = regexp.MustCompile(`\[(\d+\.\d{1,9})\]`)
	// journalctl -o short-unix: "1795453716.000032 host ts2phc[4327]: msg"
	unixPrefixRe = regexp.MustCompile(`^(\d+)\.(\d{1,9})\s`)
)

// ParseLine parses one log line. ok is false for lines that are not servo
// lines (NMEA warnings, startup messages, ...).
func ParseLine(line string) (s Sample, ok bool) {
	m := servoRe.FindStringSubmatch(line)
	if m == nil {
		return Sample{}, false
	}
	off, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return Sample{}, false
	}
	servo, err := strconv.Atoi(m[3])
	if err != nil {
		return Sample{}, false
	}
	// The clock name becomes a label value, which must be valid UTF-8.
	s = Sample{Clock: strings.ToValidUTF8(m[1], "\uFFFD"), OffsetNs: off, Servo: servo}
	if m[4] != "" {
		if f, err := strconv.ParseFloat(m[4], 64); err == nil {
			s.FreqPPB, s.HasFreq = f, true
		}
	}
	if mm := monoRe.FindStringSubmatch(line); mm != nil {
		if v, err := strconv.ParseFloat(mm[1], 64); err == nil {
			s.Monotonic = v
		}
	}
	if um := unixPrefixRe.FindStringSubmatch(line); um != nil {
		sec, err1 := strconv.ParseInt(um[1], 10, 64)
		frac := um[2] + "000000000"[:9-len(um[2])]
		nsec, err2 := strconv.ParseInt(frac, 10, 64)
		if err1 == nil && err2 == nil {
			s.Time = time.Unix(sec, nsec)
		}
	}
	return s, true
}

// Latest returns the last servo sample per clock found in log output,
// in order of first appearance.
func Latest(out []byte) []Sample {
	idx := map[string]int{}
	var res []Sample
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		s, ok := ParseLine(string(line))
		if !ok {
			continue
		}
		if i, seen := idx[s.Clock]; seen {
			res[i] = s
			continue
		}
		idx[s.Clock] = len(res)
		res = append(res, s)
	}
	return res
}
