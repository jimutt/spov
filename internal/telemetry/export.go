package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Options say which exporters to start.
//
// @decision(DL-011)
type Options struct {
	// PrometheusListen is the address of the /metrics listener; empty
	// starts none.
	PrometheusListen string
	// Getenv reads the OTLP endpoint variables; nil reads nothing.
	Getenv func(string) string
	// Version is the build, for service.version and spov_build_info.
	Version string
	// Warn reports export trouble. It never stops the server.
	Warn func(string)
}

// OTLPEnabled reports whether the environment names an OTLP endpoint.
//
// @decision(DL-011)
func (o Options) OTLPEnabled() bool {
	if o.Getenv == nil {
		return false
	}
	return strings.TrimSpace(o.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")) != "" ||
		strings.TrimSpace(o.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != ""
}

// Running is what Start started: the metrics to record on, nil when no
// exporter is configured, and the address the Prometheus listener is
// bound to, if one is.
type Running struct {
	Metrics        *Metrics
	PrometheusAddr string
	shutdown       []func(context.Context) error
}

// Shutdown flushes and stops the exporters, within ctx.
func (r *Running) Shutdown(ctx context.Context) {
	if r == nil {
		return
	}
	for _, f := range r.shutdown {
		_ = f(ctx)
	}
}

// Start builds the meter provider with the exporters o asks for. With
// none it returns a nil Metrics, which records nothing. A listener that
// cannot bind, or an OTLP exporter that cannot be built, is warned about
// and left out; the server runs on without it.
//
// @decision(DL-011)
func Start(ctx context.Context, o Options) (*Running, error) {
	warn := o.Warn
	if warn == nil {
		warn = func(string) {}
	}
	exportWarn := throttled(o.Warn, 15*time.Minute)
	if o.PrometheusListen == "" && !o.OTLPEnabled() {
		return &Running{}, nil
	}
	run := &Running{}
	var readers []sdkmetric.Option

	if o.PrometheusListen != "" {
		reg := prometheus.NewRegistry()
		exp, err := otelprom.New(otelprom.WithRegisterer(reg))
		if err != nil {
			return nil, fmt.Errorf("telemetry: the Prometheus exporter: %w", err)
		}
		ln, err := net.Listen("tcp", o.PrometheusListen)
		if err != nil {
			warn(fmt.Sprintf("metrics: not serving Prometheus metrics: %v", err))
		} else {
			readers = append(readers, sdkmetric.WithReader(exp))
			mux := http.NewServeMux()
			mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
			srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
			go func() {
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					exportWarn(fmt.Sprintf("metrics: the Prometheus listener stopped: %v", err))
				}
			}()
			run.PrometheusAddr = ln.Addr().String()
			run.shutdown = append(run.shutdown, srv.Shutdown)
		}
	}
	if o.OTLPEnabled() {
		// Only OTLP over HTTP is offered; a gRPC protocol setting would
		// otherwise fail quietly as export errors.
		for _, k := range []string{"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL"} {
			if p := strings.TrimSpace(o.Getenv(k)); p != "" && p != "http/protobuf" {
				warn(fmt.Sprintf("metrics: %s=%s is ignored; metrics are exported over OTLP/HTTP (http/protobuf)", k, p))
			}
		}
		// The exporter reads its own OTEL_EXPORTER_OTLP_* variables from
		// the process environment: endpoint, headers, timeout and TLS.
		exp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			warn(fmt.Sprintf("metrics: not exporting over OTLP: %v", err))
		} else {
			readers = append(readers, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
		}
	}
	if len(readers) == 0 {
		return run, nil
	}

	// A failed export reaches the global handler; it is reported, at
	// most every 15 minutes, and never stops anything.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		exportWarn(fmt.Sprintf("metrics: %v", err))
	}))
	// service.name is always spov; OTEL_SERVICE_NAME does not rename it.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", "spov"),
		attribute.String("service.version", o.Version),
		attribute.String("service.instance.id", instanceID()),
	))
	if err != nil {
		warn(fmt.Sprintf("metrics: not exporting: the resource: %v", err))
		run.Shutdown(ctx)
		return &Running{}, nil
	}
	provider := sdkmetric.NewMeterProvider(append(readers, sdkmetric.WithResource(res))...)
	m, err := NewMetrics(provider.Meter("github.com/jimutt/spov"), o.Version)
	if err != nil {
		_ = provider.Shutdown(ctx)
		run.Shutdown(ctx)
		warn(fmt.Sprintf("metrics: not exporting: creating the instruments: %v", err))
		return &Running{}, nil
	}
	run.Metrics = m
	run.shutdown = append(run.shutdown, provider.Shutdown)
	return run, nil
}

// instanceID tells this process's series from another replica's.
func instanceID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// throttled passes a warning through at most once per interval, so an
// unreachable collector does not flood the log.
func throttled(warn func(string), every time.Duration) func(string) {
	if warn == nil {
		return func(string) {}
	}
	var (
		mu   sync.Mutex
		last time.Time
	)
	return func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		if !last.IsZero() && time.Since(last) < every {
			return
		}
		last = time.Now()
		warn(msg)
	}
}
