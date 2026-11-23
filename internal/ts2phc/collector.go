package ts2phc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sys/unix"
)

var (
	descUp = prometheus.NewDesc("ts2phc_up",
		"1 if the newest ts2phc servo line is not older than --ts2phc.max-age.", nil, nil)
	descServo = prometheus.NewDesc("ts2phc_servo_state",
		"Servo state of the newest line (0=s0 unlocked, 1=s1 jump/acquiring, 2=s2 locked, 3=s3 locked stable).", []string{"clock"}, nil)
	descOffset = prometheus.NewDesc("ts2phc_master_offset_nanoseconds",
		"Offset of the PHC from the PPS source in the newest ts2phc servo line.", []string{"clock"}, nil)
	descFreq = prometheus.NewDesc("ts2phc_frequency_ppb",
		"Frequency adjustment of the PHC in the newest ts2phc servo line, ppb.", []string{"clock"}, nil)
	descAge = prometheus.NewDesc("ts2phc_log_age_seconds",
		"Age of the newest ts2phc servo line.", []string{"clock"}, nil)
)

// Source reads recent ts2phc log output.
type Source interface {
	// Read returns recent log output and, for files, the modification
	// time (zero if unknown).
	Read(ctx context.Context) ([]byte, time.Time, error)
}

// Journal reads the systemd journal through journalctl.
type Journal struct {
	Journalctl string // path of journalctl
	Unit       string
	Lines      int
}

// Read implements Source.
func (j Journal) Read(ctx context.Context) ([]byte, time.Time, error) {
	// No shell: the unit name is passed as a single argument.
	cmd := exec.CommandContext(ctx, j.Journalctl, //nolint:gosec // fixed binary from a flag, no shell
		"--unit="+j.Unit, "--lines="+strconv.Itoa(j.Lines),
		"--output=short-unix", "--no-pager", "--quiet")
	cmd.Env = []string{"LC_ALL=C", "SYSTEMD_COLORS=0"}
	out, err := cmd.Output()
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("journalctl: %w", err)
	}
	return out, time.Time{}, nil
}

// File reads the tail of a log file that ts2phc writes with -m (stdout).
type File struct {
	Path string
	// TailBytes is how much of the file end is read.
	TailBytes int64
}

// Read implements Source.
func (f File) Read(_ context.Context) ([]byte, time.Time, error) {
	fh, err := os.Open(f.Path)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer func() { _ = fh.Close() }()
	st, err := fh.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	off := max(st.Size()-f.TailBytes, 0)
	b, err := io.ReadAll(io.NewSectionReader(fh, off, st.Size()-off))
	return b, st.ModTime(), err
}

// Age returns the age of a sample: from the journal timestamp if known,
// else from ts2phc's CLOCK_MONOTONIC line prefix, else from the file's
// modification time.
func Age(s Sample, now, mtime time.Time, mono func() (float64, bool)) (time.Duration, bool) {
	if !s.Time.IsZero() {
		return now.Sub(s.Time), true
	}
	if s.Monotonic > 0 {
		if m, ok := mono(); ok {
			return time.Duration((m - s.Monotonic) * float64(time.Second)), true
		}
	}
	if mtime.IsZero() {
		return 0, false
	}
	return now.Sub(mtime), true
}

func monotonicNow() (float64, bool) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, false
	}
	return float64(ts.Sec) + float64(ts.Nsec)/1e9, true
}

// Collector exports the newest ts2phc servo line on every scrape.
type Collector struct {
	src     Source
	maxAge  time.Duration
	timeout time.Duration
	log     *slog.Logger
	now     func() time.Time
}

// New returns a collector.
func New(src Source, maxAge, timeout time.Duration, log *slog.Logger) *Collector {
	return &Collector{src: src, maxAge: maxAge, timeout: timeout, log: log, now: time.Now}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descUp, descServo, descOffset, descFreq, descAge} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	out, mtime, err := c.src.Read(ctx)
	if err != nil {
		c.log.Warn("reading ts2phc log failed", "err", err)
		ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, 0)
		return
	}
	now := c.now()
	up := 0.0
	for _, s := range Latest(out) {
		ch <- prometheus.MustNewConstMetric(descServo, prometheus.GaugeValue, float64(s.Servo), s.Clock)
		ch <- prometheus.MustNewConstMetric(descOffset, prometheus.GaugeValue, float64(s.OffsetNs), s.Clock)
		if s.HasFreq {
			ch <- prometheus.MustNewConstMetric(descFreq, prometheus.GaugeValue, s.FreqPPB, s.Clock)
		}
		if age, ok := Age(s, now, mtime, monotonicNow); ok {
			age = max(age, 0)
			ch <- prometheus.MustNewConstMetric(descAge, prometheus.GaugeValue, age.Seconds(), s.Clock)
			if age <= c.maxAge {
				up = 1
			}
		}
	}
	ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, up)
}
