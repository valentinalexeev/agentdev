package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
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
	if !strings.Contains(body, `agentdev_usage_paused{agentdev_agent="alice",agentdev_instance="agentdev",agentdev_reason="five-hour"} 1`) {
		t.Errorf("paused gauge missing:\n%s", body)
	}
	if strings.Contains(body, `"bob"`) {
		t.Error("ended pause exported")
	}
	if _, _, err := newOperatorMetrics("http://c:4318", u.pausedNow); err == nil {
		t.Error("plain http OTLP accepted")
	}
	t.Setenv("TELEMETRY_INSTANCE", "Bad Name")
	if _, _, err := newOperatorMetrics("", u.pausedNow); err == nil {
		t.Error("bad TELEMETRY_INSTANCE accepted")
	}
}

// #196 (ADR 0002 D4): port 9464 of claude-proxy and the operator admits only the scraper pods of the scraper
// namespace (not Grafana or the OTLP collector next to them, review of #276); no rule lets an agent pod (or any pod
// of the agents namespace) reach it; and no Service exposes 9464.
func TestMetricsPortOnlyFromScraper(t *testing.T) {
	vals := map[string]string{"AGENTS_NS": "coding-agents", "PLATFORM_NS": "agentdev-platform", "EXEC_NS": "agentdev-executors",
		"CLAUDE_PROXY_IMAGE": "img", "IMAGE_PULL_POLICY": "Never", "CLAUDE_BUDGET_UNITS": "0", "CLAUDE_PRICES_DATA": "{}",
		"TELEMETRY_OTLP_URL": "", "TELEMETRY_INSTANCE": "agentdev", "TELEMETRY_SCRAPER_NS": "monitoring", "TELEMETRY_SCRAPER_SELECTOR": `{"app.kubernetes.io/name": "prometheus"}`, "CLAUDE_JOB_BUDGET_UNITS": "0", "JOB_TIMEOUT": "3600", "MAX_BUDGET_USD": "", "CLAUDE_CODE_TELEMETRY": "false", "CLAUDE_CODE_OTLP_URL": "", "CLAUDE_SECRET_SOURCES": "[]"}
	docs := append(renderTmpl(t, "../../manifests/claude-proxy.yaml.tmpl", vals), renderTmpl(t, "../../manifests/operator.yaml.tmpl", vals)...)
	seen := 0
	for _, d := range docs {
		var svc corev1.Service
		if yaml.Unmarshal([]byte(d), &svc) == nil && svc.Kind == "Service" {
			for _, sp := range svc.Spec.Ports {
				if sp.Port == 9464 || sp.TargetPort.IntValue() == 9464 || sp.TargetPort.String() == "metrics" {
					t.Errorf("Service %s exposes the metrics port", svc.Name)
				}
			}
		}
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
				if f.PodSelector == nil || len(f.PodSelector.MatchLabels) == 0 || f.PodSelector.MatchLabels["app.kubernetes.io/name"] != "prometheus" ||
					f.IPBlock != nil || f.NamespaceSelector == nil ||
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

func TestRunGuardValues(t *testing.T) {
	if guardValue("90", `^[0-9]{1,7}$`, "3600") != "90" || guardValue("$(id)", `^[0-9]{1,7}$`, "3600") != "3600" {
		t.Error("JOB_TIMEOUT check")
	}
	re := `^([0-9]{1,6}(\.[0-9]{1,2})?)?$`
	if guardValue("2.5", re, "") != "2.5" || guardValue("", re, "") != "" || guardValue("1.234", re, "") != "" {
		t.Error("MAX_BUDGET_USD check")
	}
	if (Defaults{}).jobTimeout() != "3600" {
		t.Error("default JOB_TIMEOUT")
	}
}
