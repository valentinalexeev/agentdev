package main

// Approvals (#67, docs/security-model.md "Approvals and permission notifications"). Forge-first: a request is an
// Approval object (created by the operator from the forge sidecar's request line), the decision is a forge comment
// `/approve <id>`, `/approve-job <id>` or `/deny <id>` on the job's issue or PR. Deciding in the UI is #80.
//
// The decider (run) polls the pending Approvals and the comments of their issue or PR and applies the decision rules:
//
//  1. agents never decide: comments by any agent's forge account (matched by account id) are ignored
//  2. bound to one request: the comment quotes the id and was created after the request, on the same issue or PR
//  3. original text only: an edited comment is ignored (and audited)
//  4. bound to the content: the status records the spec's content hash; the sidecar checks it against its own
//  5. checked here, not in the pod: the API reads the comment from the forge and checks the commenter's write access
//  6. atomic: the status is written with the object's resourceVersion; the first valid decision wins
//
// Lifecycle (owner on #107): one record per request, ending allowed, denied or void. A timeout is not a state: it
// only ends the agent's wait, the record stays pending and decidable. A pending record becomes void when its sidecar
// says so (job ended, superseded: POST /internal/approvals/{agent}/{id}/void), when its issue or PR is closed, or when
// the pod that asked is gone. A decision comment on a void or decided record is ignored and audited. The sidecar
// reads the decision from the internal listener (INTERNAL_ADDR, agent pods only).

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var approvalsGVR = schema.GroupVersionResource{Group: "agentdev.dev", Version: "v1alpha1", Resource: "approvals"}

// approval is the API view of an Approval object.
type approval struct {
	Name        string    `json:"name"`
	ID          string    `json:"id"`
	Agent       string    `json:"agent"`
	Repo        string    `json:"repo"`
	Forge       string    `json:"forge"`
	Job         string    `json:"job"`
	Source      string    `json:"source"`
	Tool        string    `json:"tool"`
	Input       string    `json:"input"` // untrusted: written by the agent
	Untrusted   bool      `json:"untrusted"`
	ContentHash string    `json:"contentHash"`
	Created     time.Time `json:"created"`
	Expires     time.Time `json:"expires"`
	State       string    `json:"state"` // pending | allowed | denied | void
	Reason      string    `json:"reason,omitempty"`
	Scope       string    `json:"scope,omitempty"`
	DecidedBy   string    `json:"decidedBy,omitempty"`
	DecidedVia  string    `json:"decidedVia,omitempty"`
	CommentID   int64     `json:"commentId,omitempty"`
	DecidedAt   time.Time `json:"decidedAt,omitzero"`
	DecidedHash string    `json:"decidedHash,omitempty"`

	rv          string
	podUID      string
	requesterID int64
	secretHash  string
}

func fromApprovalObject(u *unstructured.Unstructured) approval {
	str := func(f ...string) string { v, _, _ := unstructured.NestedString(u.Object, f...); return v }
	tm := func(f ...string) time.Time { t, _ := time.Parse(time.RFC3339, str(f...)); return t }
	cid, _, _ := unstructured.NestedInt64(u.Object, "status", "commentId")
	rid, _, _ := unstructured.NestedInt64(u.Object, "spec", "requesterId")
	a := approval{Name: u.GetName(), ID: str("spec", "id"), Agent: str("spec", "agent"), Repo: str("spec", "repo"),
		Forge: str("spec", "forge"), Job: str("spec", "job"), Source: str("spec", "source"), Tool: str("spec", "tool"),
		Input: str("spec", "input"), Untrusted: true, ContentHash: str("spec", "contentHash"), Created: tm("spec", "created"),
		Expires: tm("spec", "expires"), State: str("status", "state"), Scope: str("status", "scope"),
		DecidedBy: str("status", "decidedBy"), DecidedVia: str("status", "decidedVia"), CommentID: cid,
		DecidedAt: tm("status", "decidedAt"), DecidedHash: str("status", "contentHash"), Reason: str("status", "reason"), rv: u.GetResourceVersion(),
		podUID:      u.GetLabels()["agentdev/pod-uid"],
		requesterID: rid, secretHash: str("spec", "secretHash")}
	if a.State == "" {
		a.State = "pending"
	}
	return a
}

// approvalStore reads Approvals and writes their status.
type approvalStore interface {
	ListApprovals(ctx context.Context) ([]*unstructured.Unstructured, error)
	GetApproval(ctx context.Context, name string) (*unstructured.Unstructured, error)
	UpdateApprovalStatus(ctx context.Context, u *unstructured.Unstructured) error
}

func (k *kubeAgents) ListApprovals(ctx context.Context) ([]*unstructured.Unstructured, error) {
	l, err := k.d.Resource(approvalsGVR).Namespace(k.ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]*unstructured.Unstructured, len(l.Items))
	for i := range l.Items {
		out[i] = &l.Items[i]
	}
	return out, nil
}

func (k *kubeAgents) GetApproval(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	return k.d.Resource(approvalsGVR).Namespace(k.ns).Get(ctx, name, metav1.GetOptions{})
}

// UpdateApprovalStatus writes the status with the object's resourceVersion: a conflict means someone else decided.
func (k *kubeAgents) UpdateApprovalStatus(ctx context.Context, u *unstructured.Unstructured) error {
	_, err := k.d.Resource(approvalsGVR).Namespace(k.ns).UpdateStatus(ctx, u, metav1.UpdateOptions{})
	return err
}

// forgeComment is what the decider needs of an issue comment.
type forgeComment struct {
	ID      int64
	UserID  int64
	Login   string
	Bot     bool // forge account type bot (GitHub); Gitea has none
	Body    string
	Created time.Time
	Updated time.Time
}

// commentReader reads issue/PR comments and resolves forge accounts. Read-only (the API's forge token).
type commentReader interface {
	Comments(ctx context.Context, repo string, number int, since time.Time) ([]forgeComment, error)
	UserID(ctx context.Context, login string) (int64, error)
	Self(ctx context.Context) (int64, error)                                 // the account of the API's own forge token
	IssueState(ctx context.Context, repo string, number int) (string, error) // open | closed (PRs too)
}

// podChecker tells whether the pod that made a request still exists (its sidecar is the only one that can act).
type podChecker interface {
	PodExists(ctx context.Context, uid string) (bool, error)
}

func (k *kubeAgents) PodExists(ctx context.Context, uid string) (bool, error) {
	l, err := k.c.CoreV1().Pods(k.ns).List(ctx, metav1.ListOptions{LabelSelector: "app=coding-agent"})
	if err != nil {
		return false, err
	}
	for _, p := range l.Items {
		if string(p.UID) == uid {
			return true, nil
		}
	}
	return false, nil
}

func (g *giteaPerms) get(ctx context.Context, path string, out any) error {
	if g == nil || g.base == "" || g.token == "" {
		return fmt.Errorf("%w: FORGE_URL or FORGE_API_TOKEN not set", errForgeUnavailable)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+g.token)
	req.Header.Set("Accept", "application/json")
	resp, err := g.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errForgeUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d", errForgeUnavailable, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

func (g *giteaPerms) Comments(ctx context.Context, repo string, number int, since time.Time) ([]forgeComment, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, fmt.Errorf("bad repo %q", repo)
	}
	var raw []struct {
		ID   int64 `json:"id"`
		User struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"user"`
		Body    string    `json:"body"`
		Created time.Time `json:"created_at"`
		Updated time.Time `json:"updated_at"`
	}
	p := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/comments?since=%s", url.PathEscape(owner), url.PathEscape(name), number,
		url.QueryEscape(since.UTC().Format(time.RFC3339)))
	if err := g.get(ctx, p, &raw); err != nil {
		return nil, err
	}
	out := make([]forgeComment, 0, len(raw))
	for _, c := range raw {
		out = append(out, forgeComment{ID: c.ID, UserID: c.User.ID, Login: c.User.Login, Body: c.Body, Created: c.Created, Updated: c.Updated})
	}
	return out, nil
}

func (g *giteaPerms) IssueState(ctx context.Context, repo string, number int) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return "", fmt.Errorf("bad repo %q", repo)
	}
	var is struct {
		State string `json:"state"`
	}
	if err := g.get(ctx, fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d", url.PathEscape(owner), url.PathEscape(name), number), &is); err != nil {
		return "", err
	}
	return is.State, nil
}

func (g *giteaPerms) Self(ctx context.Context) (int64, error) {
	var u struct {
		ID int64 `json:"id"`
	}
	if err := g.get(ctx, "/api/v1/user", &u); err != nil {
		return 0, err
	}
	return u.ID, nil
}

func (g *giteaPerms) UserID(ctx context.Context, login string) (int64, error) {
	var u struct {
		ID int64 `json:"id"`
	}
	if err := g.get(ctx, "/api/v1/users/"+url.PathEscape(login), &u); err != nil {
		return 0, err
	}
	return u.ID, nil
}

var (
	approvalIDRe = regexp.MustCompile(`^[a-f0-9]{16}$`)
	secretRe     = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

var decisionRe = regexp.MustCompile(`(?im)^[ \t]*/(approve|approve-job|deny)[ \t]+([a-f0-9]{16})\b`)

// commandLines drops fenced code blocks (``` or ~~~, any length) and quoted lines ("> "), so a command quoted from
// tool output or an earlier comment never counts: only a line the person wrote themselves.
func commandLines(body string) string {
	var out []string
	fence := ""
	for _, l := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		t := strings.TrimLeft(l, " \t")
		if fence != "" {
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = t[:3]
			for len(fence) < len(t) && t[len(fence)] == fence[0] {
				fence += string(fence[0])
			}
			continue
		}
		if strings.HasPrefix(t, ">") || strings.HasPrefix(l, "    ") || strings.HasPrefix(l, "\t") {
			continue // quote or indented code block
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// decider applies forge decisions to pending Approvals.
type decider struct {
	store  approvalStore
	agents agentLister
	forge  commentReader
	perms  permChecker
	pods   podChecker // nil: do not void requests of gone pods
	now    func() time.Time
	// platform forge accounts that never decide (APPROVAL_DENY_ACCOUNTS: router or App bot, the API's own token user,
	// any other service account), matched by login, case-insensitive
	denyLogins map[string]bool

	mu       sync.Mutex
	agentIDs map[string]int64    // agent forge login -> account id (rule 1)
	seen     map[int64]time.Time // comment ids already audited as refused, with the request's expiry (pruned after it)
}

func newDecider(store approvalStore, agents agentLister, forge commentReader, perms permChecker) *decider {
	return &decider{store: store, agents: agents, forge: forge, perms: perms, now: func() time.Time { return time.Now().UTC() },
		agentIDs: map[string]int64{}, seen: map[int64]time.Time{}, denyLogins: map[string]bool{}}
}

// run polls every interval until ctx ends.
func (d *decider) run(ctx context.Context, every time.Duration) {
	for {
		if err := d.pass(ctx); err != nil {
			log.Printf("approvals: %v", err)
		}
		if !sleepCtx(ctx, every) {
			return
		}
	}
}

// agentAccounts returns the forge account ids of every agent. The forge login of an agent is its name: the operator
// renders FORGE_USER=<name> and the forge token Secret agent-<name> belongs to that account. The requesting agent is
// also excluded by its own account id (spec.requesterId, from GET /user with its token), so a login that differs from
// the name cannot approve its own request. An account that cannot be resolved is logged and skipped: the requester
// check still holds, and the other agents are excluded when the forge answers again.
func (d *decider) agentAccounts(ctx context.Context) (map[int64]string, error) {
	list, err := d.agents.List(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	for _, a := range list {
		d.mu.Lock()
		id, ok := d.agentIDs[a.Name]
		d.mu.Unlock()
		if !ok {
			if id, err = d.forge.UserID(ctx, a.Name); err != nil {
				log.Printf("approvals: forge account of agent %s: %v (skipped in this pass)", a.Name, err)
				continue
			}
			d.mu.Lock()
			d.agentIDs[a.Name] = id
			d.mu.Unlock()
		}
		out[id] = a.Name
	}
	// the API's own token account never decides either (it can read every repository)
	if self, err := d.forge.Self(ctx); err == nil && self > 0 {
		out[self] = "agentdev-api"
	}
	return out, nil
}

// pass looks at every pending Approval once.
func (d *decider) pass(ctx context.Context) error {
	objs, err := d.store.ListApprovals(ctx)
	if err != nil {
		return err
	}
	var pending, closed []*unstructured.Unstructured
	for _, u := range objs {
		a := fromApprovalObject(u)
		switch {
		case a.State == "pending":
			pending = append(pending, u)
		case !a.DecidedAt.IsZero() && d.now().Sub(a.DecidedAt) < lateWindow:
			closed = append(closed, u)
		}
	}
	if len(pending) == 0 && len(closed) == 0 {
		return nil
	}
	agentIDs, err := d.agentAccounts(ctx)
	if err != nil {
		return err
	}
	d.pruneSeen()
	for _, u := range pending {
		if err := d.decide(ctx, u, agentIDs); err != nil {
			log.Printf("approval %s: %v", u.GetName(), err)
		}
	}
	for _, u := range closed {
		if err := d.late(ctx, fromApprovalObject(u)); err != nil {
			log.Printf("approval %s: %v", u.GetName(), err)
		}
	}
	return nil
}

// pruneSeen forgets refused comments after the late window (their request is no longer read).
func (d *decider) pruneSeen() {
	cut := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, exp := range d.seen {
		if exp.Before(cut) {
			delete(d.seen, id)
		}
	}
}

func (d *decider) refuse(a approval, c forgeComment, reason string) {
	d.mu.Lock()
	_, dup := d.seen[c.ID]
	d.seen[c.ID] = d.now().Add(lateWindow) // audited once; forgotten after the late window
	d.mu.Unlock()
	if dup {
		return
	}
	audit(auditEntry{Who: auditWho{ForgeUser: c.Login}, Action: "approval-decision", Agent: a.Agent, Result: "denied",
		Reason: reason, Request: approvalAudit(a, c.ID)})
}

// lateWindow: how long after a request closed (decided or void) later decision comments on it are still audited.
const lateWindow = 7 * 24 * time.Hour

// commands returns the comments that carry a decision command for request a, in creation order, with the verb.
func (d *decider) commands(ctx context.Context, a approval) ([]forgeComment, []string, error) {
	_, num, _ := strings.Cut(a.Job, "-")
	n, err := strconv.Atoi(num)
	if err != nil {
		return nil, nil, fmt.Errorf("bad job %q", a.Job)
	}
	comments, err := d.forge.Comments(ctx, a.Repo, n, a.Created)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(comments, func(i, j int) bool {
		return comments[i].Created.Before(comments[j].Created) || (comments[i].Created.Equal(comments[j].Created) && comments[i].ID < comments[j].ID)
	})
	var cs []forgeComment
	var verbs []string
	for _, c := range comments {
		for _, m := range decisionRe.FindAllStringSubmatch(commandLines(c.Body), -1) {
			if m[2] == a.ID {
				cs, verbs = append(cs, c), append(verbs, strings.ToLower(m[1]))
				break
			}
		}
	}
	return cs, verbs, nil
}

// late audits decision comments written after a request closed: they have no effect.
func (d *decider) late(ctx context.Context, a approval) error {
	if a.Forge != "" && a.Forge != "gitea" {
		return nil
	}
	cs, _, err := d.commands(ctx, a)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.ID != a.CommentID && c.Created.After(a.DecidedAt.Add(-time.Second)) {
			d.refuse(a, c, "request is "+a.State+": a later decision has no effect")
		}
	}
	return nil
}

func approvalAudit(a approval, commentID int64) map[string]any {
	m := map[string]any{"approval": a.ID, "repo": a.Repo, "job": a.Job, "tool": a.Tool, "contentHash": a.ContentHash}
	if commentID != 0 {
		m["commentId"] = commentID
	}
	return m
}

// decide checks one pending Approval: void conditions first (pod gone, issue or PR closed), then the decision
// comments in creation order; the first valid decision wins. There is no expiry (owner on #107).
func (d *decider) decide(ctx context.Context, u *unstructured.Unstructured, agentIDs map[int64]string) error {
	a := fromApprovalObject(u)
	kind, num, ok := strings.Cut(a.Job, "-")
	n, err := strconv.Atoi(num)
	if !ok || err != nil || (kind != "issue" && kind != "pr") {
		return fmt.Errorf("bad job %q", a.Job)
	}
	if d.pods != nil && a.podUID != "" && a.Source != "executor" {
		if alive, err := d.pods.PodExists(ctx, a.podUID); err == nil && !alive {
			return d.void(ctx, u, a, "the agent pod that asked is gone")
		}
	}
	if a.Forge != "" && a.Forge != "gitea" {
		return nil // GitHub decisions are not supported yet: the request stays pending until void; see #87
	}
	// an executor apply (#69) is requested on the merged, so closed, pull request: closing does not end it (its own
	// deadline, spec.approvalHours, is enforced by the operator)
	if st, err := d.forge.IssueState(ctx, a.Repo, n); err == nil && st == "closed" && a.Source != "executor" {
		return d.void(ctx, u, a, "the "+map[string]string{"issue": "issue", "pr": "pull request"}[kind]+" is closed")
	}
	cs, verbs, err := d.commands(ctx, a)
	if err != nil {
		return err
	}
	for i, c := range cs {
		verb := verbs[i]
		switch {
		case c.UserID == a.requesterID || a.requesterID <= 0 || agentIDs[c.UserID] != "" || c.Bot ||
			d.denyLogins[strings.ToLower(c.Login)]:
			d.refuse(a, c, "decision by an agent or bot account is ignored")
			continue
		case c.Created.Before(a.Created.Truncate(time.Second)): // forge times have second precision
			d.refuse(a, c, "comment is older than the request")
			continue
		case !c.Updated.IsZero() && c.Updated.After(c.Created.Add(time.Second)):
			d.refuse(a, c, "edited comment is ignored (security event)")
			log.Printf("AGENTDEV_SECURITY_NOTE edited approval comment %d on %s by %s", c.ID, a.Repo, c.Login)
			continue
		}
		p, err := d.perms.Permission(ctx, a.Forge, a.Repo, c.Login)
		if err != nil {
			return err // forge unavailable: try again next pass (fail closed)
		}
		if p < permWrite {
			d.refuse(a, c, fmt.Sprintf("%s has %s access to %s, write is needed", c.Login, p, a.Repo))
			continue
		}
		st := map[string]any{"state": "allowed", "scope": "once"}
		switch verb {
		case "approve-job":
			if a.Source != "executor" { // an executor apply is single-use (#69): approve-job counts as once
				st["scope"] = "job"
			}
		case "deny":
			st = map[string]any{"state": "denied"}
		}
		return d.write(ctx, u, a, st, c, "approval-"+st["state"].(string))
	}
	return nil
}

// void closes a pending request as no longer needed.
func (d *decider) void(ctx context.Context, u *unstructured.Unstructured, a approval, reason string) error {
	return d.write(ctx, u, a, map[string]any{"state": "void", "reason": reason}, forgeComment{}, "approval-void")
}

// write sets the status with a compare-and-set on resourceVersion and audits the result.
func (d *decider) write(ctx context.Context, u *unstructured.Unstructured, a approval, st map[string]any, c forgeComment, action string) error {
	st["contentHash"] = a.ContentHash
	st["decidedAt"] = d.now().Format(time.RFC3339)
	if c.ID != 0 {
		st["decidedBy"], st["decidedVia"], st["commentId"] = c.Login, "forge", c.ID
	}
	obj := u.DeepCopy()
	obj.Object["status"] = st
	e := auditEntry{Who: auditWho{ForgeUser: c.Login}, Action: action, Agent: a.Agent, Request: approvalAudit(a, c.ID)}
	if r, ok := st["reason"].(string); ok {
		e.Reason = r
	}
	if c.ID != 0 {
		e.Grant = "repo-write"
	}
	if err := d.store.UpdateApprovalStatus(ctx, obj); err != nil {
		if apierrors.IsConflict(err) {
			e.Result, e.Reason = "denied", "another decision was first"
			audit(e)
			return nil
		}
		e.Result, e.Reason = "error", err.Error()
		audit(e)
		return err
	}
	e.Result = "ok"
	audit(e)
	return nil
}

// ---- HTTP ----

// internalDecision: GET /internal/approvals/{agent}/{id}, for the forge sidecar. Answers state, scope, decider and the
// decided content hash, nothing else. The caller must send X-Approval-Secret, the per-request secret whose SHA-256 is
// spec.secretHash: only the sidecar that made the request has it. The listener is reachable from agent pods only
// (NetworkPolicy). Before the operator has created the object, any caller gets "pending" (nothing to learn).
func internalDecision(store approvalStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent, id := r.PathValue("agent"), r.PathValue("id")
		if !agentNameRe.MatchString(agent) || !approvalIDRe.MatchString(id) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		u, err := store.GetApproval(r.Context(), agent+"-"+id)
		if apierrors.IsNotFound(err) {
			writeJSON(w, http.StatusOK, map[string]string{"state": "pending"}) // not created yet (operator lag)
			return
		}
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "cannot read approval"})
			return
		}
		a := fromApprovalObject(u)
		// bound to the caller: only the sidecar that made the request knows the secret behind spec.secretHash
		if !callerOwns(a, agent, r.Header.Get("X-Approval-Secret")) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"state": a.State, "scope": a.Scope, "decidedBy": a.DecidedBy,
			"contentHash": a.DecidedHash})
	})
}

// internalVoid: POST /internal/approvals/{agent}/{id}/void {"reason"}, for the sidecar that made the request (same
// secret as internalDecision): the job ended or a newer request superseded it. Only a pending request changes.
func internalVoid(store approvalStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent, id := r.PathValue("agent"), r.PathValue("id")
		if !agentNameRe.MatchString(agent) || !approvalIDRe.MatchString(id) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
		reason := "superseded"
		switch body.Reason {
		case "the job ended", "superseded by a newer request":
			reason = body.Reason
		}
		u, err := store.GetApproval(r.Context(), agent+"-"+id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		a := fromApprovalObject(u)
		if !callerOwns(a, agent, r.Header.Get("X-Approval-Secret")) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		if a.State != "pending" {
			writeJSON(w, http.StatusOK, map[string]string{"state": a.State})
			return
		}
		obj := u.DeepCopy()
		obj.Object["status"] = map[string]any{"state": "void", "reason": reason, "contentHash": a.ContentHash,
			"decidedAt": time.Now().UTC().Format(time.RFC3339)}
		e := auditEntry{Action: "approval-void", Agent: a.Agent, Reason: reason, Request: approvalAudit(a, 0)}
		if err := store.UpdateApprovalStatus(r.Context(), obj); err != nil {
			e.Result = "error"
			if apierrors.IsConflict(err) {
				e.Result, e.Reason = "denied", "a decision was first"
			}
			audit(e)
			writeJSON(w, http.StatusConflict, map[string]string{"error": "changed meanwhile"})
			return
		}
		e.Result = "ok"
		audit(e)
		writeJSON(w, http.StatusOK, map[string]string{"state": "void"})
	})
}

// callerOwns: the caller is the sidecar that made the request (it knows the secret behind spec.secretHash).
func callerOwns(a approval, agent, sec string) bool {
	sum := sha256.Sum256([]byte(sec))
	return a.Agent == agent && secretRe.MatchString(a.secretHash) && len(sec) == 64 &&
		subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(a.secretHash)) == 1
}

// approvalReader serves GET /api/approvals and GET /api/approvals/{name}: requests of the repositories where the
// caller has write access (checked on the forge), or all for the admin group.
type approvalReader struct {
	store approvalStore
	w     *writer
}

func (ar *approvalReader) visible(ctx context.Context, id *identity) func(repo, forge string) bool {
	if ar.w.isAdmin(id) {
		return func(string, string) bool { return true }
	}
	cache := map[string]bool{}
	return func(repo, forge string) bool {
		key := forge + "\x00" + repo // the same owner/name on two forges are two projects
		if v, ok := cache[key]; ok {
			return v
		}
		p, err := ar.w.repoPerm(ctx, id, forge, repo)
		cache[key] = err == nil && p >= permWrite
		return cache[key]
	}
}

func (ar *approvalReader) list(rw http.ResponseWriter, r *http.Request) {
	id := r.Context().Value(ctxIdentity{}).(*identity)
	q := r.URL.Query()
	state, agent, repo := q.Get("state"), q.Get("agent"), q.Get("project")
	switch state {
	case "", "all", "pending", "allowed", "denied", "void", "decided":
	default:
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "state: pending, decided, allowed, denied, void or all"})
		return
	}
	objs, err := ar.store.ListApprovals(r.Context())
	if err != nil {
		log.Printf("list approvals: %v", err)
		writeJSON(rw, http.StatusBadGateway, map[string]string{"error": "cannot read approvals"})
		return
	}
	can := ar.visible(r.Context(), id)
	out := []approval{}
	for _, u := range objs {
		a := fromApprovalObject(u)
		if (agent != "" && a.Agent != agent) || (repo != "" && a.Repo != repo) {
			continue
		}
		if state != "" && state != "all" && state != a.State && !(state == "decided" && a.State != "pending") {
			continue
		}
		if !can(a.Repo, a.Forge) {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	writeJSON(rw, http.StatusOK, map[string]any{"approvals": out})
}

func (ar *approvalReader) get(rw http.ResponseWriter, r *http.Request) {
	id := r.Context().Value(ctxIdentity{}).(*identity)
	name := r.PathValue("name")
	if len(name) > 100 || !agentNameRe.MatchString(name) {
		writeJSON(rw, http.StatusBadRequest, map[string]string{"error": "bad name"})
		return
	}
	u, err := ar.store.GetApproval(r.Context(), name)
	if err != nil {
		code := http.StatusBadGateway
		if apierrors.IsNotFound(err) {
			code = http.StatusNotFound
		}
		writeJSON(rw, code, map[string]string{"error": "not found"})
		return
	}
	a := fromApprovalObject(u)
	if !ar.visible(r.Context(), id)(a.Repo, a.Forge) {
		writeJSON(rw, http.StatusNotFound, map[string]string{"error": "not found"}) // do not reveal other projects
		return
	}
	writeJSON(rw, http.StatusOK, a)
}
