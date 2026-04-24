package smartmon

import (
	"encoding/json"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	descHealth = prometheus.NewDesc(
		"smartmon_health_passed",
		"SMART overall health assessment (1=passed, 0=failed)",
		[]string{"disk"}, nil,
	)
	descTemp = prometheus.NewDesc(
		"smartmon_temperature_celsius",
		"Drive temperature in Celsius",
		[]string{"disk"}, nil,
	)
	descPowerOn = prometheus.NewDesc(
		"smartmon_power_on_hours_total",
		"Hours the drive has been powered on",
		[]string{"disk"}, nil,
	)
	descReallocated = prometheus.NewDesc(
		"smartmon_reallocated_sectors_total",
		"Number of reallocated sectors",
		[]string{"disk"}, nil,
	)
	descPending = prometheus.NewDesc(
		"smartmon_pending_sectors",
		"Current pending sector count",
		[]string{"disk"}, nil,
	)
	descUncorrectable = prometheus.NewDesc(
		"smartmon_uncorrectable_errors_total",
		"Uncorrectable error count",
		[]string{"disk"}, nil,
	)
)

// Collector runs smartctl on a TTL and exposes SMART metrics.
// It is safe for concurrent use.
type Collector struct {
	mu       sync.Mutex
	cache    []prometheus.Metric
	cachedAt time.Time
	maxAge   time.Duration
	logger   *slog.Logger
}

func NewCollector(maxAge time.Duration, logger *slog.Logger) *Collector {
	return &Collector{maxAge: maxAge, logger: logger}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descHealth
	ch <- descTemp
	ch <- descPowerOn
	ch <- descReallocated
	ch <- descPending
	ch <- descUncorrectable
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cache != nil && time.Since(c.cachedAt) < c.maxAge {
		for _, m := range c.cache {
			ch <- m
		}
		return
	}

	c.cache = c.scrape()
	c.cachedAt = time.Now()
	for _, m := range c.cache {
		ch <- m
	}
}

func (c *Collector) scrape() []prometheus.Metric {
	devices, err := scanDevices()
	if err != nil {
		c.logger.Error("smartctl scan failed", "err", err)
		return nil
	}
	var out []prometheus.Metric
	for _, dev := range devices {
		ms, err := collectDevice(dev)
		if err != nil {
			c.logger.Warn("smartctl failed", "device", dev, "err", err)
			continue
		}
		out = append(out, ms...)
	}
	return out
}

// ---- smartctl JSON types ----

type scanResult struct {
	Devices []struct {
		Name string `json:"name"`
	} `json:"devices"`
}

type deviceData struct {
	SmartStatus struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature struct {
		Current *int `json:"current"`
	} `json:"temperature"`
	AtaSmartAttributes struct {
		Table []struct {
			ID  int `json:"id"`
			Raw struct {
				Value int `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
}

func (d *deviceData) attrRaw(id int) (int, bool) {
	for _, a := range d.AtaSmartAttributes.Table {
		if a.ID == id {
			return a.Raw.Value, true
		}
	}
	return 0, false
}

// ---- collection ----

func scanDevices() ([]string, error) {
	out, err := exec.Command("smartctl", "--scan", "--json").Output()
	if err != nil {
		// smartctl exits non-zero when no devices are found; treat that as empty.
		if len(out) == 0 {
			return nil, err
		}
	}
	var result scanResult
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(result.Devices))
	for _, d := range result.Devices {
		names = append(names, d.Name)
	}
	return names, nil
}

func collectDevice(device string) ([]prometheus.Metric, error) {
	out, err := exec.Command("smartctl", "--json", "-H", "-A", "-i", device).Output()
	if err != nil && len(out) == 0 {
		return nil, err
	}
	var data deviceData
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, err
	}

	lbl := diskLabel(device)
	var ms []prometheus.Metric

	health := 0.0
	if data.SmartStatus.Passed {
		health = 1.0
	}
	ms = append(ms, prometheus.MustNewConstMetric(descHealth, prometheus.GaugeValue, health, lbl))

	temp := data.Temperature.Current
	if temp == nil {
		if v, ok := data.attrRaw(194); ok {
			temp = &v
		}
	}
	if temp != nil {
		ms = append(ms, prometheus.MustNewConstMetric(descTemp, prometheus.GaugeValue, float64(*temp), lbl))
	}

	if v, ok := data.attrRaw(9); ok {
		ms = append(ms, prometheus.MustNewConstMetric(descPowerOn, prometheus.CounterValue, float64(v), lbl))
	}
	if v, ok := data.attrRaw(5); ok {
		ms = append(ms, prometheus.MustNewConstMetric(descReallocated, prometheus.CounterValue, float64(v), lbl))
	}
	if v, ok := data.attrRaw(197); ok {
		ms = append(ms, prometheus.MustNewConstMetric(descPending, prometheus.GaugeValue, float64(v), lbl))
	}
	if v, ok := data.attrRaw(198); ok {
		ms = append(ms, prometheus.MustNewConstMetric(descUncorrectable, prometheus.CounterValue, float64(v), lbl))
	}

	return ms, nil
}

func diskLabel(device string) string {
	s := strings.TrimPrefix(device, "/dev/")
	return strings.ReplaceAll(s, "/", "_")
}
