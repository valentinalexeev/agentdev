package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// githubDriver: GitHub (github.com, or GHES; FORGE_API_URL overrides the API base) through one GitHub App (#2). All agents act as the
// App's installation; the App's private key and the installation tokens minted from it exist only in this process.
//
//   - App JWT (RS256, 9 min) -> POST /app/installations/{id}/access_tokens, narrowed to the agent's repos and to the
//     permissions the loop needs. The token is cached in memory and renewed 10 min before it expires.
//   - REST: the agent's /api/v1/<path> goes to <api>/<path> with "Bearer <installation token>".
//   - git: x-access-token:<installation token>, only for FORGE_URL's origin (credential.go).
//   - Identity: everything the agent writes (comments, PR bodies, review replies) gets a fixed signature line with
//     the agent's name, added here, so the shared App bot still shows which agent wrote it.
type githubDriver struct {
	c          *Config
	api        string // REST base without a trailing slash
	appID      string
	installID  string // optional; looked up from the first repo when empty
	key        *rsa.PrivateKey
	client     *http.Client
	now        func() time.Time
	mu         sync.Mutex
	token      string
	expires    time.Time
	botLogin   string // <app-slug>[bot]
	botID      int64
	identityAt time.Time
	retryAt    time.Time // GitHub asked to wait (rate limit) before the next App call
}

// apiError is a non-2xx answer of an App call; retry is GitHub's Retry-After or rate-limit reset, if any.
type apiError struct {
	status int
	retry  time.Duration
	what   string
}

func (e *apiError) Error() string { return fmt.Sprintf("github %s: HTTP %d", e.what, e.status) }

// retryAfter reads Retry-After (seconds) or, with X-RateLimit-Remaining 0, X-RateLimit-Reset (epoch); capped at 1 h.
func retryAfter(h http.Header, now time.Time) time.Duration {
	var d time.Duration
	if v, err := strconv.Atoi(h.Get("Retry-After")); err == nil && v > 0 {
		d = time.Duration(v) * time.Second
	} else if h.Get("X-RateLimit-Remaining") == "0" {
		if v, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && v > now.Unix() {
			d = time.Unix(v, 0).Sub(now)
		}
	}
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// observe is called with every proxied answer: a 401 drops the cached token (revoked or rotated), so the next call
// mints a new one instead of reusing a dead token until its expiry.
func (d *githubDriver) observe(status int) {
	if status == http.StatusUnauthorized {
		d.mu.Lock()
		d.token, d.expires = "", time.Time{}
		d.mu.Unlock()
	}
}

// checkQuery: list calls page at most 100 items (GitHub's maximum) and at most 100 pages deep; page and per_page
// must be plain numbers. Other query keys are passed as they are (filters such as state, labels, since).
func (d *githubDriver) checkQuery(q url.Values) error {
	for _, k := range []string{"per_page", "page"} {
		for _, v := range q[k] {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 || strconv.Itoa(n) != v {
				return refuse("%s=%q: a number from 1 to 100", k, v)
			}
		}
	}
	return nil
}

var (
	idRe       = regexp.MustCompile(`^[1-9][0-9]{0,15}$`)
	tokenRe    = regexp.MustCompile(`^[A-Za-z0-9_.-]{20,512}$`)
	slugRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	ghLoginRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	agentRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	tokenPerms = map[string]string{"contents": "write", "issues": "write", "pull_requests": "write",
		"metadata": "read", "checks": "read", "statuses": "read"}
)

func githubAPIBase(forgeURL string) (string, error) {
	u, err := url.Parse(forgeURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return "", fmt.Errorf("FORGE_URL %q: a plain https:// URL", forgeURL)
	}
	// FORGE_API_URL, not GITHUB_API_URL: GitHub Actions and other tools set that name for their own use
	// the API gets the App JWT and the installation tokens: only https, and only the forge's own API host
	// (api.github.com for github.com, the forge host itself for GHES), so a wrong setting cannot send them elsewhere
	if v := strings.TrimRight(os.Getenv("FORGE_API_URL"), "/"); v != "" {
		a, err := url.Parse(v)
		want := u.Host
		if strings.EqualFold(u.Host, "github.com") {
			want = "api.github.com"
		}
		if err != nil || a.Scheme != "https" || !strings.EqualFold(a.Host, want) || a.User != nil || a.RawQuery != "" || a.Fragment != "" {
			return "", fmt.Errorf("FORGE_API_URL %q: must be https://%s[/path]", v, want)
		}
		return v, nil
	}
	if strings.EqualFold(u.Host, "github.com") {
		return "https://api.github.com", nil
	}
	return forgeURL + "/api/v3", nil // GitHub Enterprise Server
}

func parseRSAKey(pemData string) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode([]byte(pemData))
	if blk == nil {
		return nil, errors.New("GITHUB_APP_PRIVATE_KEY is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, errors.New("GITHUB_APP_PRIVATE_KEY: not an RSA private key")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("GITHUB_APP_PRIVATE_KEY: not an RSA private key")
	}
	return rk, nil
}

func newGitHubDriver(c *Config) (*githubDriver, error) {
	d := &githubDriver{c: c, appID: c.GitHubAppID, installID: c.GitHubInstallationID, now: time.Now,
		client: &http.Client{Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if !idRe.MatchString(d.appID) {
		return nil, fmt.Errorf("GITHUB_APP_ID %q is not a number", d.appID)
	}
	if d.installID != "" && !idRe.MatchString(d.installID) {
		return nil, fmt.Errorf("GITHUB_INSTALLATION_ID %q is not a number", d.installID)
	}
	if !agentRe.MatchString(c.ForgeUser) {
		return nil, fmt.Errorf("FORGE_USER %q: on GitHub this is the agent name (lowercase letters, digits, -)", c.ForgeUser)
	}
	if !agentRe.MatchString(c.AgentName) {
		return nil, fmt.Errorf("AGENT_NAME %q: lowercase letters, digits, -", c.AgentName)
	}
	owner := ""
	for _, r := range c.Repos {
		o, n, ok := strings.Cut(r, "/")
		if !ok || !ghLoginRe.MatchString(o) || !regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`).MatchString(n) {
			return nil, fmt.Errorf("REPO %q is not owner/name", r)
		}
		if owner != "" && !strings.EqualFold(owner, o) {
			return nil, errors.New("REPO: on GitHub all repos of one agent must have the same owner (one installation)")
		}
		owner = o
	}
	var err error
	if d.key, err = parseRSAKey(c.GitHubAppKey); err != nil {
		return nil, err
	}
	if d.api, err = githubAPIBase(c.ForgeURL); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *githubDriver) kind() string { return "github" }

func (d *githubDriver) apiURL(path, rawQuery string) string {
	u := d.api + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u
}

func (d *githubDriver) remoteURL(repo string) string { return d.c.ForgeURL + "/" + repo + ".git" }

func (d *githubDriver) authorization(ctx context.Context) (string, error) {
	t, err := d.installationToken(ctx)
	if err != nil {
		return "", err
	}
	return "Bearer " + t, nil
}

func (d *githubDriver) gitCredential(ctx context.Context) (string, string, error) {
	t, err := d.installationToken(ctx)
	return "x-access-token", t, err
}

// appJWT is the App's own credential (RS256, issued 60 s in the past against clock drift, valid 9 minutes).
func (d *githubDriver) appJWT() (string, error) {
	now := d.now()
	enc := base64.RawURLEncoding.EncodeToString
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	cl, _ := json.Marshal(map[string]any{"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": d.appID})
	signing := enc(hdr) + "." + enc(cl)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, d.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc(sig), nil
}

// appCall is one request with the App JWT; out gets the JSON answer on a 2xx.
func (d *githubDriver) appCall(ctx context.Context, method, path string, in, out any) error {
	jwt, err := d.appJWT()
	if err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.api+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return errors.New("github unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return &apiError{status: resp.StatusCode, retry: retryAfter(resp.Header, d.now()), what: method + " " + path}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// installationToken returns a cached token, or mints one narrowed to the agent's repos and the loop's permissions.
func (d *githubDriver) installationToken(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.token != "" && d.now().Add(10*time.Minute).Before(d.expires) {
		return d.token, nil
	}
	// rate limited: do not hammer the App endpoints; an unexpired token is still used meanwhile
	if d.now().Before(d.retryAt) {
		if d.token != "" && d.now().Before(d.expires) {
			return d.token, nil
		}
		return "", fmt.Errorf("installation token: GitHub asked to wait until %s", d.retryAt.UTC().Format(time.RFC3339))
	}
	backoff := func(err error) {
		var ae *apiError
		if errors.As(err, &ae) {
			wait := ae.retry
			if wait == 0 && (ae.status == 403 || ae.status == 429 || ae.status >= 500) {
				wait = time.Minute
			}
			d.retryAt = d.now().Add(wait)
		}
	}
	if d.installID == "" {
		var inst struct {
			ID json.Number `json:"id"`
		}
		if err := d.appCall(ctx, "GET", "/repos/"+d.c.Repos[0]+"/installation", nil, &inst); err != nil {
			backoff(err)
			return "", fmt.Errorf("installation lookup: %v (is the App installed on %s?)", err, d.c.Repos[0])
		}
		if !idRe.MatchString(inst.ID.String()) {
			return "", errors.New("installation lookup: bad id")
		}
		d.installID = inst.ID.String()
	}
	names := make([]string, 0, len(d.c.Repos))
	for _, r := range d.c.Repos {
		_, n, _ := strings.Cut(r, "/")
		names = append(names, n)
	}
	var tok struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := d.appCall(ctx, "POST", "/app/installations/"+d.installID+"/access_tokens",
		map[string]any{"repositories": names, "permissions": tokenPerms}, &tok); err != nil {
		backoff(err)
		return "", fmt.Errorf("installation token: %v", err)
	}
	exp, err := time.Parse(time.RFC3339, tok.ExpiresAt)
	if !tokenRe.MatchString(tok.Token) || err != nil || !exp.After(d.now()) {
		return "", errors.New("installation token: bad answer")
	}
	d.token, d.expires = tok.Token, exp
	return d.token, nil
}

// identity is the App's bot user (<slug>[bot] and its id), for the commit author; cached for an hour.
func (d *githubDriver) identity(ctx context.Context) (string, int64, error) {
	d.mu.Lock()
	if d.botLogin != "" && d.now().Sub(d.identityAt) < time.Hour {
		defer d.mu.Unlock()
		return d.botLogin, d.botID, nil
	}
	d.mu.Unlock()
	var app struct {
		Slug string `json:"slug"`
	}
	if err := d.appCall(ctx, "GET", "/app", nil, &app); err != nil {
		return "", 0, err
	}
	if !slugRe.MatchString(app.Slug) {
		return "", 0, errors.New("app: bad slug")
	}
	login := app.Slug + "[bot]"
	tok, err := d.installationToken(ctx)
	if err != nil {
		return "", 0, err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", d.api+"/users/"+url.PathEscape(login), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := d.client.Do(req)
	if err != nil {
		return "", 0, errors.New("github unreachable")
	}
	defer resp.Body.Close()
	var u struct {
		Login string      `json:"login"`
		ID    json.Number `json:"id"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u) != nil || u.Login != login {
		return "", 0, fmt.Errorf("bot user %s: HTTP %d", login, resp.StatusCode)
	}
	id, err := strconv.ParseInt(u.ID.String(), 10, 64)
	if err != nil || id <= 0 || !idRe.MatchString(u.ID.String()) {
		return "", 0, errors.New("bot user: bad id")
	}
	d.mu.Lock()
	d.botLogin, d.botID, d.identityAt = login, id, d.now()
	d.mu.Unlock()
	return login, id, nil
}

// signature is the fixed line under everything an agent writes on GitHub; it is added here, not by the model.
func (d *githubDriver) signature() string {
	return "\n\n---\n<sub>🤖 agent `" + d.c.AgentName + "` (agentdev)</sub>"
}

// signBody appends the agent signature to the "body" of a JSON object (a body is required, and must be a string).
func signBody(s *server, body []byte) ([]byte, error) {
	d, ok := s.driver().(*githubDriver)
	if !ok {
		return body, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, refuse("bad JSON body")
	}
	b, ok := m["body"].(string)
	if !ok {
		return nil, refuse("body must be a string")
	}
	m["body"] = b + d.signature()
	return json.Marshal(m)
}

// checkOwnHeadGitHub: on GitHub a head may also be "owner:branch" (a fork). Only a plain branch of the agent's own.
func checkOwnHeadGitHub(s *server, body []byte) error {
	var pr struct {
		Head     string `json:"head"`
		HeadRepo any    `json:"head_repo"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return refuse("bad PR body")
	}
	if strings.Contains(pr.Head, ":") || pr.HeadRepo != nil {
		return refuseAs("push-foreign-branch", "high", "PR head %q is not a branch of this repo", pr.Head)
	}
	return checkBranch(s.c.ForgeUser, pr.Head)
}

// the calls the loop makes on GitHub (issues by label, PRs, comments, reviews, review threads, checks)
func (d *githubDriver) rules() []rule {
	r := func(m, p string, chk func(*server, []byte) error) rule {
		return mkRule(m, strings.ReplaceAll(p, "[0-9]+", "[1-9][0-9]{0,15}"), chk)
	}
	sign := func(ru rule) rule { ru.rewrite = signBody; return ru }
	return []rule{
		r("GET", repoRe, nil),
		r("GET", repoRe+`/issues`, nil),
		r("GET", repoRe+`/issues/[0-9]+`, nil),
		r("GET", repoRe+`/issues/[0-9]+/comments`, nil),
		r("GET", repoRe+`/issues/[0-9]+/events`, nil), // who added the agent:<name> label (#130 review)
		r("GET", repoRe+`/collaborators/[A-Za-z0-9][A-Za-z0-9-]{0,38}/permission`, nil),
		sign(r("POST", repoRe+`/issues/[0-9]+/comments`, nil)),
		r("GET", repoRe+`/pulls`, nil),
		sign(r("POST", repoRe+`/pulls`, checkOwnHeadGitHub)),
		r("GET", repoRe+`/pulls/[0-9]+`, nil),
		r("GET", repoRe+`/pulls/[0-9]+/reviews`, nil),
		r("GET", repoRe+`/pulls/[0-9]+/reviews/[0-9]+/comments`, nil),
		r("GET", repoRe+`/pulls/[0-9]+/comments`, nil),
		sign(r("POST", repoRe+`/pulls/[0-9]+/comments/[0-9]+/replies`, nil)),
		r("POST", repoRe+`/pulls/[0-9]+/requested_reviewers`, checkReviewRequest),
		r("GET", repoRe+`/commits/[0-9a-f]{40}/check-runs`, nil),
		r("GET", repoRe+`/commits/[0-9a-f]{40}/status`, nil),
	}
}

// checkReviewRequest: the agent asks people for a review, never teams; at most 5 plain logins.
func checkReviewRequest(_ *server, body []byte) error {
	var rq map[string]json.RawMessage
	if err := json.Unmarshal(body, &rq); err != nil {
		return refuse("bad review request body")
	}
	var who []string
	for k, v := range rq {
		if k != "reviewers" || json.Unmarshal(v, &who) != nil {
			return refuseAs("api-not-allowed", "medium", "review request: only \"reviewers\" (logins) is allowed")
		}
	}
	if len(who) == 0 || len(who) > 5 {
		return refuse("review request: 1 to 5 reviewers")
	}
	for _, l := range who {
		if !ghLoginRe.MatchString(l) {
			return refuse("review request: %q is not a login", l)
		}
	}
	return nil
}
