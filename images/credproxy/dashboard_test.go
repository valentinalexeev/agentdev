package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Review of #278: every metric the Grafana dashboard queries is exported by claude-proxy (telemetry.go) or the
// operator (images/operator/metrics.go), under its Prometheus name. A renamed instrument fails here, not in Grafana.
func TestDashboardMetricsExist(t *testing.T) {
	var exported = map[string]bool{}
	inst := regexp.MustCompile(`(Int64|Float64)(Histogram|Counter|ObservableGauge)\("([a-z0-9_.]+)"`)
	for _, f := range []string{"telemetry.go", "../operator/metrics.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range inst.FindAllStringSubmatch(string(b), -1) {
			base := strings.ReplaceAll(m[3], ".", "_")
			switch m[2] {
			case "Counter":
				exported[base+"_total"] = true
			case "Histogram":
				for _, s := range []string{"_bucket", "_sum", "_count"} {
					exported[base+s] = true
				}
			default:
				exported[base] = true
			}
		}
	}
	// the histogram unit "s" becomes a _seconds suffix in the Prometheus exporter
	for k := range exported {
		if strings.HasPrefix(k, "gen_ai_client_operation_duration_") {
			exported[strings.Replace(k, "duration_", "duration_seconds_", 1)] = true
		}
	}
	raw, err := os.ReadFile("../../grafana/agentdev-llm-cost.json")
	if err != nil {
		t.Fatal(err)
	}
	var dash any
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatal(err)
	}
	var exprs []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if e, ok := x["expr"].(string); ok {
				exprs = append(exprs, e)
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(dash)
	if len(exprs) < 5 {
		t.Fatalf("only %d queries found", len(exprs))
	}
	name := regexp.MustCompile(`\b((?:agentdev|gen_ai)_[a-z0-9_]+)`)
	for _, e := range exprs {
		for _, m := range name.FindAllStringSubmatch(e, -1) {
			n := m[1]
			if strings.HasPrefix(n, "agentdev_agent") || n == "agentdev_cache" || n == "agentdev_reason" ||
				strings.HasPrefix(n, "gen_ai_response_model") || strings.HasPrefix(n, "gen_ai_token_type") {
				continue // labels, not metrics
			}
			if !exported[n] {
				t.Errorf("dashboard queries %s, which nothing exports (query %q)", n, e)
			}
		}
	}
}
