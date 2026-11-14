package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Telemetry (#196, ADR 0002 D4): OpenTelemetry metrics with the GenAI semantic conventions, exported on a separate
// Prometheus port (METRICS_LISTEN, default :9464; the NetworkPolicy admits only the scraper) and, when
// TELEMETRY_OTLP_URL is an https URL, over OTLP (metrics and one "chat" span per upstream request). No prompt or
// completion content is ever recorded. The job id is never a metric label (unbounded cardinality): it is only on spans.

type telemetry struct {
	tokens   metric.Int64Histogram
	duration metric.Float64Histogram
	units    metric.Int64Counter
	usd      metric.Float64Counter
	refused  metric.Int64Counter
	tracer   trace.Tracer
	handler  http.Handler
	shutdown func(context.Context) error

	mu   sync.Mutex
	util map[[2]string]float64 // (secret, window) -> utilization 0..1
}

const genAIProvider = "anthropic"

// newTelemetry builds the meters. otlpURL "" = Prometheus only. b supplies the budget gauges.
func newTelemetry(otlpURL string, b *budget) (*telemetry, error) {
	t := &telemetry{util: map[[2]string]float64{}, shutdown: func(context.Context) error { return nil }}
	inst, err := telemetryInstance(os.Getenv("TELEMETRY_INSTANCE"))
	if err != nil {
		return nil, err
	}
	res := resource.NewSchemaless(semconv.ServiceName("claude-proxy"), attribute.String("service.namespace", "agentdev"),
		attribute.String(instanceAttr, inst))
	reg := prometheus.NewRegistry()
	// agentdev_instance on every series, so one external Prometheus and Grafana can tell instances apart (#278 review)
	promExp, err := otelprom.New(otelprom.WithRegisterer(reg), otelprom.WithoutScopeInfo(),
		otelprom.WithResourceAsConstantLabels(attribute.NewAllowKeysFilter(instanceAttr)))
	if err != nil {
		return nil, err
	}
	opts := []sdkmetric.Option{sdkmetric.WithResource(res), sdkmetric.WithReader(promExp)}
	var tp *sdktrace.TracerProvider
	if otlpURL != "" {
		u, err := url.Parse(otlpURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return nil, errors.New("TELEMETRY_OTLP_URL must be an https URL without credentials")
		}
		ctx := context.Background()
		mexp, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(otlpURL+"/v1/metrics"))
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp, sdkmetric.WithInterval(60*time.Second))))
		texp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(otlpURL+"/v1/traces"))
		if err != nil {
			return nil, err
		}
		tp = sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(texp))
	}
	mp := sdkmetric.NewMeterProvider(opts...)
	m := mp.Meter("github.com/valentinalexeev/agentdev/images/credproxy")
	if tp != nil {
		t.tracer = tp.Tracer("github.com/valentinalexeev/agentdev/images/credproxy")
		t.shutdown = func(ctx context.Context) error { return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx)) }
	} else {
		t.tracer = noop.NewTracerProvider().Tracer("")
		t.shutdown = mp.Shutdown
	}
	if t.tokens, err = m.Int64Histogram("gen_ai.client.token.usage", metric.WithUnit("{token}"),
		metric.WithDescription("Number of tokens used per request, by token type"),
		metric.WithExplicitBucketBoundaries(1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304)); err != nil {
		return nil, err
	}
	if t.duration, err = m.Float64Histogram("gen_ai.client.operation.duration", metric.WithUnit("s"),
		metric.WithDescription("Duration of the model request, until the response body ended"),
		metric.WithExplicitBucketBoundaries(0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92, 163.84)); err != nil {
		return nil, err
	}
	if t.units, err = m.Int64Counter("agentdev.cost.units", metric.WithUnit("{unit}"),
		metric.WithDescription("Cost units (input-token equivalents of the reference model)")); err != nil {
		return nil, err
	}
	if t.usd, err = m.Float64Counter("agentdev.cost.usd", // no unit: Prometheus name agentdev_cost_usd_total

		metric.WithDescription("Estimated USD, only when claude-proxy-prices sets usd_per_million_units")); err != nil {
		return nil, err
	}
	if t.refused, err = m.Int64Counter("agentdev.requests.refused", metric.WithUnit("{request}"),
		metric.WithDescription("Requests refused by the proxy or rate limited upstream, by reason")); err != nil {
		return nil, err
	}
	used, err := m.Int64ObservableGauge("agentdev.budget.used", metric.WithUnit("{unit}"),
		metric.WithDescription("Cost units used by the agent in the rolling 24 h window"))
	if err != nil {
		return nil, err
	}
	limit, err := m.Int64ObservableGauge("agentdev.budget.limit", metric.WithUnit("{unit}"),
		metric.WithDescription("Budget of the agent in cost units per 24 h; 0 = no limit"))
	if err != nil {
		return nil, err
	}
	util, err := m.Float64ObservableGauge("agentdev.ratelimit.utilization", // no unit: keeps the Prometheus name without a _ratio suffix

		metric.WithDescription("Last seen utilization of the subscription's unified rate-limit window (0..1)"))
	if err != nil {
		return nil, err
	}
	if _, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		if b != nil {
			for _, s := range b.snapshot() {
				a := metric.WithAttributes(attribute.String("agentdev.agent", s.Agent))
				o.ObserveInt64(used, s.Used, a)
				o.ObserveInt64(limit, s.Limit, a)
			}
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		for k, v := range t.util {
			o.ObserveFloat64(util, v, metric.WithAttributes(attribute.String("agentdev.subscription", k[0]), attribute.String("agentdev.window", k[1])))
		}
		return nil
	}, used, limit, util); err != nil {
		return nil, err
	}
	t.handler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return t, nil
}

// record is called once per finished /v1/messages response.
func (t *telemetry) record(ctx context.Context, agent, job, model string, tc tokenCounts, units int64, usd float64, hasUSD bool, start, end time.Time, status int) {
	if t == nil {
		return
	}
	if model == "" {
		model = "unknown"
	}
	base := []attribute.KeyValue{
		attribute.String("gen_ai.provider.name", genAIProvider),
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.response.model", model),
		attribute.String("agentdev.agent", agent),
	}
	for i, n := range tc {
		if n == 0 {
			continue
		}
		typ, cache := "input", ""
		switch i {
		case tokOut:
			typ = "output"
		case tokCacheRead:
			cache = "read"
		case tokCacheWrite5m, tokCacheWrite1h:
			cache = "write"
		}
		a := append(append([]attribute.KeyValue{}, base...), attribute.String("gen_ai.token.type", typ), attribute.String("agentdev.cache", cache))
		t.tokens.Record(ctx, n, metric.WithAttributes(a...))
	}
	t.duration.Record(ctx, end.Sub(start).Seconds(), metric.WithAttributes(base...))
	t.units.Add(ctx, units, metric.WithAttributes(base...))
	if hasUSD {
		t.usd.Add(ctx, usd, metric.WithAttributes(base...))
	}
	// one span per upstream request; the job id is here, never on a metric
	_, span := t.tracer.Start(ctx, "chat "+model, trace.WithSpanKind(trace.SpanKindClient), trace.WithTimestamp(start),
		trace.WithAttributes(append(base,
			attribute.String("agentdev.job", job),
			attribute.Int64("gen_ai.usage.input_tokens", tc.input()),
			attribute.Int64("gen_ai.usage.output_tokens", tc[tokOut]),
			attribute.Int64("agentdev.usage.cache_read_tokens", tc[tokCacheRead]),
			attribute.Int64("agentdev.cost.units", units),
			attribute.Int("http.response.status_code", status))...))
	span.End(trace.WithTimestamp(end))
}

// refuse counts a refused request; reason is one of budget, job_budget, paused, rate_limited, bad_key.
func (t *telemetry) refuse(agent, reason string) {
	if t == nil {
		return
	}
	t.refused.Add(context.Background(), 1, metric.WithAttributes(attribute.String("agentdev.agent", agent), attribute.String("agentdev.reason", reason)))
}

// window stores the last utilization of a subscription's window (5h, 7d).
func (t *telemetry) window(secret, name string, w *window) {
	if t == nil || w == nil {
		return
	}
	t.mu.Lock()
	t.util[[2]string{secret, name}] = w.Utilization
	t.mu.Unlock()
}

// serveMetrics runs the Prometheus endpoint on its own listener (port 9464), separate from the proxy port.
func (t *telemetry) serveMetrics(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", t.handler)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
		_ = t.shutdown(c)
	}()
	log.Printf("metrics on %s/metrics", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("metrics listener: %v", err)
	}
}

// instanceAttr names the agentdev instance (TELEMETRY_INSTANCE, default "agentdev"). Prometheus label:
// agentdev_instance (the label "instance" belongs to the scrape target).
const instanceAttr = "agentdev.instance"

var instanceRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func telemetryInstance(v string) (string, error) {
	if v == "" {
		return "agentdev", nil
	}
	if !instanceRe.MatchString(v) {
		return "", errors.New("TELEMETRY_INSTANCE must be a DNS label (a-z, 0-9, -; at most 63 characters)")
	}
	return v, nil
}
