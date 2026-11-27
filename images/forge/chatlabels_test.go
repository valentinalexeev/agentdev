package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testChatSession = "chat-alice-0123abcd"

// fakeChatAPI answers POST /internal/chat/{session}/label-asked: "bob" allowed exactly the calls in allowed
// (repo#issue:labels joined by ","); everything else is 403. It records what the sidecar asked.
func fakeChatAPI(t *testing.T, allowed map[string]bool) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Repo   string   `json:"repo"`
			Issue  int64    `json:"issue"`
			Labels []string `json:"labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		key := in.Repo + "#" + itoa64(in.Issue) + ":" + strings.Join(in.Labels, ",")
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.Path+" "+key)
		mu.Unlock()
		if r.Method != "POST" || r.URL.Path != "/internal/chat/"+testChatSession+"/label-asked" || !allowed[key] {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, `{"person":"bob"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), asked...) }
}

func itoa64(n int64) string { b, _ := json.Marshal(n); return string(b) }

// logTo sends the sidecar's log lines (s.logf) to w for the test.
func logTo(t *testing.T, w io.Writer) {
	old := log.Writer()
	log.SetOutput(w)
	t.Cleanup(func() { log.SetOutput(old) })
}

// #571: a chat pod adds labels to issues and PRs, agent:* included, only when the API names the person who allowed
// exactly that call in the console; the label write is logged with that person.
func TestChatLabelsGitHub(t *testing.T) {
	d, _, calls := newTestGitHub(t)
	api, asked := fakeChatAPI(t, map[string]bool{
		"octo-org/repo#12:bug,triage":   true, // an issue, plain labels
		"octo-org/repo#12:agent:alice":  true, // route the issue to the agent: a person asked for it
		"octo-org/repo#15:agent:bob":    true, // bob's PR, another agent's label
		"octo-org/repo#12:agent:carol ": true, // never reaches the API: not a clean label name
	})
	d.c.ChatSession, d.c.APIURL = testChatSession, api.URL
	s := newServer(d.c)
	s.drv, s.rules = d, d.rules()
	h := s.routes()
	var logs bytes.Buffer
	logTo(t, &logs)
	var events bytes.Buffer
	old := eventOut
	eventOut = &events
	t.Cleanup(func() { eventOut = old })

	for _, c := range []struct{ p, b string }{
		{"/api/v1/repos/octo-org/repo/issues/12/labels", `{"labels":["bug","triage"]}`},
		{"/api/v1/repos/octo-org/repo/issues/12/labels", `{"labels":["agent:alice"]}`},
		{"/api/v1/repos/octo-org/repo/issues/15/labels", `{"labels":["agent:bob"]}`},
	} {
		n := len(calls())
		if rec := call(t, h, "POST", c.p, c.b); rec.Code/100 != 2 {
			t.Fatalf("%s %s: %d %s", c.p, c.b, rec.Code, rec.Body)
		}
		var cs []ghCall
		for _, x := range calls()[n:] {
			if strings.HasPrefix(x.path, "/repos/octo-org/repo/issues/") { // not the App token calls
				cs = append(cs, x)
			}
		}
		if len(cs) != 1 || cs[0].method != "POST" || "/api/v1"+cs[0].path != c.p || cs[0].body != c.b {
			t.Fatalf("label call not forwarded as sent: %+v", cs)
		}
	}
	if !strings.Contains(logs.String(), "chat "+testChatSession+": labels agent:alice on octo-org/repo#12, asked by bob") {
		t.Errorf("label write not logged with the person:\n%s", logs.String())
	}

	refused := []struct{ name, p, b, sev string }{
		{"nobody allowed it", "/api/v1/repos/octo-org/repo/issues/12/labels", `{"labels":["wontfix"]}`, "medium"},
		// #236: the loop's allow-list (needs-decision, in-progress) gives a chat pod nothing without a person
		{"loop allow-list label nobody allowed", "/api/v1/repos/octo-org/repo/issues/12/labels", `{"labels":["needs-decision"]}`, "medium"},
		{"agent label nobody allowed", "/api/v1/repos/octo-org/repo/issues/13/labels", `{"labels":["agent:alice"]}`, "high"},
		{"agent label, case", "/api/v1/repos/octo-org/repo/issues/13/labels", `{"labels":["Agent:alice"]}`, "high"},
		{"allowed set plus one", "/api/v1/repos/octo-org/repo/issues/12/labels", `{"labels":["bug","triage","x"]}`, "medium"},
		{"no labels", "/api/v1/repos/octo-org/repo/issues/12/labels", `{"labels":[]}`, ""},
		{"eleven labels", "/api/v1/repos/octo-org/repo/issues/12/labels", `{"labels":["a","b","c","d","e","f","g","h","i","j","k"]}`, ""},
		{"label twice", "/api/v1/repos/octo-org/repo/issues/12/labels", `{"labels":["bug","bug"]}`, ""},
		{"trailing space", "/api/v1/repos/octo-org/repo/issues/12/labels", `{"labels":["agent:carol "]}`, ""},
		{"invisible character", "/api/v1/repos/octo-org/repo/issues/12/labels", `{"labels":["agent:al\u200bice"]}`, ""},
		{"label id", "/api/v1/repos/octo-org/repo/issues/12/labels", `{"labels":[5]}`, ""},
		{"extra key", "/api/v1/repos/octo-org/repo/issues/12/labels", `{"labels":["bug","triage"],"x":1}`, ""},
		{"other repo", "/api/v1/repos/other/repo/issues/12/labels", `{"labels":["bug","triage"]}`, ""},
	}
	for _, c := range refused {
		n, na := len(calls()), len(asked())
		events.Reset()
		if rec := call(t, h, "POST", c.p, c.b); rec.Code != http.StatusForbidden {
			t.Errorf("%s: want 403, got %d", c.name, rec.Code)
		}
		if len(calls()) != n {
			t.Errorf("%s: a call reached GitHub: %+v", c.name, calls()[n:])
		}
		if c.sev == "" && len(asked()) != na {
			t.Errorf("%s: a bad body reached the API: %v", c.name, asked()[na:])
		}
		if c.sev != "" && !strings.Contains(events.String(), `"rule":"chat-label-not-asked","severity":"`+c.sev+`"`) {
			t.Errorf("%s: want a %s chat-label-not-asked event, got %s", c.name, c.sev, events.String())
		}
	}
	for _, m := range []string{"PUT", "DELETE"} { // replace or remove labels: never, in a chat either
		if rec := call(t, h, m, "/api/v1/repos/octo-org/repo/issues/12/labels", `{"labels":["bug"]}`); rec.Code != http.StatusForbidden {
			t.Errorf("%s labels: want 403, got %d", m, rec.Code)
		}
	}
	// the API is gone: fail closed
	api.Close()
	if rec := call(t, h, "POST", "/api/v1/repos/octo-org/repo/issues/12/labels", `{"labels":["bug","triage"]}`); rec.Code != http.StatusForbidden {
		t.Errorf("API unreachable: want 403, got %d", rec.Code)
	}
}

// #571: the agent pod (no CHAT_SESSION) keeps the #227 rule: the API is never asked, issues are never labelled
func TestLoopLabelsUnchanged(t *testing.T) {
	d, _, calls := newTestGitHub(t)
	api, asked := fakeChatAPI(t, map[string]bool{"octo-org/repo#12:agent:alice": true, "octo-org/repo#14:bug": true})
	d.c.APIURL = api.URL
	s := newServer(d.c)
	s.drv, s.rules = d, d.rules()
	h := s.routes()
	if rec := call(t, h, "POST", "/api/v1/repos/octo-org/repo/issues/14/labels", `{"labels":["agent:alice"]}`); rec.Code/100 != 2 {
		t.Fatalf("own label on own PR: %d %s", rec.Code, rec.Body)
	}
	for _, b := range []string{`{"labels":["agent:alice"]}`, `{"labels":["bug"]}`} {
		for _, n := range []string{"12", "14"} {
			if n == "14" && b == `{"labels":["agent:alice"]}` {
				continue
			}
			before := len(calls())
			if rec := call(t, h, "POST", "/api/v1/repos/octo-org/repo/issues/"+n+"/labels", b); rec.Code != http.StatusForbidden {
				t.Errorf("loop, #%s %s: want 403, got %d", n, b, rec.Code)
			}
			for _, x := range calls()[before:] {
				if x.method != "GET" {
					t.Errorf("loop, #%s %s: a write reached GitHub: %+v", n, b, x)
				}
			}
		}
	}
	if len(asked()) != 0 {
		t.Errorf("the loop asked the chat API: %v", asked())
	}
}

// #571: the same chat policy on Gitea
func TestChatLabelsGitea(t *testing.T) {
	forge, calls := fakeForge(t)
	api, _ := fakeChatAPI(t, map[string]bool{"owner/repo#3:agent:alice": true})
	s := newServer(&Config{ForgeURL: forge.URL, ForgeUser: "alice", Token: "s3cret", Repos: []string{"owner/repo"},
		ChatSession: testChatSession, APIURL: api.URL})
	h := s.routes()
	if rec := call(t, h, "POST", "/api/v1/repos/owner/repo/issues/3/labels", `{"labels":["agent:alice"]}`); rec.Code != 200 {
		t.Fatalf("allowed label: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, "POST", "/api/v1/repos/owner/repo/issues/4/labels", `{"labels":["agent:alice"]}`); rec.Code != http.StatusForbidden {
		t.Fatalf("not allowed: want 403, got %d", rec.Code)
	}
	if len(*calls) != 1 || !strings.HasPrefix((*calls)[0], "POST /api/v1/repos/owner/repo/issues/3/labels ") {
		t.Fatalf("forge calls: %v", *calls)
	}
}

// #571: read-only release state (tags, releases, milestones) on both forges, in both modes
func TestReleaseReads(t *testing.T) {
	gd, _, _ := newTestGitHub(t)
	gs := newServer(gd.c)
	gs.drv, gs.rules = gd, gd.rules()
	forge, _ := fakeForge(t)
	ts := newServer(&Config{ForgeURL: forge.URL, ForgeUser: "alice", Token: "s3cret", Repos: []string{"owner/repo"}})
	for name, c := range map[string]struct {
		h    http.Handler
		repo string
	}{"github": {gs.routes(), "octo-org/repo"}, "gitea": {ts.routes(), "owner/repo"}} {
		for _, p := range []string{"/tags?per_page=100", "/releases", "/releases/latest", "/releases/42", "/releases/tags/v1.3.0-rc.1",
			"/milestones?state=all", "/milestones/7"} {
			if rec := call(t, c.h, "GET", "/api/v1/repos/"+c.repo+p, ""); rec.Code != 200 {
				t.Errorf("%s GET %s: %d %s", name, p, rec.Code, rec.Body)
			}
		}
		for _, x := range []struct{ m, p string }{{"POST", "/releases"}, {"PATCH", "/releases/42"}, {"DELETE", "/releases/42"},
			{"POST", "/milestones"}, {"PATCH", "/milestones/7"}, {"POST", "/tags"}, {"GET", "/releases/0"},
			{"GET", "/releases/tags/.hidden"}, {"GET", "/releases/tags/a/b"}} {
			if rec := call(t, c.h, x.m, "/api/v1/repos/"+c.repo+x.p, `{}`); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s %s: want 403, got %d", name, x.m, x.p, rec.Code)
			}
		}
		if rec := call(t, c.h, "GET", "/api/v1/repos/other/repo/tags", ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s: other repo tags: want 403, got %d", name, rec.Code)
		}
	}
}
