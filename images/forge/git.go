package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// gitRepo is the sidecar's own bare mirror of the forge repo, on a volume only the sidecar mounts. The agent
// container never shares a git directory with it: sync hands the agent a bundle, and a push arrives as a bundle
// (plain data) that is fetched into this mirror. So nothing Claude writes (worktrees, .git/config, hooks) is ever
// read by a git process that holds the token.
type gitRepo struct {
	c  *Config
	mu sync.Mutex // git refuses concurrent ref updates
}

// policyError is a refusal by the push policy (HTTP 403), as opposed to a git or forge failure (HTTP 502).
type policyError struct{ msg string }

func (e *policyError) Error() string { return e.msg }

func refuse(format string, a ...any) error { return &policyError{fmt.Sprintf(format, a...)} }

// forgeOrigin is scheme://host[:port] of the forge: the credential helper answers only for exactly this.
func forgeOrigin(forgeURL string) (scheme, host string, err error) {
	u, err := url.Parse(forgeURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", fmt.Errorf("bad FORGE_URL %q", forgeURL)
	}
	return u.Scheme, u.Host, nil
}

func (g *gitRepo) remoteURL() string { return g.c.ForgeURL + "/" + g.c.Repos[0] + ".git" }

// run runs git with every repo- or user-controlled program turned off: no hooks, no fsmonitor, no global or system
// config, no prompts, and a credential helper (this binary) that answers only the forge's own origin.
func (g *gitRepo) run(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	base := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "credential.helper=", "-c", "credential.helper=" + g.c.Self + " credential",
		"-c", "credential.useHttpPath=false", "-c", "protocol.ext.allow=never", "-c", "http.followRedirects=false"}
	cmd := exec.CommandContext(ctx, g.c.Git, append(base, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=", "SSH_ASKPASS=", "HOME="+filepath.Dir(g.c.MirrorDir))
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(redact(errb.String(), g.c.Token)))
	}
	return strings.TrimSpace(out.String()), nil
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}

func (g *gitRepo) git(ctx context.Context, args ...string) (string, error) {
	return g.run(ctx, nil, append([]string{"--git-dir", g.c.MirrorDir}, args...)...)
}

// init creates the mirror if needed (the volume is the sidecar's own; an emptyDir is re-cloned after a restart).
func (g *gitRepo) init() error {
	if _, err := os.Stat(filepath.Join(g.c.MirrorDir, "HEAD")); err != nil {
		if err := os.MkdirAll(filepath.Dir(g.c.MirrorDir), 0o755); err != nil {
			return err
		}
		if _, err := g.run(context.Background(), nil, "init", "-q", "--bare", g.c.MirrorDir); err != nil {
			return err
		}
	}
	return os.MkdirAll(g.c.IncomingDir, 0o700)
}

// Sync fetches every branch of the forge into the mirror (refs/heads/*, pruned) and writes a bundle of all branches
// to w, which the agent container fetches from. Serialized with pushes.
func (g *gitRepo) Sync(ctx context.Context, w io.Writer) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.git(ctx, "fetch", "-q", "--prune", "--no-write-fetch-head", g.remoteURL(), "+refs/heads/*:refs/heads/*"); err != nil {
		return err
	}
	if heads, _ := g.git(ctx, "for-each-ref", "--count=1", "refs/heads/"); heads == "" {
		return errors.New("the forge repo has no branches")
	}
	cmd := exec.CommandContext(ctx, g.c.Git, "-c", "core.hooksPath=/dev/null", "--git-dir", g.c.MirrorDir, "bundle", "create", "-q", "-", "--branches")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git bundle: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	return nil
}

var refPart = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// checkBranch: the agent pushes only to agent/<FORGE_USER>/<name>[/<name>...], plain names, no tricks.
func checkBranch(user, branch string) error {
	prefix := "agent/" + user + "/"
	if !strings.HasPrefix(branch, prefix) || len(branch) == len(prefix) || len(branch) > 200 {
		return refuse("push refused: branch %q is not under %s", branch, prefix)
	}
	for _, p := range strings.Split(branch, "/") {
		if !refPart.MatchString(p) || strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".lock") || strings.Contains(p, "..") {
			return refuse("push refused: branch %q is not a plain branch name", branch)
		}
	}
	return nil
}

// protectedHit returns the first changed path under one of the protected prefixes ("dir/" or an exact file).
func protectedHit(paths, protected []string) string {
	for _, p := range paths {
		p = strings.TrimPrefix(p, "/")
		for _, pr := range protected {
			pr = strings.TrimPrefix(pr, "/")
			if p == strings.TrimSuffix(pr, "/") || (strings.HasSuffix(pr, "/") && strings.HasPrefix(p, pr)) {
				return p
			}
		}
	}
	return ""
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Push takes a git bundle whose HEAD is the new tip of branch, checks it against the policy and pushes it to the
// forge without force. base is the default branch, used to compute what the branch changes in total.
func (g *gitRepo) Push(ctx context.Context, branch, base string, bundle io.Reader) (string, error) {
	if err := checkBranch(g.c.ForgeUser, branch); err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	f, err := os.CreateTemp(g.c.IncomingDir, "push-*.bundle")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	n, err := io.Copy(f, io.LimitReader(bundle, g.c.MaxPushBytes+1))
	f.Close()
	if err != nil {
		return "", err
	}
	if n > g.c.MaxPushBytes {
		return "", refuse("push refused: larger than %d MB", g.c.MaxPushBytes>>20)
	}
	if n == 0 {
		return "", refuse("push refused: empty bundle")
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmpRef := "refs/incoming/" + hex.EncodeToString(rnd[:])
	if _, err := g.git(ctx, "bundle", "verify", "-q", f.Name()); err != nil {
		return "", refuse("push refused: not a valid bundle against the synced repo (sync first): %v", err)
	}
	if _, err := g.git(ctx, "fetch", "-q", "--no-write-fetch-head", f.Name(), "HEAD:"+tmpRef); err != nil {
		return "", refuse("push refused: bundle has no HEAD: %v", err)
	}
	defer func() { _, _ = g.git(context.Background(), "update-ref", "-d", tmpRef) }()
	tip, err := g.git(ctx, "rev-parse", "--verify", tmpRef+"^{commit}")
	if err != nil {
		return "", refuse("push refused: HEAD is not a commit")
	}

	// every path the push changes: each new non-merge commit (catches add-then-remove), and the branch's net change
	// against the default branch (catches what a merge commit brings in)
	changed, err := g.git(ctx, "log", "--no-merges", "--no-renames", "--name-only", "--format=", tip, "--not", "--branches")
	if err != nil {
		return "", err
	}
	if base != "" {
		if mb, err := g.git(ctx, "merge-base", tip, "refs/heads/"+base); err == nil {
			net, err := g.git(ctx, "diff", "--no-renames", "--name-only", mb, tip)
			if err != nil {
				return "", err
			}
			changed += "\n" + net
		}
	}
	if hit := protectedHit(lines(changed), g.c.Protected); hit != "" {
		return "", refuse("push refused: %s is a protected path (%s); a person has to make this change",
			hit, strings.Join(g.c.Protected, ", "))
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	// no "+": the forge refuses a non-fast-forward, so an agent can never rewrite its pushed history
	if _, err := g.git(ctx, "push", "-q", "--no-verify", g.remoteURL(), tip+":refs/heads/"+branch); err != nil {
		return "", err
	}
	if _, err := g.git(ctx, "update-ref", "refs/heads/"+branch, tip); err != nil {
		return "", err
	}
	return tip, nil
}
