// Command gnss_ptp_exporter exports Prometheus metrics of a GNSS/PTP time
// server: gpsd, ptp4l, ts2phc and (optionally) a u-blox receiver.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/thomas-schreck/gnss_ptp_exporter/internal/gpsd"
	"github.com/thomas-schreck/gnss_ptp_exporter/internal/ptp4l"
	"github.com/thomas-schreck/gnss_ptp_exporter/internal/ts2phc"
	"github.com/thomas-schreck/gnss_ptp_exporter/internal/ubx"
)

// DefaultListenAddress uses port 10063, unused in the Prometheus default
// port allocations list when this exporter was written.
const DefaultListenAddress = ":10063"

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

type config struct {
	listen      string
	metricsPath string
	logLevel    string

	gpsd       bool
	gpsdAddr   string
	gpsdMaxAge time.Duration

	ptp4l        bool
	ptp4lSocket  string
	ptp4lDomain  uint
	ptp4lTimeout time.Duration

	ts2phc        bool
	ts2phcSource  string
	ts2phcUnit    string
	ts2phcFile    string
	ts2phcLines   int
	ts2phcMaxAge  time.Duration
	ts2phcJournal string

	ublox         bool
	ubloxDevice   string
	ubloxInterval time.Duration
	ubloxTimeout  time.Duration

	showVersion bool
}

func parseFlags(args []string, stderr io.Writer) (config, error) {
	var c config
	fs := flag.NewFlagSet("gnss_ptp_exporter", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.listen, "web.listen-address", DefaultListenAddress, "Address to listen on for HTTP requests.")
	fs.StringVar(&c.metricsPath, "web.telemetry-path", "/metrics", "Path under which to expose metrics.")
	fs.StringVar(&c.logLevel, "log.level", "info", "Log level: debug, info, warn, error.")

	fs.BoolVar(&c.gpsd, "collector.gpsd", true, "Enable the gpsd collector.")
	fs.StringVar(&c.gpsdAddr, "gpsd.address", "localhost:2947", "gpsd address (host:port).")
	fs.DurationVar(&c.gpsdMaxAge, "gpsd.max-age", 10*time.Second, "Reports older than this count as missing.")

	fs.BoolVar(&c.ptp4l, "collector.ptp4l", true, "Enable the ptp4l collector.")
	fs.StringVar(&c.ptp4lSocket, "ptp4l.socket", "/var/run/ptp4lro", "ptp4l management socket; the read-only socket (uds_ro_address) is enough.")
	fs.UintVar(&c.ptp4lDomain, "ptp4l.domain", 0, "PTP domain number of the ptp4l instance.")
	fs.DurationVar(&c.ptp4lTimeout, "ptp4l.timeout", 2*time.Second, "Timeout for all ptp4l queries of one scrape.")

	fs.BoolVar(&c.ts2phc, "collector.ts2phc", true, "Enable the ts2phc collector.")
	fs.StringVar(&c.ts2phcSource, "ts2phc.source", "journald", "Where to read ts2phc's log: journald or file.")
	fs.StringVar(&c.ts2phcUnit, "ts2phc.unit", "ts2phc.service", "systemd unit of ts2phc (source journald).")
	fs.StringVar(&c.ts2phcJournal, "ts2phc.journalctl", "journalctl", "journalctl binary (source journald).")
	fs.IntVar(&c.ts2phcLines, "ts2phc.lines", 100, "Journal lines to read per scrape (source journald).")
	fs.StringVar(&c.ts2phcFile, "ts2phc.file", "", "Log file ts2phc writes to (source file).")
	fs.DurationVar(&c.ts2phcMaxAge, "ts2phc.max-age", time.Minute, "ts2phc_up is 0 when the newest servo line is older.")

	fs.BoolVar(&c.ublox, "collector.ublox", false, "Enable the u-blox collector (UBX MON-RF and NAV-STATUS polled through gpsd).")
	fs.StringVar(&c.ubloxDevice, "ublox.device", "", "gpsd device path of the receiver; empty: the first u-blox device gpsd reports.")
	fs.DurationVar(&c.ubloxInterval, "ublox.interval", time.Minute, "How often to poll the receiver.")
	fs.DurationVar(&c.ubloxTimeout, "ublox.timeout", 5*time.Second, "Timeout of one poll.")

	fs.BoolVar(&c.showVersion, "version", false, "Print the version and exit.")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if c.ptp4lDomain > 255 {
		return c, fmt.Errorf("--ptp4l.domain must be 0..255")
	}
	switch c.ts2phcSource {
	case "journald":
		if c.ts2phcLines < 1 {
			return c, fmt.Errorf("--ts2phc.lines must be positive")
		}
	case "file":
		if c.ts2phc && c.ts2phcFile == "" {
			return c, fmt.Errorf("--ts2phc.source=file needs --ts2phc.file")
		}
	default:
		return c, fmt.Errorf("--ts2phc.source must be journald or file")
	}
	if c.ubloxInterval < time.Second || c.ubloxTimeout <= 0 {
		return c, fmt.Errorf("--ublox.interval must be at least 1s and --ublox.timeout positive")
	}
	if !strings.HasPrefix(c.metricsPath, "/") {
		return c, fmt.Errorf("--web.telemetry-path must start with /")
	}
	return c, nil
}

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "devel"
}

func main() {
	cfg, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if cfg.showVersion {
		fmt.Println("gnss_ptp_exporter", buildVersion())
		return
	}
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newLogger(level string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("--log.level: %w", err)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})), nil
}

func run(cfg config) error {
	log, err := newLogger(cfg.logLevel)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo(),
	)
	if err := register(ctx, reg, cfg, log); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle(cfg.metricsPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorLog:            slog.NewLogLogger(log.Handler(), slog.LevelError),
		MaxRequestsInFlight: 4,
		Timeout:             30 * time.Second,
	}))
	if cfg.metricsPath != "/" {
		landing := []byte("<html><head><title>gnss_ptp_exporter</title></head><body>" +
			"<h1>gnss_ptp_exporter</h1><p><a href=\"" + html.EscapeString(cfg.metricsPath) +
			"\">Metrics</a></p></body></html>\n")
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(landing)
		})
	}
	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "address", cfg.listen, "version", buildVersion())

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func buildInfo() prometheus.Collector {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "gnss_ptp_exporter_build_info",
		Help:        "Version of gnss_ptp_exporter and the Go toolchain it was built with; value is always 1.",
		ConstLabels: prometheus.Labels{"version": buildVersion(), "goversion": runtime.Version()},
	})
	g.Set(1)
	return g
}

func register(ctx context.Context, reg *prometheus.Registry, cfg config, log *slog.Logger) error {
	if cfg.gpsd {
		c := gpsd.New(cfg.gpsdAddr, cfg.gpsdMaxAge, log.With("collector", "gpsd"))
		go c.Run(ctx)
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	if cfg.ptp4l {
		c := ptp4l.New(cfg.ptp4lSocket, uint8(cfg.ptp4lDomain), cfg.ptp4lTimeout, log.With("collector", "ptp4l")) //nolint:gosec // range checked in parseFlags
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	if cfg.ts2phc {
		var src ts2phc.Source = ts2phc.Journal{Journalctl: cfg.ts2phcJournal, Unit: cfg.ts2phcUnit, Lines: cfg.ts2phcLines}
		if cfg.ts2phcSource == "file" {
			src = ts2phc.File{Path: cfg.ts2phcFile, TailBytes: 64 << 10}
		}
		c := ts2phc.New(src, cfg.ts2phcMaxAge, 5*time.Second, log.With("collector", "ts2phc"))
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	if cfg.ublox {
		c := ubx.New(cfg.gpsdAddr, cfg.ubloxDevice, cfg.ubloxInterval, cfg.ubloxTimeout, log.With("collector", "ublox"))
		go c.Run(ctx)
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	return nil
}
