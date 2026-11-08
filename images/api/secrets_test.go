package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

const valuesNS = "agentdev-secret-values"

func secretsEnv(t *testing.T, forgeURL string) *tenv {
	t.Helper()
	d := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{agentsGVR: "AgentList", projectSecretsGVR: "ProjectSecretList"}, agentObj("alice", "owner/x", false))
	k := &kubeAgents{c: fake.NewSimpleClientset(), d: d, ns: "coding-agents"}
	buf := &bytes.Buffer{}
	old := auditOut
	auditOut = buf
	t.Cleanup(func() { auditOut = old })
	now = func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) }
	wr := &writer{store: k, perms: newGiteaPerms(forgeURL, "svc"), adminGroup: "agentdev-admins"}
	wr.secrets = &secretsAPI{w: wr, ps: k, values: &kubeValues{k: k, ns: valuesNS}, agents: k}
	return &tenv{h: routes(idVerify, k, nil, wr), k: k, audit: buf}
}

func (e *tenv) value(t *testing.T, name string) string {
	t.Helper()
	s, err := e.k.c.CoreV1().Secrets(valuesNS).Get(context.Background(), "agentdev-ps-"+name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return string(s.Data["value"])
}

const npmBody = `{"name":"npm","scope":"project","project":"owner/x","mode":"broker","broker":{"upstream":"https://npm.example.com"},"value":"tok-SECRET-123"}`

func TestSecretsAuthorization(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	cases := []struct {
		name, method, path, token, body string
		down                            bool
		want                            int
	}{
		{"admin creates", "POST", "/api/secrets", "admin", npmBody, false, 201},
		{"group creates without forge", "POST", "/api/secrets", "group", npmBody, true, 201},
		{"writer cannot create", "POST", "/api/secrets", "writer", npmBody, false, 403},
		{"reader cannot create", "POST", "/api/secrets", "reader", npmBody, false, 403},
		{"forge down: admin refused", "POST", "/api/secrets", "admin", npmBody, true, 503},
		{"agent scope uses the agent's repo", "POST", "/api/secrets", "admin",
			`{"name":"a1","scope":"agent","agent":"alice","mode":"broker","broker":{"upstream":"https://h.example"},"value":"v-123456789"}`, false, 201},
		{"unknown agent", "POST", "/api/secrets", "admin",
			`{"name":"a1","scope":"agent","agent":"ghost","mode":"broker","broker":{"upstream":"https://h.example"},"value":"v"}`, false, 400},
		{"no token", "POST", "/api/secrets", "", npmBody, false, 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url := gitea.URL
			if c.down {
				url = "http://127.0.0.1:1"
			}
			e := secretsEnv(t, url)
			w := e.call(c.method, c.path, c.token, c.body)
			if w.Code != c.want {
				t.Fatalf("status %d, want %d: %s", w.Code, c.want, w.Body)
			}
			if strings.Contains(e.audit.String(), "SECRET-123") || strings.Contains(w.Body.String(), "SECRET-123") {
				t.Fatal("value in the audit log or the response")
			}
			if c.want == 201 && e.value(t, "npm")+e.value(t, "a1") == "" {
				t.Error("value not written")
			}
			if c.want >= 300 && e.value(t, "npm") != "" {
				t.Error("value written on a refused request")
			}
		})
	}
}

func TestSecretsValidation(t *testing.T) {
	e := secretsEnv(t, "")
	cases := map[string]string{
		"reserved env":   `{"name":"x","scope":"project","project":"owner/x","mode":"env","env":{"name":"ANTHROPIC_API_KEY","riskAccepted":true},"value":"v"}`,
		"env no risk":    `{"name":"x","scope":"project","project":"owner/x","mode":"env","env":{"name":"DB_PASSWORD"},"value":"v"}`,
		"http upstream":  `{"name":"x","scope":"project","project":"owner/x","mode":"broker","broker":{"upstream":"http://h.example"},"value":"v"}`,
		"both modes":     `{"name":"x","scope":"project","project":"owner/x","mode":"broker","broker":{"upstream":"https://h"},"env":{"name":"A"},"value":"v"}`,
		"no value":       `{"name":"x","scope":"project","project":"owner/x","mode":"broker","broker":{"upstream":"https://h.example"}}`,
		"blank value":    `{"name":"x","scope":"project","project":"owner/x","mode":"broker","broker":{"upstream":"https://h.example"},"value":"  "}`,
		"bad name":       `{"name":"X_Y","scope":"project","project":"owner/x","mode":"broker","broker":{"upstream":"https://h.example"},"value":"v"}`,
		"scope mismatch": `{"name":"x","scope":"project","agent":"alice","project":"owner/x","mode":"broker","broker":{"upstream":"https://h.example"},"value":"v"}`,
		"unknown field":  `{"name":"x","scope":"project","project":"owner/x","mode":"broker","broker":{"upstream":"https://h.example"},"value":"v","status":{}}`,
		"bad scheme":     `{"name":"x","scope":"project","project":"owner/x","mode":"broker","broker":{"upstream":"https://h.example","scheme":"digest"},"value":"v"}`,
		"traversal repo": `{"name":"x","scope":"project","project":"../x","mode":"broker","broker":{"upstream":"https://h.example"},"value":"v"}`,
	}
	for name, body := range cases {
		if w := e.call("POST", "/api/secrets", "group", body); w.Code != 400 {
			t.Errorf("%s: status %d %s", name, w.Code, w.Body)
		}
	}
}

func TestSecretsRotateListDelete(t *testing.T) {
	gitea := fakeGitea(t)
	defer gitea.Close()
	e := secretsEnv(t, gitea.URL)
	if w := e.call("POST", "/api/secrets", "admin", npmBody); w.Code != 201 {
		t.Fatal(w.Body)
	}
	if w := e.call("PUT", "/api/secrets/npm/value", "writer", `{"value":"new-SECRET-456"}`); w.Code != 403 {
		t.Errorf("writer rotated: %d", w.Code)
	}
	if w := e.call("PUT", "/api/secrets/npm/value", "admin", `{"value":"new-SECRET-456"}`); w.Code != 204 {
		t.Fatalf("rotate %d %s", w.Code, w.Body)
	}
	if e.value(t, "npm") != "new-SECRET-456" {
		t.Error("rotation not written")
	}
	if a := e.lastAudit(t); a.Action != "secret-rotate" || a.Result != "ok" || a.Grant != "repo-admin" {
		t.Errorf("audit %+v", a)
	}
	w := e.call("GET", "/api/secrets", "admin", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"npm"`) || strings.Contains(w.Body.String(), "SECRET") {
		t.Errorf("list %d %s", w.Code, w.Body)
	}
	if w := e.call("GET", "/api/secrets", "reader", ""); !strings.Contains(w.Body.String(), `"secrets":[]`) {
		t.Errorf("reader sees secrets: %s", w.Body)
	}
	if w := e.call("DELETE", "/api/secrets/npm", "admin", ""); w.Code != 204 {
		t.Fatalf("delete %d %s", w.Code, w.Body)
	}
	if e.value(t, "npm") != "" {
		t.Error("value kept after delete")
	}
	if _, err := e.k.GetPS(context.Background(), "npm"); err == nil {
		t.Error("object kept after delete")
	}
	if w := e.call("DELETE", "/api/secrets/npm", "admin", ""); w.Code != 404 {
		t.Errorf("second delete %d", w.Code)
	}
	if strings.Contains(e.audit.String(), "SECRET") {
		t.Error("value in the audit log")
	}
}

func TestSecretsObject(t *testing.T) {
	e := secretsEnv(t, "")
	body := `{"name":"db","scope":"project","project":"owner/x","forge":"gitea","mode":"env","env":{"name":"DB_PASSWORD","riskAccepted":true},"description":"test db","value":"pw-123456"}`
	if w := e.call("POST", "/api/secrets", "group", body); w.Code != 201 {
		t.Fatal(w.Body)
	}
	u, _ := e.k.GetPS(context.Background(), "db")
	spec, _, _ := unstructured.NestedMap(u.Object, "spec")
	if spec["mode"] != "env" || spec["env"].(map[string]any)["riskAccepted"] != true || spec["broker"] != nil || spec["description"] != "test db" {
		t.Errorf("spec %v", spec)
	}
	if strings.Contains(strings.ToLower(jsonOf(u.Object)), "pw-123456") {
		t.Error("value in the object")
	}
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
