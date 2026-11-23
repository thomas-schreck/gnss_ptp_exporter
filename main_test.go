package main

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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

// The exporter must not be able to change ptp4l. The only code that
// builds a management SET is the clockClass helper's gmsettings package,
// which Go's internal rule keeps out of the exporter; check both.
func TestExporterCannotSet(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.Contains(pkg, "/cmd/") {
			t.Errorf("exporter depends on %s", pkg)
		}
	}
	err = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, w := range []string{"protocol.SET", "protocol.COMMAND"} {
			if bytes.Contains(b, []byte(w)) && path != filepath.Join("cmd", "gnss_ptp_clockclass", "internal", "gmsettings", "gmsettings.go") {
				t.Errorf("%s uses %s", path, w)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
