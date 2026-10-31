package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func fakeVerify(_ context.Context, raw string) (*identity, error) {
	if raw == "good" {
		return &identity{Subject: "s1", Username: "owner"}, nil
	}
	return nil, errors.New("bad token")
}

func sts(name string, replicas, ready int32, ann map[string]string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-" + name, Namespace: "coding-agents",
			Labels: map[string]string{"app": "coding-agent", "agent": name}, Annotations: ann},
		Spec:   appsv1.StatefulSetSpec{Replicas: &replicas},
		Status: appsv1.StatefulSetStatus{ReadyReplicas: ready},
	}
}

func testServer() http.Handler {
	c := fake.NewSimpleClientset(
		sts("bob", 1, 0, nil),
		sts("alice", 1, 1, map[string]string{"agentdev/repos": "owner/a, owner/b", "agentdev/forge": "gitea"}),
		sts("carol", 0, 0, nil),
		sts("dave", 0, 0, map[string]string{"agentdev/blocked": "true"}),
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "coding-agents"}}, // not an agent
	)
	return routes(fakeVerify, &kubeAgents{c: c, ns: "coding-agents"})
}

func do(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestUnauthenticated(t *testing.T) {
	h := testServer()
	for _, p := range []string{"/api/agents", "/api/me"} {
		if w := do(t, h, p, ""); w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s without token: %d", p, w.Code)
		}
		if w := do(t, h, p, "forged"); w.Code != http.StatusUnauthorized {
			t.Errorf("%s with bad token: %d", p, w.Code)
		}
	}
	if w := do(t, h, "/healthz", ""); w.Code != http.StatusOK {
		t.Errorf("healthz: %d", w.Code)
	}
}

func TestAgentsOverview(t *testing.T) {
	w := do(t, testServer(), "/api/agents", "good")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got struct{ Agents []agent }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"alice": "running", "bob": "starting", "carol": "stopped", "dave": "blocked"}
	if len(got.Agents) != len(want) {
		t.Fatalf("agents: %+v", got.Agents)
	}
	for i, a := range got.Agents {
		if want[a.Name] != a.Phase {
			t.Errorf("%s: phase %s, want %s", a.Name, a.Phase, want[a.Name])
		}
		if i > 0 && got.Agents[i-1].Name > a.Name {
			t.Errorf("not sorted: %v", got.Agents)
		}
	}
	if a := got.Agents[0]; len(a.Repos) != 2 || a.Repos[1] != "owner/b" || a.Forge != "gitea" {
		t.Errorf("alice: %+v", a)
	}
}

func TestMe(t *testing.T) {
	w := do(t, testServer(), "/api/me", "good")
	var id identity
	_ = json.Unmarshal(w.Body.Bytes(), &id)
	if w.Code != http.StatusOK || id.Username != "owner" {
		t.Errorf("me: %d %+v", w.Code, id)
	}
}
