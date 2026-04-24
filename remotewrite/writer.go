package remotewrite

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/golang/snappy"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus"
)

const maxBackoff = 5 * time.Minute

// Options configures a Writer.
type Options struct {
	QueueSize int
	Username  string
	Password  string
	Hostname  string // stamped as "host" label on every series
}

// Writer gathers metrics from a prometheus.Gatherer and pushes them to a
// remote_write endpoint on a fixed interval, with retry on failure.
type Writer struct {
	url      string
	gatherer prometheus.Gatherer
	logger   *slog.Logger
	queue    chan []timeSeries
	client   *http.Client
	opts     Options
}

// New creates a Writer.
func New(url string, gatherer prometheus.Gatherer, logger *slog.Logger, opts Options) *Writer {
	return &Writer{
		url:      url,
		gatherer: gatherer,
		logger:   logger,
		queue:    make(chan []timeSeries, opts.QueueSize),
		client:   &http.Client{Timeout: 30 * time.Second},
		opts:     opts,
	}
}

// Run starts the collection ticker and the sender loop. It blocks until the
// process exits.
func (w *Writer) Run(interval time.Duration) {
	go w.collect(interval)
	w.send()
}

func (w *Writer) collect(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		mfs, err := w.gatherer.Gather()
		if err != nil {
			var multiErr prometheus.MultiError
			if !errors.As(err, &multiErr) {
				w.logger.Error("gather failed", "err", err)
				continue
			}
			for _, e := range multiErr {
				w.logger.Debug("partial gather error", "err", e)
			}
		}
		w.logger.Debug("gathered", "families", len(mfs))
		if w.logger.Enabled(nil, slog.LevelDebug) {
			names := make([]string, len(mfs))
			for i, mf := range mfs {
				names[i] = mf.GetName()
			}
			w.logger.Debug("metric families", "names", names)
		}
		batch := familiesToTimeSeries(mfs, time.Now(), w.opts.Hostname)
		w.logger.Debug("batch", "series", len(batch))
		select {
		case w.queue <- batch:
		default:
			// Queue full: drop oldest to make room for current reading.
			<-w.queue
			w.queue <- batch
			w.logger.Warn("queue full, dropped oldest batch")
		}
	}
}

// send drains the queue, retrying each batch with exponential backoff until it
// succeeds. This means newer batches wait behind a failing one, which
// preserves ordering and prevents data loss during transient outages.
func (w *Writer) send() {
	for batch := range w.queue {
		backoff := time.Second
		for {
			if err := w.write(batch); err == nil {
				break
			} else {
				w.logger.Error("remote write failed", "err", err, "retry_in", backoff)
			}
			time.Sleep(backoff)
			backoff = min(backoff*2, maxBackoff)
		}
	}
}

func (w *Writer) write(tss []timeSeries) error {
	payload := encodeWriteRequest(tss)
	compressed := snappy.Encode(nil, payload)

	req, err := http.NewRequest(http.MethodPost, w.url, bytes.NewReader(compressed))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	if w.opts.Username != "" && w.opts.Password != "" {
		req.SetBasicAuth(w.opts.Username, w.opts.Password)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}
	return nil
}

// ---- metric conversion ----

type label struct {
	name, value string
}

type sample struct {
	value     float64
	timestamp int64 // Unix milliseconds
}

type timeSeries struct {
	labels  []label
	samples []sample
}

func familiesToTimeSeries(mfs []*dto.MetricFamily, now time.Time, hostname string) []timeSeries {
	tsMs := now.UnixMilli()
	var out []timeSeries

	for _, mf := range mfs {
		name := mf.GetName()
		for _, m := range mf.GetMetric() {
			base := baseLabels(name, m.GetLabel(), hostname)

			switch mf.GetType() {
			case dto.MetricType_GAUGE:
				out = append(out, series(base, m.GetGauge().GetValue(), tsMs))
			case dto.MetricType_COUNTER:
				out = append(out, series(base, m.GetCounter().GetValue(), tsMs))
			case dto.MetricType_UNTYPED:
				out = append(out, series(base, m.GetUntyped().GetValue(), tsMs))

			case dto.MetricType_SUMMARY:
				s := m.GetSummary()
				for _, q := range s.GetQuantile() {
					ls := append(cloneLabels(base), label{"quantile", fmt.Sprintf("%g", q.GetQuantile())})
					sortLabels(ls)
					out = append(out, series(ls, q.GetValue(), tsMs))
				}
				out = append(out, series(baseLabels(name+"_sum", m.GetLabel(), hostname), s.GetSampleSum(), tsMs))
				out = append(out, series(baseLabels(name+"_count", m.GetLabel(), hostname), float64(s.GetSampleCount()), tsMs))

			case dto.MetricType_HISTOGRAM:
				h := m.GetHistogram()
				bucketBase := baseLabels(name+"_bucket", m.GetLabel(), hostname)
				for _, b := range h.GetBucket() {
					ls := append(cloneLabels(bucketBase), label{"le", fmt.Sprintf("%g", b.GetUpperBound())})
					sortLabels(ls)
					out = append(out, series(ls, float64(b.GetCumulativeCount()), tsMs))
				}
				infls := append(cloneLabels(bucketBase), label{"le", "+Inf"})
				sortLabels(infls)
				out = append(out, series(infls, float64(h.GetSampleCount()), tsMs))
				out = append(out, series(baseLabels(name+"_sum", m.GetLabel(), hostname), h.GetSampleSum(), tsMs))
				out = append(out, series(baseLabels(name+"_count", m.GetLabel(), hostname), float64(h.GetSampleCount()), tsMs))
			}
		}
	}
	return out
}

func baseLabels(name string, lps []*dto.LabelPair, hostname string) []label {
	capacity := len(lps) + 1
	if hostname != "" {
		capacity++
	}
	ls := make([]label, 0, capacity)
	ls = append(ls, label{"__name__", name})
	if hostname != "" {
		ls = append(ls, label{"host", hostname})
	}
	for _, lp := range lps {
		ls = append(ls, label{lp.GetName(), lp.GetValue()})
	}
	sortLabels(ls)
	return ls
}

func cloneLabels(ls []label) []label {
	out := make([]label, len(ls))
	copy(out, ls)
	return out
}

func sortLabels(ls []label) {
	sort.Slice(ls, func(i, j int) bool { return ls[i].name < ls[j].name })
}

func series(ls []label, v float64, tsMs int64) timeSeries {
	return timeSeries{labels: ls, samples: []sample{{v, tsMs}}}
}

// ---- minimal proto3 encoding for Prometheus remote_write v1 ----
//
// WriteRequest  { repeated TimeSeries timeseries = 1; }
// TimeSeries    { repeated Label labels = 1; repeated Sample samples = 2; }
// Label         { string name = 1; string value = 2; }
// Sample        { double value = 1; int64 timestamp = 2; }

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendLenDelim(b []byte, field int, data []byte) []byte {
	b = appendVarint(b, uint64(field<<3|2)) // wire type 2
	b = appendVarint(b, uint64(len(data)))
	return append(b, data...)
}

func appendString(b []byte, field int, s string) []byte {
	if s == "" {
		return b
	}
	return appendLenDelim(b, field, []byte(s))
}

func appendDouble(b []byte, field int, v float64) []byte {
	b = appendVarint(b, uint64(field<<3|1)) // wire type 1 (64-bit)
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], math.Float64bits(v))
	return append(b, buf[:]...)
}

func appendInt64(b []byte, field int, v int64) []byte {
	b = appendVarint(b, uint64(field<<3|0)) // wire type 0 (varint)
	return appendVarint(b, uint64(v))
}

func encodeLabel(l label) []byte {
	var b []byte
	b = appendString(b, 1, l.name)
	b = appendString(b, 2, l.value)
	return b
}

func encodeSample(s sample) []byte {
	var b []byte
	b = appendDouble(b, 1, s.value)
	b = appendInt64(b, 2, s.timestamp)
	return b
}

func encodeTimeSeries(ts timeSeries) []byte {
	var b []byte
	for _, l := range ts.labels {
		b = appendLenDelim(b, 1, encodeLabel(l))
	}
	for _, s := range ts.samples {
		b = appendLenDelim(b, 2, encodeSample(s))
	}
	return b
}

func encodeWriteRequest(tss []timeSeries) []byte {
	var b []byte
	for _, ts := range tss {
		b = appendLenDelim(b, 1, encodeTimeSeries(ts))
	}
	return b
}
