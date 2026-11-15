package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeJWT builds an unsigned three-part token with an exp claim. The fake API server decides what it means.
func fakeJWT(name string, exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	p, _ := json.Marshal(map[string]any{"sub": name, "exp": exp.Unix()})
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(p) + "." + enc([]byte("sig-"+name))
}

type reviewAnswer struct {
	auth bool
	aud  []string
	user string
}

// fakeAPIServer answers TokenReviews from a table keyed by the token's sub claim.
func fakeAPIServer(t *testing.T, answers map[string]reviewAnswer, calls *int32) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		if r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" || r.Header.Get("Authorization") != "Bearer proxy-sa" {
			http.Error(w, "no", 403)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Spec struct {
				Token     string   `json:"token"`
				Audiences []string `json:"audiences"`
			} `json:"spec"`
		}
		_ = json.Unmarshal(b, &req)
		if len(req.Spec.Audiences) != 1 || req.Spec.Audiences[0] != "claude-proxy" {
			t.Errorf("review audiences %v", req.Spec.Audiences)
		}
		p := strings.Split(req.Spec.Token, ".")
		pl, _ := base64.RawURLEncoding.DecodeString(p[1])
		var c struct{ Sub string }
		_ = json.Unmarshal(pl, &c)
		a := answers[c.Sub]
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"status":{"authenticated":%t,"audiences":%s,"user":{"username":%q}}}`, a.auth, mustJSON(a.aud), a.user)
	}))
	t.Cleanup(s.Close)
	return s
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func withReviewer(f *fixture, mode string, api *httptest.Server) *tokenReviewer {
	s := f.srv.Config.Handler.(*server)
	s.authMode = mode
	s.reviewer = newTokenReviewer(api.URL, "coding-agents", api.Client(), func() (string, error) { return "proxy-sa", nil })
	return s.reviewer
}

var reviewTable = map[string]reviewAnswer{
	"alice":    {true, []string{"claude-proxy"}, "system:serviceaccount:coding-agents:agent-alice"},
	"bob":      {true, []string{"claude-proxy"}, "system:serviceaccount:coding-agents:agent-bob"},
	"otheraud": {true, []string{"other"}, "system:serviceaccount:coding-agents:agent-alice"},
	"unauth":   {false, nil, ""},
	"otherns":  {true, []string{"claude-proxy"}, "system:serviceaccount:default:agent-alice"},
	"notagent": {true, []string{"claude-proxy"}, "system:serviceaccount:coding-agents:webhook-router"},
}

func TestTokenAuth(t *testing.T) {
	var calls int32
	api := fakeAPIServer(t, reviewTable, &calls)
	f := newFixture(t, 0)
	withReviewer(f, authBoth, api)
	f.upstream = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
	}
	exp := time.Now().Add(10 * time.Minute)
	for _, c := range []struct {
		name, tok string
		status    int
		agent     string
	}{
		{"alice", fakeJWT("alice", exp), 200, "alice"},
		{"bob token alone is bob", fakeJWT("bob", exp), 200, "bob"},
		{"wrong audience", fakeJWT("otheraud", exp), 401, ""},
		{"unauthenticated", fakeJWT("unauth", exp), 401, ""},
		{"other namespace", fakeJWT("otherns", exp), 401, ""},
		{"not an agent SA", fakeJWT("notagent", exp), 401, ""},
	} {
		f.logs.Reset()
		resp := f.post("/v1/messages", c.tok, `{}`, nil)
		resp.Body.Close()
		if resp.StatusCode != c.status {
			t.Errorf("%s: status %d", c.name, resp.StatusCode)
		}
		// the upstream credential shows the identity: bob has his own API key, alice uses claude-shared
		if c.agent == "bob" && f.got.Header.Get("X-Api-Key") != "sk-ant-api-bob" {
			t.Errorf("%s: not sent as bob", c.name)
		}
		if c.agent == "alice" && f.got.Header.Get("Authorization") != "Bearer sk-ant-oat-shared" {
			t.Errorf("%s: not sent as alice", c.name)
		}
		if c.status == 401 && !strings.Contains(f.logs.String(), "wrong agent token") {
			t.Errorf("%s: no token event: %s", c.name, f.logs.String())
		}
	}
	// the x-api-key header (apiKeyHelper) is accepted as well
	f.logs.Reset()
	resp := f.post("/v1/messages", "", `{}`, map[string]string{"X-Api-Key": fakeJWT("alice", exp)})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("x-api-key token: %d", resp.StatusCode)
	}
	// mode both still takes the HMAC key
	resp = f.post("/v1/messages", agentKey(testSigning, "alice"), `{}`, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("both + key: %d", resp.StatusCode)
	}
}

func TestAuthModes(t *testing.T) {
	var calls int32
	api := fakeAPIServer(t, reviewTable, &calls)
	f := newFixture(t, 0)
	f.upstream = func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) }
	tok := fakeJWT("alice", time.Now().Add(10*time.Minute))

	withReviewer(f, authToken, api)
	f.logs.Reset()
	resp := f.post("/v1/messages", agentKey(testSigning, "alice"), `{}`, nil)
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(f.logs.String(), "wrong agent key (auth mode token)") {
		t.Errorf("token mode + key: %d %s", resp.StatusCode, f.logs.String())
	}
	resp = f.post("/v1/messages", tok, `{}`, nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("token mode + token: %d", resp.StatusCode)
	}

	withReviewer(f, authKey, api)
	before := atomic.LoadInt32(&calls)
	resp = f.post("/v1/messages", tok, `{}`, nil)
	resp.Body.Close()
	if resp.StatusCode != 401 || atomic.LoadInt32(&calls) != before {
		t.Errorf("key mode + token: %d, reviews %d", resp.StatusCode, atomic.LoadInt32(&calls)-before)
	}
}

func TestTokenCache(t *testing.T) {
	var calls int32
	api := fakeAPIServer(t, reviewTable, &calls)
	now := time.Unix(1_800_000_000, 0)
	r := newTokenReviewer(api.URL, "coding-agents", api.Client(), func() (string, error) { return "proxy-sa", nil })
	r.now = func() time.Time { return now }
	ctx := t.Context()

	long := fakeJWT("alice", now.Add(time.Hour))
	if r.verify(ctx, long) != "alice" || r.verify(ctx, long) != "alice" || calls != 1 {
		t.Fatalf("positive cache: calls %d", calls)
	}
	now = now.Add(posCacheMax + time.Second) // capped at 5 min even though exp is later
	r.verify(ctx, long)
	if calls != 2 {
		t.Errorf("5 min cap: calls %d", calls)
	}

	short := fakeJWT("bob", now.Add(30*time.Second)) // exp earlier than 5 min
	r.verify(ctx, short)
	now = now.Add(31 * time.Second)
	r.verify(ctx, short)
	if calls != 4 {
		t.Errorf("exp cap: calls %d", calls)
	}

	bad := fakeJWT("unauth", now.Add(time.Hour))
	r.verify(ctx, bad)
	r.verify(ctx, bad)
	if calls != 5 {
		t.Errorf("negative cache: calls %d", calls)
	}
	now = now.Add(negCacheTTL + time.Second)
	r.verify(ctx, bad)
	if calls != 6 {
		t.Errorf("negative expiry: calls %d", calls)
	}

	for i := 0; i < cacheMaxSize+50; i++ {
		r.verify(ctx, fakeJWT(fmt.Sprintf("x%d", i), now.Add(time.Hour)))
	}
	if len(r.cache) > cacheMaxSize {
		t.Errorf("cache size %d", len(r.cache))
	}
}

func TestTokenReviewDown(t *testing.T) {
	f := newFixture(t, 0)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 500) }))
	defer api.Close()
	withReviewer(f, authBoth, api)
	resp := f.post("/v1/messages", fakeJWT("alice", time.Now().Add(time.Hour)), `{}`, nil)
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(f.logs.String(), "proxy-tokenreview-error") {
		t.Errorf("api down: %d %s", resp.StatusCode, f.logs.String())
	}
}

func TestLooksLikeJWT(t *testing.T) {
	if looksLikeJWT(agentKey(testSigning, "alice")) || looksLikeJWT("a..b") || looksLikeJWT("a.b.c d") || !looksLikeJWT("a.b.c") {
		t.Error("looksLikeJWT")
	}
}
