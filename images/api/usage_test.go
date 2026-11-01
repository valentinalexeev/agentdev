package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUsageEffectiveClamp(t *testing.T) {
	s := usageSettings{Global: thresholds{70, 85}, Projects: map[string]thresholds{"o/low": {50, 0}, "o/high": {95, 99}}}
	if g := (usageSettings{}).global(); g != (thresholds{80, 90}) {
		t.Fatalf("defaults: %+v", g)
	}
	if e := s.effective("o/low"); e != (thresholds{50, 85}) {
		t.Fatalf("project lowers 5h only: %+v", e)
	}
	if e := s.effective("o/high"); e != (thresholds{70, 85}) {
		t.Fatalf("project above global is clamped: %+v", e)
	}
	if e := s.effectiveAll([]string{"o/low", "o/high"}); e != (thresholds{50, 85}) {
		t.Fatalf("lowest over all repos: %+v", e)
	}
}

func TestUsageLimitsAPI(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := newEnv(t, gitea.URL)
	settings := func() usageSettings {
		s, _, err := e.k.Settings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	cases := []struct {
		name, method, path, token, body string
		want                            int
		result                          string
	}{
		{"writer cannot set global", "PUT", "/api/usage/global", "writer", `{"fiveHour":50}`, 403, "denied"},
		{"admin group sets global", "PUT", "/api/usage/global", "group", `{"fiveHour":70,"weekly":85}`, 200, "ok"},
		{"global out of range", "PUT", "/api/usage/global", "group", `{"weekly":101}`, 400, "invalid"},
		{"global unknown field", "PUT", "/api/usage/global", "group", `{"daily":10}`, 400, "invalid"},
		{"reader cannot set project", "PUT", "/api/usage/projects/owner/x", "reader", `{"fiveHour":50}`, 403, "denied"},
		{"writer lowers project", "PUT", "/api/usage/projects/owner/x", "writer", `{"fiveHour":50}`, 200, "ok"},
		{"writer cannot raise above global", "PUT", "/api/usage/projects/owner/x", "writer", `{"weekly":95}`, 400, "invalid"},
		{"unknown project", "PUT", "/api/usage/projects/owner/none", "writer", `{"fiveHour":50}`, 404, "invalid"},
	}
	for _, c := range cases {
		w := e.call(c.method, c.path, c.token, c.body)
		if w.Code != c.want {
			t.Fatalf("%s: status %d, want %d: %s", c.name, w.Code, c.want, w.Body)
		}
		a := e.lastAudit(t)
		if a.Action != "set-limits" || a.Result != c.result {
			t.Fatalf("%s: audit %+v", c.name, a)
		}
	}
	// review #102 finding 6: a refused attempt records the current values
	e.call("PUT", "/api/usage/global", "writer", `{"fiveHour":10}`)
	if a := e.lastAudit(t); a.Result != "denied" || !strings.Contains(string(mustJSON(t, a.Request)), `"before":{"fiveHour":70,"weekly":85}`) {
		t.Fatalf("refused attempt without before: %+v", a.Request)
	}
	s := settings()
	if s.Global != (thresholds{70, 85}) || s.Projects["owner/x"] != (thresholds{FiveHour: 50}) {
		t.Fatalf("stored: %+v", s)
	}
	// clear the project value with null
	if w := e.call("PUT", "/api/usage/projects/owner/x", "writer", `{"fiveHour":null}`); w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	if _, ok := settings().Projects["owner/x"]; ok {
		t.Fatal("project value not cleared")
	}
	if w := e.call("PUT", "/api/usage/global", "", `{"fiveHour":50}`); w.Code != 401 || e.lastAudit(t).Result != "unauthenticated" {
		t.Fatalf("anonymous: %d", w.Code)
	}
	// review #102: a writer of the project sees it and its agent; a reader sees only the global limits; an admin all
	for _, c := range []struct {
		token   string
		visible bool
	}{{"writer", true}, {"group", true}, {"reader", false}} {
		w := e.call("GET", "/api/usage", c.token, "")
		var v usageView
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Global != (thresholds{70, 85}) {
			t.Fatalf("GET /api/usage as %s: %d %s", c.token, w.Code, w.Body)
		}
		_, sees := v.Agents["alice"]
		if sees != c.visible || (c.visible && v.Effective["owner/x"] != (thresholds{70, 85})) ||
			(!c.visible && len(v.Effective)+len(v.Projects) != 0) {
			t.Fatalf("GET /api/usage as %s: visibility want %v: %s", c.token, c.visible, w.Body)
		}
		if c.token != "group" && v.State != nil {
			t.Fatalf("GET /api/usage as %s: subscription state shown to a non-admin: %s", c.token, w.Body)
		}
	}
}
