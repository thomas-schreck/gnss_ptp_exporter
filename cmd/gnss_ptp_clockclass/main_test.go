package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/facebook/time/ptp/protocol"

	"github.com/thomas-schreck/gnss_ptp_exporter/cmd/gnss_ptp_clockclass/internal/clockclass"
	"github.com/thomas-schreck/gnss_ptp_exporter/cmd/gnss_ptp_clockclass/internal/gmsettings"
)

func TestParseFlags(t *testing.T) {
	c, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.ptp4lSocket != "/var/run/ptp4l" || c.ptp4lROSocket != "/var/run/ptp4lro" || c.dryRun ||
		c.maxAge != 75*time.Second || c.lockOffset != 100 || c.holdOffset != 1000 || c.holdover != 15*time.Minute ||
		c.utcOffset != 37 || c.timeSource != 0x20 || c.interval != time.Second {
		t.Errorf("defaults %+v", c)
	}
	if c, err := parseFlags([]string{"--time-source=0xA0", "--dry-run"}, io.Discard); err != nil || c.timeSource != 0xa0 || !c.dryRun {
		t.Errorf("%+v %v", c, err)
	}
	for _, args := range [][]string{
		{"--ptp4l.domain=256"}, {"--time-source=256"}, {"--utc-offset=40000"}, {"--interval=1ms"},
		{"--max-age=0s"}, {"--lock-offset=2000"}, {"--ts2phc.backlog=0"}, {"extra"}, {"--no-such-flag"},
	} {
		if _, err := parseFlags(args, io.Discard); err == nil {
			t.Errorf("%s accepted", strings.Join(args, " "))
		}
	}
}

func TestParseUnits(t *testing.T) {
	out := []byte("MainPID=1013\nActiveState=active\n\nActiveState=active\nMainPID=4327\n")
	if u, err := parseUnits(out); err != nil || u != (clockclass.Units{PTP4LPID: 1013, Ts2phcActive: true}) {
		t.Errorf("%+v %v", u, err)
	}
	out = []byte("MainPID=0\nActiveState=inactive\n\nMainPID=0\nActiveState=failed\n")
	if u, err := parseUnits(out); err != nil || u != (clockclass.Units{}) {
		t.Errorf("%+v %v", u, err)
	}
	// A deactivating ptp4l still has a PID; it does not count.
	out = []byte("MainPID=1013\nActiveState=deactivating\n\nMainPID=1\nActiveState=active\n")
	if u, err := parseUnits(out); err != nil || u.PTP4LPID != 0 {
		t.Errorf("%+v %v", u, err)
	}
	for _, bad := range []string{"", "MainPID=1\n", "MainPID=x\nActiveState=active\n\nActiveState=active\n"} {
		if _, err := parseUnits([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestWriteAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state")
	for _, s := range []string{"1 2\n", "3 4\n"} {
		if err := writeAtomic(p, []byte(s)); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != s {
			t.Errorf("%q", b)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".*")); len(m) != 0 {
		t.Errorf("leftovers %v", m)
	}
}

// fakePTP4L serves ptp4l's two sockets: GETs on the read-only one, SETs on
// the read-write one, sharing the settings like ptp4l.
type fakePTP4L struct {
	ro, rw string
	mu     sync.Mutex
	cur    gmsettings.Settings
	roReqs []byte // action of every request on the read-only socket
	rwReqs [][]byte
}

func (f *fakePTP4L) serve(t *testing.T, path string, rw bool) {
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Net: "unixgram", Name: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUnix(buf)
			if err != nil {
				return
			}
			req := append([]byte{}, buf[:n]...)
			if len(req) < 54 || binary.BigEndian.Uint16(req[52:54]) != uint16(gmsettings.ID) {
				continue
			}
			f.mu.Lock()
			if rw {
				f.rwReqs = append(f.rwReqs, req)
				if tlv, err := gmsettings.Decode(req[48:]); err == nil && req[46] == byte(protocol.SET) {
					f.cur = gmsettings.FromTLV(tlv.(*gmsettings.TLV))
				}
			} else {
				f.roReqs = append(f.roReqs, req[46])
			}
			tlv, _ := f.cur.TLV().MarshalBinary()
			f.mu.Unlock()
			resp := append(append([]byte{}, req[:48]...), tlv...)
			binary.BigEndian.PutUint16(resp[2:4], uint16(len(resp)))
			resp[46] = byte(protocol.RESPONSE)
			_, _ = conn.WriteToUnix(resp, from)
		}
	}()
}

func newFakePTP4L(t *testing.T) *fakePTP4L {
	dir := t.TempDir()
	f := &fakePTP4L{ro: filepath.Join(dir, "ptp4lro"), rw: filepath.Join(dir, "ptp4l")}
	f.cur = gmsettings.Settings{ClockClass: 248, ClockAccuracy: 0xfe, OffsetScaledLogVariance: 0xffff,
		CurrentUTCOffset: 37, CurrentUTCOffsetValid: true, PTPTimescale: true, TimeSource: 0x20}
	f.serve(t, f.ro, false)
	f.serve(t, f.rw, true)
	return f
}

// fakeBin writes an executable shell script.
func fakeBin(t *testing.T, dir, name, body string) string {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func fakeConfig(t *testing.T, f *fakePTP4L, dry bool) config {
	dir := t.TempDir()
	cfg, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg.dryRun = dry
	cfg.ptp4lSocket, cfg.ptp4lROSocket = f.rw, f.ro
	cfg.interval, cfg.ptp4lTimeout = 50*time.Millisecond, time.Second
	cfg.stateFile = filepath.Join(dir, "state")
	cfg.systemctl = fakeBin(t, dir, "systemctl",
		`printf 'MainPID=1013\nActiveState=active\n\nMainPID=4327\nActiveState=active\n'`)
	cfg.chronyc = fakeBin(t, dir, "chronyc", `echo 'Leap status     : Normal'`)
	// A locked ts2phc line stamped now, then the stream stays open.
	cfg.journalctl = fakeBin(t, dir, "journalctl",
		`echo "$(date +%s).000040 timeserver ts2phc[4327]: [1.839] /dev/ptp0 offset 3 s2 freq +140"; exec sleep 5`)
	return cfg
}

func runFor(t *testing.T, cfg config, d time.Duration) string {
	var logs bytes.Buffer
	logOutput = &syncWriter{w: &logs}
	defer func() { logOutput = os.Stderr }()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if err := run(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	return logs.String()
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(b)
}

func TestRunDryRunSendsNothing(t *testing.T) {
	f := newFakePTP4L(t)
	cfg := fakeConfig(t, f, true)
	logs := runFor(t, cfg, 700*time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rwReqs) != 0 {
		t.Errorf("dry run sent %d datagrams to the read-write socket", len(f.rwReqs))
	}
	if len(f.roReqs) < 3 {
		t.Errorf("%d GETs", len(f.roReqs))
	}
	for _, a := range f.roReqs {
		if a != byte(protocol.GET) {
			t.Errorf("action %d on the read-only socket", a)
		}
	}
	if _, err := os.Stat(cfg.stateFile); err == nil {
		t.Error("dry run wrote the state file")
	}
	if !strings.Contains(logs, "would SET GRANDMASTER_SETTINGS_NP clockClass 6 clockAccuracy 0x21") {
		t.Errorf("logs:\n%s", logs)
	}
}

func TestRunSetsOnce(t *testing.T) {
	f := newFakePTP4L(t)
	cfg := fakeConfig(t, f, false)
	logs := runFor(t, cfg, 700*time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rwReqs) != 1 || f.rwReqs[0][46] != byte(protocol.SET) || f.cur.ClockClass != 6 || f.cur.ClockAccuracy != 0x21 {
		t.Fatalf("%d SETs, settings %+v\n%s", len(f.rwReqs), f.cur, logs)
	}
	if !strings.Contains(logs, `msg="PTP clockClass 248 -> 6"`) {
		t.Errorf("logs:\n%s", logs)
	}
	b, err := os.ReadFile(cfg.stateFile)
	if err != nil || !strings.HasPrefix(string(b), "1013 ") {
		t.Errorf("state %q %v", b, err)
	}
}

func TestVersion(t *testing.T) {
	if realMain([]string{"--version"}) != 0 || realMain([]string{"--bad"}) != 2 {
		t.Error("exit codes")
	}
}
