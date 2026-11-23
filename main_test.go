package main

import (
	"io"
	"strings"
	"testing"
)

func TestParseFlagsDefaults(t *testing.T) {
	c, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.listen != ":10063" || !c.gpsd || !c.ptp4l || !c.ts2phc || c.ublox {
		t.Errorf("defaults %+v", c)
	}
	if c.ptp4lSocket != "/var/run/ptp4lro" || c.ts2phcSource != "journald" {
		t.Errorf("defaults %+v", c)
	}
}

func TestParseFlagsErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--ptp4l.domain=256"},
		{"--ts2phc.source=syslog"},
		{"--ts2phc.source=file"},
		{"--ts2phc.lines=0"},
		{"--ublox.interval=10ms"},
		{"--web.telemetry-path=metrics"},
		{"extra"},
		{"--no-such-flag"},
	} {
		if _, err := parseFlags(args, io.Discard); err == nil {
			t.Errorf("%s accepted", strings.Join(args, " "))
		}
	}
	if _, err := parseFlags([]string{"--ts2phc.source=file", "--collector.ts2phc=false"}, io.Discard); err != nil {
		t.Errorf("disabled collector still validated: %v", err)
	}
	if _, err := parseFlags([]string{"--ts2phc.source=file", "--ts2phc.file=/var/log/ts2phc.log"}, io.Discard); err != nil {
		t.Error(err)
	}
}

func TestLogger(t *testing.T) {
	if _, err := newLogger("verbose"); err == nil {
		t.Error("bad level accepted")
	}
	if _, err := newLogger("debug"); err != nil {
		t.Error(err)
	}
}
