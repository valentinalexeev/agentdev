package main

// forge-init (#442): the optional setup of an existing, external Gitea for agentdev. The Job forge-init-<hash>
// (infra/ansible/tasks/forge_init.yml) runs `agentdev-operator forge-init` with an admin token of that Gitea. It uses
// the Gitea REST API only (never kubectl exec, never the gitea CLI) and only checks or sets state, so a second run
// changes nothing:
//   - optional: the agent accounts (create_accounts; random password, agents only use their tokens)
//   - per repository: agents as write collaborators, agentdev-api as admin collaborator, agentdev-router as read
//     collaborator, the label agent:<name>, one webhook to the router, branch protection of the default branch
// It mints no tokens: the agents' tokens stay owner-provided Secret Manager values (secrets-only default). The admin
// token and the webhook secret come from env (secretKeyRef) and are never printed.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"
)

// ForgeInitConfig is the JSON in FORGE_INIT_CONFIG (rendered by the playbook, no secret values).
type ForgeInitConfig struct {
	Agents         []ForgeInitAgent `json:"agents"`
	APIUser        string           `json:"api_user,omitempty"`    // agentdev-api: admin collaborator (permission checks)
	RouterUser     string           `json:"router_user,omitempty"` // agentdev-router: read collaborator (wake poll)
	Reviewer       string           `json:"reviewer"`              // only this account's approval merges
	HookURL        string           `json:"hook_url,omitempty"`    // router webhook; empty: no webhook
	CreateAccounts bool             `json:"create_accounts,omitempty"`
	Protect        bool             `json:"protect_branch"`
}

// ForgeInitAgent is one agent and its repositories ("owner/name", comma separated as in instance.yml).
type ForgeInitAgent struct {
	Name  string `json:"name"`
	User  string `json:"user"`
	Repos string `json:"repos"`
}

type giteaAdmin struct {
	base   string // .../api/v1
	token  string
	secret string // webhook secret
	hc     *http.Client
	out    io.Writer
	change int
}

var hookEvents = []string{"issues", "issue_assign", "issue_comment", "pull_request", "pull_request_assign",
	"pull_request_comment", "pull_request_review_approved", "pull_request_review_rejected", "pull_request_review_comment"}

// runForgeInit is the entry point of `agentdev-operator forge-init`.
func runForgeInit(out io.Writer) int {
	var cfg ForgeInitConfig
	if err := json.Unmarshal([]byte(os.Getenv("FORGE_INIT_CONFIG")), &cfg); err != nil {
		fmt.Fprintln(out, "forge-init: FORGE_INIT_CONFIG:", err)
		return 2
	}
	g := &giteaAdmin{base: strings.TrimRight(os.Getenv("FORGE_URL"), "/") + "/api/v1", token: os.Getenv("GITEA_ADMIN_TOKEN"),
		secret: os.Getenv("WEBHOOK_SECRET"), out: out,
		hc: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if err := g.run(cfg); err != nil {
		fmt.Fprintln(out, "forge-init: failed:", err)
		return 1
	}
	fmt.Fprintf(out, "forge-init: done, %d change(s)\n", g.change)
	return 0
}

func (c ForgeInitConfig) validate(base string, token, secret string) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("FORGE_URL must be an http(s) URL")
	}
	if token == "" {
		return fmt.Errorf("GITEA_ADMIN_TOKEN is empty (Secret forge-init)")
	}
	if c.HookURL != "" && secret == "" {
		return fmt.Errorf("hook_url is set but WEBHOOK_SECRET is empty")
	}
	if c.Protect && c.Reviewer == "" {
		return fmt.Errorf("protect_branch needs reviewer")
	}
	for _, a := range c.Agents {
		if a.Name == "" || a.User == "" || a.Repos == "" {
			return fmt.Errorf("agent %q: name, user and repos are required", a.Name)
		}
		for _, r := range splitRepos(a.Repos) {
			if strings.Count(r, "/") != 1 || strings.Contains(r, "..") {
				return fmt.Errorf("agent %s: repo %q is not owner/name", a.Name, r)
			}
		}
	}
	return nil
}

func splitRepos(s string) []string {
	var out []string
	for _, r := range strings.Split(s, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

func (g *giteaAdmin) run(c ForgeInitConfig) error {
	if err := c.validate(strings.TrimSuffix(g.base, "/api/v1"), g.token, g.secret); err != nil {
		return err
	}
	if c.CreateAccounts {
		for _, a := range c.Agents {
			if err := g.ensureUser(a.User); err != nil {
				return err
			}
		}
	}
	// repo -> agents on it
	repos := map[string][]ForgeInitAgent{}
	for _, a := range c.Agents {
		for _, r := range splitRepos(a.Repos) {
			repos[r] = append(repos[r], a)
		}
	}
	names := make([]string, 0, len(repos))
	for r := range repos {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, r := range names {
		var info struct {
			DefaultBranch string `json:"default_branch"`
		}
		if st, err := g.do("GET", "/repos/"+r, nil, &info); err != nil || st != 200 {
			return fmt.Errorf("repo %s: HTTP %d %v (the repository must exist; forge-init does not create it)", r, st, err)
		}
		for _, a := range repos[r] {
			if err := g.ensureCollaborator(r, a.User, "write"); err != nil {
				return err
			}
			if err := g.ensureLabel(r, "agent:"+a.Name); err != nil {
				return err
			}
		}
		if c.APIUser != "" {
			if err := g.ensureCollaborator(r, c.APIUser, "admin"); err != nil {
				return err
			}
		}
		if c.RouterUser != "" {
			if err := g.ensureCollaborator(r, c.RouterUser, "read"); err != nil {
				return err
			}
		}
		if c.HookURL != "" {
			if err := g.ensureHook(r, c.HookURL); err != nil {
				return err
			}
		}
		if c.Protect {
			if err := g.ensureProtection(r, info.DefaultBranch, c.Reviewer); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *giteaAdmin) do(method, path string, body, into any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, g.base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "token "+g.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if into != nil && resp.StatusCode/100 == 2 {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(into); err != nil {
			return resp.StatusCode, err
		}
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return resp.StatusCode, fmt.Errorf("%s %s: HTTP %d (admin token wrong or not an admin)", method, path, resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func (g *giteaAdmin) changed(format string, a ...any) {
	g.change++
	fmt.Fprintf(g.out, "forge-init: "+format+"\n", a...)
}

func (g *giteaAdmin) ensureUser(user string) error {
	st, err := g.do("GET", "/users/"+url.PathEscape(user), nil, nil)
	if err != nil {
		return err
	}
	if st == 200 {
		return nil
	}
	if st != 404 {
		return fmt.Errorf("user %s: HTTP %d", user, st)
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	st, err = g.do("POST", "/admin/users", map[string]any{"username": user, "email": user + "@agents.local",
		"password": hex.EncodeToString(b) + "Aa1!", "must_change_password": false, "send_notify": false}, nil)
	if err != nil || st != 201 {
		return fmt.Errorf("create user %s: HTTP %d %v", user, st, err)
	}
	g.changed("account %s created (give it a token in Secret Manager)", user)
	return nil
}

func (g *giteaAdmin) ensureCollaborator(repo, user, perm string) error {
	var p struct {
		Permission string `json:"permission"`
	}
	st, err := g.do("GET", "/repos/"+repo+"/collaborators/"+url.PathEscape(user)+"/permission", nil, &p)
	if err != nil {
		return err
	}
	if st == 200 && p.Permission == perm {
		return nil
	}
	st, err = g.do("PUT", "/repos/"+repo+"/collaborators/"+url.PathEscape(user), map[string]string{"permission": perm}, nil)
	if err != nil || st != 204 {
		return fmt.Errorf("collaborator %s on %s: HTTP %d %v", user, repo, st, err)
	}
	g.changed("%s: %s is %s collaborator", repo, user, perm)
	return nil
}

func (g *giteaAdmin) ensureLabel(repo, name string) error {
	var labels []struct {
		Name string `json:"name"`
	}
	if st, err := g.do("GET", "/repos/"+repo+"/labels?limit=100", nil, &labels); err != nil || st != 200 {
		return fmt.Errorf("labels of %s: HTTP %d %v", repo, st, err)
	}
	for _, l := range labels {
		if l.Name == name {
			return nil
		}
	}
	st, err := g.do("POST", "/repos/"+repo+"/labels", map[string]string{"name": name, "color": "#5319e7",
		"description": "Work of this agent (issues and its own PRs)"}, nil)
	if err != nil || st != 201 {
		return fmt.Errorf("label %s on %s: HTTP %d %v", name, repo, st, err)
	}
	g.changed("%s: label %s", repo, name)
	return nil
}

// ensureHook keeps exactly one hook to hookURL. Gitea never returns the hook secret, so an existing hook gets the
// secret again with PATCH on every run (a rotated Secret Manager value reaches Gitea; not counted as a change).
func (g *giteaAdmin) ensureHook(repo, hookURL string) error {
	var hooks []struct {
		ID     int64             `json:"id"`
		Config map[string]string `json:"config"`
	}
	if st, err := g.do("GET", "/repos/"+repo+"/hooks?limit=50", nil, &hooks); err != nil || st != 200 {
		return fmt.Errorf("hooks of %s: HTTP %d %v", repo, st, err)
	}
	cfg := map[string]string{"url": hookURL, "content_type": "json", "secret": g.secret}
	found := false
	for _, h := range hooks {
		if h.Config["url"] != hookURL {
			continue
		}
		if found { // a duplicate would deliver every event twice
			if st, err := g.do("DELETE", fmt.Sprintf("/repos/%s/hooks/%d", repo, h.ID), nil, nil); err != nil || st != 204 {
				return fmt.Errorf("delete duplicate hook on %s: HTTP %d %v", repo, st, err)
			}
			g.changed("%s: duplicate router hook removed", repo)
			continue
		}
		found = true
		st, err := g.do("PATCH", fmt.Sprintf("/repos/%s/hooks/%d", repo, h.ID),
			map[string]any{"active": true, "events": hookEvents, "config": cfg}, nil)
		if err != nil || st != 200 {
			return fmt.Errorf("update hook on %s: HTTP %d %v", repo, st, err)
		}
	}
	if found {
		return nil
	}
	st, err := g.do("POST", "/repos/"+repo+"/hooks", map[string]any{"type": "gitea", "active": true,
		"events": hookEvents, "config": cfg}, nil)
	if err != nil || st != 201 {
		return fmt.Errorf("create hook on %s: HTTP %d %v", repo, st, err)
	}
	g.changed("%s: router hook created", repo)
	return nil
}

// protectionRule is the same rule as roles/gitea_forge (kind): no direct push, one approval by the reviewer, a new
// push dismisses it (#7).
func protectionRule(branch, reviewer string) map[string]any {
	return map[string]any{"rule_name": branch, "enable_push": false, "required_approvals": float64(1),
		"enable_approvals_whitelist": true, "approvals_whitelist_username": []any{reviewer},
		"dismiss_stale_approvals": true, "block_on_rejected_reviews": true, "block_on_outdated_branch": false}
}

func (g *giteaAdmin) ensureProtection(repo, branch, reviewer string) error {
	if branch == "" {
		branch = "main"
	}
	want := protectionRule(branch, reviewer)
	var have map[string]any
	path := "/repos/" + repo + "/branch_protections/" + url.PathEscape(branch)
	st, err := g.do("GET", path, nil, &have)
	if err != nil {
		return err
	}
	if st == 200 {
		same := true
		for k, v := range want {
			if !reflect.DeepEqual(have[k], v) {
				same = false
				break
			}
		}
		if same {
			return nil
		}
		st, err = g.do("PATCH", path, want, nil)
		if err != nil || st != 200 {
			return fmt.Errorf("update branch protection %s on %s: HTTP %d %v", branch, repo, st, err)
		}
	} else {
		st, err = g.do("POST", "/repos/"+repo+"/branch_protections", want, nil)
		if err != nil || st != 201 {
			return fmt.Errorf("branch protection %s on %s: HTTP %d %v", branch, repo, st, err)
		}
	}
	g.changed("%s: branch protection of %s", repo, branch)
	return nil
}
