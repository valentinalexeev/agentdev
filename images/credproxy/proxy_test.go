package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testSigning = []byte("0123456789abcdef0123456789abcdef-signing")

type fixture struct {
	t        *testing.T
	srv      *httptest.Server
	got      *http.Request
	gotBody  string
	upstream func(w http.ResponseWriter, r *http.Request)
	logs     *bytes.Buffer
	b        *budget
	credDir  string
}

func newFixture(t *testing.T, def int64) *fixture {
	f := &fixture{t: t, logs: &bytes.Buffer{}}
	out = f.logs
	t.Cleanup(func() { out = os.Stdout })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.got = r.Clone(r.Context())
		b, _ := io.ReadAll(r.Body)
		f.gotBody = string(b)
		f.upstream(w, r)
	}))
	t.Cleanup(up.Close)
	f.credDir = t.TempDir()
	os.MkdirAll(filepath.Join(f.credDir, "claude-shared"), 0o700)
	os.MkdirAll(filepath.Join(f.credDir, "claude-bob"), 0o700)
	os.MkdirAll(filepath.Join(f.credDir, "claude-carol"), 0o700) // optional volume of a Secret that does not exist: empty
	os.WriteFile(filepath.Join(f.credDir, "claude-shared", "CLAUDE_CODE_OAUTH_TOKEN"), []byte("sk-ant-oat-shared\n"), 0o600)
	os.WriteFile(filepath.Join(f.credDir, "claude-bob", "ANTHROPIC_API_KEY"), []byte("sk-ant-api-bob"), 0o600)
	u, _ := url.Parse(up.URL)
	f.b = newBudget(def, t.TempDir(), filepath.Join(t.TempDir(), "state.json"))
	f.srv = httptest.NewServer(newServer(testSigning, credStore{f.credDir}, f.b, u, up.Client().Transport))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fixture) post(path, key, body string, hdr map[string]string) *http.Response {
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func lines(buf *bytes.Buffer, prefix string) []string {
	var r []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(l, prefix) {
			r = append(r, strings.TrimPrefix(l, prefix))
		}
	}
	return r
}

func TestKeys(t *testing.T) {
	k := agentKey(testSigning, "alice")
	if verifyKey(testSigning, k) != "alice" {
		t.Fatal("valid key refused")
	}
	for _, bad := range []string{"", "alice", "alice.", "bob" + k[5:], k + "0", "Alice" + k[5:], agentKey([]byte("other-signing-key-of-32-bytes-or-more"), "alice")} {
		if verifyKey(testSigning, bad) != "" {
			t.Errorf("key %q accepted", bad)
		}
	}
	if verifyKey([]byte("short"), agentKey([]byte("short"), "alice")) != "" {
		t.Error("short signing key accepted")
	}
	// the shell computation of the deploy gives the same key
	sh := exec.Command("sh", "-c", `printf 'agentdev-claude-proxy/v1:%s' alice | openssl dgst -sha256 -hmac "$K" -r | cut -d' ' -f1`)
	sh.Env = append(os.Environ(), "K="+string(testSigning))
	if o, err := sh.Output(); err == nil && "alice."+strings.TrimSpace(string(o)) != k {
		t.Errorf("openssl key %s differs from %s", o, k)
	}
	if callerKey("Bearer x", "y") != "x" || callerKey("", " y ") != "y" {
		t.Error("callerKey")
	}
}

func TestSwapsCredentialAndMetersJSON(t *testing.T) {
	f := newFixture(t, 0)
	reset5 := time.Now().Add(2 * time.Hour).Unix()
	f.upstream = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
		w.Header().Set("anthropic-ratelimit-unified-5h-reset", jsonInt(reset5))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m","usage":{"input_tokens":10,"cache_read_input_tokens":5,"output_tokens":7}}`)
	}
	resp := f.post("/v1/messages", agentKey(testSigning, "alice"), `{"model":"x"}`, map[string]string{"X-Api-Key": "smuggled", "Anthropic-Beta": "foo"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"output_tokens":7`) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if f.got.Header.Get("Authorization") != "Bearer sk-ant-oat-shared" || f.got.Header.Get("X-Api-Key") != "" {
		t.Errorf("credential not swapped: %v", f.got.Header)
	}
	if f.got.Header.Get("Anthropic-Beta") != "foo,oauth-2025-04-20" {
		t.Errorf("beta header %q", f.got.Header.Get("Anthropic-Beta"))
	}
	if f.gotBody != `{"model":"x"}` {
		t.Errorf("body changed: %s", f.gotBody)
	}
	u := lines(f.logs, usagePrefix)
	if len(u) != 1 {
		t.Fatalf("usage lines %v", u)
	}
	var ul usageLine
	json.Unmarshal([]byte(u[0]), &ul)
	if ul.Agent != "alice" || ul.Secret != "claude-shared" || ul.InputTokens != 15 || ul.OutputTokens != 7 || ul.FiveHour == nil || ul.FiveHour.Utilization != 0.42 || ul.FiveHour.ResetsAt != reset5 {
		t.Errorf("usage %+v", ul)
	}
	if strings.Contains(f.logs.String(), "sk-ant") {
		t.Error("credential printed")
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestAPIKeyPerAgent(t *testing.T) {
	f := newFixture(t, 0)
	f.upstream = func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) }
	resp := f.post("/v1/messages", agentKey(testSigning, "bob"), `{}`, nil)
	resp.Body.Close()
	if f.got.Header.Get("X-Api-Key") != "sk-ant-api-bob" || f.got.Header.Get("Authorization") != "" {
		t.Errorf("headers %v", f.got.Header)
	}
}

func TestSSEUsage(t *testing.T) {
	f := newFixture(t, 0)
	f.upstream = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"cache_creation_input_tokens\":20,\"output_tokens\":1}}}\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":30}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":55}}\n\n")
	}
	resp := f.post("/v1/messages", agentKey(testSigning, "alice"), `{}`, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	var ul usageLine
	u := lines(f.logs, usagePrefix)
	if len(u) != 1 {
		t.Fatalf("usage %v", u)
	}
	json.Unmarshal([]byte(u[0]), &ul)
	if ul.InputTokens != 120 || ul.OutputTokens != 55 {
		t.Errorf("usage %+v", ul)
	}
}

func TestRateLimited(t *testing.T) {
	f := newFixture(t, 0)
	reset := time.Now().Add(3 * time.Hour).Unix()
	f.upstream = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-reset", jsonInt(reset))
		w.WriteHeader(429)
		io.WriteString(w, `{"type":"error"}`)
	}
	resp := f.post("/v1/messages", agentKey(testSigning, "alice"), `{}`, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	var ul usageLine
	json.Unmarshal([]byte(lines(f.logs, usagePrefix)[0]), &ul)
	if !ul.RateLimited || ul.ResetsAt != reset {
		t.Errorf("usage %+v", ul)
	}
}

func TestRefusals(t *testing.T) {
	f := newFixture(t, 0)
	called := false
	f.upstream = func(w http.ResponseWriter, r *http.Request) { called = true }
	for _, c := range []struct {
		path, key string
		status    int
		rule      string
	}{
		{"/v1/messages", "", 401, "proxy-key-invalid"},
		{"/v1/messages", "alice.deadbeef", 401, "proxy-key-invalid"},
		{"/v1/models", agentKey(testSigning, "alice"), 403, "proxy-path"},
		{"/v1/messages/../files", agentKey(testSigning, "alice"), 403, "proxy-path"},
		{"/v1%2Fmessages", agentKey(testSigning, "alice"), 403, "proxy-path"},
		{"/v1/messages?x=1", agentKey(testSigning, "alice"), 403, "proxy-path"},
	} {
		resp := f.post(c.path, c.key, `{}`, nil)
		resp.Body.Close()
		if resp.StatusCode != c.status {
			t.Errorf("%s: status %d", c.path, resp.StatusCode)
		}
		if !strings.Contains(f.logs.String(), `"rule":"`+c.rule+`"`) {
			t.Errorf("%s: no event %s", c.path, c.rule)
		}
	}
	if called {
		t.Error("refused request reached upstream")
	}
	// agent with no credential at all
	os.RemoveAll(filepath.Join(f.credDir, "claude-shared"))
	resp := f.post("/v1/messages", agentKey(testSigning, "carol"), `{}`, nil)
	resp.Body.Close()
	if resp.StatusCode != 503 || called {
		t.Errorf("no credential: %d", resp.StatusCode)
	}
}

func TestBudget(t *testing.T) {
	f := newFixture(t, 100)
	f.upstream = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"usage":{"input_tokens":60,"output_tokens":50}}`)
	}
	key := agentKey(testSigning, "alice")
	for i, want := range []int{200, 429, 429} {
		resp := f.post("/v1/messages", key, `{}`, nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("request %d: %d", i, resp.StatusCode)
		}
	}
	if n := strings.Count(f.logs.String(), "budget-exhausted"); n != 1 {
		t.Errorf("%d budget events, want 1", n)
	}
	// another agent has its own budget; a per-agent file raises alice's
	resp := f.post("/v1/messages", agentKey(testSigning, "bob"), `{}`, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("bob: %d", resp.StatusCode)
	}
	os.WriteFile(filepath.Join(f.b.dir, "alice"), []byte("1000\n"), 0o644)
	resp = f.post("/v1/messages", key, `{}`, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("alice after raise: %d", resp.StatusCode)
	}
	// a bad override falls back to the default
	os.WriteFile(filepath.Join(f.b.dir, "alice"), []byte("$(id)"), 0o644)
	if f.b.limit("alice") != 100 {
		t.Error("bad override taken")
	}
	// status shows spend, never credentials
	r, _ := http.Get(f.srv.URL + "/status")
	b, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(b), `"agent":"alice"`) || strings.Contains(string(b), "sk-ant") {
		t.Errorf("status %s", b)
	}
}

func TestBudgetWindowAndState(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	dir := t.TempDir()
	b := newBudget(100, "", filepath.Join(dir, "s.json"))
	b.now = func() time.Time { return now }
	b.add("alice", 100)
	if ok, _, _ := b.allow("alice"); ok {
		t.Error("over budget allowed")
	}
	b2 := newBudget(100, "", filepath.Join(dir, "s.json")) // restart keeps the spend
	b2.now = b.now
	if ok, used, _ := b2.allow("alice"); ok || used != 100 {
		t.Errorf("state not kept: %d", used)
	}
	now = now.Add(25 * time.Hour)
	if ok, used, _ := b.allow("alice"); !ok || used != 0 {
		t.Errorf("window did not roll: %d", used)
	}
}

func TestMeterCapsJSON(t *testing.T) {
	m := &tokenMeter{}
	m.Write(bytes.Repeat([]byte("x"), meterCap+1))
	if in, o := m.totals(); in != 0 || o != 0 {
		t.Error("oversized body counted")
	}
}

// review #119 finding 1: a client asking for gzip must not make the meter count 0 tokens.
func TestGzipDoesNotBypassMeter(t *testing.T) {
	f := newFixture(t, 0)
	f.upstream = func(w http.ResponseWriter, r *http.Request) {
		body := `{"usage":{"input_tokens":40,"output_tokens":2}}`
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			io.WriteString(zw, body)
			zw.Close()
			return
		}
		io.WriteString(w, body)
	}
	resp := f.post("/v1/messages", agentKey(testSigning, "alice"), `{}`, map[string]string{"Accept-Encoding": "gzip"})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	var ul usageLine
	json.Unmarshal([]byte(lines(f.logs, usagePrefix)[0]), &ul)
	if ul.InputTokens != 40 || ul.OutputTokens != 2 {
		t.Errorf("gzip response metered as %+v", ul)
	}
}

func TestBetaQueryAllowed(t *testing.T) {
	f := newFixture(t, 0)
	f.upstream = func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) }
	resp := f.post("/v1/messages?beta=true", agentKey(testSigning, "alice"), `{}`, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 || f.got.URL.RawQuery != "beta=true" {
		t.Errorf("status %d query %q", resp.StatusCode, f.got.URL.RawQuery)
	}
}
