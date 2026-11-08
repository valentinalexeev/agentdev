package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsGenAI(t *testing.T) {
	f := newFixture(t, 1000)
	tel, err := newTelemetry("", f.b)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Config.Handler.(*server).tel = tel
	f.upstream = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.5")
		w.Header().Set("anthropic-ratelimit-unified-5h-reset", "1")
		io.WriteString(w, `{"model":"claude-sonnet-4-5","usage":{"input_tokens":10,"cache_read_input_tokens":100,"output_tokens":2}}`)
	}
	key := agentKey(testSigning, "alice")
	resp := f.post("/v1/messages", key, `{"messages":[{"role":"user","content":"SECRET-PROMPT"}]}`, map[string]string{"X-Agentdev-Job": "issue-7"})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	resp = f.post("/v1/messages", "bad", `{}`, nil)
	resp.Body.Close()

	rec := httptest.NewRecorder()
	tel.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`gen_ai_client_token_usage_count{agentdev_agent="alice",agentdev_cache="read",gen_ai_operation_name="chat",gen_ai_provider_name="anthropic",gen_ai_response_model="claude-sonnet-4-5",gen_ai_token_type="input"`,
		`gen_ai_client_operation_duration_seconds_count{`,
		`agentdev_cost_units_total{agentdev_agent="alice",gen_ai_operation_name="chat",gen_ai_provider_name="anthropic",gen_ai_response_model="claude-sonnet-4-5"} 30`,
		`agentdev_budget_used{agentdev_agent="alice"} 30`,
		`agentdev_budget_limit{agentdev_agent="alice"} 1000`,
		`agentdev_requests_refused_total{agentdev_agent="",agentdev_reason="bad_key"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in\n%s", want, body)
		}
	}
	if strings.Contains(body, "issue-7") || strings.Contains(body, "SECRET-PROMPT") || strings.Contains(body, "sk-ant") {
		t.Error("job id, prompt or credential on /metrics")
	}
	if strings.Contains(body, "agentdev_cost_usd") {
		t.Error("USD without prices")
	}
	if _, err := newTelemetry("http://collector:4318", nil); err == nil {
		t.Error("plain http OTLP accepted")
	}
	if _, err := newTelemetry("https://u:p@collector", nil); err == nil {
		t.Error("OTLP URL with credentials accepted")
	}
}

func TestMetricNamesWithUSD(t *testing.T) {
	tel, _ := newTelemetry("", nil)
	tel.record(t.Context(), "a", "j", "m", tokenCounts{tokIn: 1}, 1, 0.5, true, time.Now(), time.Now(), 200)
	tel.window("claude-shared", "5h", &window{Utilization: 0.3})
	rec := httptest.NewRecorder()
	tel.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{"agentdev_cost_usd_total{", `agentdev_ratelimit_utilization{agentdev_subscription="claude-shared",agentdev_window="5h"} 0.3`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("missing %s in\n%s", want, rec.Body)
		}
	}
}
