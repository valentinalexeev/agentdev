package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeProxy behaves like the egress squid for forge hosts (#162): a CONNECT without the expected
// Proxy-Authorization gets 403 at once (never a 407 challenge); with it, the tunnel goes to backend.
func fakeProxy(t *testing.T, backend, wantAuth string, allow bool) (*httptest.Server, *atomic.Int32) {
	var denied atomic.Int32
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || !allow || r.Header.Get("Proxy-Authorization") != wantAuth {
			denied.Add(1)
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		up, err := net.Dial("tcp", backend)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			up.Close()
			return
		}
		go func() { io.Copy(up, buf); up.Close() }()
		io.Copy(conn, up)
		conn.Close()
	}))
	t.Cleanup(p.Close)
	return p, &denied
}

func TestSyncThroughAuthProxy(t *testing.T) {
	execPath := sh(t, t.TempDir(), "--exec-path")
	f := newFixture(t)
	// the forge over HTTPS (git http-backend), only reachable through the proxy
	backend := httptest.NewTLSServer(&cgi.Handler{Path: filepath.Join(execPath, "git-http-backend"),
		Env: []string{"GIT_PROJECT_ROOT=" + filepath.Join(f.tmp, "forge"), "GIT_HTTP_EXPORT_ALL=1"}})
	defer backend.Close()
	const key = "s3cr3t-proxy-key"
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("forge:"+key))
	f.c.ForgeURL = "https://forge.invalid"
	f.g.drv = nil
	t.Setenv("GIT_SSL_NO_VERIFY", "1")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	sh(t, f.agent, "checkout", "-q", "-b", "x")
	sh(t, f.tmp, "--git-dir", f.forge, "branch", "proxied", "main")

	t.Run("credential sent on the first CONNECT", func(t *testing.T) {
		p, denied := fakeProxy(t, backend.Listener.Addr().String(), want, true)
		t.Setenv("HTTPS_PROXY", strings.Replace(p.URL, "://", "://forge:"+key+"@", 1))
		var b bytes.Buffer
		if err := f.g.Sync(context.Background(), &b); err != nil {
			t.Fatalf("sync through the proxy: %v", err)
		}
		if n := denied.Load(); n != 0 {
			t.Fatalf("proxy refused %d CONNECTs without Proxy-Authorization", n)
		}
		if out, _ := f.g.git(context.Background(), "rev-parse", "--verify", "refs/heads/proxied"); out == "" {
			t.Fatal("branch not fetched")
		}
	})
	t.Run("proxy credential is not in errors", func(t *testing.T) {
		p, _ := fakeProxy(t, backend.Listener.Addr().String(), want, false)
		t.Setenv("HTTPS_PROXY", strings.Replace(p.URL, "://", "://forge:"+key+"@", 1))
		t.Setenv("GIT_CURL_VERBOSE", "1") // makes git print the proxy and its headers
		err := f.g.Sync(context.Background(), io.Discard)
		if err == nil {
			t.Fatal("sync succeeded through a refusing proxy")
		}
		if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), strings.TrimPrefix(want, "Basic ")) {
			t.Fatalf("proxy credential leaked: %v", err)
		}
	})
}
