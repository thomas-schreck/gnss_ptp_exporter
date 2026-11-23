package gpsd

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thomas-schreck/gnss_ptp_exporter/internal/promtest"
)

// zed_f9t.jsonl is recorded gpsd 3.25 output of a u-blox ZED-F9T
// (position fields replaced). Its SKY reports alternate between a
// per-signal list (69 entries, uSat 35) and a per-satellite list
// (36 entries, uSat 22).
func fixtureLines(t testing.TB) [][]byte {
	t.Helper()
	b, err := os.ReadFile("testdata/zed_f9t.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimSpace(b), []byte("\n"))
}

func skyReports(t *testing.T) []Report {
	var out []Report
	for _, l := range fixtureLines(t) {
		r, err := ParseLine(l)
		if err != nil {
			t.Fatalf("ParseLine: %v", err)
		}
		if r.Class == "SKY" && len(r.Satellites) > 0 {
			out = append(out, r)
		}
	}
	if len(out) < 3 {
		t.Fatalf("fixture has %d SKY reports with satellites", len(out))
	}
	return out
}

func TestDedupeMultiSignal(t *testing.T) {
	want := map[string]ConstellationStats{
		"GPS":     {Visible: 13, Used: 8, SNRCount: 8, SNRMax: 49},
		"Galileo": {Visible: 13, Used: 8, SNRCount: 8, SNRMax: 52},
		"BeiDou":  {Visible: 10, Used: 6, SNRCount: 6, SNRMax: 53},
	}
	wantMean := map[string]float64{"GPS": 42.75, "Galileo": 46.75, "BeiDou": 45.1666666667}
	for i, r := range skyReports(t) {
		s := Summarize(Dedupe(r.Satellites))
		// Both report shapes must give the same satellite counts.
		if s.All.Visible != 36 || s.All.Used != 22 {
			t.Errorf("report %d (%d entries): visible %d used %d, want 36/22", i, len(r.Satellites), s.All.Visible, s.All.Used)
		}
		if i != 0 {
			continue
		}
		for name, w := range want {
			g := s.ByGNSS[name]
			if g.Visible != w.Visible || g.Used != w.Used || g.SNRCount != w.SNRCount || g.SNRMax != w.SNRMax {
				t.Errorf("%s: got %+v, want %+v", name, g, w)
			}
			if math.Abs(g.SNRMean()-wantMean[name]) > 1e-6 {
				t.Errorf("%s: mean %v, want %v", name, g.SNRMean(), wantMean[name])
			}
		}
		if len(s.Systems) != 3 {
			t.Errorf("systems %v", s.Systems)
		}
	}
}

func TestDedupeMerge(t *testing.T) {
	i := func(v int) *int { return &v }
	sats := Dedupe([]RawSatellite{
		{GNSSID: i(0), SVID: i(5), SigID: i(0), SS: 30, Used: false},
		{GNSSID: i(0), SVID: i(5), SigID: i(3), SS: 41, Used: true},
		{GNSSID: i(2), SVID: i(5), SS: 20, Used: false}, // same svid, other system
		{PRN: 70, SS: 10}, // no gnssid: GLONASS by PRN
		{PRN: 999, SS: 0},
	})
	if len(sats) != 4 {
		t.Fatalf("got %d satellites: %+v", len(sats), sats)
	}
	if sats[0] != (Satellite{GNSS: "GPS", SVID: 5, SS: 41, Used: true}) {
		t.Errorf("merged satellite %+v", sats[0])
	}
	if sats[2].GNSS != "GLONASS" || sats[3].GNSS != "other" {
		t.Errorf("PRN fallback: %+v", sats[2:])
	}
}

func TestParseLineRejects(t *testing.T) {
	for _, l := range []string{"", "$GPGGA,...", "{", "[1]"} {
		if _, err := ParseLine([]byte(l)); err == nil {
			t.Errorf("ParseLine(%q) accepted", l)
		}
	}
}

func TestCollector(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := New("unused", 10*time.Second, slog.New(slog.DiscardHandler))
	c.now = func() time.Time { return now }

	got := promtest.Gather(t, c)
	promtest.Expect(t, got, map[string]float64{"gpsd_up": 0, "gpsd_fix_mode": 0})
	promtest.Absent(t, got, "gpsd_satellites_used")

	c.connected = true
	for _, l := range fixtureLines(t) {
		c.HandleLine(l)
	}
	got = promtest.Gather(t, c)
	promtest.Expect(t, got, map[string]float64{
		"gpsd_up":                 1,
		"gpsd_fix_mode":           3,
		"gpsd_satellites_visible": 36,
		"gpsd_satellites_used":    22,
		`gpsd_satellites_used_by_gnss{gnss="GPS"}`:       8,
		`gpsd_satellites_used_by_gnss{gnss="all"}`:       22,
		`gpsd_satellites_visible_by_gnss{gnss="BeiDou"}`: 10,
		`gpsd_snr_used_max_dbhz{gnss="all"}`:             53,
		`gpsd_snr_used_mean_dbhz{gnss="Galileo"}`:        46.625, // newest SKY report
		"gpsd_sky_age_seconds":                           0,
	})

	// Stale data: gpsd_up drops, satellite gauges disappear.
	now = now.Add(11 * time.Second)
	got = promtest.Gather(t, c)
	promtest.Expect(t, got, map[string]float64{"gpsd_up": 0, "gpsd_fix_mode": 0, "gpsd_sky_age_seconds": 11})
	promtest.Absent(t, got, "gpsd_satellites_used", `gpsd_snr_used_max_dbhz{gnss="all"}`)
}

// fakeGPSD serves the fixture to the first client that sends ?WATCH.
func fakeGPSD(t *testing.T, lines [][]byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, `{"class":"VERSION","release":"3.25"}`+"\n")
		cmd, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil || !strings.HasPrefix(cmd, "?WATCH=") {
			return
		}
		for _, l := range lines {
			if _, err := conn.Write(append(l, '\n')); err != nil {
				return
			}
		}
		time.Sleep(time.Second)
	}()
	return ln.Addr().String()
}

func TestCollectorStream(t *testing.T) {
	addr := fakeGPSD(t, fixtureLines(t))
	c := New(addr, 10*time.Second, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := promtest.Gather(t, c)
		if got["gpsd_up"] == 1 && got["gpsd_satellites_used"] == 22 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("collector did not pick up the stream")
}

func FuzzParseLine(f *testing.F) {
	for _, l := range fixtureLines(f) {
		f.Add(l)
	}
	f.Add([]byte(`{"class":"SKY","satellites":[{"PRN":1,"used":true,"ss":-5}]}`))
	f.Add([]byte(`{"class":"SKY","satellites":[{"gnssid":99,"svid":-1,"ss":1e308,"used":true}]}`))
	f.Fuzz(func(t *testing.T, line []byte) {
		r, err := ParseLine(line)
		if err != nil {
			return
		}
		sats := Dedupe(r.Satellites)
		if len(sats) > len(r.Satellites) {
			t.Fatalf("dedupe grew %d -> %d", len(r.Satellites), len(sats))
		}
		s := Summarize(sats)
		if s.All.Used > s.All.Visible || s.All.SNRCount > s.All.Used {
			t.Fatalf("inconsistent summary %+v", s.All)
		}
		c := New("unused", time.Minute, slog.New(slog.DiscardHandler))
		c.connected = true
		c.HandleLine(line)
		_ = promtest.Gather(t, c)
	})
}
