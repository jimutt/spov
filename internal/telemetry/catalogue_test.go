package telemetry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/jimutt/spov/internal/store"
)

// recordEverything calls every recording method once, so every series
// exists in a collection.
func recordEverything(m *Metrics) {
	ctx := context.Background()
	m.APIRequest(ctx, "get-session", 200, time.Millisecond)
	m.QuotaRefused(ctx, QuotaJobs)
	m.JobEnded(ctx, store.JobKindReport, store.JobSucceeded, EndedDeclared, time.Hour)
	m.JobQueued(ctx, time.Minute)
	m.JobDelivered(ctx, time.Minute)
	m.JobGate(ctx, "check", "passed")
	m.JobDeliverable(ctx, store.DeliverablePR, store.DeliverableVerified)
	m.SessionLaunched(ctx, KindJob, false, StepSandbox, time.Minute)
	m.SessionStopped(ctx, KindInteractive, TerminationStopped, time.Hour)
	m.Reconciled(ctx, errors.New("x"), time.Second)
	m.SupervisorPass(ctx, nil)
	m.RecorderGap(ctx)
	m.RecorderReconnect(ctx)
	m.TokenRenewal(ctx, OutcomeRenewed)
	m.ProxyRecovery(ctx, OutcomeRecreated)
	m.ImageBuilt(ctx, nil)
	m.SetShared(Snapshot{
		Jobs:         map[store.JobState]int64{store.JobQueued: 2},
		Sessions:     map[SessionKey]int64{{State: store.StateRunning, Kind: KindJob}: 1},
		OldestQueued: time.Minute,
	})
}

// prometheusFamilies records everything and gathers it as Prometheus
// would scrape it: family name to label keys.
func prometheusFamilies(t *testing.T) map[string]map[string]bool {
	t.Helper()
	reg := prometheus.NewRegistry()
	exp, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		t.Fatal(err)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := NewMetrics(provider.Meter("test"), "v-test")
	if err != nil {
		t.Fatal(err)
	}
	recordEverything(m)
	got, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]bool{}
	for _, f := range got {
		keys := map[string]bool{}
		for _, metric := range f.GetMetric() {
			for _, l := range metric.GetLabel() {
				keys[l.GetName()] = true
			}
		}
		out[f.GetName()] = keys
	}
	return out
}

// documented reads the catalogue table in docs/metrics.md: each row's
// Prometheus name and its label keys.
func documented(t *testing.T) map[string]map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "metrics.md"))
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("^\\| `(spov_[a-z_]+)` \\| [a-z]+ \\| ([^|]*) \\|")
	key := regexp.MustCompile("`([a-z_]+)`")
	out := map[string]map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		mm := row.FindStringSubmatch(line)
		if mm == nil {
			continue
		}
		keys := map[string]bool{}
		for _, k := range key.FindAllStringSubmatch(mm[2], -1) {
			keys[k[1]] = true
		}
		out[mm[1]] = keys
	}
	return out
}

// Every metric spov exports is in docs/metrics.md with exactly its label
// keys, and every documented metric exists, so the definitions and the
// dashboard cannot drift from the code.
//
// @decision(DL-011)
func TestEveryMetricIsDocumented(t *testing.T) {
	docs := documented(t)
	if len(docs) == 0 {
		t.Fatal("no catalogue rows found in docs/metrics.md")
	}
	got := prometheusFamilies(t)
	for name, keys := range got {
		if !strings.HasPrefix(name, "spov_") {
			continue
		}
		want, ok := docs[name]
		if !ok {
			t.Errorf("%s is exported but not documented in docs/metrics.md", name)
			continue
		}
		for k := range keys {
			if !strings.HasPrefix(k, "otel_scope_") && !want[k] {
				t.Errorf("%s has label %q, which docs/metrics.md does not list", name, k)
			}
		}
		for k := range want {
			if !keys[k] {
				t.Errorf("docs/metrics.md lists label %q on %s, which it does not have", k, name)
			}
		}
	}
	for name := range docs {
		if _, ok := got[name]; !ok {
			t.Errorf("docs/metrics.md documents %s, which is not exported", name)
		}
	}
}

// The dashboard queries only metrics the catalogue documents, so a
// renamed metric fails here rather than as an empty panel.
//
// @decision(DL-011)
func TestTheDashboardQueriesDocumentedMetrics(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "grafana", "spov-dashboard.json"))
	if err != nil {
		t.Fatal(err)
	}
	docs := documented(t)
	names := regexp.MustCompile(`spov_[a-z_]+`).FindAllString(string(raw), -1)
	if len(names) == 0 {
		t.Fatal("the dashboard queries no spov metric")
	}
	for _, name := range names {
		base := name
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			base = strings.TrimSuffix(base, suffix)
		}
		if _, ok := docs[base]; !ok {
			t.Errorf("the dashboard queries %s, which docs/metrics.md does not document", name)
		}
	}
}
