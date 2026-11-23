package ubx

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	descPollSuccess = prometheus.NewDesc("gnss_ubx_poll_success",
		"1 if the last UBX poll returned both MON-RF and NAV-STATUS.", nil, nil)
	descPollTime = prometheus.NewDesc("gnss_ubx_last_poll_timestamp_seconds",
		"Unix time of the last UBX poll attempt.", nil, nil)
	descJamInd = prometheus.NewDesc("gnss_rf_jamming_indicator",
		"CW jamming indicator per RF block (0 = none, 255 = strong).", []string{"block"}, nil)
	descJamState = prometheus.NewDesc("gnss_rf_jamming_state",
		"Receiver jamming state per RF block (0=unknown or disabled, 1=ok, 2=warning, 3=critical).", []string{"block"}, nil)
	descNoise = prometheus.NewDesc("gnss_rf_noise_per_ms",
		"Noise level per RF block as measured by the receiver.", []string{"block"}, nil)
	descAGC = prometheus.NewDesc("gnss_rf_agc_ratio",
		"AGC monitor per RF block, agcCnt/8191 (0..1).", []string{"block"}, nil)
	descAntStatus = prometheus.NewDesc("gnss_rf_antenna_status",
		"Antenna supervisor state per RF block (0=INIT, 1=DONTKNOW, 2=OK, 3=SHORT, 4=OPEN).", []string{"block"}, nil)
	descAntPower = prometheus.NewDesc("gnss_rf_antenna_power",
		"Antenna power state per RF block (0=OFF, 1=ON, 2=DONTKNOW).", []string{"block"}, nil)
	descSpoof = prometheus.NewDesc("gnss_spoofing_state",
		"Receiver spoofing detection from NAV-STATUS (0=unknown or off, 1=no spoofing, 2=spoofing indicated, 3=multiple indications).", nil, nil)
)

// Result is the outcome of one poll.
type Result struct {
	At        time.Time
	RF        []RFBlock
	HaveRF    bool
	Nav       NavStatus
	HaveNav   bool
	Device    string
	LastError error
}

// Collector polls a u-blox receiver through gpsd at a fixed interval and
// exports the last result. Polling runs in the background so that a scrape
// never waits for the serial line.
type Collector struct {
	addr     string
	device   string
	interval time.Duration
	timeout  time.Duration
	log      *slog.Logger

	mu   sync.Mutex
	last Result
}

// New returns a collector. device may be empty: the first gpsd device whose
// driver is u-blox is used.
func New(addr, device string, interval, timeout time.Duration, log *slog.Logger) *Collector {
	return &Collector{addr: addr, device: device, interval: interval, timeout: timeout, log: log}
}

// Run polls until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		pctx, cancel := context.WithTimeout(ctx, c.timeout)
		r := PollGPSD(pctx, c.addr, c.device)
		cancel()
		if r.LastError != nil && ctx.Err() == nil {
			c.log.Warn("UBX poll incomplete", "addr", c.addr, "device", r.Device, "err", r.LastError)
		}
		c.mu.Lock()
		c.last = r
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PollGPSD asks gpsd to forward a MON-RF and a NAV-STATUS poll to the
// receiver and collects the answers from gpsd's raw (binary) stream. Both
// are poll requests without payload; they do not change the receiver
// configuration.
func PollGPSD(ctx context.Context, addr, device string) (r Result) {
	r.At = time.Now()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		r.LastError = err
		return r
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	br := bufio.NewReaderSize(conn, 64*1024)
	if device == "" {
		device, err = findDevice(conn, br)
		if err != nil {
			r.LastError = err
			return r
		}
	}
	r.Device = device
	dev, _ := json.Marshal(device)
	cmds := fmt.Sprintf("?WATCH={\"device\":%s,\"enable\":true,\"raw\":2}\n", dev) +
		fmt.Sprintf("?DEVICE={\"path\":%s,\"hexdata\":%q}\n", dev, hex.EncodeToString(Poll(ClassMON, IDMonRF))) +
		fmt.Sprintf("?DEVICE={\"path\":%s,\"hexdata\":%q}\n", dev, hex.EncodeToString(Poll(ClassNAV, IDNavStatus)))
	if _, err := conn.Write([]byte(cmds)); err != nil {
		r.LastError = err
		return r
	}

	var sc Scanner
	buf := make([]byte, 4096)
	for !r.HaveRF || !r.HaveNav {
		n, err := br.Read(buf)
		_, _ = sc.Write(buf[:n])
		for {
			f, ok := sc.Next()
			if !ok {
				break
			}
			r.apply(f)
		}
		if err != nil {
			if !r.HaveRF || !r.HaveNav {
				r.LastError = fmt.Errorf("waiting for %s: %w", r.missing(), err)
			}
			return r
		}
	}
	return r
}

func (r *Result) apply(f Frame) {
	switch {
	case f.Class == ClassMON && f.ID == IDMonRF:
		if b, err := ParseMonRF(f.Payload); err == nil {
			r.RF, r.HaveRF = b, true
		}
	case f.Class == ClassNAV && f.ID == IDNavStatus:
		if n, err := ParseNavStatus(f.Payload); err == nil {
			r.Nav, r.HaveNav = n, true
		}
	}
}

func (r *Result) missing() string {
	var m []string
	if !r.HaveRF {
		m = append(m, "MON-RF")
	}
	if !r.HaveNav {
		m = append(m, "NAV-STATUS")
	}
	return strings.Join(m, " and ")
}

// findDevice asks gpsd for its devices and returns the first u-blox one.
func findDevice(conn net.Conn, br *bufio.Reader) (string, error) {
	if _, err := conn.Write([]byte("?DEVICES;\n")); err != nil {
		return "", err
	}
	for {
		line, err := br.ReadSlice('\n')
		if err != nil {
			return "", fmt.Errorf("reading gpsd device list: %w", err)
		}
		if dev, ok, err := DeviceFromDevices(line); ok || err != nil {
			return dev, err
		}
	}
}

// ErrNoUblox is returned when gpsd reports no u-blox device.
var ErrNoUblox = errors.New("gpsd reports no u-blox device")

// DeviceFromDevices inspects one gpsd JSON line. For a DEVICES report it
// returns the path of the first u-blox device (ok=true) or ErrNoUblox.
func DeviceFromDevices(line []byte) (string, bool, error) {
	var v struct {
		Class   string `json:"class"`
		Devices []struct {
			Path   string `json:"path"`
			Driver string `json:"driver"`
		} `json:"devices"`
	}
	if json.Unmarshal(line, &v) != nil || v.Class != "DEVICES" {
		return "", false, nil
	}
	for _, d := range v.Devices {
		if strings.Contains(strings.ToLower(d.Driver), "u-blox") && d.Path != "" {
			return d.Path, true, nil
		}
	}
	return "", false, ErrNoUblox
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descPollSuccess, descPollTime, descJamInd, descJamState,
		descNoise, descAGC, descAntStatus, descAntPower, descSpoof} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	r := c.last
	c.mu.Unlock()
	Emit(ch, r)
}

// Emit writes the metrics of one poll result.
func Emit(ch chan<- prometheus.Metric, r Result) {
	if r.At.IsZero() {
		ch <- prometheus.MustNewConstMetric(descPollSuccess, prometheus.GaugeValue, 0)
		return
	}
	ok := 0.0
	if r.HaveRF && r.HaveNav {
		ok = 1
	}
	ch <- prometheus.MustNewConstMetric(descPollSuccess, prometheus.GaugeValue, ok)
	ch <- prometheus.MustNewConstMetric(descPollTime, prometheus.GaugeValue, float64(r.At.UnixNano())/1e9)
	seen := map[uint8]bool{}
	for _, b := range r.RF {
		// A malformed MON-RF may repeat a block ID; a duplicate series
		// would fail the whole scrape.
		if seen[b.BlockID] {
			continue
		}
		seen[b.BlockID] = true
		id := strconv.Itoa(int(b.BlockID))
		ch <- prometheus.MustNewConstMetric(descJamInd, prometheus.GaugeValue, float64(b.JamInd), id)
		ch <- prometheus.MustNewConstMetric(descJamState, prometheus.GaugeValue, float64(b.JammingState), id)
		ch <- prometheus.MustNewConstMetric(descNoise, prometheus.GaugeValue, float64(b.NoisePerMS), id)
		ch <- prometheus.MustNewConstMetric(descAGC, prometheus.GaugeValue, b.AGCRatio(), id)
		ch <- prometheus.MustNewConstMetric(descAntStatus, prometheus.GaugeValue, float64(b.AntStatus), id)
		ch <- prometheus.MustNewConstMetric(descAntPower, prometheus.GaugeValue, float64(b.AntPower), id)
	}
	if r.HaveNav {
		ch <- prometheus.MustNewConstMetric(descSpoof, prometheus.GaugeValue, float64(r.Nav.SpoofDetState))
	}
}
