package ptp4l

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/facebook/time/ptp/protocol"
	"github.com/prometheus/client_golang/prometheus"
)

// maxPorts bounds the PORT_DATA_SET queries per scrape.
const maxPorts = 64

var (
	descUp = prometheus.NewDesc("ptp4l_up",
		"1 if ptp4l answered on its management socket.", nil, nil)
	descPortState = prometheus.NewDesc("ptp4l_port_state",
		"PTP port state, IEEE 1588 numbering (1=INITIALIZING, 2=FAULTY, 3=DISABLED, 4=LISTENING, 5=PRE_MASTER, 6=MASTER, 7=PASSIVE, 8=UNCALIBRATED, 9=SLAVE).",
		[]string{"port"}, nil)
	descGMRole = prometheus.NewDesc("ptp4l_grandmaster_role",
		"1 if this clock is the grandmaster (its identity is the grandmaster identity and a port is MASTER); 0 while ptp4l_up is 0.", nil, nil)
	descClockClass = prometheus.NewDesc("ptp4l_clock_class",
		"clockClass of the local clock (DEFAULT_DATA_SET), which a grandmaster announces (6 locked to a primary reference, 7 holdover, 248 default).", nil, nil)
	descClockAccuracy = prometheus.NewDesc("ptp4l_clock_accuracy",
		"clockAccuracy enumeration of the local clock (e.g. 0x21 = 33 = within 100 ns, 0xFE = 254 = unknown).", nil, nil)
	descGMClockClass = prometheus.NewDesc("ptp4l_grandmaster_clock_class",
		"clockClass of the current grandmaster (PARENT_DATA_SET).", nil, nil)
	descStepsRemoved = prometheus.NewDesc("ptp4l_steps_removed",
		"Number of boundary clocks between this clock and the grandmaster.", nil, nil)
	descOffset = prometheus.NewDesc("ptp4l_offset_from_master_nanoseconds",
		"Offset from the master (CURRENT_DATA_SET); 0 on a grandmaster.", nil, nil)
	descPathDelay = prometheus.NewDesc("ptp4l_mean_path_delay_nanoseconds",
		"Mean path delay to the master (CURRENT_DATA_SET); 0 on a grandmaster.", nil, nil)
	descGMPresent = prometheus.NewDesc("ptp4l_gm_present",
		"1 if ptp4l is synchronized to a remote grandmaster (TIME_STATUS_NP gmPresent); 0 on a grandmaster itself.", nil, nil)
	descInfo = prometheus.NewDesc("ptp4l_clock_info",
		"Identities of the local clock and the grandmaster; value is always 1.",
		[]string{"clock_identity", "grandmaster_identity", "domain"}, nil)
)

// State is what one scrape learns from ptp4l.
type State struct {
	Default *protocol.DefaultDataSetTLV
	Parent  *protocol.ParentDataSetTLV
	Current *protocol.CurrentDataSetTLV
	Time    *protocol.TimeStatusNPTLV
	Ports   []*PortDataSetTLV
}

// GrandmasterRole reports whether this clock is the grandmaster.
func (s State) GrandmasterRole() bool {
	if s.Default == nil || s.Parent == nil || s.Default.ClockIdentity != s.Parent.GrandmasterIdentity {
		return false
	}
	for _, p := range s.Ports {
		if p.PortState == protocol.PortStateMaster || p.PortState == protocol.PortStateGrandMaster {
			return true
		}
	}
	return false
}

// Query reads the data sets from ptp4l.
func Query(path string, domain uint8, timeout time.Duration) (State, error) {
	var s State
	c, err := Dial(path, domain, time.Now().Add(timeout))
	if err != nil {
		return s, err
	}
	defer func() { _ = c.Close() }()

	tlv, err := c.Get(protocol.IDDefaultDataSet, AllPorts)
	if err != nil {
		return s, fmt.Errorf("DEFAULT_DATA_SET: %w", err)
	}
	var ok bool
	if s.Default, ok = tlv.(*protocol.DefaultDataSetTLV); !ok {
		return s, fmt.Errorf("DEFAULT_DATA_SET: unexpected %T", tlv)
	}
	if tlv, err = c.Get(protocol.IDParentDataSet, AllPorts); err != nil {
		return s, fmt.Errorf("PARENT_DATA_SET: %w", err)
	}
	s.Parent, _ = tlv.(*protocol.ParentDataSetTLV)
	if tlv, err = c.Get(protocol.IDCurrentDataSet, AllPorts); err != nil {
		return s, fmt.Errorf("CURRENT_DATA_SET: %w", err)
	}
	s.Current, _ = tlv.(*protocol.CurrentDataSetTLV)
	// TIME_STATUS_NP is a linuxptp extension; tolerate its absence.
	if tlv, err = c.Get(protocol.IDTimeStatusNP, AllPorts); err == nil {
		s.Time, _ = tlv.(*protocol.TimeStatusNPTLV)
	}
	n := min(int(s.Default.NumberPorts), maxPorts)
	for port := 1; port <= n; port++ {
		tlv, err := c.Get(protocol.IDPortDataSet, uint16(port))
		if err != nil {
			return s, fmt.Errorf("PORT_DATA_SET port %d: %w", port, err)
		}
		if p, ok := tlv.(*PortDataSetTLV); ok {
			s.Ports = append(s.Ports, p)
		}
	}
	return s, nil
}

// Collector queries ptp4l on every scrape.
type Collector struct {
	path    string
	domain  uint8
	timeout time.Duration
	log     *slog.Logger
}

// New returns a collector for the management socket at path.
func New(path string, domain uint8, timeout time.Duration, log *slog.Logger) *Collector {
	return &Collector{path: path, domain: domain, timeout: timeout, log: log}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descUp, descPortState, descGMRole, descClockClass, descClockAccuracy,
		descGMClockClass, descStepsRemoved, descOffset, descPathDelay, descGMPresent, descInfo} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	s, err := Query(c.path, c.domain, c.timeout)
	if err != nil {
		c.log.Warn("querying ptp4l failed", "socket", c.path, "err", err)
		ch <- prometheus.MustNewConstMetric(descUp, prometheus.GaugeValue, 0)
		ch <- prometheus.MustNewConstMetric(descGMRole, prometheus.GaugeValue, 0)
		return
	}
	Emit(ch, s)
}

// Emit writes the metrics for a successful query.
func Emit(ch chan<- prometheus.Metric, s State) {
	g := func(d *prometheus.Desc, v float64, l ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, l...)
	}
	g(descUp, 1)
	g(descGMRole, b2f(s.GrandmasterRole()))
	if d := s.Default; d != nil {
		g(descClockClass, float64(d.ClockQuality.ClockClass))
		g(descClockAccuracy, float64(d.ClockQuality.ClockAccuracy))
		gm := ""
		if s.Parent != nil {
			gm = s.Parent.GrandmasterIdentity.String()
		}
		g(descInfo, 1, d.ClockIdentity.String(), gm, strconv.Itoa(int(d.DomainNumber)))
	}
	if p := s.Parent; p != nil {
		g(descGMClockClass, float64(p.GrandmasterClockQuality.ClockClass))
	}
	if cur := s.Current; cur != nil {
		g(descStepsRemoved, float64(cur.StepsRemoved))
		g(descOffset, cur.OffsetFromMaster.Nanoseconds())
		g(descPathDelay, cur.MeanPathDelay.Nanoseconds())
	}
	if t := s.Time; t != nil {
		g(descGMPresent, b2f(t.GMPresent != 0))
	}
	for _, p := range s.Ports {
		g(descPortState, float64(p.PortState), strconv.Itoa(int(p.PortIdentity.PortNumber)))
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
