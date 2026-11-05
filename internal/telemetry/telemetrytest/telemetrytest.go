// Package telemetrytest records metrics in memory so a test can read
// back what was recorded.
package telemetrytest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/jimutt/spov/internal/telemetry"
)

// Reader is a Metrics over an in-memory reader.
type Reader struct {
	Metrics *telemetry.Metrics
	t       *testing.T
	reader  *sdkmetric.ManualReader
}

// New returns metrics a test can read back.
func New(t *testing.T) *Reader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := telemetry.NewMetrics(provider.Meter("test"), "v-test")
	if err != nil {
		t.Fatal(err)
	}
	return &Reader{Metrics: m, t: t, reader: reader}
}

// Count is the total of a counter's data points, or the number of
// observations of a histogram, whose labels include every key=value
// pair in labels.
func (r *Reader) Count(name string, labels ...string) int64 {
	r.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.reader.Collect(context.Background(), &rm); err != nil {
		r.t.Fatal(err)
	}
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					if matches(p.Attributes, labels) {
						n += p.Value
					}
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					if matches(p.Attributes, labels) {
						n += p.Value
					}
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					if matches(p.Attributes, labels) {
						n += int64(p.Count)
					}
				}
			}
		}
	}
	return n
}

func matches(set attribute.Set, labels []string) bool {
	for i := 0; i+1 < len(labels); i += 2 {
		v, ok := set.Value(attribute.Key(labels[i]))
		if !ok || v.AsString() != labels[i+1] {
			return false
		}
	}
	return true
}
