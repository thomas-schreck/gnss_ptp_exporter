package gpsd

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	maxLine        = 1 << 20 // gpsd SKY reports of multi-band receivers exceed 8 KiB
	readTimeout    = 10 * time.Second
	reconnectDelay = 3 * time.Second
)

var (
	descUp = prometheus.NewDesc("gpsd_up",
		"1 if gpsd is connected and sent a TPV report within --gpsd.max-age.", nil, nil)
	descFixMode = prometheus.NewDesc("gpsd_fix_mode",
		"gpsd fix mode of the latest TPV report (0=unknown, 1=no fix, 2=2D, 3=3D); 0 while gpsd_up is 0.", nil, nil)
	descVisible = prometheus.NewDesc("gpsd_satellites_visible",
		"Satellites in the latest SKY report, each counted once across its signals.", nil, nil)
	descUsed = prometheus.NewDesc("gpsd_satellites_used",
		"Satellites used in the solution, each counted once across its signals.", nil, nil)
	descVisibleByGNSS = prometheus.NewDesc("gpsd_satellites_visible_by_gnss",
		"Satellites in the latest SKY report by constellation (gnss=\"all\" for the sum).", []string{"gnss"}, nil)
	descUsedByGNSS = prometheus.NewDesc("gpsd_satellites_used_by_gnss",
		"Satellites used in the solution by constellation (gnss=\"all\" for the sum).", []string{"gnss"}, nil)
	descSNRMean = prometheus.NewDesc("gpsd_snr_used_mean_dbhz",
		"Mean C/N0 of the satellites used in the solution (best signal per satellite), by constellation.", []string{"gnss"}, nil)
	descSNRMax = prometheus.NewDesc("gpsd_snr_used_max_dbhz",
		"Max C/N0 of the satellites used in the solution, by constellation.", []string{"gnss"}, nil)
	descSkyAge = prometheus.NewDesc("gpsd_sky_age_seconds",
		"Seconds since the last SKY report with a satellite list.", nil, nil)
)

// Collector keeps a streaming connection to gpsd and exports the latest
// TPV and SKY state on every scrape.
type Collector struct {
	addr   string
	maxAge time.Duration
	log    *slog.Logger
	now    func() time.Time

	mu        sync.Mutex
	connected bool
	tpvAt     time.Time
	mode      int
	skyAt     time.Time
	sky       Summary
}

// New returns a collector for the gpsd at addr (host:port). Data older
// than maxAge counts as missing.
func New(addr string, maxAge time.Duration, log *slog.Logger) *Collector {
	return &Collector{addr: addr, maxAge: maxAge, log: log, now: time.Now}
}

// Run keeps the gpsd connection up until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	for {
		err := c.stream(ctx)
		c.mu.Lock()
		c.connected = false
		c.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		c.log.Warn("gpsd connection lost, reconnecting", "addr", c.addr, "err", err, "in", reconnectDelay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay):
		}
	}
}

func (c *Collector) stream(ctx context.Context) error {
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := d.DialContext(dctx, "tcp", c.addr)
	cancel()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if _, err := conn.Write([]byte("?WATCH={\"enable\":true,\"json\":true}\n")); err != nil {
		return err
	}
	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	for {
		// A wedged gpsd can keep the socket open without sending anything.
		if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			return err
		}
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return err
			}
			return fmt.Errorf("gpsd closed the connection")
		}
		c.HandleLine(sc.Bytes())
	}
}

// HandleLine applies one line of gpsd output to the collector state.
func (c *Collector) HandleLine(line []byte) {
	r, err := ParseLine(line)
	if err != nil {
		return
	}
	now := c.now()
	switch r.Class {
	case "TPV":
		c.mu.Lock()
		c.tpvAt = now
		c.mode = r.Mode
		c.mu.Unlock()
	case "SKY":
		// gpsd also sends DOP-only SKY reports without satellites; and a
		// ZED-F9T alternates between per-signal and per-satellite lists.
		// Deduplication makes both kinds give the same counts.
		if len(r.Satellites) == 0 {
			return
		}
		s := Summarize(Dedupe(r.Satellites))
		c.mu.Lock()
		c.skyAt = now
		c.sky = s
		c.mu.Unlock()
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descUp, descFixMode, descVisible, descUsed,
		descVisibleByGNSS, descUsedByGNSS, descSNRMean, descSNRMax, descSkyAge} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	up := c.connected && !c.tpvAt.IsZero() && now.Sub(c.tpvAt) <= c.maxAge
	mode := 0
	if up {
		mode = c.mode
	}
	ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, b2f(up))
	ch <- prometheus.MustNewConstMetric(descFixMode, prometheus.GaugeValue, float64(mode))

	if c.skyAt.IsZero() {
		return
	}
	ch <- prometheus.MustNewConstMetric(descSkyAge, prometheus.GaugeValue, now.Sub(c.skyAt).Seconds())
	if !c.connected || now.Sub(c.skyAt) > c.maxAge {
		return
	}
	s := c.sky
	ch <- prometheus.MustNewConstMetric(descVisible, prometheus.GaugeValue, float64(s.All.Visible))
	ch <- prometheus.MustNewConstMetric(descUsed, prometheus.GaugeValue, float64(s.All.Used))
	emit := func(name string, st ConstellationStats) {
		ch <- prometheus.MustNewConstMetric(descVisibleByGNSS, prometheus.GaugeValue, float64(st.Visible), name)
		ch <- prometheus.MustNewConstMetric(descUsedByGNSS, prometheus.GaugeValue, float64(st.Used), name)
		if st.SNRCount > 0 {
			ch <- prometheus.MustNewConstMetric(descSNRMean, prometheus.GaugeValue, st.SNRMean(), name)
			ch <- prometheus.MustNewConstMetric(descSNRMax, prometheus.GaugeValue, st.SNRMax, name)
		}
	}
	for _, name := range s.Systems {
		emit(name, s.ByGNSS[name])
	}
	emit("all", s.All)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
