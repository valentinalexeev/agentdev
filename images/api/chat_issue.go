package main

// Issue creation for chat drafts (#346). Only POST /api/sessions/{id}/drafts/{did}/create calls it, after the live
// write-access check of the person. The session itself has no forge token.
//
//   - Gitea: the token CHAT_ISSUE_TOKEN (Secret agentdev-api-chat) of a site admin account with scope write:issue,
//     used with the Sudo header, so Gitea records the person as the author of the issue.
//   - GitHub: an installation token of the agents' App narrowed to this one repository and issues: write. GitHub
//     shows the App as the author; the body names the person (a user-to-server token flow is #346 follow-up work).
// Without the credential every create gives 503 (fail closed).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type forgeIssues struct {
	giteaURL   string
	giteaToken string
	github     *githubForge
	hc         *http.Client
}

func newForgeIssues(giteaURL, giteaToken string, gh *githubForge) *forgeIssues {
	return &forgeIssues{giteaURL: strings.TrimRight(giteaURL, "/"), giteaToken: giteaToken, github: gh,
		hc: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (f *forgeIssues) CreateIssue(ctx context.Context, forge, repo, asUser, title, body string) (int, string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return 0, "", err
	}
	if validForgeLogin(asUser) == "" {
		return 0, "", fmt.Errorf("no forge login for the person")
	}
	in := map[string]string{"title": title, "body": body}
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	switch forge {
	case "", "gitea":
		if f.giteaURL == "" || f.giteaToken == "" {
			return 0, "", fmt.Errorf("%w: CHAT_ISSUE_TOKEN not set", errForgeUnavailable)
		}
		b, _ := json.Marshal(in)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			fmt.Sprintf("%s/api/v1/repos/%s/%s/issues", f.giteaURL, url.PathEscape(owner), url.PathEscape(name)), bytes.NewReader(b))
		if err != nil {
			return 0, "", err
		}
		req.Header.Set("Authorization", "token "+f.giteaToken)
		req.Header.Set("Sudo", asUser) // the person is the author
		req.Header.Set("Content-Type", "application/json")
		resp, err := f.hc.Do(req)
		if err != nil {
			return 0, "", fmt.Errorf("%w: %v", errForgeUnavailable, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			return 0, "", fmt.Errorf("%w: gitea create issue: HTTP %d", errForgeUnavailable, resp.StatusCode)
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
			return 0, "", err
		}
	case "github":
		g := f.github
		if g == nil || g.key == nil {
			return 0, "", fmt.Errorf("%w: creating issues on GitHub needs the GitHub App", errForgeUnavailable)
		}
		tok, err := g.issueToken(ctx, repo)
		if err != nil {
			return 0, "", err
		}
		if err := g.call(ctx, http.MethodPost, "/repos/"+owner+"/"+name+"/issues", "Bearer "+tok, in, &out); err != nil {
			return 0, "", err
		}
	default:
		return 0, "", fmt.Errorf("%w: forge %q", errForgeUnavailable, forge)
	}
	return out.Number, out.HTMLURL, nil
}

// issueToken mints a short-lived installation token with issues: write on this one repository. It is not cached
// and not shared with the read-only tokens of the permission checks.
func (g *githubForge) issueToken(ctx context.Context, repo string) (string, error) {
	_, name, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	jwt, err := g.appJWT()
	if err != nil {
		return "", err
	}
	var in struct {
		ID json.Number `json:"id"`
	}
	if err := g.call(ctx, http.MethodGet, "/repos/"+repo+"/installation", "Bearer "+jwt, nil, &in); err != nil {
		return "", err
	}
	if !ghIDRe.MatchString(in.ID.String()) {
		return "", fmt.Errorf("%w: installation lookup: bad id", errForgeUnavailable)
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := g.call(ctx, http.MethodPost, "/app/installations/"+in.ID.String()+"/access_tokens", "Bearer "+jwt,
		map[string]any{"repositories": []string{name}, "permissions": map[string]string{"issues": "write", "metadata": "read"}}, &tok); err != nil {
		return "", err
	}
	if !ghTokenRe.MatchString(tok.Token) {
		return "", fmt.Errorf("%w: installation token: bad answer", errForgeUnavailable)
	}
	return tok.Token, nil
}
