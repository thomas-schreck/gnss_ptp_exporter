package sdnotify

import (
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestNotify(t *testing.T) {
	if err := (Notifier{}).Notify("READY=1"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Net: "unixgram", Name: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	t.Setenv("NOTIFY_SOCKET", path)
	n := FromEnv()
	if !n.Enabled() {
		t.Fatal("not enabled")
	}
	if err := n.Notify("WATCHDOG=1"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	k, err := conn.Read(buf)
	if err != nil || string(buf[:k]) != "WATCHDOG=1" {
		t.Errorf("got %q, %v", buf[:k], err)
	}
}

func TestAbstract(t *testing.T) {
	name := "@sdnotify-test-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Net: "unixgram", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	t.Setenv("NOTIFY_SOCKET", name)
	if err := FromEnv().Notify("READY=1"); err != nil {
		t.Fatal(err)
	}
}

func TestWatchdog(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "")
	if _, ok := Watchdog(); ok {
		t.Error("enabled without WATCHDOG_USEC")
	}
	t.Setenv("WATCHDOG_USEC", "10000000")
	t.Setenv("WATCHDOG_PID", "")
	if d, ok := Watchdog(); !ok || d != 10*time.Second {
		t.Errorf("%v %v", d, ok)
	}
	t.Setenv("WATCHDOG_PID", "1")
	if _, ok := Watchdog(); ok {
		t.Error("watchdog meant for another process")
	}
}
