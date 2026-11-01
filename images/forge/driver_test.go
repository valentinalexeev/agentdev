package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestGiteaDriverUnchanged(t *testing.T) {
	c := &Config{ForgeURL: "http://gitea:3000", ForgeUser: "alice", Token: "tok", Repos: []string{"o/r"}}
	for _, k := range []string{"", "gitea"} {
		c.ForgeKind = k
		d, err := newDriver(c)
		if err != nil || d.kind() != "gitea" {
			t.Fatalf("kind %q: %v", k, err)
		}
		if got := d.apiURL("/repos/o/r/issues", "state=open"); got != "http://gitea:3000/api/v1/repos/o/r/issues?state=open" {
			t.Errorf("apiURL: %s", got)
		}
		if a, _ := d.authorization(context.Background()); a != "token tok" {
			t.Errorf("auth: %s", a)
		}
		if u, p, _ := d.gitCredential(context.Background()); u != "alice" || p != "tok" {
			t.Errorf("git credential: %s %s", u, p)
		}
		if got := d.remoteURL("o/r"); got != "http://gitea:3000/o/r.git" {
			t.Errorf("remote: %s", got)
		}
		if len(d.rules()) != 12 {
			t.Errorf("rules: %d", len(d.rules()))
		}
	}
	c.ForgeKind = "bitbucket"
	if _, err := newDriver(c); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

// the git child's credential (FORGE_GIT_*) wins over the sidecar's Gitea token; a newline never reaches git
func TestCredentialHelperDriverCredential(t *testing.T) {
	t.Setenv("FORGE_URL", "https://github.com")
	t.Setenv("FORGE_USER", "alice")
	t.Setenv("FORGE_TOKEN", "")
	ask := func() string {
		var out bytes.Buffer
		credentialHelper([]string{"get"}, strings.NewReader("protocol=https\nhost=github.com\n\n"), &out)
		return out.String()
	}
	if ask() != "" {
		t.Fatal("answered without any token")
	}
	t.Setenv("FORGE_GIT_USER", "x-access-token")
	t.Setenv("FORGE_GIT_PASSWORD", "ghs_abc")
	if got := ask(); got != "username=x-access-token\npassword=ghs_abc\n" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("FORGE_GIT_PASSWORD", "ghs_abc\nhost=evil")
	if got := ask(); got != "" {
		t.Fatalf("newline passed: %q", got)
	}
}
