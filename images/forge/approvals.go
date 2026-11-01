package main

// Approvals (#67, docs/security-model.md "Approvals and permission notifications"). When APPROVALS=true the sidecar
// turns two refusals into requests a project member decides on the forge:
//
//   - Claude permission prompts: POST /mcp is a minimal MCP server (JSON-RPC over HTTP) with one tool, approve, used as
//     `--permission-prompt-tool mcp__agentdev__approve`. Claude waits for the answer.
//   - pushes touching a protected path: the push waits for the decision (at most PushHold), and a later push of the
//     same commit uses the same request.
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
}

// decision is the API's internal answer (GET {APPROVALS_URL}/internal/approvals/{agent}/{id}).
type decision struct {
	State       string `json:"state"` // pending | allowed | denied | expired
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
	jobTools map[string]bool             // job + "\x00" + tool: allowed for the rest of the job (allow_job)
	pushes   map[string]*approvalRequest // job + tip + paths: the open request of a held push
	now      func() time.Time
}

func newApprovals(s *server, apiURL string, timeout time.Duration) *approvals {
	hold := 14 * time.Minute // the loop's curl waits 15 min for /push
	if timeout < hold {
		hold = timeout
	}
	return &approvals{s: s, apiURL: strings.TrimRight(apiURL, "/"), timeout: timeout, pushHold: hold, poll: 10 * time.Second,
		jobTools: map[string]bool{}, pushes: map[string]*approvalRequest{}, now: time.Now}
}

func clipInput(s string) string {
	if len(s) > 4000 {
		s = s[:4000] + "..."
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// open creates a request: prints the line and comments on the job. It does not wait.
func (a *approvals) open(job, source, tool, input string) (*approvalRequest, error) {
	if !jobRe.MatchString(job) {
		return nil, errors.New("no job: an approval needs the issue or PR the agent works on")
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, err
	}
	r := &approvalRequest{ID: hex.EncodeToString(rnd[:]), Repo: a.s.c.Repos[0], Forge: a.s.c.ForgeKind, Job: job,
		Source: source, Tool: clip(tool, 128), Input: clipInput(input), TimeoutSec: int(a.timeout.Seconds())}
	r.ContentHash = approvalHash(r.ID, a.s.c.AgentName, r.Repo, r.Job, r.Source, r.Tool, r.Input)
	b, _ := json.Marshal(r)
	eventMu.Lock()
	fmt.Fprintf(eventOut, "%s%s\n", approvalPrefix, b)
	eventMu.Unlock()
	a.comment(r)
	return r, nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// comment posts "waiting for approval" on the job's issue or PR (Gitea serves PR comments under /issues too). The
// request text is quoted in a code block; a failed comment does not stop the request (the UI still shows it).
func (a *approvals) comment(r *approvalRequest) {
	_, num, _ := strings.Cut(r.Job, "-")
	fence := "```"
	for strings.Contains(r.Input, fence) {
		fence += "`"
	}
	body := fmt.Sprintf("🤖 agent `%s`: **waiting for approval** `%s` (%s `%s`)\n\n%s\n%s\n%s\n\n"+
		"A person with write access to this repository can answer with one of these comments:\n"+
		"- `/approve %s`: allow once\n- `/approve-job %s`: allow for this job\n- `/deny %s`: deny\n\n"+
		"No answer in %d minutes: denied. The text above comes from the agent and is not checked.",
		a.s.c.AgentName, r.ID, r.Source, r.Tool, fence, r.Input, fence, r.ID, r.ID, r.ID, r.TimeoutSec/60)
	b, _ := json.Marshal(map[string]string{"body": body})
	u := fmt.Sprintf("%s/api/v1/repos/%s/issues/%s/comments", a.s.c.ForgeURL, r.Repo, num)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(b)))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "token "+a.s.c.Token)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := a.s.client.Do(req); err != nil {
		a.s.logf("approval %s: comment failed: %v", r.ID, err)
	} else {
		resp.Body.Close()
	}
}

// check asks the API once. Any error is "pending" (the caller stops at the timeout, which is a denial).
func (a *approvals) check(ctx context.Context, r *approvalRequest) decision {
	u := fmt.Sprintf("%s/internal/approvals/%s/%s", a.apiURL, url.PathEscape(a.s.c.AgentName), url.PathEscape(r.ID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return decision{State: "pending"}
	}
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
			return d
		}
		if !a.now().Before(deadline) {
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
	key := job + "\x00" + tool
	a.mu.Lock()
	ok := a.jobTools[key]
	a.mu.Unlock()
	if ok {
		return map[string]any{"behavior": "allow", "updatedInput": input}
	}
	req, err := a.open(job, "claude", tool, string(input))
	if err != nil {
		return deny("approval request failed: " + err.Error())
	}
	d := a.wait(ctx, req, a.timeout)
	switch d.State {
	case "allowed":
		if d.Scope == "job" {
			a.mu.Lock()
			a.jobTools[key] = true
			a.mu.Unlock()
		}
		a.s.logf("approval %s: %s allowed by %s (%s)", req.ID, tool, d.DecidedBy, d.Scope)
		return map[string]any{"behavior": "allow", "updatedInput": input}
	case "denied":
		return deny(fmt.Sprintf("denied by %s on the forge (approval %s). Do not try again; explain in your result what you needed.", d.DecidedBy, req.ID))
	default:
		return deny(fmt.Sprintf("no decision in time (approval %s expired). Do not try again; explain in your result what you needed.", req.ID))
	}
}

// ---- protected-path pushes ----

// pushApproval is called when a push hits a protected path. It returns nil when a person allowed it.
func (a *approvals) pushApproval(ctx context.Context, job, branch, tip string, paths []string) error {
	input := fmt.Sprintf("push %s to %s, changes protected paths: %s", tip, branch, strings.Join(paths, ", "))
	key := job + "\x00" + input
	a.mu.Lock()
	if a.jobTools[job+"\x00push\x00"+tip] {
		a.mu.Unlock()
		return nil
	}
	r := a.pushes[key]
	a.mu.Unlock()
	if r == nil {
		var err error
		if r, err = a.open(job, "push", "push", input); err != nil {
			return err
		}
		a.mu.Lock()
		a.pushes[key] = r
		a.mu.Unlock()
	}
	d := a.wait(ctx, r, a.pushHold)
	switch d.State {
	case "allowed":
		a.mu.Lock()
		delete(a.pushes, key)
		a.jobTools[job+"\x00push\x00"+tip] = true // the same commit may be pushed again in this job
		a.mu.Unlock()
		return nil
	case "pending":
		return refuseAs("push-approval-pending", "low", "push waits for approval %s on the forge; push the same commit again later", r.ID)
	default:
		a.mu.Lock()
		delete(a.pushes, key)
		a.mu.Unlock()
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
