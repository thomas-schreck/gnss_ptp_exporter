// Package gpsd reads TPV and SKY reports from gpsd's JSON protocol and
// exports fix and satellite metrics.
package gpsd

import (
	"encoding/json"
	"errors"
	"sort"
)

// gnssNames maps gpsd's gnssid (u-blox numbering) to constellation names.
// The names match the time-stack-collector shell script this exporter
// replaces, so existing dashboards keep working.
var gnssNames = map[int]string{
	0: "GPS",
	1: "SBAS",
	2: "Galileo",
	3: "BeiDou",
	4: "IMES",
	5: "QZSS",
	6: "GLONASS",
	7: "NavIC",
}

// GNSSName returns the constellation name for a gpsd gnssid.
func GNSSName(id int) string {
	if n, ok := gnssNames[id]; ok {
		return n
	}
	return "other"
}

// ErrNotJSON is returned for lines that are not a gpsd JSON object.
var ErrNotJSON = errors.New("gpsd: line is not a JSON object")

// Report holds the fields of a gpsd report this exporter uses.
type Report struct {
	Class      string         `json:"class"`
	Device     string         `json:"device"`
	Mode       int            `json:"mode"`
	Satellites []RawSatellite `json:"satellites"`
	Devices    []Device       `json:"devices"`
}

// Device is one entry of a DEVICES report.
type Device struct {
	Path   string `json:"path"`
	Driver string `json:"driver"`
}

// RawSatellite is one entry of a SKY report's satellites array. Multi-band
// receivers such as the u-blox ZED-F9T report one entry per signal, so the
// same satellite can appear several times with different sigid values.
type RawSatellite struct {
	PRN    int     `json:"PRN"`
	GNSSID *int    `json:"gnssid"`
	SVID   *int    `json:"svid"`
	SigID  *int    `json:"sigid"`
	SS     float64 `json:"ss"`
	Used   bool    `json:"used"`
}

// ParseLine decodes one line of gpsd output.
func ParseLine(line []byte) (Report, error) {
	var r Report
	if len(line) == 0 || line[0] != '{' {
		return r, ErrNotJSON
	}
	if err := json.Unmarshal(line, &r); err != nil {
		return Report{}, err
	}
	return r, nil
}

// Satellite is one physical satellite after merging its signals.
type Satellite struct {
	GNSS string
	SVID int
	SS   float64 // best carrier-to-noise density of all its signals, dB-Hz
	Used bool    // at least one signal is used in the solution
}

type satKey struct {
	gnss int
	svid int
}

// Dedupe merges the per-signal entries of a SKY report into one entry per
// satellite, keyed by (gnssid, svid). A satellite counts as used if any of
// its signals is used; its signal strength is that of its best signal.
// Entries without gnssid/svid (older gpsd or non-u-blox receivers) are
// keyed by PRN.
func Dedupe(raw []RawSatellite) []Satellite {
	best := make(map[satKey]Satellite, len(raw))
	order := make([]satKey, 0, len(raw))
	for _, r := range raw {
		k := satKey{gnss: -1, svid: r.PRN}
		gnss := "other"
		if r.GNSSID != nil && r.SVID != nil {
			k = satKey{gnss: *r.GNSSID, svid: *r.SVID}
			gnss = GNSSName(*r.GNSSID)
		} else if g, ok := gnssFromPRN(r.PRN); ok {
			k = satKey{gnss: -2, svid: r.PRN}
			gnss = g
		}
		cur, ok := best[k]
		if !ok {
			order = append(order, k)
			best[k] = Satellite{GNSS: gnss, SVID: k.svid, SS: r.SS, Used: r.Used}
			continue
		}
		if r.SS > cur.SS {
			cur.SS = r.SS
		}
		cur.Used = cur.Used || r.Used
		best[k] = cur
	}
	out := make([]Satellite, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}

// gnssFromPRN classifies NMEA-style PRNs that gpsd reports when the
// receiver gives no gnssid (gpsd's documented PRN ranges).
func gnssFromPRN(prn int) (string, bool) {
	switch {
	case prn >= 1 && prn <= 63:
		return "GPS", true
	case prn >= 64 && prn <= 96:
		return "GLONASS", true
	case prn >= 120 && prn <= 158:
		return "SBAS", true
	case prn >= 159 && prn <= 163, prn >= 401 && prn <= 437:
		return "BeiDou", true
	case prn >= 193 && prn <= 202:
		return "QZSS", true
	case prn >= 211 && prn <= 246, prn >= 301 && prn <= 336:
		return "Galileo", true
	}
	return "", false
}

// ConstellationStats aggregates the satellites of one constellation.
type ConstellationStats struct {
	Visible int
	Used    int
	// SNR statistics over used satellites with a signal (ss > 0).
	SNRCount int
	SNRSum   float64
	SNRMax   float64
}

// SNRMean returns the mean C/N0 of the used satellites with a signal.
func (c ConstellationStats) SNRMean() float64 {
	if c.SNRCount == 0 {
		return 0
	}
	return c.SNRSum / float64(c.SNRCount)
}

// Summary is the per-constellation view of one SKY report.
type Summary struct {
	All     ConstellationStats
	ByGNSS  map[string]ConstellationStats
	Systems []string // constellation names, sorted
}

// Summarize aggregates deduplicated satellites.
func Summarize(sats []Satellite) Summary {
	s := Summary{ByGNSS: map[string]ConstellationStats{}}
	for _, sat := range sats {
		c := s.ByGNSS[sat.GNSS]
		add(&c, sat)
		s.ByGNSS[sat.GNSS] = c
		add(&s.All, sat)
	}
	for name := range s.ByGNSS {
		s.Systems = append(s.Systems, name)
	}
	sort.Strings(s.Systems)
	return s
}

func add(c *ConstellationStats, sat Satellite) {
	c.Visible++
	if !sat.Used {
		return
	}
	c.Used++
	if sat.SS > 0 {
		c.SNRCount++
		c.SNRSum += sat.SS
		if sat.SS > c.SNRMax {
			c.SNRMax = sat.SS
		}
	}
}
