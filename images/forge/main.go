// forge: the agent pod's forge sidecar (#64, security model phase 1). It is the only process in the pod that holds
// FORGE_TOKEN (and the router's WAKE_TOKEN and REGISTER_TOKEN). The agent container talks to it over localhost:
//
//	/api/v1/...   allow-listed Gitea REST calls for the agent's own repo (issues, comments, PRs, reviews)
//	POST /sync    fetch the forge into the sidecar's own bare mirror (only the sidecar mounts it); answers a git bundle
//	POST /push    body = a git bundle; pushed to agent/<FORGE_USER>/* only, protected paths refused, size-limited
//	POST /usage   the loop's usage report after a Claude run, printed as the AGENTDEV_USAGE line (#93)
//	POST /idle    ask the router to stop this agent (scale to zero), with the wake token held here
//	POST /merge   merge the agent's own PR once a person approved its head (MERGE_MODE=gated only, merge.go, #578)
//
// and it receives the router's wake calls on the pod port (webhook listener, formerly webhook-listener.js).
// The localhost API has no auth: the pod is one trust unit, and the policy holds even when Claude calls it directly.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	ForgeURL, ForgeUser, Token string
	// NoUsage (#66): the Claude credential proxy reports usage, so POST /usage is refused (USAGE_REPORTS=off)
	NoUsage                  bool
	Repos                    []string // owner/name, the only repos the API and git talk to
	Listen, WebhookListen    string
	MirrorDir                string // the sidecar's own bare mirror, on a volume only the sidecar mounts
	IncomingDir              string // private temp dir for uploaded bundles
	Protected                []string
	MaxPushBytes             int64
	WakeFile                 string
	RouterURL, SelfURL       string
	WakeToken, RegisterToken string
	APIURL                   string // agentdev-api internal listener (#425 run log); empty: run events are dropped
	AgentName, ForgeKind     string
	// #578: MERGE_MODE off (default) | gated, MERGE_METHOD merge | squash | rebase (merge.go)
	MergeMode, MergeMethod string
	MergeApprovers         []string
	// GitHub App (FORGE_KIND=github, #2): only in this container
	GitHubAppID, GitHubInstallationID, GitHubAppKey string
	RegisterEvery                                   time.Duration
	Git                                             string // git binary
	Self                                            string // this binary, used as git credential helper
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func loadConfig() (*Config, error) {
	c := &Config{
		ForgeURL: strings.TrimRight(os.Getenv("FORGE_URL"), "/"), ForgeUser: os.Getenv("FORGE_USER"),
		Token: os.Getenv("FORGE_TOKEN"), Repos: splitList(os.Getenv("REPO")),
		Listen:        getenv("FORGE_API_LISTEN", "127.0.0.1:7070"),
		WebhookListen: getenv("WEBHOOK_LISTEN", ":"+getenv("WEBHOOK_PORT", "8080")),
		MirrorDir:     getenv("MIRROR_DIR", "/forge/repo.git"),
		Protected:     splitList(getenv("PROTECTED_PATHS", ".github/workflows/,.gitea/workflows/,.forgejo/workflows/")),
		WakeFile:      getenv("WAKE_FILE", "/state/agent-wake"),
		RouterURL:     os.Getenv("ROUTER_URL"), SelfURL: os.Getenv("SELF_URL"),
		WakeToken: os.Getenv("WAKE_TOKEN"), RegisterToken: os.Getenv("REGISTER_TOKEN"),
		AgentName: getenv("AGENT_NAME", os.Getenv("FORGE_USER")), ForgeKind: getenv("FORGE_KIND", "gitea"),
		Git: getenv("GIT", "git"), NoUsage: os.Getenv("USAGE_REPORTS") == "off",
		GitHubAppID: os.Getenv("GITHUB_APP_ID"), GitHubInstallationID: os.Getenv("GITHUB_INSTALLATION_ID"),
		GitHubAppKey: os.Getenv("GITHUB_APP_PRIVATE_KEY"),
		// #425: the API's internal listener (the operator sets APPROVALS_URL on every agent pod)
		APIURL: getenv("API_INTERNAL_URL", os.Getenv("APPROVALS_URL")),
	}
	// preferred: the key as a file (a Secret volume mounted only into this container), so it is never in any
	// process environment; /proc/<pid>/environ keeps the original environment even after os.Unsetenv
	if f := os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE"); f != "" && c.ForgeKind == "github" {
		b, err := os.ReadFile(f)
		if err != nil || len(b) > 16<<10 {
			return nil, fmt.Errorf("GITHUB_APP_PRIVATE_KEY_FILE: cannot read %s (or larger than 16 KiB)", f)
		}
		c.GitHubAppKey = string(b)
	}
	c.IncomingDir = getenv("INCOMING_DIR", filepath.Join(filepath.Dir(c.MirrorDir), "incoming"))
	mb, err := strconv.ParseInt(getenv("MAX_PUSH_MB", "50"), 10, 64)
	if err != nil || mb <= 0 {
		return nil, fmt.Errorf("MAX_PUSH_MB: %q", os.Getenv("MAX_PUSH_MB"))
	}
	c.MaxPushBytes = mb << 20
	every, err := strconv.Atoi(getenv("REGISTER_EVERY", "60"))
	if err != nil || every <= 0 {
		return nil, fmt.Errorf("REGISTER_EVERY: %q", os.Getenv("REGISTER_EVERY"))
	}
	c.RegisterEvery = time.Duration(every) * time.Second
	if c.MergeMode, c.MergeMethod, c.MergeApprovers, err = mergeConfig(getenv); err != nil {
		return nil, err
	}
	if c.ForgeKind == "github" {
		// the App's key replaces FORGE_TOKEN; the driver checks the App values (github.go)
		if c.ForgeURL == "" || c.ForgeUser == "" || len(c.Repos) == 0 {
			return nil, fmt.Errorf("FORGE_URL, FORGE_USER and REPO are required")
		}
	} else if c.ForgeURL == "" || c.ForgeUser == "" || c.Token == "" || len(c.Repos) == 0 {
		return nil, fmt.Errorf("FORGE_URL, FORGE_USER, FORGE_TOKEN and REPO are required")
	}
	if c.Self, err = os.Executable(); err != nil {
		return nil, err
	}
	return c, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "credential" { // git credential helper mode (see credential.go)
		os.Exit(credentialHelper(os.Args[2:], os.Stdin, os.Stdout))
	}
	log.SetFlags(0)
	log.SetPrefix("")
	syscall.Umask(0o022) // the agent container reads the mirror; nothing here is group- or world-writable
	c, err := loadConfig()
	if err != nil {
		log.Fatalf("forge: %v", err)
	}
	drv, err := newDriver(c)
	if err != nil {
		log.Fatalf("forge: %v", err)
	}
	// the key is parsed; drop it from the environment so no child process (git, the credential helper) inherits it
	_ = os.Unsetenv("GITHUB_APP_PRIVATE_KEY")
	s := newServer(c)
	s.drv, s.rules, s.git.drv = drv, drv.rules(), drv
	if on, apiURL, timeout := approvalsConfig(getenv); on { // #67
		s.enableApprovals(apiURL, timeout)
		// #310: cost approvals read the job state from claude-proxy (CLAUDE_PROXY_URL, set by the operator)
		s.appr.proxyURL = strings.TrimRight(os.Getenv("CLAUDE_PROXY_URL"), "/")
		s.logf("approvals on: %s, timeout %s", apiURL, timeout)
	}
	if c.MergeMode == "gated" && !s.mergeOn() {
		s.logf("MERGE_MODE=gated is supported on GitHub only: the merge endpoint stays off on %s", drv.kind())
	} else if s.mergeOn() {
		s.logf("merge mode gated: the agent may merge its own PRs once a person approved the head (method %s)", c.MergeMethod)
	}
	if err := s.git.init(); err != nil {
		log.Fatalf("forge: mirror: %v", err)
	}
	startWebhook(c, s.logf)
	s.logf("forge API on %s, repos=%s, protected=%s", c.Listen, strings.Join(c.Repos, ","), strings.Join(c.Protected, ","))
	srv := &http.Server{Addr: c.Listen, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	// #593: on SIGTERM the sidecar keeps serving while the loop pushes the work in progress of its jobs, until the
	// loop says it is done (a loop-exit line in the wake file) or FORGE_TERM_HOLD_SECONDS pass
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	go func() {
		<-term
		hold := termHold(getenv("FORGE_TERM_HOLD_SECONDS", "25"))
		s.logf("SIGTERM: serving the loop's last pushes for at most %s", hold)
		waitLoopExit(c.WakeFile, hold, 200*time.Millisecond)
		os.Exit(0)
	}()
	log.Fatal(srv.ListenAndServe())
}

// termHold: FORGE_TERM_HOLD_SECONDS, 0..300, default 25 (a Spot VM gets 30 s in all)
func termHold(v string) time.Duration {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 300 {
		n = 25
	}
	return time.Duration(n) * time.Second
}

// waitLoopExit returns once the wake file grows by a line with "loop-exit" (the loop's SIGTERM handler is done), or
// after max. Only the end of the file written after the call is read.
func waitLoopExit(wakeFile string, max, poll time.Duration) bool {
	var off int64
	if fi, err := os.Stat(wakeFile); err == nil {
		off = fi.Size()
	}
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if f, err := os.Open(wakeFile); err == nil {
			if fi, err := f.Stat(); err == nil && fi.Size() > off {
				buf := make([]byte, min(fi.Size()-off, 1<<16))
				if n, _ := f.ReadAt(buf, off); n > 0 && strings.Contains(string(buf[:n]), " loop-exit") {
					f.Close()
					return true
				}
			}
			f.Close()
		}
		time.Sleep(poll)
	}
	return false
}
