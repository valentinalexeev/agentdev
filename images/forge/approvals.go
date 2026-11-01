package main

// Approvals (#67, docs/security-model.md "Approvals and permission notifications"). When APPROVALS=true the sidecar
// turns two refusals into requests a project member decides on the forge:
//
//   - Claude permission prompts: POST /mcp is a minimal MCP server (JSON-RPC over HTTP) with one tool, approve, used as
//     `--permission-prompt-tool mcp__agentdev__approve`. Claude waits for the answer.
//   - pushes touching a protected path: the push waits for the decision (at most PushHold).
//
// Lifecycle (owner on #107): one record per request, ending allowed, denied or void. A request is identified by its
// fingerprint (job + tool + exact input; for a push job + branch + protected paths). Asking again with the same
// fingerprint reuses the open record: no new line, no new comment. A timeout only ends the agent's wait (the action
// fails closed); the record stays pending and decidable, and the next ask with the same fingerprint uses its decision.
// A record becomes void when it is no longer needed: the job ends (POST /job-done from the loop), a newer push of the
// same branch and paths supersedes it, the issue or PR closes, or the pod is gone (the last two found by the API).
// On GitHub (#87) the calls go through the driver (GitHub API, installation token), the requester is the App bot and
// the job check uses the label agent:<name>; the API decides and checks the issue/PR state as on Gitea. A void the sidecar cannot deliver (API unreachable,
// 3 attempts) leaves the record pending until the pod-gone check; a pod that serves several jobs keeps it until restart.
// The sidecar edits its forge comment on each change of state.
//
// A request is one line on stdout, `AGENTDEV_APPROVAL_REQUEST {json}`; the operator turns it into an Approval (it
// trusts that line only from this container). The sidecar comments "waiting for approval <id>" on the job's issue or
// PR, then polls agentdev-api's internal endpoint for the decision, which the API takes from the forge comments after
// checking the commenter (images/api/approvals.go). The sidecar acts only on a decision for the content hash it
// computed itself, and fails closed: an error, a timeout or an unknown answer is a denial.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const approvalPrefix = "AGENTDEV_APPROVAL_REQUEST "

// decisionCmdRe matches a decision command in a comment body. The sidecar never posts one (rule 1: agents never decide).
var decisionCmdRe = regexp.MustCompile(`(?im)^\s*/(approve|approve-job|deny)\b`)

type approvalRequest struct {
	ID          string `json:"id"`
	Repo        string `json:"repo"`
	Forge       string `json:"forge"`
	Job         string `json:"job"`
	Source      string `json:"source"` // claude | push
	Tool        string `json:"tool"`
	Input       string `json:"input"`
	ContentHash string `json:"contentHash"`
	TimeoutSec  int    `json:"timeoutSeconds"`
	RequesterID int64  `json:"requesterId"` // the agent's forge account id (GET /user with its own token)
	SecretHash  string `json:"secretHash"`  // sha256 of secret; only the sidecar knows secret

	secret    string // sent when reading the decision (X-Approval-Secret); never logged
	fp        string // fingerprint: job + source + what is asked
	commentID int64  // the "waiting for approval" comment, edited on each change of state
	state     string // last known: pending | allowed | denied | void
	waitedOut bool   // the agent stopped waiting once (comment says so)
}

// decision is the API's internal answer (GET {APPROVALS_URL}/internal/approvals/{agent}/{id}).
type decision struct {
	State       string `json:"state"` // pending | allowed | denied | void
	Scope       string `json:"scope"` // once | job
	DecidedBy   string `json:"decidedBy"`
	ContentHash string `json:"contentHash"`
}

// approvalHash: same fields and order as images/operator/approvals.go ApprovalHash.
func approvalHash(id, agent, repo, job, source, tool, input string) string {
	h := sha256.New()
	for _, s := range []string{id, agent, repo, job, source, tool, input} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

type approvals struct {
	s        *server
	timeout  time.Duration
	pushHold time.Duration
	poll     time.Duration
	apiURL   string // agentdev-api internal listener, e.g. http://agentdev-api-internal.agentdev.svc:8081
	mu       sync.Mutex
	jobTools map[string]bool             // job + tool + exact input: allowed again in this job (allow_job)
	reqs     map[string]*approvalRequest // fingerprint -> the one record of that request (pending or denied)
	inflight map[string]chan struct{}    // fingerprint -> closed when the request being created for it is in reqs
	voidWait time.Duration               // first pause between void attempts (doubles each time)
	now      func() time.Time
	selfID   int64 // the agent's forge account id, looked up once
}

func newApprovals(s *server, apiURL string, timeout time.Duration) *approvals {
	hold := 14 * time.Minute // the loop's curl waits 15 min for /push
	if timeout < hold {
		hold = timeout
	}
	return &approvals{s: s, apiURL: strings.TrimRight(apiURL, "/"), timeout: timeout, pushHold: hold, poll: 10 * time.Second,
		jobTools: map[string]bool{}, reqs: map[string]*approvalRequest{}, inflight: map[string]chan struct{}{},
		voidWait: 2 * time.Second, now: time.Now}
}

// secretRe matches common credential shapes; redact replaces them before the request text leaves the sidecar (forge
// comment, log line, Approval object, UI). The agent container holds no forge, wake or register token (#64), so this
// is defence in depth against a secret Claude read from the repository or an issue (GHAS review of #107).
var secretRe = regexp.MustCompile(`(?i)(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|` +
	`sk-[A-Za-z0-9_-]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----|` +
	`((?:password|passwd|secret|token|api[_-]?key|authorization)["']?\s*[:=]\s*["']?(?:bearer\s+|token\s+)?)[^\s"',}]{6,})`)

func (a *approvals) redact(s string) string {
	if t := a.s.c.Token; len(t) >= 8 {
		s = strings.ReplaceAll(s, t, "[redacted]")
	}
	return secretRe.ReplaceAllStringFunc(s, func(m string) string {
		if sm := secretRe.FindStringSubmatch(m); sm[2] != "" {
			return sm[2] + "[redacted]"
		}
		return "[redacted]"
	})
}

func clipInput(s string) string {
	if len(s) > 4000 {
		s = s[:4000] + "..."
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// request returns the record for fingerprint fp, creating it (log line + forge comment) only if there is none. A
// push record whose input differs (a new commit on the same branch and paths) is superseded: voided, then replaced.
func (a *approvals) request(job, source, tool, input, fp string) (*approvalRequest, error) {
	want := clipInput(a.redact(input))
	// One creator per fingerprint: a concurrent call with the same fp (parallel protected pushes, a repeated prompt)
	// waits for the creator, then reuses its record instead of filing a second one (Reviewer on #107).
	a.mu.Lock()
	for {
		ch := a.inflight[fp]
		if ch == nil {
			break
		}
		a.mu.Unlock()
		<-ch
		a.mu.Lock()
	}
	r := a.reqs[fp]
	if r != nil && r.Input == want {
		a.mu.Unlock()
		return r, nil
	}
	ch := make(chan struct{})
	a.inflight[fp] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.inflight, fp)
		a.mu.Unlock()
		close(ch)
	}()
	if r != nil {
		a.void(r, "superseded by a newer request")
	}
	r, err := a.open(job, source, tool, input)
	if err != nil {
		return nil, err
	}
	r.fp = fp
	a.mu.Lock()
	a.reqs[fp] = r
	a.mu.Unlock()
	return r, nil
}

// open creates a request: prints the line and comments on the job. It does not wait.
func (a *approvals) open(job, source, tool, input string) (*approvalRequest, error) {
	if !jobRe.MatchString(job) {
		return nil, errors.New("no job: an approval needs the issue or PR the agent works on")
	}
	// the job comes from the agent container (X-Agentdev-Job): check on the forge that it is this agent's work
	if err := a.ownJob(job); err != nil {
		emitEvent(a.s.c.AgentName, "", "approval-foreign-job", "high", fmt.Sprintf("approval for %s: %v", job, err))
		return nil, err
	}
	self, err := a.requester()
	if err != nil {
		return nil, err
	}
	var rnd [8]byte
	var sec [32]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(sec[:]); err != nil {
		return nil, err
	}
	sh := sha256.Sum256([]byte(hex.EncodeToString(sec[:])))
	r := &approvalRequest{ID: hex.EncodeToString(rnd[:]), Repo: a.s.c.Repos[0], Forge: a.s.c.ForgeKind, Job: job,
		Source: source, Tool: clip(tool, 128), Input: clipInput(a.redact(input)), TimeoutSec: int(a.timeout.Seconds()),
		RequesterID: self, SecretHash: hex.EncodeToString(sh[:]), secret: hex.EncodeToString(sec[:]), state: "pending"}
	r.ContentHash = approvalHash(r.ID, a.s.c.AgentName, r.Repo, r.Job, r.Source, r.Tool, r.Input)
	b, _ := json.Marshal(r)
	eventMu.Lock()
	fmt.Fprintf(eventOut, "%s%s\n", approvalPrefix, b)
	eventMu.Unlock()
	r.commentID = a.postComment(r.Job, a.commentBody(r, ""))
	return r, nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// authorize sets the forge credential of the driver (Gitea token, or the GitHub App installation token, #87).
func (a *approvals) authorize(ctx context.Context, req *http.Request) error {
	h, err := a.s.driver().authorization(ctx)
	if err != nil {
		return fmt.Errorf("forge credential: %v", err)
	}
	req.Header.Set("Authorization", h)
	return nil
}

// forgeGet reads a forge API path with the agent's token into out.
func (a *approvals) forgeGet(path string, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.s.driver().apiURL(path, ""), nil)
	if err != nil {
		return err
	}
	if err := a.authorize(ctx, req); err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.s.client.Do(req)
	if err != nil {
		return errors.New("forge unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("forge: HTTP %d for %s", resp.StatusCode, path)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// requester is the agent's forge account id, so the API can refuse decisions from it whatever its login is.
func (a *approvals) requester() (int64, error) {
	a.mu.Lock()
	id := a.selfID
	a.mu.Unlock()
	if id > 0 {
		return id, nil
	}
	// GitHub (#87): every agent writes as the App's bot account, so the requester is that account
	if d, ok := a.s.driver().(*githubDriver); ok {
		_, bid, err := d.identity(context.Background())
		if err != nil || bid <= 0 {
			return 0, fmt.Errorf("cannot read the App bot account: %v", err)
		}
		a.mu.Lock()
		a.selfID = bid
		a.mu.Unlock()
		return bid, nil
	}
	var u struct {
		ID int64 `json:"id"`
	}
	if err := a.forgeGet("/user", &u); err != nil || u.ID <= 0 {
		return 0, fmt.Errorf("cannot read the agent's forge account: %v", err)
	}
	a.mu.Lock()
	a.selfID = u.ID
	a.mu.Unlock()
	return u.ID, nil
}

// ownJob checks that job is work of this agent: an issue assigned to it, or a PR from its own branch.
func (a *approvals) ownJob(job string) error {
	kind, num, _ := strings.Cut(job, "-")
	if kind == "issue" {
		var is struct {
			Assignees []struct {
				Login string `json:"login"`
			} `json:"assignees"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		}
		if err := a.forgeGet(fmt.Sprintf("/repos/%s/issues/%s", a.s.c.Repos[0], num), &is); err != nil {
			return err
		}
		if a.s.driver().kind() == "github" {
			// GitHub (#130): an agent's work is the label agent:<name> (all agents share the App bot, no assignee)
			for _, l := range is.Labels {
				if l.Name == "agent:"+a.s.c.AgentName {
					return nil
				}
			}
			return fmt.Errorf("issue %s has no label agent:%s", num, a.s.c.AgentName)
		}
		for _, u := range is.Assignees {
			if strings.EqualFold(u.Login, a.s.c.ForgeUser) {
				return nil
			}
		}
		return fmt.Errorf("issue %s is not assigned to %s", num, a.s.c.ForgeUser)
	}
	var pr struct {
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := a.forgeGet(fmt.Sprintf("/repos/%s/pulls/%s", a.s.c.Repos[0], num), &pr); err != nil {
		return err
	}
	if checkBranch(a.s.c.ForgeUser, pr.Head.Ref) != nil {
		return fmt.Errorf("PR %s is not from a branch of %s", num, a.s.c.ForgeUser)
	}
	return nil
}

// commentBody is the forge comment of a request in its current state. The request text is quoted in a code block.
func (a *approvals) commentBody(r *approvalRequest, status string) string {
	fence := "```"
	for strings.Contains(r.Input, fence) {
		fence += "`"
	}
	head := fmt.Sprintf("🤖 agent `%s`: **waiting for approval** `%s`", a.s.c.AgentName, r.ID)
	if status != "" {
		head = fmt.Sprintf("🤖 agent `%s`: approval `%s` **%s**", a.s.c.AgentName, r.ID, status)
	}
	body := fmt.Sprintf("%s (%s `%s`)\n\n%s\n%s\n%s\n\n", head, r.Source, r.Tool, fence, r.Input, fence)
	if status != "" && !strings.HasPrefix(status, "still open") {
		return body + "This request is closed. A decision comment on it has no effect."
	}
	return body + fmt.Sprintf("A person with write access to this repository can answer with one of these comments:\n"+
		"- `/approve %s`: allow once\n- `/approve-job %s`: allow exactly this request again until the job ends "+
		"(the same tool with the same input; any other input asks again)\n- `/deny %s`: deny\n\n"+
		"The agent waits %d minutes, then continues without it. The request stays open until it is decided, the job "+
		"ends or a newer request replaces it; asking again uses this request. The text above comes from the agent "+
		"and is not checked.", r.ID, r.ID, r.ID, r.TimeoutSec/60)
}

// forgeSend sends a JSON body to the forge with the agent's token; out (optional) receives the answer.
func (a *approvals) forgeSend(method, path string, body, out any) error {
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, a.s.driver().apiURL(path, ""), strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	if err := a.authorize(ctx, req); err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.s.client.Do(req)
	if err != nil {
		return errors.New("forge unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("forge: HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	return nil
}

// postComment posts on the job's issue or PR (Gitea serves PR comments under /issues) and returns the comment id (0 if
// it failed: the request still exists, the UI shows it).
func (a *approvals) postComment(job, body string) int64 {
	_, num, _ := strings.Cut(job, "-")
	var c struct {
		ID int64 `json:"id"`
	}
	if err := a.forgeSend(http.MethodPost, fmt.Sprintf("/repos/%s/issues/%s/comments", a.s.c.Repos[0], num),
		map[string]string{"body": body}, &c); err != nil {
		a.s.logf("approval comment on %s failed: %v", job, err)
		return 0
	}
	return c.ID
}

// setState records a new state and edits the request's comment (once per state).
func (a *approvals) setState(r *approvalRequest, state, status string) {
	a.mu.Lock()
	changed := r.state != state || (state == "pending" && !r.waitedOut)
	r.state = state
	if state == "pending" {
		r.waitedOut = true
	}
	a.mu.Unlock()
	if !changed || r.commentID == 0 {
		return
	}
	if err := a.forgeSend(http.MethodPatch, fmt.Sprintf("/repos/%s/issues/comments/%d", a.s.c.Repos[0], r.commentID),
		map[string]string{"body": a.commentBody(r, status)}, nil); err != nil {
		a.s.logf("approval %s: edit comment: %v", r.ID, err)
	}
}

// void asks the API to close a pending request as void (authenticated with the request's secret), forgets it and
// edits the comment. Already decided requests stay as they are.
func (a *approvals) void(r *approvalRequest, reason string) {
	a.mu.Lock()
	if a.reqs[r.fp] == r {
		delete(a.reqs, r.fp)
	}
	pending := r.state == "pending"
	a.mu.Unlock()
	if !pending {
		return
	}
	u := fmt.Sprintf("%s/internal/approvals/%s/%s/void", a.apiURL, url.PathEscape(a.s.c.AgentName), url.PathEscape(r.ID))
	b, _ := json.Marshal(map[string]string{"reason": reason})
	// Retry: if the API stays unreachable, the record stays pending until the API sees that the pod is gone
	// (docs/security-model.md, lifecycle).
	wait := a.voidWait
	for attempt := 1; ; attempt++ {
		code, err := a.postVoid(u, string(b), r.secret)
		if err == nil && code == http.StatusOK {
			a.setState(r, "void", "void: "+reason)
			return
		}
		if err == nil && code < 500 {
			return // 409: decided meanwhile; 4xx: nothing a retry fixes
		}
		if attempt == 3 {
			a.s.logf("approval %s: void failed after %d attempts: %v %d (the API voids it when the pod is gone)", r.ID, attempt, err, code)
			return
		}
		time.Sleep(wait)
		wait *= 2
	}
}

func (a *approvals) postVoid(u, body, secret string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Approval-Secret", secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.s.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// jobDone voids every open request of job and drops the job's grants (POST /job-done?job=, from the loop).
func (a *approvals) jobDone(w http.ResponseWriter, r *http.Request) {
	job := r.URL.Query().Get("job")
	if !jobRe.MatchString(job) {
		http.Error(w, "bad job", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	var open []*approvalRequest
	for _, q := range a.reqs {
		if q.Job == job {
			open = append(open, q)
		}
	}
	for k := range a.jobTools {
		if strings.HasPrefix(k, job+"\x00") {
			delete(a.jobTools, k)
		}
	}
	a.mu.Unlock()
	for _, q := range open {
		a.void(q, "the job ended")
	}
	w.WriteHeader(http.StatusNoContent)
}

// check asks the API once. Any error is "pending" (the caller stops at the timeout, which is a denial).
func (a *approvals) check(ctx context.Context, r *approvalRequest) decision {
	u := fmt.Sprintf("%s/internal/approvals/%s/%s", a.apiURL, url.PathEscape(a.s.c.AgentName), url.PathEscape(r.ID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return decision{State: "pending"}
	}
	req.Header.Set("X-Approval-Secret", r.secret)
	resp, err := a.s.client.Do(req)
	if err != nil {
		return decision{State: "pending"}
	}
	defer resp.Body.Close()
	var d decision
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&d) != nil {
		return decision{State: "pending"}
	}
	if d.State == "allowed" && d.ContentHash != r.ContentHash {
		a.s.logf("approval %s: decision for another content hash, treated as denied", r.ID)
		emitEvent(a.s.c.AgentName, r.Job, "approval-hash-mismatch", "high", "approval "+r.ID)
		return decision{State: "denied"}
	}
	return d
}

// wait polls until a decision, the request's expiry or max (whichever is first). A pending answer at the end is
// returned as pending.
func (a *approvals) wait(ctx context.Context, r *approvalRequest, max time.Duration) decision {
	deadline := a.now().Add(max)
	for {
		d := a.check(ctx, r)
		if d.State != "pending" && d.State != "" {
			switch d.State {
			case "allowed":
				a.setState(r, "allowed", "allowed by "+d.DecidedBy)
			case "denied":
				a.setState(r, "denied", "denied by "+d.DecidedBy)
			default:
				a.setState(r, d.State, d.State)
			}
			return d
		}
		if !a.now().Before(deadline) {
			a.setState(r, "pending", "still open: the agent stopped waiting and continued without it; a decision applies the next time it asks")
			return decision{State: "pending"}
		}
		select {
		case <-ctx.Done():
			return decision{State: "pending"}
		case <-time.After(a.poll):
		}
	}
}

// ---- Claude permission prompts: a minimal MCP server ----

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (a *approvals) mcp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req rpcReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 { // a notification (notifications/initialized): no answer
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]string{"name": "agentdev", "version": "1"}}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{
			"name":        "approve",
			"description": "Permission prompt: asks a project member on the forge to allow a tool call.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"tool_name": map[string]string{"type": "string"}, "input": map[string]string{"type": "object"},
				"tool_use_id": map[string]string{"type": "string"}}, "required": []string{"tool_name", "input"}},
		}}}
	case "tools/call":
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				ToolName string          `json:"tool_name"`
				Input    json.RawMessage `json:"input"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name != "approve" {
			writeRPC(w, req.ID, nil, &rpcError{-32602, "unknown tool"})
			return
		}
		out := a.prompt(r.Context(), r.Header.Get("X-Agentdev-Job"), p.Arguments.ToolName, p.Arguments.Input)
		b, _ := json.Marshal(out)
		result = map[string]any{"content": []any{map[string]string{"type": "text", "text": string(b)}}}
	default:
		writeRPC(w, req.ID, nil, &rpcError{-32601, "method not found"})
		return
	}
	writeRPC(w, req.ID, result, nil)
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, e *rpcError) {
	w.Header().Set("Content-Type", "application/json")
	m := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		m["error"] = e
	} else {
		m["result"] = result
	}
	_ = json.NewEncoder(w).Encode(m)
}

// prompt answers one Claude permission prompt: {"behavior":"allow","updatedInput":...} or {"behavior":"deny",...}.
func (a *approvals) prompt(ctx context.Context, job, tool string, input json.RawMessage) map[string]any {
	deny := func(msg string) map[string]any { return map[string]any{"behavior": "deny", "message": msg} }
	if len(input) == 0 || string(input) == "null" {
		input = json.RawMessage(`{}`)
	}
	if tool == "" {
		return deny("no tool name")
	}
	// allow_job covers the same tool with the same input only (review of #107): what the person read
	key := job + "\x00" + tool + "\x00" + clipInput(a.redact(string(input)))
	a.mu.Lock()
	ok := a.jobTools[key]
	a.mu.Unlock()
	if ok {
		return map[string]any{"behavior": "allow", "updatedInput": input}
	}
	req, err := a.request(job, "claude", tool, string(input), "claude\x00"+key)
	if err != nil {
		return deny("approval request failed: " + err.Error())
	}
	d := a.wait(ctx, req, a.timeout)
	switch d.State {
	case "allowed":
		a.mu.Lock()
		if d.Scope == "job" {
			a.jobTools[key] = true
		}
		delete(a.reqs, req.fp) // once is used up; a later ask is a new request
		a.mu.Unlock()
		a.s.logf("approval %s: %s allowed by %s (%s)", req.ID, tool, d.DecidedBy, d.Scope)
		return map[string]any{"behavior": "allow", "updatedInput": input}
	case "denied":
		return deny(fmt.Sprintf("denied by %s on the forge (approval %s). Do not try again; explain in your result what you needed.", d.DecidedBy, req.ID))
	case "void":
		return deny(fmt.Sprintf("approval %s is void. Explain in your result what you needed.", req.ID))
	default:
		return deny(fmt.Sprintf("no decision yet on approval %s; the request stays open on the forge. Do not try again "+
			"in this run; explain in your result what you needed.", req.ID))
	}
}

// ---- protected-path pushes ----

// pushApproval is called when a push hits a protected path. It returns nil when a person allowed it. The record is
// keyed by job, branch and paths, not the commit: a new commit supersedes (voids) the open request of the old one.
func (a *approvals) pushApproval(ctx context.Context, job, branch, tip string, paths []string) error {
	input := fmt.Sprintf("push %s to %s, changes protected paths: %s", tip, branch, strings.Join(paths, ", "))
	a.mu.Lock()
	if a.jobTools[job+"\x00push\x00"+tip] {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	r, err := a.request(job, "push", "push", input, "push\x00"+job+"\x00"+branch+"\x00"+strings.Join(paths, ","))
	if err != nil {
		return err
	}
	d := a.wait(ctx, r, a.pushHold)
	switch d.State {
	case "allowed":
		a.mu.Lock()
		delete(a.reqs, r.fp)
		a.jobTools[job+"\x00push\x00"+tip] = true // the same commit may be pushed again in this job
		a.mu.Unlock()
		return nil
	case "pending":
		return refuseAs("push-approval-pending", "low", "push waits for approval %s on the forge; the request stays open, push the same commit again later", r.ID)
	default:
		return refuseAs("push-protected-path", "high", "push refused: approval %s %s", r.ID, d.State)
	}
}

func approvalsConfig(get func(string, string) string) (on bool, apiURL string, timeout time.Duration) {
	min, err := strconv.Atoi(get("APPROVAL_TIMEOUT_MINUTES", "30"))
	if err != nil || min <= 0 || min > 24*60 {
		min = 30
	}
	apiURL = get("APPROVALS_URL", "")
	return get("APPROVALS", "false") == "true" && apiURL != "", apiURL, time.Duration(min) * time.Minute
}
