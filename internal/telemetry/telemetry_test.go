package telemetry

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/jimutt/spov/internal/store"
)

// manual is a Metrics over an in-memory reader, for reading back what
// was recorded.
func manual(t *testing.T) (*Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := NewMetrics(provider.Meter("test"), "v-test")
	if err != nil {
		t.Fatal(err)
	}
	return m, reader
}

// attributesOf collects every data point's attributes for one metric.
func attributesOf(t *testing.T, r *sdkmetric.ManualReader, name string) []attribute.Set {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var out []attribute.Set
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					out = append(out, p.Attributes)
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					out = append(out, p.Attributes)
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					out = append(out, p.Attributes)
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					out = append(out, p.Attributes)
				}
			}
		}
	}
	return out
}

// A nil Metrics is what a server with no exporter has; every method must
// be safe on it.
//
// @decision(DL-011)
func TestANilMetricsRecordsNothing(t *testing.T) {
	var m *Metrics
	recordEverything(m)
	m.ClearShared()
}

// A label value outside its set is recorded as other, so no caller can
// widen a label with an ID or a name.
//
// @decision(DL-011)
func TestUnknownLabelValuesBecomeOther(t *testing.T) {
	m, r := manual(t)
	ctx := context.Background()
	m.JobEnded(ctx, store.JobKind("job_01ABC"), store.JobState("weird"), EndedBy("acme/app"), 0)
	m.JobGate(ctx, "gate-from-a-sandbox", "maybe")
	m.QuotaRefused(ctx, QuotaLimit("user@example.com"))
	m.RequestLimited(ctx, RequestLimit("203.0.113.7"))
	m.SessionLaunched(ctx, SessionKind("x"), false, LaunchStep("https://example.com/step"), time.Second)
	for _, name := range []string{"spov_job_endings", "spov_job_gates", "spov_quota_refusals", "spov_requests_limited", "spov_session_launches"} {
		sets := attributesOf(t, r, name)
		if len(sets) == 0 {
			t.Fatalf("%s recorded nothing", name)
		}
		for _, set := range sets {
			for _, kv := range set.ToSlice() {
				if kv.Key == "outcome" && name == "spov_session_launches" {
					continue
				}
				if v := kv.Value.AsString(); v != "other" {
					t.Errorf("%s: %s = %q, want other", name, kv.Key, v)
				}
			}
		}
	}
}

// Shared state is reported only while a snapshot is published, and not
// at all once it is cleared: a replica that stops leading falls silent.
//
// @decision(DL-011)
func TestSharedStateIsReportedOnlyWhileLeading(t *testing.T) {
	m, r := manual(t)
	if got := attributesOf(t, r, "spov_jobs"); len(got) != 0 {
		t.Errorf("a replica that never led reports %d job series", len(got))
	}
	m.SetShared(Snapshot{Jobs: map[store.JobState]int64{store.JobQueued: 3}, OldestQueued: time.Minute})
	if got := attributesOf(t, r, "spov_jobs"); len(got) != len(unfinishedJobStates) {
		t.Errorf("the leader reports %d job series, want one per unfinished state", len(got))
	}
	if got := attributesOf(t, r, "spov_job_oldest_queued_age"); len(got) != 1 {
		t.Errorf("the leader reports %d oldest-queued series, want 1", len(got))
	}
	m.ClearShared()
	for _, name := range []string{"spov_jobs", "spov_sessions", "spov_job_oldest_queued_age"} {
		if got := attributesOf(t, r, name); len(got) != 0 {
			t.Errorf("after leading ended, %s still reports %d series", name, len(got))
		}
	}
}

// With nothing configured, nothing is exported and no listener opens.
//
// @decision(DL-011)
func TestNothingConfiguredExportsNothing(t *testing.T) {
	run, err := Start(context.Background(), Options{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	if run.Metrics != nil || run.PrometheusAddr != "" {
		t.Errorf("an unconfigured server started metrics: %+v", run)
	}
	run.Shutdown(context.Background())
}

// The Prometheus listener serves /metrics with spov's series and the
// resource attributes, and nothing else.
//
// @decision(DL-011)
func TestThePrometheusListenerServesMetrics(t *testing.T) {
	run, err := Start(context.Background(), Options{PrometheusListen: "127.0.0.1:0", Version: "v-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { run.Shutdown(context.Background()) })
	if run.Metrics == nil || run.PrometheusAddr == "" {
		t.Fatalf("the listener did not start: %+v", run)
	}
	run.Metrics.QuotaRefused(context.Background(), QuotaSessions)
	body := get(t, "http://"+run.PrometheusAddr+"/metrics", http.StatusOK)
	for _, want := range []string{`spov_build_info{`, `version="v-test"`, `spov_quota_refusals_total{`, `service_name="spov"`} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
	get(t, "http://"+run.PrometheusAddr+"/", http.StatusNotFound)
}

// A listener that cannot bind is warned about, and the server starts.
//
// @decision(DL-011)
func TestAListenerThatCannotBindDoesNotStopTheServer(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	var warned []string
	run, err := Start(context.Background(), Options{
		PrometheusListen: taken.Addr().String(),
		Warn:             func(s string) { warned = append(warned, s) },
	})
	if err != nil {
		t.Fatalf("a taken port failed the start: %v", err)
	}
	defer run.Shutdown(context.Background())
	if len(warned) == 0 || !strings.Contains(warned[0], "not serving Prometheus metrics") {
		t.Errorf("warnings = %v", warned)
	}
	if run.PrometheusAddr != "" {
		t.Errorf("a failed bind reported an address: %s", run.PrometheusAddr)
	}
}

// OTLP is on when either standard endpoint variable is set.
//
// @decision(DL-011)
func TestOTLPFollowsTheStandardEndpointVariables(t *testing.T) {
	for env, want := range map[string]bool{
		"":                                    false,
		"OTEL_EXPORTER_OTLP_ENDPOINT":         true,
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": true,
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":  false,
	} {
		o := Options{Getenv: func(k string) string {
			if k == env {
				return "http://collector:4318"
			}
			return ""
		}}
		if got := o.OTLPEnabled(); got != want {
			t.Errorf("with %q set, OTLPEnabled = %v", env, got)
		}
	}
}

// Repeated export failures are reported at most once per interval.
func TestExportWarningsAreThrottled(t *testing.T) {
	var n int
	warn := throttled(func(string) { n++ }, time.Hour)
	for range 5 {
		warn("collector unreachable")
	}
	if n != 1 {
		t.Errorf("five failures warned %d times, want 1", n)
	}
}

func get(t *testing.T, url string, want int) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Errorf("GET %s = %d, want %d", url, resp.StatusCode, want)
	}
	return string(b)
}

// A gRPC protocol setting is warned about rather than quietly pushing
// HTTP at a gRPC port.
//
// @decision(DL-011)
func TestAGRPCProtocolSettingIsWarnedAbout(t *testing.T) {
	var warned []string
	env := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:1",
		"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc",
	}
	run, err := Start(context.Background(), Options{
		Getenv: func(k string) string { return env[k] },
		Warn:   func(s string) { warned = append(warned, s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Shutdown(context.Background())
	if run.Metrics == nil {
		t.Error("OTLP did not start")
	}
	if len(warned) == 0 || !strings.Contains(warned[0], "OTEL_EXPORTER_OTLP_PROTOCOL=grpc is ignored") {
		t.Errorf("warnings = %v", warned)
	}
}
