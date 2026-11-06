package main

import "testing"

func TestParseCostLabels(t *testing.T) {
	got := parseCostLabels(" service=agentdev, environment=prod,Bad=x,empty=,noeq,app=x y")
	want := map[string]string{"service": "agentdev", "environment": "prod", "empty": ""}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if len(parseCostLabels("")) != 0 {
		t.Fatal("empty input gives labels")
	}
}

func TestCostLabelsOnAgentPod(t *testing.T) {
	r, c := setup(t, agent("alice", nil))
	r.D.CostLabels = map[string]string{"service": "agentdev", "environment": "prod", "app": "evil"}
	reconcile(t, r, "alice")
	s := getSTS(t, c, "alice")
	for _, l := range []map[string]string{s.Labels, s.Spec.Template.Labels} {
		if l["service"] != "agentdev" || l["environment"] != "prod" || l["app"] != "coding-agent" || l["agent"] != "alice" {
			t.Fatalf("labels %v", l)
		}
	}
	if len(s.Spec.Selector.MatchLabels) != 1 {
		t.Fatalf("selector changed: %v", s.Spec.Selector.MatchLabels)
	}
}
