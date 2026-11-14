package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

// Operator telemetry (#196, ADR 0002 D4): the OpenTelemetry metric agentdev.usage.paused (1 per agent and reason
// while the agent's subscription is paused, #93), exported on a separate Prometheus port (METRICS_LISTEN, default
// :9464; the NetworkPolicy admits only the scraper) and over OTLP when TELEMETRY_OTLP_URL is an https URL. The
// controller-runtime metrics server stays off (BindAddress "0").

// newOperatorMetrics returns the /metrics handler and a shutdown function. paused lists the current pauses.
func newOperatorMetrics(otlpURL string, paused func() map[string]Pause) (http.Handler, func(context.Context) error, error) {
	inst, err := telemetryInstance(os.Getenv("TELEMETRY_INSTANCE"))
	if err != nil {
		return nil, nil, err
	}
	res := resource.NewSchemaless(semconv.ServiceName("agentdev-operator"), attribute.String("service.namespace", "agentdev"),
		attribute.String(instanceAttr, inst))
	reg := prometheus.NewRegistry()
	// agentdev_instance on every series, so one external Prometheus and Grafana can tell instances apart (#278 review)
	promExp, err := otelprom.New(otelprom.WithRegisterer(reg), otelprom.WithoutScopeInfo(),
		otelprom.WithResourceAsConstantLabels(attribute.NewAllowKeysFilter(instanceAttr)))
	if err != nil {
		return nil, nil, err
	}
	opts := []sdkmetric.Option{sdkmetric.WithResource(res), sdkmetric.WithReader(promExp)}
	if otlpURL != "" {
		u, err := url.Parse(otlpURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return nil, nil, errors.New("TELEMETRY_OTLP_URL must be an https URL without credentials")
		}
		exp, err := otlpmetrichttp.New(context.Background(), otlpmetrichttp.WithEndpointURL(otlpURL+"/v1/metrics"))
		if err != nil {
			return nil, nil, err
		}
		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(60*time.Second))))
	}
	mp := sdkmetric.NewMeterProvider(opts...)
	m := mp.Meter("github.com/valentinalexeev/agentdev/images/operator")
	g, err := m.Int64ObservableGauge("agentdev.usage.paused", // no unit: keeps the Prometheus name without a _ratio suffix

		metric.WithDescription("1 while new runs of the agent are paused by a subscription usage limit (#93), by reason"))
	if err != nil {
		return nil, nil, err
	}
	if _, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for agent, p := range paused() {
			o.ObserveInt64(g, 1, metric.WithAttributes(attribute.String("agentdev.agent", agent), attribute.String("agentdev.reason", p.Reason)))
		}
		return nil
	}, g); err != nil {
		return nil, nil, err
	}
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), mp.Shutdown, nil
}

// pausedNow returns a copy of the pauses in effect (for the metric callback).
func (u *Usage) pausedNow() map[string]Pause {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now().Unix()
	if u.Now != nil {
		now = u.Now().Unix()
	}
	out := make(map[string]Pause, len(u.paused))
	for k, p := range u.paused {
		if p.Until > now {
			out[k] = p
		}
	}
	return out
}

// serveOperatorMetrics serves /metrics on addr until ctx ends.
func serveOperatorMetrics(ctx context.Context, addr string, h http.Handler, shutdown func(context.Context) error) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", h)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
		_ = shutdown(c)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// guardValue returns v if it matches re, else def (run guards, #196: values reach the worker's argv).
func guardValue(v, re, def string) string {
	if regexp.MustCompile(re).MatchString(v) {
		return v
	}
	return def
}

// jobTimeout is JOB_TIMEOUT for the worker; the default 3600 when unset (tests build Defaults by hand).
func (d Defaults) jobTimeout() string {
	if d.JobTimeout == "" {
		return "3600"
	}
	return d.JobTimeout
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
