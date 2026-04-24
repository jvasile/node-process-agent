package main

import (
	_ "embed"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"time"

	pecollector "github.com/ncabatoff/process-exporter/collector"
	peconfig "github.com/ncabatoff/process-exporter/config"
	"github.com/prometheus/client_golang/prometheus"
	necollector "github.com/prometheus/node_exporter/collector"

	"github.com/jvasile/node-process-agent/remotewrite"
	"github.com/jvasile/node-process-agent/smartmon"
)

//go:embed VERSION
var versionFile string

var version = strings.TrimSpace(versionFile)

func upstreamVersion(module string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, dep := range info.Deps {
		if dep.Path == module {
			if dep.Replace != nil {
				return dep.Replace.Version
			}
			return dep.Version
		}
	}
	return "unknown"
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "node-process-agent %s\n", version)
		fmt.Fprintf(os.Stderr, "  node_exporter:    %s\n", upstreamVersion("github.com/prometheus/node_exporter"))
		fmt.Fprintf(os.Stderr, "  process-exporter: %s\n", upstreamVersion("github.com/ncabatoff/process-exporter"))
		fmt.Fprintf(os.Stderr, "\nFlags:\n")
		flag.PrintDefaults()
	}

	showVersion := flag.Bool("version", false, "Print version and exit")
	vmURL := flag.String("victoria-metrics-url", "http://localhost:8428/api/v1/write", "Victoria Metrics remote_write endpoint")
	interval := flag.Duration("interval", 15*time.Second, "Metric collection interval")
	processConfig := flag.String("process-config", "", "Path to process-exporter YAML config file")
	queueSize := flag.Int("queue-size", 100, "Max queued batches before dropping oldest")
	smartInterval := flag.Duration("smart-interval", 5*time.Minute, "How often to re-run smartctl (0 to disable SMART collection)")
	username := flag.String("username", "", "HTTP Basic Auth username for remote_write")
	passwordFile := flag.String("password-file", "", "File containing HTTP Basic Auth password")
	defaultHostname, _ := os.Hostname()
	hostname := flag.String("hostname", defaultHostname, "Hostname label attached to all metrics")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	var password string
	if *passwordFile != "" {
		info, err := os.Stat(*passwordFile)
		if err != nil {
			logger.Error("reading password file failed", "path", *passwordFile, "err", err)
			os.Exit(1)
		}
		if info.Mode().Perm()&0o004 != 0 {
			logger.Error("password file is world-readable, tighten permissions to 640 or stricter", "path", *passwordFile)
			os.Exit(1)
		}
		raw, err := os.ReadFile(*passwordFile)
		if err != nil {
			logger.Error("reading password file failed", "path", *passwordFile, "err", err)
			os.Exit(1)
		}
		password = strings.TrimRight(string(raw), "\r\n")
	}

	reg := prometheus.NewRegistry()

	nc, err := necollector.NewNodeCollector(logger)
	if err != nil {
		logger.Error("node collector init failed", "err", err)
		os.Exit(1)
	}
	if err := reg.Register(nc); err != nil {
		logger.Error("node collector register failed", "err", err)
		os.Exit(1)
	}

	if *processConfig != "" {
		cfg, err := peconfig.ReadFile(*processConfig, false)
		if err != nil {
			logger.Error("process config read failed", "path", *processConfig, "err", err)
			os.Exit(1)
		}
		pc, err := pecollector.NewProcessCollector(pecollector.ProcessCollectorOption{
			ProcFSPath:        "/proc",
			Children:          true,
			Threads:           true,
			GatherSMaps:       false,
			Namer:             cfg.MatchNamers,
			RemoveEmptyGroups: true,
		})
		if err != nil {
			logger.Error("process collector init failed", "err", err)
			os.Exit(1)
		}
		if err := reg.Register(pc); err != nil {
			logger.Error("process collector register failed", "err", err)
			os.Exit(1)
		}
	}

	if *smartInterval > 0 {
		sc := smartmon.NewCollector(*smartInterval, logger)
		if err := reg.Register(sc); err != nil {
			logger.Error("smartmon collector register failed", "err", err)
			os.Exit(1)
		}
	}

	w := remotewrite.New(*vmURL, reg, logger, remotewrite.Options{
		QueueSize: *queueSize,
		Username:  *username,
		Password:  password,
		Hostname:  *hostname,
	})
	logger.Info("starting", "url", *vmURL, "interval", *interval, "hostname", *hostname)
	w.Run(*interval)
}
