package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Forge interface additions (#236): labels from a fixed allow-list, draft PRs and ready-for-review, edits of the
// agent's own comments, the files of a PR. Every write is checked against facts read from the forge with the
// agent's own credential, never against what the request claims.

// labelAllowed: the labels an agent may set or clear (owner decision #236 q2). agent:<self> only on its own PR
// (#227), never on an issue: that would give the agent work it was not given.
func labelAllowed(s *server, name string) bool {
	return name == "needs-decision" || name == "in-progress" || name == "agent:"+s.c.ForgeUser
}

// ownPR reads PR n of repo and refuses it unless its head is a branch of this agent in this very repository.
func ownPR(s *server, ctx context.Context, repo, n string) (map[string]any, error) {
	var pr map[string]any
	if err := s.forgeGet(ctx, fmt.Sprintf("/repos/%s/pulls/%s", repo, n), &pr); err != nil {
		return nil, refuseAs("api-not-allowed", "medium", "#%s is not a pull request of this agent (%v)", n, err)
	}
	var head struct {
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	}
	b, _ := json.Marshal(pr["head"])
	_ = json.Unmarshal(b, &head)
	// a fork can name its branch agent/<name>/x too: the head must be a branch of this very repository
	if head.Repo == nil || !strings.EqualFold(head.Repo.FullName, repo) || checkBranch(s.c.ForgeUser, head.Ref) != nil {
		return nil, refuseAs("api-not-allowed", "high", "PR #%s is not from a branch of %s in %s", n, s.c.ForgeUser, repo)
	}
	return pr, nil
}

// labelTarget: an issue given to this agent (Gitea: assigned to it; GitHub: labelled agent:<name>), or its own PR.
// isPR reports which one, so the caller can refuse agent:<self> on an issue.
func labelTarget(s *server, ctx context.Context, repo, n string) (isPR bool, err error) {
	var is struct {
		PullRequest any `json:"pull_request"`
		Assignees   []struct {
			Login string `json:"login"`
		} `json:"assignees"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := s.forgeGet(ctx, fmt.Sprintf("/repos/%s/issues/%s", repo, n), &is); err != nil {
		return false, refuseAs("api-not-allowed", "medium", "label: cannot read #%s (%v)", n, err)
	}
	if is.PullRequest != nil {
		_, err := ownPR(s, ctx, repo, n)
		return true, err
	}
	if s.driver().kind() == "github" {
		for _, l := range is.Labels {
			if l.Name == "agent:"+s.c.ForgeUser {
				return false, nil
			}
		}
	} else {
		for _, a := range is.Assignees {
			if strings.EqualFold(a.Login, s.c.ForgeUser) {
				return false, nil
			}
		}
	}
	return false, refuseAs("api-not-allowed", "high", "label: issue #%s is not given to %s", n, s.c.ForgeUser)
}

// checkLabelAdd: POST /issues/{n}/labels with {"labels":[names]} from the allow-list, on a target of this agent.
func checkLabelAdd(s *server, ctx context.Context, m []string, body []byte) error {
	var rq map[string]json.RawMessage
	if err := json.Unmarshal(body, &rq); err != nil {
		return refuse("bad label body")
	}
	var labels []string
	for k, v := range rq {
		if k != "labels" || json.Unmarshal(v, &labels) != nil {
			return refuseAs("api-not-allowed", "medium", "label: only \"labels\" (a list of names) is allowed")
		}
	}
	if len(labels) == 0 || len(labels) > 3 {
		return refuseAs("api-not-allowed", "medium", "label: 1 to 3 labels")
	}
	self := false
	for _, l := range labels {
		if !labelAllowed(s, l) {
			return refuseAs("label-not-allowed", "medium", "label %q is not on the agent allow-list", l)
		}
		self = self || l == "agent:"+s.c.ForgeUser
	}
	if len(m) < 3 {
		return refuse("label: no issue number")
	}
	isPR, err := labelTarget(s, ctx, m[1], m[2])
	if err != nil {
		return err
	}
	if self && !isPR {
		return refuseAs("label-not-allowed", "high", "label agent:%s only on the agent's own PR", s.c.ForgeUser)
	}
	return nil
}

// checkLabelRemove: DELETE /issues/{n}/labels/{name} (GitHub) or /labels/{id} (Gitea, the id is resolved to its
// name first). Only needs-decision and in-progress; the agent's own routing label stays.
func checkLabelRemove(s *server, ctx context.Context, m []string, _ []byte) error {
	if len(m) < 4 {
		return refuse("label: no issue number")
	}
	name := m[3]
	if s.driver().kind() == "github" {
		var err error
		if name, err = url.PathUnescape(m[3]); err != nil {
			return refuse("label: bad name")
		}
	} else {
		var l struct {
			Name string `json:"name"`
		}
		if !idRe.MatchString(m[3]) || s.forgeGet(ctx, fmt.Sprintf("/repos/%s/labels/%s", m[1], m[3]), &l) != nil {
			return refuseAs("api-not-allowed", "medium", "label: unknown label id %q", m[3])
		}
		name = l.Name
	}
	if name != "needs-decision" && name != "in-progress" {
		return refuseAs("label-not-allowed", "medium", "removing label %q is not allowed", name)
	}
	_, err := labelTarget(s, ctx, m[1], m[2])
	return err
}

// checkCommentEdit: PATCH /issues/comments/{id} with only {"body": ...}, only on a comment this agent wrote. On
// Gitea the author is the agent's own user; on GitHub every agent is the same App bot, so the comment must also end
// with this agent's signature line (added by this sidecar, so another agent's comment has another name).
func checkCommentEdit(s *server, ctx context.Context, m []string, body []byte) error {
	var rq map[string]json.RawMessage
	if err := json.Unmarshal(body, &rq); err != nil {
		return refuse("bad comment body")
	}
	var text string
	for k, v := range rq {
		if k != "body" || json.Unmarshal(v, &text) != nil {
			return refuseAs("api-not-allowed", "medium", "comment edit: only \"body\" (a string) is allowed")
		}
	}
	if len(m) < 3 {
		return refuse("comment edit: no id")
	}
	var c struct {
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := s.forgeGet(ctx, fmt.Sprintf("/repos/%s/issues/comments/%s", m[1], m[2]), &c); err != nil {
		return refuseAs("api-not-allowed", "medium", "comment edit: cannot read comment %s (%v)", m[2], err)
	}
	if d, ok := s.driver().(*githubDriver); ok {
		login, _, err := d.identity(ctx)
		if err != nil || c.User.Login != login || !strings.HasSuffix(strings.TrimRight(c.Body, "\n"), strings.TrimRight(d.signature(), "\n")) {
			return refuseAs("comment-not-own", "high", "comment %s is not by agent %s", m[2], s.c.AgentName)
		}
		return nil
	}
	if !strings.EqualFold(c.User.Login, s.c.ForgeUser) {
		return refuseAs("comment-not-own", "high", "comment %s is by %q, not %s", m[2], c.User.Login, s.c.ForgeUser)
	}
	return nil
}

// checkReady: POST /pulls/{n}/ready_for_review (a sidecar path, not a forge one), empty body, own PR only.
func checkReady(s *server, ctx context.Context, m []string, body []byte) error {
	if len(bytes.TrimSpace(body)) != 0 && string(bytes.TrimSpace(body)) != "{}" {
		return refuseAs("api-not-allowed", "medium", "ready for review: no body")
	}
	if len(m) < 3 {
		return refuse("ready for review: no PR number")
	}
	_, err := ownPR(s, ctx, m[1], m[2])
	return err
}

// wipRe: Gitea's default work-in-progress title prefixes (a Gitea PR is a draft while its title has one)
var wipRe = regexp.MustCompile(`(?i)^\s*(wip:|\[wip\])\s*`)

// serveReady marks the PR ready: GitHub GraphQL markPullRequestReadyForReview, Gitea a title without the WIP prefix.
func serveReady(s *server, ctx context.Context, m []string, _ []byte) (int, []byte, error) {
	pr, err := ownPR(s, ctx, m[1], m[2])
	if err != nil {
		return 0, nil, err
	}
	if d, ok := s.driver().(*githubDriver); ok {
		id, _ := pr["node_id"].(string)
		if id == "" || len(id) > 100 {
			return 0, nil, errors.New("PR has no node id")
		}
		q := map[string]any{"query": `mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{isDraft}}}`,
			"variables": map[string]string{"id": id}}
		return s.forgeSend(ctx, "POST", d.graphqlURL(), q)
	}
	title, _ := pr["title"].(string)
	for wipRe.MatchString(title) {
		title = wipRe.ReplaceAllString(title, "")
	}
	if title == "" {
		title = "PR #" + m[2]
	}
	return s.forgeSend(ctx, "PATCH", s.driver().apiURL(fmt.Sprintf("/repos/%s/pulls/%s", m[1], m[2]), ""), map[string]string{"title": title})
}

// graphqlURL: api.github.com/graphql, or <host>/api/graphql on GitHub Enterprise Server
func (d *githubDriver) graphqlURL() string {
	if b, ok := strings.CutSuffix(d.api, "/api/v3"); ok {
		return b + "/api/graphql"
	}
	return d.api + "/graphql"
}

// forgeSend: one write with the agent's credential to an upstream URL built by the driver; returns status and body.
func (s *server) forgeSend(ctx context.Context, method, u string, in any) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	auth, err := s.driver().authorization(ctx)
	if err != nil {
		return 0, nil, errors.New("forge credential unavailable")
	}
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, errors.New("forge unreachable")
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// GraphQL answers 200 with an errors list
	if resp.StatusCode == 200 && bytes.Contains(out, []byte(`"errors"`)) {
		return http.StatusBadGateway, out, nil
	}
	return resp.StatusCode, out, nil
}

// ifaceRules: the #236 rules, the same on both drivers (path numbers as the driver writes them)
func ifaceRules(num string) []rule {
	pr := func(m, p string, chk func(*server, context.Context, []string, []byte) error) rule {
		ru := mkRule(m, p, nil)
		ru.checkPath = chk
		return ru
	}
	ready := pr("POST", repoRe+`/pulls/(`+num+`)/ready_for_review`, checkReady)
	ready.serve = serveReady
	return []rule{
		pr("POST", repoRe+`/issues/(`+num+`)/labels`, checkLabelAdd),
		pr("DELETE", repoRe+`/issues/(`+num+`)/labels/([A-Za-z0-9:_%.-]{1,100})`, checkLabelRemove),
		pr("PATCH", repoRe+`/issues/comments/(`+num+`)`, checkCommentEdit),
		ready,
		mkRule("GET", repoRe+`/pulls/`+num+`/files`, nil),
	}
}
