// Package sdnotify implements the parts of systemd's sd_notify protocol
// the helper needs: READY=1, WATCHDOG=1 and STOPPING=1 over
// $NOTIFY_SOCKET, and the watchdog interval from $WATCHDOG_USEC.
package sdnotify

import (
	"net"
	"os"
	"strconv"
	"time"
)

// Notifier sends state strings to systemd. The zero value (no socket)
// does nothing.
type Notifier struct {
	addr *net.UnixAddr
}

// FromEnv returns a notifier for $NOTIFY_SOCKET (empty: no-op). A leading
// "@" names an abstract socket, which package net handles itself.
func FromEnv() Notifier {
	p := os.Getenv("NOTIFY_SOCKET")
	if p == "" {
		return Notifier{}
	}
	return Notifier{addr: &net.UnixAddr{Net: "unixgram", Name: p}}
}

// Enabled reports whether a socket is configured.
func (n Notifier) Enabled() bool { return n.addr != nil }

// Notify sends one state string, e.g. "READY=1".
func (n Notifier) Notify(state string) error {
	if n.addr == nil {
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, n.addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte(state))
	return err
}

// Watchdog returns the interval systemd expects pings within
// ($WATCHDOG_USEC), if the watchdog is enabled for this process.
func Watchdog() (time.Duration, bool) {
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return 0, false
	}
	us, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || us <= 0 {
		return 0, false
	}
	return time.Duration(us) * time.Microsecond, true
}
