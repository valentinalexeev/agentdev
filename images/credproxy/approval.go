package main

// Soft job limit with approval (#310, ADR 0002 D6). CLAUDE_JOB_APPROVAL_UNITS (0 = off) is a soft limit per job id,
// below the hard CLAUDE_JOB_BUDGET_UNITS. At the soft limit the proxy answers 429 with the marker
// "agentdev:job_approval" and prints the event job-approval-needed once per job and step. The agent loop then asks a
// person through the approval flow of #67 (source cost, created by the forge sidecar from GET /status of this proxy).
//
// Only an allowed Approval raises the limit, by CLAUDE_JOB_APPROVAL_STEP_UNITS per step. The proxy reads the Approvals
// itself from the Kubernetes API (get and list in the agents namespace, nothing else): their status is written only by
// agentdev-api after a person decided, and their spec only by the operator from the forge sidecar's line. No header,
// no request body and no file the agent can write changes the limit. The worker never tells the proxy to raise it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// approvalMarker is in the message of the soft-limit 429; the agent loop looks for it (images/worker/agent-loop.sh).
const approvalMarker = "agentdev:job_approval"

type approvalConfig struct {
	soft int64 // units per job before the first approval; 0 = off
	step int64 // units each allowed approval adds
}

// approvalFromEnv reads CLAUDE_JOB_APPROVAL_UNITS and CLAUDE_JOB_APPROVAL_STEP_UNITS (default: the soft limit). The
// soft limit must be lower than the hard limit when both are set.
func approvalFromEnv(getenv func(string) string, hard int64) (approvalConfig, error) {
	num := func(name, def string) (int64, error) {
		v := getenv(name)
		if v == "" {
			v = def
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, errors.New(name + " must be a non-negative integer")
		}
		return n, nil
	}
	soft, err := num("CLAUDE_JOB_APPROVAL_UNITS", "0")
	if err != nil {
		return approvalConfig{}, err
	}
	step, err := num("CLAUDE_JOB_APPROVAL_STEP_UNITS", strconv.FormatInt(soft, 10))
	if err != nil {
		return approvalConfig{}, err
	}
	if soft > 0 && hard > 0 && soft >= hard {
		return approvalConfig{}, errors.New("CLAUDE_JOB_APPROVAL_UNITS must be lower than CLAUDE_JOB_BUDGET_UNITS")
	}
	if soft > 0 && step == 0 {
		step = soft
	}
	return approvalConfig{soft: soft, step: step}, nil
}

// approvalSource lists the steps a person allowed for one job of one agent.
type approvalSource interface {
	granted(ctx context.Context, agent, job string) (int64, error)
}

// softLimit refuses a request of a job at its soft limit. It returns true when it answered the request.
func (s *server) softLimit(w http.ResponseWriter, agent, job string) bool {
	if s.approval.soft <= 0 || job == "" {
		return false
	}
	used := s.jobs.used(agent, job)
	if used < s.approval.soft {
		return false
	}
	var steps int64
	if s.approvals != nil {
		n, err := s.approvals.granted(context.Background(), agent, job)
		if err != nil {
			// fail closed: without the decisions the limit stays where it is
			emitEvent(agent, "proxy-approval-read-error", "medium", clip(err.Error(), 200))
		}
		steps = n
	}
	limit := s.approval.soft + steps*s.approval.step
	if used < limit {
		s.jobs.approvalGranted(agent, job, limit)
		return false
	}
	s.tel.refuse(agent, "job_approval")
	if s.jobs.askApproval(agent, job, steps+1, limit) {
		emitEvent(agent, "job-approval-needed", "low", fmt.Sprintf("job %s used %d of %d units (CLAUDE_JOB_APPROVAL_UNITS): "+
			"approval step %d asks for %d more units", job, used, limit, steps+1, s.approval.step))
	}
	// x-should-retry: false, so Claude Code does not retry the request; the loop asks for the approval instead
	w.Header().Set("X-Should-Retry", "false")
	apiError(w, http.StatusTooManyRequests, "rate_limit_error", fmt.Sprintf("claude-proxy: %s: job %s of agent %s used %d of %d units "+
		"and waits for an approval (CLAUDE_JOB_APPROVAL_UNITS)", approvalMarker, job, agent, used, limit))
	return true
}

// ---- Approvals from the Kubernetes API ----

var costStepRe = regexp.MustCompile(`^step-([1-9][0-9]{0,5})$`)

// kubeApprovals reads Approvals (agentdev.dev/v1alpha1) of source cost with a short cache (ttl, also for a refused
// job, so a model that calls the proxy in a loop cannot load the API server). It uses the proxy's own
// service account (Role agentdev-claude-proxy-approvals: get and list approvals in the agents namespace).
type kubeApprovals struct {
	base   string // https://<api server>
	ns     string
	client *http.Client
	bearer func() (string, error)
	now    func() time.Time
	ttl    time.Duration

	mu    sync.Mutex
	cache map[string]grantEntry
}

type grantEntry struct {
	n   int64
	exp time.Time
}

type approvalObject struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		ID          string `json:"id"`
		Agent       string `json:"agent"`
		Repo        string `json:"repo"`
		Job         string `json:"job"`
		Source      string `json:"source"`
		Tool        string `json:"tool"`
		Input       string `json:"input"`
		ContentHash string `json:"contentHash"`
	} `json:"spec"`
	Status struct {
		State       string `json:"state"`
		ContentHash string `json:"contentHash"`
	} `json:"status"`
}

// approvalHash: same fields and order as images/operator/approvals.go ApprovalHash.
func approvalHash(fields ...string) string {
	h := sha256.New()
	for _, f := range fields {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// countGranted counts the distinct steps allowed for agent and job. An object counts only when every field agrees: the
// labels select, the spec is checked again (agent, job, source, tool, content hash) and the decision must cover the
// spec's content hash.
func countGranted(items []approvalObject, agent, job string) int64 {
	steps := map[string]bool{}
	for _, a := range items {
		sp := a.Spec
		if a.Status.State != "allowed" || sp.Source != "cost" || sp.Agent != agent || sp.Job != job || !costStepRe.MatchString(sp.Tool) {
			continue
		}
		if sp.ContentHash != approvalHash(sp.ID, sp.Agent, sp.Repo, sp.Job, sp.Source, sp.Tool, sp.Input) || a.Status.ContentHash != sp.ContentHash {
			continue
		}
		steps[sp.Tool] = true
	}
	return int64(len(steps))
}

func (k *kubeApprovals) granted(ctx context.Context, agent, job string) (int64, error) {
	key := agent + "/" + job
	now := k.now()
	k.mu.Lock()
	if e, ok := k.cache[key]; ok && now.Before(e.exp) {
		k.mu.Unlock()
		return e.n, nil
	}
	k.mu.Unlock()
	tok, err := k.bearer()
	if err != nil {
		return 0, fmt.Errorf("approvals: own token: %w", err)
	}
	sel := "agent=" + agent + ",agentdev/job=" + job + ",agentdev/source=cost"
	u := k.base + "/apis/agentdev.dev/v1alpha1/namespaces/" + url.PathEscape(k.ns) + "/approvals?labelSelector=" + url.QueryEscape(sel)
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := k.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("approvals: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("approvals: status %d", resp.StatusCode)
	}
	var list struct {
		Items []approvalObject `json:"items"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<20)).Decode(&list); err != nil {
		return 0, fmt.Errorf("approvals: decode: %w", err)
	}
	n := countGranted(list.Items, agent, job)
	k.mu.Lock()
	if len(k.cache) > 4096 {
		k.cache = map[string]grantEntry{}
	}
	k.cache[key] = grantEntry{n, now.Add(k.ttl)}
	k.mu.Unlock()
	return n, nil
}

// inClusterApprovals builds the reader from the pod's service account files (same files as the TokenReview client).
func inClusterApprovals(ns string) (*kubeApprovals, error) {
	r, err := inClusterReviewer(ns)
	if err != nil {
		return nil, err
	}
	base := r.url[:len(r.url)-len("/apis/authentication.k8s.io/v1/tokenreviews")]
	return &kubeApprovals{base: base, ns: ns, client: r.client, bearer: r.bearer, now: time.Now, ttl: 10 * time.Second,
		cache: map[string]grantEntry{}}, nil
}
