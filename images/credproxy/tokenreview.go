package main

// Workload identity for agents (#352): the agent pod has a projected service account token with audience
// claude-proxy, renewed by the kubelet (10 min expiry). The agent sends it instead of the HMAC key (keys.go). The proxy
// checks it with the Kubernetes TokenReview API and maps system:serviceaccount:<agents ns>:agent-<name> to the agent.
// Only the standard library is used: the in-cluster service account token and CA of the proxy pod authenticate the
// review call (the proxy's service account is bound to system:auth-delegator).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	proxyAudience = "claude-proxy"
	posCacheMax   = 5 * time.Minute
	negCacheTTL   = 10 * time.Second
	cacheMaxSize  = 1024
	saDir         = "/var/run/secrets/kubernetes.io/serviceaccount"
)

// Auth modes (CLAUDE_PROXY_AUTH): key = HMAC agent key only (before #352), token = TokenReview only, both = either.
const (
	authKey   = "key"
	authToken = "token"
	authBoth  = "both"
)

type cacheEntry struct {
	agent   string // "" = refused
	expires time.Time
}

// tokenReviewer checks projected tokens against the API server and caches the results by sha256(token).
type tokenReviewer struct {
	url       string // https://<host>:<port>/apis/authentication.k8s.io/v1/tokenreviews
	namespace string // namespace of the agent service accounts
	client    *http.Client
	bearer    func() (string, error) // the proxy's own service account token (re-read: it is rotated too)
	now       func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]cacheEntry
}

func newTokenReviewer(apiURL, namespace string, client *http.Client, bearer func() (string, error)) *tokenReviewer {
	return &tokenReviewer{url: strings.TrimRight(apiURL, "/") + "/apis/authentication.k8s.io/v1/tokenreviews",
		namespace: namespace, client: client, bearer: bearer, now: time.Now, cache: map[[32]byte]cacheEntry{}}
}

// inClusterReviewer builds a reviewer from the pod's service account files and KUBERNETES_SERVICE_HOST/PORT.
func inClusterReviewer(namespace string) (*tokenReviewer, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("KUBERNETES_SERVICE_HOST/PORT not set")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("no CA certificate in " + saDir + "/ca.crt")
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	bearer := func() (string, error) {
		b, err := os.ReadFile(saDir + "/token")
		return strings.TrimSpace(string(b)), err
	}
	return newTokenReviewer("https://"+net.JoinHostPort(host, port), namespace, client, bearer), nil
}

// looksLikeJWT: three non-empty base64url parts. An HMAC key (<agent>.<hex>) has two parts.
func looksLikeJWT(s string) bool {
	p := strings.Split(s, ".")
	if len(p) != 3 {
		return false
	}
	for _, x := range p {
		if x == "" || strings.Trim(x, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
			return false
		}
	}
	return true
}

// jwtExp reads exp from the (unverified) payload. It only shortens the cache time; the API server did the check.
func jwtExp(tok string) (time.Time, bool) {
	p := strings.Split(tok, ".")
	if len(p) != 3 {
		return time.Time{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(p[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &c) != nil || c.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(c.Exp, 0), true
}

// agentFor maps a reviewed username to the agent name, or "".
func (t *tokenReviewer) agentFor(user string) string {
	name, ok := strings.CutPrefix(user, "system:serviceaccount:"+t.namespace+":agent-")
	if !ok || !agentRe.MatchString(name) {
		return ""
	}
	return name
}

// verify returns the agent of a valid token, or "".
func (t *tokenReviewer) verify(ctx context.Context, tok string) string {
	key := sha256.Sum256([]byte(tok))
	now := t.now()
	t.mu.Lock()
	if e, ok := t.cache[key]; ok && now.Before(e.expires) {
		t.mu.Unlock()
		return e.agent
	}
	t.mu.Unlock()

	agent, err := t.review(ctx, tok)
	if err != nil {
		// API server not reachable: refuse and do not cache, the next request asks again
		emitEvent("", "proxy-tokenreview-error", "medium", clip(err.Error(), 200))
		return ""
	}
	exp := now.Add(negCacheTTL)
	if agent != "" {
		exp = now.Add(posCacheMax)
		if e, ok := jwtExp(tok); ok && e.Before(exp) {
			exp = e
		}
		if !now.Before(exp) {
			return agent // valid now, but expires at once: no cache entry
		}
	}
	t.mu.Lock()
	if len(t.cache) >= cacheMaxSize {
		for k, e := range t.cache { // drop expired entries, then anything, to stay bounded
			if !now.Before(e.expires) {
				delete(t.cache, k)
			}
		}
		for k := range t.cache {
			if len(t.cache) < cacheMaxSize {
				break
			}
			delete(t.cache, k)
		}
	}
	t.cache[key] = cacheEntry{agent, exp}
	t.mu.Unlock()
	return agent
}

func (t *tokenReviewer) review(ctx context.Context, tok string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "authentication.k8s.io/v1", "kind": "TokenReview",
		"spec": map[string]any{"token": tok, "audiences": []string{proxyAudience}},
	})
	bearer, err := t.bearer()
	if err != nil {
		return "", fmt.Errorf("tokenreview: own token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tokenreview: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tokenreview: status %d", resp.StatusCode)
	}
	var r struct {
		Status struct {
			Authenticated bool     `json:"authenticated"`
			Audiences     []string `json:"audiences"`
			User          struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&r); err != nil {
		return "", fmt.Errorf("tokenreview: decode: %w", err)
	}
	if !r.Status.Authenticated {
		return "", nil
	}
	aud := false
	for _, a := range r.Status.Audiences {
		aud = aud || a == proxyAudience
	}
	if !aud {
		return "", nil
	}
	return t.agentFor(r.Status.User.Username), nil
}
