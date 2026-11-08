package main

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type brokerFixture struct {
	srv  *httptest.Server
	up   *httptest.Server
	got  *http.Request
	logs *bytes.Buffer
	dir  string
	resp func(w http.ResponseWriter, r *http.Request)
}

func newBrokerFixture(t *testing.T) *brokerFixture {
	t.Setenv("ALLOW_HTTP_UPSTREAM", "true")
	f := &brokerFixture{logs: &bytes.Buffer{}}
	out = f.logs
	t.Cleanup(func() { out = os.Stdout })
	f.up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.got = r.Clone(r.Context())
		if f.resp != nil {
			f.resp(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"tarball":"`+f.up.URL+`/base/pkg/-/pkg-1.0.0.tgz","echo":"s3cr3t-npm-token"}`)
	}))
	t.Cleanup(f.up.Close)
	f.dir = t.TempDir()
	os.MkdirAll(filepath.Join(f.dir, "values"), 0o700)
	os.WriteFile(filepath.Join(f.dir, "values", "npm"), []byte("s3cr3t-npm-token\n"), 0o600)
	routes := `{"npm":{"upstream":"` + f.up.URL + `/base","header":"Authorization","scheme":"bearer","agents":["alice"]},
	            "raw":{"upstream":"` + f.up.URL + `","header":"X-Token","scheme":"raw","agents":["alice"]},
	            "empty":{"upstream":"` + f.up.URL + `","agents":["alice"]}}`
	os.WriteFile(filepath.Join(f.dir, "routes.json"), []byte(routes), 0o600)
	os.WriteFile(filepath.Join(f.dir, "values", "raw"), []byte("rawvalue-123"), 0o600)
	f.srv = httptest.NewServer(newBroker(testSigning, filepath.Join(f.dir, "routes.json"), filepath.Join(f.dir, "values"), "http://credential-broker.test:8080", f.up.Client().Transport))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *brokerFixture) get(t *testing.T, path, auth string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestBrokerAddsCredentialAndRewrites(t *testing.T) {
	f := newBrokerFixture(t)
	key := agentKey(testSigning, "alice")
	resp, body := f.get(t, "/s/npm/pkg?x=1", "Bearer "+key, map[string]string{"Cookie": "c=1", "X-Api-Key": "smuggled"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	if f.got.URL.Path != "/base/pkg" || f.got.URL.RawQuery != "x=1" {
		t.Errorf("upstream path %s?%s", f.got.URL.Path, f.got.URL.RawQuery)
	}
	if f.got.Header.Get("Authorization") != "Bearer s3cr3t-npm-token" || f.got.Header.Get("Cookie") != "" || f.got.Header.Get("X-Api-Key") != "" {
		t.Errorf("headers %v", f.got.Header)
	}
	if !strings.Contains(body, "http://credential-broker.test:8080/s/npm/pkg/-/pkg-1.0.0.tgz") {
		t.Errorf("tarball link not rewritten: %s", body)
	}
	if strings.Contains(body, "s3cr3t") || strings.Contains(f.logs.String(), "s3cr3t") {
		t.Errorf("value leaked: body %s logs %s", body, f.logs)
	}
	ev := lines(f.logs, eventPrefix)
	if len(ev) != 1 || !strings.Contains(ev[0], `"rule":"broker-use"`) {
		t.Errorf("events %v", ev)
	}
	f.get(t, "/s/npm/pkg", "Bearer "+key, nil) // second use in the hour: no new event
	if len(lines(f.logs, eventPrefix)) != 1 {
		t.Error("one broker-use event per hour expected")
	}
}

func TestBrokerBasicAuthAndRawHeader(t *testing.T) {
	f := newBrokerFixture(t)
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("any:"+agentKey(testSigning, "alice")))
	resp, body := f.get(t, "/s/raw/v2/", basic, map[string]string{"X-Token": "agent-chosen"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	if f.got.Header.Get("X-Token") != "rawvalue-123" || f.got.Header.Get("Authorization") != "" {
		t.Errorf("headers %v", f.got.Header)
	}
}

func TestBrokerRefusals(t *testing.T) {
	f := newBrokerFixture(t)
	alice := "Bearer " + agentKey(testSigning, "alice")
	bob := "Bearer " + agentKey(testSigning, "bob")
	cases := []struct {
		path, auth, rule string
		status           int
	}{
		{"/s/npm/x", "", "broker-key-invalid", 401},
		{"/s/npm/x", "Bearer alice.00", "broker-key-invalid", 401},
		{"/s/npm/x", bob, "broker-secret-denied", 403},
		{"/s/nope/x", alice, "broker-unknown-secret", 404},
		{"/s/npm/../x", alice, "", 0}, // the client cleans it; checked below with a raw path
		{"/s/npm/a%2F..%2Fx", alice, "broker-path", 400},
		{"/s/npm/%2e%2e/x", alice, "broker-path", 400},
		{"/s/empty/x", alice, "", 503},
		{"/other", alice, "", 404},
	}
	for _, c := range cases {
		if c.status == 0 {
			continue
		}
		f.got = nil
		f.logs.Reset()
		resp, body := f.get(t, c.path, c.auth, nil)
		if resp.StatusCode != c.status {
			t.Errorf("%s: status %d %s", c.path, resp.StatusCode, body)
		}
		if f.got != nil {
			t.Errorf("%s: forwarded", c.path)
		}
		if c.rule != "" && !strings.Contains(f.logs.String(), `"rule":"`+c.rule+`"`) {
			t.Errorf("%s: no %s event: %s", c.path, c.rule, f.logs)
		}
	}
	// a raw dot-dot segment, sent as written
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/s/npm/x", nil)
	req.URL.Opaque = "/s/npm/../../etc"
	req.Header.Set("Authorization", alice)
	f.got = nil
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 || f.got != nil {
		t.Errorf("dot-dot: status %d forwarded %v", resp.StatusCode, f.got != nil)
	}
}

func TestBrokerRotation(t *testing.T) {
	f := newBrokerFixture(t)
	os.WriteFile(filepath.Join(f.dir, "values", "npm"), []byte("rotated-token-2"), 0o600)
	f.get(t, "/s/npm/pkg", "Bearer "+agentKey(testSigning, "alice"), nil)
	if f.got.Header.Get("Authorization") != "Bearer rotated-token-2" {
		t.Errorf("rotated value not used: %v", f.got.Header)
	}
}

func TestTarget(t *testing.T) {
	up := mustURL("https://h.example/base/")
	if u, ok := target(up, "a/b", "q=1"); !ok || u.String() != "https://h.example/base/a/b?q=1" {
		t.Errorf("target %v %v", u, ok)
	}
	for _, bad := range []string{"..", "a/../..", "a/%2e%2e", "a%2fb", `a\b`, "./a"} {
		if _, ok := target(up, bad, ""); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }
