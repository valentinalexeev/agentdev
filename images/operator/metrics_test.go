package main

import (
	networkingv1 "k8s.io/api/networking/v1"
	"net/http"
	"net/http/httptest"
	"sigs.k8s.io/yaml"
	"strings"
	"testing"
	"time"
)

func TestOperatorPausedMetric(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	u := &Usage{Now: func() time.Time { return now }, paused: map[string]Pause{
		"alice": {Until: now.Add(time.Hour).Unix(), Reason: "five-hour"},
		"bob":   {Until: now.Add(-time.Hour).Unix(), Reason: "weekly"}, // ended
	}}
	h, _, err := newOperatorMetrics("", u.pausedNow)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `agentdev_usage_paused{agentdev_agent="alice",agentdev_reason="five-hour"} 1`) {
		t.Errorf("paused gauge missing:\n%s", body)
	}
	if strings.Contains(body, `"bob"`) {
		t.Error("ended pause exported")
	}
	if _, _, err := newOperatorMetrics("http://c:4318", u.pausedNow); err == nil {
		t.Error("plain http OTLP accepted")
	}
}

// #196 (ADR 0002 D4): port 9464 of claude-proxy and the operator admits only the scraper namespace; no rule lets an
// agent pod (or any pod of the agents namespace) reach it.
func TestMetricsPortOnlyFromScraper(t *testing.T) {
	vals := map[string]string{"AGENTS_NS": "coding-agents", "PLATFORM_NS": "agentdev-platform", "EXEC_NS": "agentdev-executors",
		"CLAUDE_PROXY_IMAGE": "img", "IMAGE_PULL_POLICY": "Never", "CLAUDE_BUDGET_UNITS": "0", "CLAUDE_PRICES_DATA": "{}",
		"TELEMETRY_OTLP_URL": "", "TELEMETRY_SCRAPER_NS": "monitoring", "CLAUDE_SECRET_SOURCES": "[]"}
	docs := append(renderTmpl(t, "../../manifests/claude-proxy.yaml.tmpl", vals), renderTmpl(t, "../../manifests/operator.yaml.tmpl", vals)...)
	seen := 0
	for _, d := range docs {
		var p networkingv1.NetworkPolicy
		if yaml.Unmarshal([]byte(d), &p) != nil || p.Kind != "NetworkPolicy" || (p.Name != "claude-proxy" && p.Name != "agentdev-operator") {
			continue
		}
		seen++
		for _, r := range p.Spec.Ingress {
			has9464 := false
			for _, port := range r.Ports {
				if port.Port != nil && port.Port.IntValue() == 9464 {
					has9464 = true
				}
			}
			if len(r.Ports) == 0 {
				t.Errorf("%s: rule without ports opens 9464", p.Name)
			}
			if !has9464 {
				continue
			}
			for _, f := range r.From {
				if f.PodSelector != nil || f.IPBlock != nil || f.NamespaceSelector == nil ||
					f.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "monitoring" {
					t.Errorf("%s: 9464 open to %+v", p.Name, f)
				}
			}
			if len(r.From) == 0 {
				t.Errorf("%s: 9464 open to all", p.Name)
			}
		}
	}
	if seen != 2 {
		t.Errorf("found %d policies", seen)
	}
}
