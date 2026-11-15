package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// R35 (#308): two users, two agents. Each user sees only the events of the own agent (write access on its
// repository); the admin group sees all; a user with read access only sees none.
func TestSecurityEventsVisibility(t *testing.T) {
	perms := map[string]string{"owner/x/wendy": "write", "owner/y/rita": "write", "owner/x/rita": "read"}
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/"), "/permission")
		p = strings.Replace(p, "/collaborators/", "/", 1)
		v, ok := perms[p]
		if r.Header.Get("Authorization") != "token svc" || !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"permission": v})
	}))
	defer forge.Close()
	d := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{agentsGVR: "AgentList"},
		agentObj("alice", "owner/x", false), agentObj("bob", "owner/y", false))
	k := &kubeAgents{c: fake.NewSimpleClientset(), d: d, ns: "coding-agents"}
	wr := &writer{store: k, perms: newGiteaPerms(forge.URL, "svc"), adminGroup: "agentdev-admins", usage: k}
	m := newMemStore()
	now := time.Now().UTC()
	for _, e := range []securityEvent{ev("alice", "high", "a1", now), ev("bob", "high", "b1", now), ev("alice", "low", "a2", now)} {
		e := e
		if _, err := m.Insert(context.Background(), &e); err != nil {
			t.Fatal(err)
		}
	}
	h := routes(idVerify, k, m, wr)
	for _, c := range []struct {
		token, query string
		want         []string
	}{
		{"writer", "", []string{"a2", "a1"}},      // wendy: write on owner/x -> alice
		{"reader", "", []string{"b1"}},            // rita: write on owner/y -> bob, read only on owner/x
		{"group", "", []string{"a2", "b1", "a1"}}, // admin group: every agent
		{"nobody", "", []string{}},                // no forge login
		{"writer", "?agent=bob", []string{}},      // a filter cannot widen the view
		{"writer", "?limit=1", []string{"a2"}},    // paging counts only visible events
	} {
		w := do(t, h, "/api/security-events"+c.query, c.token)
		var p page
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatalf("%s %s: %d %s", c.token, c.query, w.Code, w.Body)
		}
		got := []string{}
		for _, e := range p.Events {
			got = append(got, e.Hash)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Fatalf("%s %s: got %v, want %v", c.token, c.query, got, c.want)
		}
	}
}

// R35 (#380): GET /api/agents lists only the agents the caller may see; the admin group sees all.
func TestAgentsVisibility(t *testing.T) {
	perms := map[string]string{"owner/x/wendy": "write", "owner/y/rita": "write", "owner/x/rita": "read"}
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/"), "/permission")
		p = strings.Replace(p, "/collaborators/", "/", 1)
		v, ok := perms[p]
		if r.Header.Get("Authorization") != "token svc" || !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"permission": v})
	}))
	defer forge.Close()
	d := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{agentsGVR: "AgentList"},
		agentObj("alice", "owner/x", false), agentObj("bob", "owner/y", false))
	k := &kubeAgents{c: fake.NewSimpleClientset(), d: d, ns: "coding-agents"}
	wr := &writer{store: k, perms: newGiteaPerms(forge.URL, "svc"), adminGroup: "agentdev-admins", usage: k}
	m := newMemStore()
	now := time.Now().UTC()
	for _, e := range []securityEvent{ev("alice", "high", "a1", now), ev("bob", "high", "b1", now)} {
		e := e
		if _, err := m.Insert(context.Background(), &e); err != nil {
			t.Fatal(err)
		}
	}
	h := routes(idVerify, k, m, wr)
	for _, c := range []struct {
		token, query string
		want         string
	}{
		{"writer", "", "alice"},
		{"reader", "", "bob"}, // read access on owner/x is not enough
		{"group", "", "alice,bob"},
		{"nobody", "", ""},
		{"writer", "?repo=owner/y", "alice"}, // a query cannot widen the view (no bob), and no 403 reveals bob
	} {
		w := do(t, h, "/api/agents"+c.query, c.token)
		var p struct{ Agents []agent }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatalf("%s %s: %d %s", c.token, c.query, w.Code, w.Body)
		}
		got := []string{}
		for _, a := range p.Agents {
			got = append(got, a.Name)
			if a.SecurityEvents != 1 {
				t.Fatalf("%s: %s count %d", c.token, a.Name, a.SecurityEvents)
			}
		}
		if strings.Join(got, ",") != c.want {
			t.Fatalf("%s %s: got %v, want %v", c.token, c.query, got, c.want)
		}
	}
}
