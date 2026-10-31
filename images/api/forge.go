package main

import (
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

// Repository permission levels, lowest first. The forge's answer is mapped onto these.
type perm int

const (
	permNone perm = iota
	permRead
	permWrite
	permAdmin
)

func (p perm) String() string {
	return [...]string{"none", "read", "write", "admin"}[p]
}

// errForgeUnavailable: the forge could not answer. Callers fail closed (no change without a live permission check).
var errForgeUnavailable = errors.New("forge unavailable")

// permChecker gives the permission of a forge user on a repository (owner/name), checked live on the forge.
type permChecker interface {
	Permission(ctx context.Context, forge, repo, user string) (perm, error)
}

// forgeUserOf maps the OIDC identity to the forge user name.
//
// ASSUMPTION (#57): the authentik username is the same as the Gitea username. This is true while people are created
// with the same name in both. When the forge accounts come from authentik (OIDC login on Gitea) or the names differ,
// replace this function: for example read a claim with the forge login, or look the user up by e-mail. An empty
// result means "no forge user", and every forge-based permission is then denied.
//
// Gitea logins are case-insensitive, authentik usernames are not: "Adam" in authentik would act as "adam" on Gitea.
// So only a username that is already lowercase and matches forgeLoginRe maps to a forge user; any other name
// (upper case, spaces, other characters) gives "" and is denied. This relies on admin settings (docs/admin.md,
// docs/security.md R42): closed enrollment and no username changes by users in authentik. Stable claim: #88.
var forgeUserOf = func(id *identity) string {
	u := id.Username
	if u != strings.ToLower(u) || len(u) > 40 || !forgeLoginRe.MatchString(u) {
		return ""
	}
	return u
}

var forgeLoginRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// giteaPerms asks Gitea: GET /api/v1/repos/{owner}/{repo}/collaborators/{user}/permission. Gitea answers this for
// another user only if the token user is a site admin or an admin of that repository (else 403, and we fail closed).
// The token scope stays read-only (read:repository, read:user); it comes from a Secret, never from git.
type giteaPerms struct {
	base  string // e.g. http://gitea.forge.svc.cluster.local:3000
	token string
	hc    *http.Client
}

func newGiteaPerms(base, token string) *giteaPerms {
	return &giteaPerms{base: strings.TrimRight(base, "/"), token: token, hc: &http.Client{Timeout: 5 * time.Second}}
}

func (g *giteaPerms) Permission(ctx context.Context, forge, repo, user string) (perm, error) {
	if forge != "" && forge != "gitea" {
		// GitHub permission checks are not implemented yet: fail closed.
		return permNone, fmt.Errorf("%w: permission check for forge %q is not supported", errForgeUnavailable, forge)
	}
	if g == nil || g.base == "" || g.token == "" {
		return permNone, fmt.Errorf("%w: FORGE_URL or FORGE_API_TOKEN not set", errForgeUnavailable)
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || user == "" {
		return permNone, nil
	}
	u := fmt.Sprintf("%s/api/v1/repos/%s/%s/collaborators/%s/permission", g.base,
		url.PathEscape(owner), url.PathEscape(name), url.PathEscape(user))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return permNone, err
	}
	req.Header.Set("Authorization", "token "+g.token)
	req.Header.Set("Accept", "application/json")
	resp, err := g.hc.Do(req)
	if err != nil {
		return permNone, fmt.Errorf("%w: %v", errForgeUnavailable, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return permNone, nil // no such user or repository (or the token cannot see it): no permission
	case resp.StatusCode != http.StatusOK:
		return permNone, fmt.Errorf("%w: HTTP %d", errForgeUnavailable, resp.StatusCode)
	}
	var body struct {
		Permission string `json:"permission"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return permNone, fmt.Errorf("%w: %v", errForgeUnavailable, err)
	}
	switch body.Permission {
	case "owner", "admin":
		return permAdmin, nil
	case "write":
		return permWrite, nil
	case "read":
		return permRead, nil
	default:
		return permNone, nil
	}
}
