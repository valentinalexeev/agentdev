package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeGitea keeps the state forge-init touches, so a test can run it twice and count the writes.
type fakeGitea struct {
	mu      sync.Mutex
	users   map[string]bool
	collab  map[string]string // repo/user -> permission
	labels  map[string][]string
	hooks   map[string][]map[string]any
	protect map[string]map[string]any
	writes  []string
	secrets []string // hook secrets seen
	auth    string
}

func newFakeGitea() *fakeGitea {
	return &fakeGitea{users: map[string]bool{"alice": true}, collab: map[string]string{}, labels: map[string][]string{},
		hooks: map[string][]map[string]any{}, protect: map[string]map[string]any{}}
}

func (f *fakeGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = r.Header.Get("Authorization")
	if f.auth != "token admintok" {
		w.WriteHeader(401)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if r.Method != "GET" {
		f.writes = append(f.writes, r.Method+" "+p)
	}
	js := func(code int, v any) { w.WriteHeader(code); _ = json.NewEncoder(w).Encode(v) }
	parts := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	case parts[0] == "users" && len(parts) == 2:
		if f.users[parts[1]] {
			js(200, map[string]any{"login": parts[1]})
		} else {
			w.WriteHeader(404)
		}
	case p == "/admin/users":
		f.users[body["username"].(string)] = true
		w.WriteHeader(201)
	case parts[0] == "repos" && len(parts) == 3:
		if parts[1]+"/"+parts[2] == "o/missing" {
			w.WriteHeader(404)
			return
		}
		js(200, map[string]any{"default_branch": "main"})
	case len(parts) >= 5 && parts[3] == "collaborators":
		key := parts[1] + "/" + parts[2] + "/" + parts[4]
		if r.Method == "PUT" {
			f.collab[key] = body["permission"].(string)
			w.WriteHeader(204)
		} else if perm, ok := f.collab[key]; ok {
			js(200, map[string]any{"permission": perm})
		} else {
			w.WriteHeader(404)
		}
	case len(parts) == 4 && parts[3] == "labels":
		repo := parts[1] + "/" + parts[2]
		if r.Method == "POST" {
			f.labels[repo] = append(f.labels[repo], body["name"].(string))
			w.WriteHeader(201)
			return
		}
		out := []map[string]string{}
		for _, l := range f.labels[repo] {
			out = append(out, map[string]string{"name": l})
		}
		js(200, out)
	case len(parts) >= 4 && parts[3] == "hooks":
		repo := parts[1] + "/" + parts[2]
		switch r.Method {
		case "POST":
			cfg := body["config"].(map[string]any)
			f.secrets = append(f.secrets, cfg["secret"].(string))
			f.hooks[repo] = append(f.hooks[repo], map[string]any{"id": len(f.hooks[repo]) + 1, "config": map[string]any{"url": cfg["url"]}})
			w.WriteHeader(201)
		case "PATCH":
			f.secrets = append(f.secrets, body["config"].(map[string]any)["secret"].(string))
			w.WriteHeader(200)
		case "DELETE":
			w.WriteHeader(204)
		default:
			js(200, f.hooks[repo])
		}
	case len(parts) >= 4 && parts[3] == "branch_protections":
		repo := parts[1] + "/" + parts[2]
		switch r.Method {
		case "POST", "PATCH":
			f.protect[repo] = body
			w.WriteHeader(map[string]int{"POST": 201, "PATCH": 200}[r.Method])
		default:
			if b, ok := f.protect[repo]; ok {
				js(200, b)
			} else {
				w.WriteHeader(404)
			}
		}
	default:
		w.WriteHeader(404)
	}
}

func forgeInitCfg() ForgeInitConfig {
	return ForgeInitConfig{Agents: []ForgeInitAgent{{Name: "alice", User: "alice", Repos: "o/r"},
		{Name: "bob", User: "bob-bot", Repos: "o/r, o/s"}}, APIUser: "agentdev-api", RouterUser: "agentdev-router",
		Reviewer: "owner", HookURL: "https://hooks.example/", CreateAccounts: true, Protect: true}
}

func TestForgeInitIdempotent(t *testing.T) {
	f := newFakeGitea()
	srv := httptest.NewServer(f)
	defer srv.Close()
	var out bytes.Buffer
	g := &giteaAdmin{base: srv.URL + "/api/v1", token: "admintok", secret: "hooksecret", hc: srv.Client(), out: &out}
	if err := g.run(forgeInitCfg()); err != nil {
		t.Fatal(err)
	}
	if !f.users["bob-bot"] || f.collab["o/r/alice"] != "write" || f.collab["o/s/bob-bot"] != "write" ||
		f.collab["o/r/agentdev-api"] != "admin" || f.collab["o/s/agentdev-router"] != "read" {
		t.Fatalf("state after the first run: users %v collab %v", f.users, f.collab)
	}
	if len(f.hooks["o/r"]) != 1 || len(f.hooks["o/s"]) != 1 || f.protect["o/r"]["approvals_whitelist_username"] == nil {
		t.Fatalf("hooks %v protect %v", f.hooks, f.protect)
	}
	if strings.Contains(out.String(), "hooksecret") || strings.Contains(out.String(), "admintok") {
		t.Fatal("a secret value is in the output")
	}
	// second run: only the hook secret refresh (PATCH hooks) writes, nothing counts as a change
	f.writes = nil
	g2 := &giteaAdmin{base: srv.URL + "/api/v1", token: "admintok", secret: "hooksecret", hc: srv.Client(), out: &out}
	if err := g2.run(forgeInitCfg()); err != nil {
		t.Fatal(err)
	}
	if g2.change != 0 {
		t.Fatalf("second run changed %d things: %s", g2.change, out.String())
	}
	for _, w := range f.writes {
		if !strings.HasPrefix(w, "PATCH ") || !strings.Contains(w, "/hooks/") {
			t.Fatalf("second run wrote %s", w)
		}
	}
	for _, s := range f.secrets {
		if s != "hooksecret" {
			t.Fatalf("hook secret %q", s)
		}
	}
}

func TestForgeInitFailsClosed(t *testing.T) {
	f := newFakeGitea()
	srv := httptest.NewServer(f)
	defer srv.Close()
	for name, tc := range map[string]struct {
		token, secret string
		cfg           func(*ForgeInitConfig)
		want          string
	}{
		"no token":        {"", "s", func(*ForgeInitConfig) {}, "GITEA_ADMIN_TOKEN"},
		"wrong token":     {"bad", "s", func(*ForgeInitConfig) {}, "HTTP 401"},
		"hook, no secret": {"admintok", "", func(*ForgeInitConfig) {}, "WEBHOOK_SECRET"},
		"missing repo":    {"admintok", "s", func(c *ForgeInitConfig) { c.Agents[0].Repos = "o/missing" }, "must exist"},
		"bad repo":        {"admintok", "s", func(c *ForgeInitConfig) { c.Agents[0].Repos = "../x" }, "not owner/name"},
		"no reviewer":     {"admintok", "s", func(c *ForgeInitConfig) { c.Reviewer = "" }, "reviewer"},
	} {
		c := forgeInitCfg()
		tc.cfg(&c)
		g := &giteaAdmin{base: srv.URL + "/api/v1", token: tc.token, secret: tc.secret, hc: srv.Client(), out: &bytes.Buffer{}}
		err := g.run(c)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", name, err, tc.want)
		}
	}
}

func TestForgeInitWithoutOptionalParts(t *testing.T) {
	f := newFakeGitea()
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := ForgeInitConfig{Agents: []ForgeInitAgent{{Name: "alice", User: "alice", Repos: "o/r"}}}
	g := &giteaAdmin{base: srv.URL + "/api/v1", token: "admintok", hc: srv.Client(), out: &bytes.Buffer{}}
	if err := g.run(c); err != nil {
		t.Fatal(err)
	}
	if len(f.hooks) != 0 || len(f.protect) != 0 || len(f.users) != 1 {
		t.Fatalf("optional parts ran: %v %v %v", f.hooks, f.protect, f.users)
	}
	if fmt.Sprint(f.writes) != "[PUT /repos/o/r/collaborators/alice POST /repos/o/r/labels]" {
		t.Fatalf("writes %v", f.writes)
	}
}
