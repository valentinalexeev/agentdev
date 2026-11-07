package main

import (
	"context"
	"fmt"
	"regexp"
)

// driver is what differs between forges (#2). The sidecar's policy (loopback only, repo allow-list, push rules,
// security events) is the same for every forge; a driver only says where the API is, how one request or one git
// operation is authenticated, and which REST calls the agent may make. Credentials stay inside the driver: nothing
// here is ever returned to the agent container.
type driver interface {
	// kind is the FORGE_KIND value ("gitea", "github")
	kind() string
	// apiURL maps the agent's path (after /api/v1) and query to the upstream URL
	apiURL(path, rawQuery string) string
	// authorization is the Authorization header value for one upstream REST request
	authorization(ctx context.Context) (string, error)
	// gitCredential is the username and password the sidecar's git sends to the forge origin
	gitCredential(ctx context.Context) (user, password string, err error)
	// remoteURL is the git URL of an allowed repo (owner/name)
	remoteURL(repo string) string
	// rules is the allow-list of REST calls
	rules() []rule
}

func newDriver(c *Config) (driver, error) {
	switch c.ForgeKind {
	case "", "gitea":
		return &giteaDriver{c: c}, nil
	case "github":
		return newGitHubDriver(c)
	default:
		return nil, fmt.Errorf("FORGE_KIND %q is not supported", c.ForgeKind)
	}
}

func mkRule(m, p string, chk func(*server, []byte) error) rule {
	return rule{method: m, path: regexp.MustCompile("^" + p + "$"), check: chk}
}

// giteaDriver: a Gitea (or Forgejo) with one personal access token per agent (FORGE_TOKEN). Unchanged behaviour of
// the sidecar before #2.
type giteaDriver struct{ c *Config }

func (d *giteaDriver) kind() string { return "gitea" }

func (d *giteaDriver) apiURL(path, rawQuery string) string {
	u := d.c.ForgeURL + "/api/v1" + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u
}

func (d *giteaDriver) authorization(context.Context) (string, error) {
	return "token " + d.c.Token, nil
}

func (d *giteaDriver) gitCredential(context.Context) (string, string, error) {
	return d.c.ForgeUser, d.c.Token, nil
}

func (d *giteaDriver) remoteURL(repo string) string { return d.c.ForgeURL + "/" + repo + ".git" }

// exactly the calls agent-loop.sh makes (issues, PRs, comments, reviews, review requests)
func (d *giteaDriver) rules() []rule {
	r := mkRule
	return []rule{
		r("GET", `/user`, nil),
		r("GET", repoRe, nil),
		r("GET", repoRe+`/issues`, nil),
		r("GET", repoRe+`/issues/[0-9]+`, nil),
		r("GET", repoRe+`/issues/[0-9]+/comments`, nil),
		r("POST", repoRe+`/issues/[0-9]+/comments`, checkNoDecision), // agents never decide approvals (#67)
		r("GET", repoRe+`/pulls`, nil),
		r("POST", repoRe+`/pulls`, checkOwnHead),
		r("GET", repoRe+`/pulls/[0-9]+`, nil),
		r("GET", repoRe+`/pulls/[0-9]+/reviews`, nil),
		r("GET", repoRe+`/pulls/[0-9]+/reviews/[0-9]+/comments`, nil),
		r("POST", repoRe+`/pulls/[0-9]+/requested_reviewers`, checkReviewRequestGitea), // #237 P6
		// #237 P1: CI results (Gitea Actions and external CI report commit statuses) for feedback runs
		r("GET", repoRe+`/commits/[0-9a-f]{40}/status`, nil),
		r("GET", repoRe+`/commits/[0-9a-f]{40}/statuses`, nil),
		// #237 P3: who assigned the agent (issue timeline) and whether that person may write. Gitea answers 403 to a
		// collaborator asking for another user's permission; the assignee list holds exactly the users with write.
		r("GET", repoRe+`/issues/[0-9]+/timeline`, nil),
		r("GET", repoRe+`/assignees`, nil),
	}
}

// giteaLoginRe: a Gitea user name (letters, digits, '-', '_', '.'; no leading or trailing '.' or '-')
var giteaLoginRe = regexp.MustCompile(`^[A-Za-z0-9_](?:[A-Za-z0-9_.-]{0,38}[A-Za-z0-9_])?$`)

// driverOrGitea is newDriver for code paths that cannot fail (tests building a server by hand); main checks the
// kind with newDriver first, so an unknown kind never gets here at runtime.
func driverOrGitea(c *Config) driver {
	if d, err := newDriver(c); err == nil {
		return d
	}
	return &giteaDriver{c: c}
}

func (s *server) driver() driver {
	if s.drv == nil {
		s.drv = driverOrGitea(s.c)
	}
	return s.drv
}
