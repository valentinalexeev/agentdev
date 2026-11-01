package main

// GitHub forge for the API (#87): the live permission check of agent changes and approval decisions on GitHub repos.
//
// Credential, in this order:
//   - the GitHub App (GITHUB_APP_ID and the private key file GITHUB_APP_PRIVATE_KEY_FILE): the API signs an App JWT
//     and mints an installation token per repository owner, with read-only permissions (metadata, issues, pull
//     requests). The key and the tokens stay in this process.
//   - GITHUB_API_TOKEN: a read-only token (for example a fine-grained token with metadata, issues and pull requests
//     read) when there is no App.
//
// Without either, every GitHub check fails closed (503 on writes; agentdev-admins can still pause, add and remove).

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
	"strings"
	"sync"
	"time"
)

var (
	ghIDRe    = regexp.MustCompile(`^[1-9][0-9]{0,15}$`)
	ghTokenRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{20,512}$`)
	ghSlugRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// read-only: enough for collaborator permissions, issue and PR comments and state
	ghTokenPerms = map[string]string{"metadata": "read", "issues": "read", "pull_requests": "read"}
)

type ghToken struct {
	token   string
	expires time.Time
}

// githubForge implements permChecker and commentReader for forge "github".
type githubForge struct {
	api    string // REST base, e.g. https://api.github.com
	appID  string
	key    *rsa.PrivateKey
	static string // GITHUB_API_TOKEN (no App)
	hc     *http.Client
	now    func() time.Time

	mu       sync.Mutex
	installs map[string]string  // owner -> installation id
	tokens   map[string]ghToken // owner -> installation token
	bot      string             // <app-slug>[bot]
	botAt    time.Time
}

// githubAPIBase: https://api.github.com for github.com, <url>/api/v3 for GitHub Enterprise Server; apiURL overrides.
func githubAPIBase(webURL, apiURL string) (string, error) {
	if apiURL != "" {
		a, err := url.Parse(apiURL)
		if err != nil || a.Scheme != "https" || a.Host == "" || a.User != nil || a.RawQuery != "" {
			return "", fmt.Errorf("GITHUB_API_URL %q: a plain https:// URL", apiURL)
		}
		return strings.TrimRight(apiURL, "/"), nil
	}
	u, err := url.Parse(webURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("GITHUB_URL %q: a plain https:// URL", webURL)
	}
	if strings.EqualFold(u.Host, "github.com") {
		return "https://api.github.com", nil
	}
	return strings.TrimRight(webURL, "/") + "/api/v3", nil
}

func parseGitHubKey(pemData []byte) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode(pemData)
	if blk == nil {
		return nil, errors.New("GitHub App private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if rk, ok := k.(*rsa.PrivateKey); err == nil && ok {
		return rk, nil
	}
	return nil, errors.New("GitHub App private key: not an RSA private key")
}

// githubFromEnv builds the GitHub forge from the environment. nil (with a log reason) when nothing is configured:
// the dispatcher then fails closed for GitHub repos.
func githubFromEnv(getenv func(string) string) (*githubForge, error) {
	appID, keyFile, static := getenv("GITHUB_APP_ID"), getenv("GITHUB_APP_PRIVATE_KEY_FILE"), getenv("GITHUB_API_TOKEN")
	if appID == "" && static == "" {
		return nil, nil
	}
	web := getenv("GITHUB_URL")
	if web == "" {
		web = "https://github.com"
	}
	base, err := githubAPIBase(web, getenv("GITHUB_API_URL"))
	if err != nil {
		return nil, err
	}
	g := newGitHubForge(base)
	if appID != "" {
		if !ghIDRe.MatchString(appID) {
			return nil, fmt.Errorf("GITHUB_APP_ID %q is not a number", appID)
		}
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("GITHUB_APP_PRIVATE_KEY_FILE: %v", err)
		}
		if g.key, err = parseGitHubKey(b); err != nil {
			return nil, err
		}
		g.appID = appID
		return g, nil
	}
	if !ghTokenRe.MatchString(static) {
		return nil, errors.New("GITHUB_API_TOKEN is not a token")
	}
	g.static = static
	return g, nil
}

func newGitHubForge(api string) *githubForge {
	return &githubForge{api: api, now: time.Now, installs: map[string]string{}, tokens: map[string]ghToken{},
		hc: &http.Client{Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// appJWT: RS256, issued 60 s in the past against clock drift, valid 9 minutes.
func (g *githubForge) appJWT() (string, error) {
	now := g.now()
	enc := base64.RawURLEncoding.EncodeToString
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	cl, _ := json.Marshal(map[string]any{"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": g.appID})
	signing := enc(hdr) + "." + enc(cl)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, g.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc(sig), nil
}

// call does one request. status 404 is returned as errNotFound so callers can map it to "no permission".
var errNotFound = errors.New("not found")

func (g *githubForge) call(ctx context.Context, method, path, auth string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.api+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: github unreachable", errForgeUnavailable)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("%w: github %s %s: HTTP %d", errForgeUnavailable, method, strings.SplitN(path, "?", 2)[0], resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("%w: %v", errForgeUnavailable, err)
	}
	return nil
}

// auth returns the Authorization header for repo's owner: the static token, or an installation token (cached,
// renewed 5 minutes before it expires).
func (g *githubForge) auth(ctx context.Context, repo string) (string, error) {
	if g == nil || (g.static == "" && g.key == nil) {
		return "", fmt.Errorf("%w: no GitHub credential (GITHUB_APP_ID or GITHUB_API_TOKEN)", errForgeUnavailable)
	}
	if g.static != "" {
		return "Bearer " + g.static, nil
	}
	owner, _, _ := strings.Cut(repo, "/")
	key := strings.ToLower(owner)
	g.mu.Lock()
	t, inst := g.tokens[key], g.installs[key]
	g.mu.Unlock()
	if t.token != "" && g.now().Add(5*time.Minute).Before(t.expires) {
		return "Bearer " + t.token, nil
	}
	jwt, err := g.appJWT()
	if err != nil {
		return "", err
	}
	if inst == "" {
		var in struct {
			ID json.Number `json:"id"`
		}
		if err := g.call(ctx, "GET", "/repos/"+repo+"/installation", "Bearer "+jwt, nil, &in); err != nil {
			if errors.Is(err, errNotFound) {
				return "", fmt.Errorf("%w: the GitHub App is not installed on %s", errForgeUnavailable, repo)
			}
			return "", err
		}
		if !ghIDRe.MatchString(in.ID.String()) {
			return "", fmt.Errorf("%w: installation lookup: bad id", errForgeUnavailable)
		}
		inst = in.ID.String()
	}
	var tok struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := g.call(ctx, "POST", "/app/installations/"+inst+"/access_tokens", "Bearer "+jwt,
		map[string]any{"permissions": ghTokenPerms}, &tok); err != nil {
		if errors.Is(err, errNotFound) {
			err = fmt.Errorf("%w: installation %s not found", errForgeUnavailable, inst)
			g.mu.Lock()
			delete(g.installs, key)
			g.mu.Unlock()
		}
		return "", err
	}
	exp, err := time.Parse(time.RFC3339, tok.ExpiresAt)
	if !ghTokenRe.MatchString(tok.Token) || err != nil || !exp.After(g.now()) {
		return "", fmt.Errorf("%w: installation token: bad answer", errForgeUnavailable)
	}
	g.mu.Lock()
	g.installs[key], g.tokens[key] = inst, ghToken{tok.Token, exp}
	g.mu.Unlock()
	return "Bearer " + tok.Token, nil
}

func (g *githubForge) get(ctx context.Context, repo, path string, out any) error {
	a, err := g.auth(ctx, repo)
	if err != nil {
		return err
	}
	return g.call(ctx, "GET", path, a, nil, out)
}

func splitRepo(repo string) (string, string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("bad repo %q", repo)
	}
	return url.PathEscape(owner), url.PathEscape(name), nil
}

// ghPerm maps GitHub's answer onto the rule table of the API (admin.md "Reference: who can change an agent"):
// admin -> admin; maintain, write -> write; triage, read -> read; anything else -> none.
func ghPerm(permission, role string) perm {
	switch strings.ToLower(role) {
	case "admin":
		return permAdmin
	case "maintain", "write":
		return permWrite
	}
	switch strings.ToLower(permission) {
	case "admin":
		return permAdmin
	case "write":
		return permWrite
	case "read":
		return permRead
	}
	return permNone
}

// Permission: GET /repos/{owner}/{repo}/collaborators/{user}/permission. 404 (no such user, repository not visible to
// the credential) is no permission; any other failure is errForgeUnavailable (fail closed).
func (g *githubForge) Permission(ctx context.Context, _ string, repo, user string) (perm, error) {
	o, n, err := splitRepo(repo)
	if err != nil || user == "" {
		return permNone, nil
	}
	var body struct {
		Permission string `json:"permission"`
		RoleName   string `json:"role_name"`
	}
	err = g.get(ctx, repo, fmt.Sprintf("/repos/%s/%s/collaborators/%s/permission", o, n, url.PathEscape(user)), &body)
	if errors.Is(err, errNotFound) {
		return permNone, nil
	}
	if err != nil {
		return permNone, err
	}
	return ghPerm(body.Permission, body.RoleName), nil
}

func (g *githubForge) Comments(ctx context.Context, repo string, number int, since time.Time) ([]forgeComment, error) {
	o, n, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	var out []forgeComment
	for page := 1; page <= 10; page++ { // at most 1000 comments since the request
		var raw []struct {
			ID   int64 `json:"id"`
			User struct {
				ID    int64  `json:"id"`
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"user"`
			Body    string    `json:"body"`
			Created time.Time `json:"created_at"`
			Updated time.Time `json:"updated_at"`
		}
		p := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?since=%s&per_page=100&page=%d", o, n, number,
			url.QueryEscape(since.UTC().Format(time.RFC3339)), page)
		if err := g.get(ctx, repo, p, &raw); err != nil {
			if errors.Is(err, errNotFound) {
				err = fmt.Errorf("%w: issue %d not found", errForgeUnavailable, number)
			}
			return nil, err
		}
		for _, c := range raw {
			out = append(out, forgeComment{ID: c.ID, UserID: c.User.ID, Login: c.User.Login, Bot: !strings.EqualFold(c.User.Type, "User"),
				Body: c.Body, Created: c.Created, Updated: c.Updated})
		}
		if len(raw) < 100 {
			break
		}
	}
	return out, nil
}

func (g *githubForge) IssueState(ctx context.Context, repo string, number int) (string, error) {
	o, n, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	var is struct {
		State string `json:"state"`
	}
	if err := g.get(ctx, repo, fmt.Sprintf("/repos/%s/%s/issues/%d", o, n, number), &is); err != nil {
		return "", err
	}
	return is.State, nil
}

// AppBot is the login of the App's bot account (<slug>[bot]), cached for an hour. Every agent on GitHub writes as
// this account, so it never decides (deny list); its comments also carry user.type "Bot", which the decider refuses.
// With a static token there is no App bot ("").
func (g *githubForge) AppBot(ctx context.Context) (string, error) {
	if g == nil || g.key == nil {
		return "", nil
	}
	g.mu.Lock()
	if g.bot != "" && g.now().Sub(g.botAt) < time.Hour {
		defer g.mu.Unlock()
		return g.bot, nil
	}
	g.mu.Unlock()
	jwt, err := g.appJWT()
	if err != nil {
		return "", err
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := g.call(ctx, "GET", "/app", "Bearer "+jwt, nil, &app); err != nil {
		return "", err
	}
	if !ghSlugRe.MatchString(app.Slug) {
		return "", fmt.Errorf("%w: app: bad slug", errForgeUnavailable)
	}
	g.mu.Lock()
	g.bot, g.botAt = app.Slug+"[bot]", g.now()
	g.mu.Unlock()
	return app.Slug + "[bot]", nil
}

// Self is the user behind a static GITHUB_API_TOKEN (it never decides). With the App there is no user: 0.
func (g *githubForge) Self(ctx context.Context) (int64, error) {
	if g == nil || g.static == "" {
		return 0, nil
	}
	var u struct {
		ID int64 `json:"id"`
	}
	if err := g.call(ctx, "GET", "/user", "Bearer "+g.static, nil, &u); err != nil {
		return 0, err
	}
	return u.ID, nil
}

// UserID is part of commentReader for Gitea agent accounts. On GitHub agents have no user account of their own (they
// write as the App bot, refused by login and by type Bot), so it is never asked.
func (g *githubForge) UserID(context.Context, string) (int64, error) {
	return 0, fmt.Errorf("%w: agents on GitHub write as the App bot", errForgeUnavailable)
}

// forgePerms dispatches the permission check by forge kind: "" and "gitea" to Gitea, "github" to GitHub. Any other
// forge, or a forge without a configured client, fails closed.
type forgePerms struct {
	gitea  permChecker
	github permChecker
}

func (f forgePerms) Permission(ctx context.Context, forge, repo, user string) (perm, error) {
	switch forge {
	case "", "gitea":
		if f.gitea != nil {
			return f.gitea.Permission(ctx, forge, repo, user)
		}
	case "github":
		if f.github != nil {
			return f.github.Permission(ctx, forge, repo, user)
		}
		return permNone, fmt.Errorf("%w: no GitHub credential configured (GITHUB_APP_ID or GITHUB_API_TOKEN)", errForgeUnavailable)
	}
	return permNone, fmt.Errorf("%w: permission check for forge %q is not supported", errForgeUnavailable, forge)
}
