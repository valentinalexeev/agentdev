package main

// Forge access of the deterministic executors (#69). The operator lists pull requests, sets the commit status
// agentdev/trusted-plan and comments with the executor's own forge account (token in a file mounted from the
// platform namespace, never in an agent pod). Gitea and GitHub use the same REST paths for everything used here.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	shaRE  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// pullRequest is the part of a forge pull request the executor uses. Every field is checked before use.
type pullRequest struct {
	Number   int64
	Head     string // head commit
	HeadRepo string // owner/name of the head repository (another one for a fork; empty if it was deleted)
	Merge    string // merge commit (merged PRs)
	Base     string
	MergedAt time.Time
}

type forgeAPI interface {
	OpenPRs(ctx context.Context) ([]pullRequest, error)
	MergedPRs(ctx context.Context) ([]pullRequest, error)
	Files(ctx context.Context, pr int64) ([]string, error)
	SetStatus(ctx context.Context, sha, state, description string) error
	Comment(ctx context.Context, pr int64, body string) error
	SelfID(ctx context.Context) (int64, error)
}

// forgeAPIBase is the REST base: Gitea <url>/api/v1, GitHub https://api.github.com (or GITHUB_API_URL).
func forgeAPIBase(kind, forgeURL, githubAPI string) string {
	if kind == "github" {
		if githubAPI == "" {
			githubAPI = "https://api.github.com"
		}
		return strings.TrimRight(githubAPI, "/")
	}
	return strings.TrimRight(forgeURL, "/") + "/api/v1"
}

type restForge struct {
	base, repo, token string
	hc                *http.Client
}

func newRestForge(base, repo, token string) (*restForge, error) {
	if !repoRE.MatchString(repo) || strings.Contains(repo, "..") {
		return nil, fmt.Errorf("invalid repository %q", repo)
	}
	return &restForge{base: base, repo: repo, token: token, hc: &http.Client{Timeout: 30 * time.Second,
		// never follow a redirect: it could carry the token to another host
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (f *restForge) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+f.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

type rawPR struct {
	Number int64 `json:"number"`
	Head   struct {
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	MergedAt *time.Time `json:"merged_at"`
	MergeSHA string     `json:"merge_commit_sha"`
}

func (f *restForge) pulls(ctx context.Context, state string) ([]pullRequest, error) {
	var raw []rawPR
	q := url.Values{"state": {state}, "per_page": {"30"}, "limit": {"30"}, "sort": {"updated"}, "direction": {"desc"}}
	if err := f.do(ctx, http.MethodGet, "/repos/"+f.repo+"/pulls?"+q.Encode(), nil, &raw); err != nil {
		return nil, err
	}
	var out []pullRequest
	for _, r := range raw {
		p := pullRequest{Number: r.Number, Head: r.Head.SHA, Base: r.Base.Ref}
		if r.Head.Repo != nil {
			p.HeadRepo = r.Head.Repo.FullName
		}
		if r.MergedAt != nil {
			p.MergedAt, p.Merge = *r.MergedAt, r.MergeSHA
		}
		out = append(out, p)
	}
	return out, nil
}

func (f *restForge) OpenPRs(ctx context.Context) ([]pullRequest, error) { return f.pulls(ctx, "open") }

func (f *restForge) MergedPRs(ctx context.Context) ([]pullRequest, error) {
	all, err := f.pulls(ctx, "closed")
	if err != nil {
		return nil, err
	}
	var out []pullRequest
	for _, p := range all {
		if !p.MergedAt.IsZero() {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *restForge) Files(ctx context.Context, pr int64) ([]string, error) {
	if pr < 1 {
		return nil, fmt.Errorf("invalid pull request number %d", pr)
	}
	var raw []struct {
		Filename string `json:"filename"`
	}
	if err := f.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100&limit=100", f.repo, pr), nil, &raw); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.Filename)
	}
	return out, nil
}

func (f *restForge) SetStatus(ctx context.Context, sha, state, description string) error {
	if !shaRE.MatchString(sha) {
		return fmt.Errorf("invalid commit %q", sha)
	}
	return f.do(ctx, http.MethodPost, "/repos/"+f.repo+"/statuses/"+sha, map[string]string{
		"state": state, "context": statusContext, "description": clip(description, 140)}, nil)
}

func (f *restForge) Comment(ctx context.Context, pr int64, body string) error {
	if pr < 1 {
		return fmt.Errorf("invalid pull request number %d", pr)
	}
	return f.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", f.repo, pr), map[string]string{"body": body}, nil)
}

func (f *restForge) SelfID(ctx context.Context) (int64, error) {
	var u struct {
		ID int64 `json:"id"`
	}
	if err := f.do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
		return 0, err
	}
	if u.ID < 1 {
		return 0, fmt.Errorf("forge returned no account id")
	}
	return u.ID, nil
}

// statusContext is the commit status branch protection requires (owner decision on #69: the trusted plan is required).
const statusContext = "agentdev/trusted-plan"

// touches reports whether a changed file is below dir (empty dir: everything). A truncated file list (100 or more
// entries) counts as touching, so a large PR is never skipped.
func touches(files []string, dir string) bool {
	if dir == "" || len(files) >= 100 {
		return true
	}
	for _, f := range files {
		if f == dir || strings.HasPrefix(f, dir+"/") {
			return true
		}
	}
	return false
}
