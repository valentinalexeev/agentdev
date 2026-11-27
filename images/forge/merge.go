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
	"sort"
	"strconv"
	"strings"
	"time"
)

// POST /merge?pr=N (localhost API, #578): the agent merges its own pull request, only with MERGE_MODE=gated (the
// Agent's spec.merge.mode; the operator sets it, nothing in the repository or in issue text can). Off (the default):
// no endpoint. The sidecar never trusts the caller: at the moment of the merge it reads every fact from the forge
// with its own credential and merges only when all of these hold:
//
//  1. the PR is open, not a draft, its head is a branch agent/<FORGE_USER>/* of this repository, and its base is the
//     repository's default branch or another branch of this agent;
//  2. an owner (a login of MERGE_APPROVERS, the Agent's spec.merge.approvers; account type User, not this agent,
//     permission write, maintain or admin) approved the exact head commit: their latest deciding review is APPROVED
//     with commit_id equal to the head. A dismissed approval, an approval of another commit or an approval by anyone
//     else never counts. No reviewer's latest review requests changes;
//  3. every check run and commit status on the head commit passed, and the forge says the PR is mergeable now
//     (mergeable_state clean or has_hooks: required checks, required reviews and "up to date" as the branch rules say);
//  4. no approval request of this agent on the PR is open (#67);
//  5. a change to a protected path (PROTECTED_PATHS) has an approval of a person for this merge of this head commit,
//     asked through the same approval requests as a protected push (source push, tool merge).
//
// The merge call pins the head (sha): GitHub refuses it when the head moved after the checks. Each merge is an audit
// entry (stdout AGENTDEV_AUDIT, and the API's audit table through its internal listener). A refusal by the policy is a
// security event and HTTP 403; a PR that is not ready yet (checks pending, approval request pending, mergeability
// not computed) is HTTP 409 without an event, so the loop can ask again later.
//
// GitHub only: on Gitea the sidecar cannot tell a person from another agent's account, so MERGE_MODE=gated keeps the
// endpoint off there (a log line at start).

// mergeMethod is fixed: the repository rule is merge commits only (CLAUDE.md). Nothing configures it, so neither the
// agent nor the Agent object can pick squash or rebase.
const mergeMethod = "merge"

// mergeConfig reads MERGE_MODE (off | gated) and MERGE_APPROVERS (comma-separated owner logins; required with gated,
// else the sidecar does not start). A MERGE_METHOD other than merge is refused at start (merge commits only).
func mergeConfig(get func(string, string) string) (mode string, approvers []string, err error) {
	mode = get("MERGE_MODE", "off")
	if mode != "off" && mode != "gated" {
		return "", nil, fmt.Errorf("MERGE_MODE %q: off or gated", mode)
	}
	if mm := get("MERGE_METHOD", mergeMethod); mm != mergeMethod {
		return "", nil, fmt.Errorf("MERGE_METHOD %q: only merge (merge commits) is allowed", mm)
	}
	if mode == "off" {
		return mode, nil, nil
	}
	for _, l := range strings.Split(get("MERGE_APPROVERS", ""), ",") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		if !ghLoginRe.MatchString(l) {
			return "", nil, fmt.Errorf("MERGE_APPROVERS: %q is not a login", l)
		}
		approvers = append(approvers, l)
	}
	if len(approvers) == 0 {
		return "", nil, errors.New("MERGE_MODE=gated needs MERGE_APPROVERS (the owner logins)")
	}
	return mode, approvers, nil
}

// isMergeApprover: login is one of MERGE_APPROVERS (case-insensitive, as GitHub logins are).
func (s *server) isMergeApprover(login string) bool {
	for _, a := range s.c.MergeApprovers {
		if strings.EqualFold(a, login) {
			return true
		}
	}
	return false
}

// mergeOn: the endpoint exists only with MERGE_MODE=gated on GitHub.
func (s *server) mergeOn() bool { return s.c.MergeMode == "gated" && s.driver().kind() == "github" }

// notReady is a PR that may become mergeable without anyone changing the policy facts (HTTP 409, no event).
type notReady struct{ msg string }

func (e *notReady) Error() string { return e.msg }

func waitFor(format string, a ...any) error { return &notReady{fmt.Sprintf(format, a...)} }

func refuseMerge(severity, format string, a ...any) error {
	return refuseAs("merge-refused", severity, format, a...)
}

var prNumRe = regexp.MustCompile(`^[1-9][0-9]{0,15}$`)

type ghUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type ghPull struct {
	State          string `json:"state"`
	Draft          bool   `json:"draft"`
	Merged         bool   `json:"merged"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	User           ghUser `json:"user"`
	Head           struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

type ghReview struct {
	User     ghUser `json:"user"`
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
}

// mergeResult is what the audit entry and the answer to the loop carry.
type mergeResult struct {
	Repo     string   `json:"repo"`
	PR       int64    `json:"pr"`
	SHA      string   `json:"sha"`
	Base     string   `json:"base"`
	Method   string   `json:"method"`
	Approver string   `json:"approver"`
	Checks   []string `json:"checks"`
	Merged   string   `json:"mergeCommit,omitempty"`
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (s *server) merge(w http.ResponseWriter, r *http.Request) {
	pr := r.URL.Query().Get("pr")
	if !prNumRe.MatchString(pr) {
		http.Error(w, "pr: a pull request number", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	res, err := s.mergePR(ctx, r.Header.Get("X-Agentdev-Job"), pr)
	var nr *notReady
	switch {
	case errors.As(err, &nr):
		s.logf("merge of PR #%s waits: %v", pr, err)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		s.logf("merge of PR #%s refused: %v", pr, err)
		s.policyEvent(r, err, "merge PR #"+pr)
		fail(w, err)
		return
	}
	s.logf("merged PR #%s at %s (approved by %s)", pr, res.SHA[:12], res.Approver)
	s.auditMerge(ctx, res)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// mergePR checks the policy of the comment at the top of this file against the forge, then merges with the head
// pinned. job is the loop's X-Agentdev-Job header (informational; used for the protected-path approval).
func (s *server) mergePR(ctx context.Context, job, pr string) (*mergeResult, error) {
	repo := s.c.Repos[0]
	n, _ := strconv.ParseInt(pr, 10, 64)
	base := fmt.Sprintf("/repos/%s/pulls/%s", repo, pr)
	var p ghPull
	if err := s.forgeGet(ctx, base, &p); err != nil {
		return nil, fmt.Errorf("PR #%s: %v", pr, err)
	}
	// 1. the agent's own open PR, from its own branch of this repository
	if p.State != "open" || p.Merged {
		return nil, refuseMerge("low", "merge refused: PR #%s is not open", pr)
	}
	if p.Head.Repo == nil || !strings.EqualFold(p.Head.Repo.FullName, repo) || checkBranch(s.c.ForgeUser, p.Head.Ref) != nil {
		return nil, refuseMerge("high", "merge refused: PR #%s is not from a branch of %s in %s", pr, s.c.ForgeUser, repo)
	}
	if d, ok := s.driver().(*githubDriver); ok {
		login, _, err := d.identity(ctx)
		if err != nil {
			return nil, errors.New("forge identity unavailable")
		}
		if !strings.EqualFold(p.User.Login, login) {
			return nil, refuseMerge("high", "merge refused: PR #%s was not opened by the agent", pr)
		}
	}
	if !shaRe.MatchString(p.Head.SHA) {
		return nil, fmt.Errorf("PR #%s: the forge gave no head commit", pr)
	}
	var rp struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := s.forgeGet(ctx, "/repos/"+repo, &rp); err != nil {
		return nil, fmt.Errorf("repository: %v", err)
	}
	if p.Base.Repo == nil || !strings.EqualFold(p.Base.Repo.FullName, repo) ||
		(p.Base.Ref != rp.DefaultBranch && checkBranch(s.c.ForgeUser, p.Base.Ref) != nil) {
		return nil, refuseMerge("high", "merge refused: PR #%s targets %q, not the default branch or a branch of %s", pr, p.Base.Ref, s.c.ForgeUser)
	}
	if p.Draft {
		return nil, refuseMerge("low", "merge refused: PR #%s is a draft", pr)
	}
	res := &mergeResult{Repo: repo, PR: n, SHA: p.Head.SHA, Base: p.Base.Ref, Method: mergeMethod}

	// 2. a person approved this exact head; nobody requests changes
	approver, err := s.mergeApprover(ctx, base, p.Head.SHA)
	if err != nil {
		return nil, err
	}
	res.Approver = approver

	// 4. no open approval request of the agent on this PR (the merge's own protected-path request aside)
	if s.appr != nil && s.appr.openOn("pr-"+pr) {
		return nil, waitFor("PR #%s has an open approval request of the agent", pr)
	}

	// 3. checks on the head commit, and the forge's own view of the branch rules
	if res.Checks, err = s.mergeChecks(ctx, repo, p.Head.SHA); err != nil {
		return nil, err
	}
	switch {
	case p.Mergeable == nil || p.MergeableState == "unknown" || p.MergeableState == "":
		return nil, waitFor("PR #%s: the forge has not computed mergeability yet", pr)
	case !*p.Mergeable || p.MergeableState == "dirty":
		return nil, waitFor("PR #%s has a merge conflict", pr)
	case p.MergeableState == "behind":
		return nil, waitFor("PR #%s is behind its base branch (the branch rules require it up to date)", pr)
	case p.MergeableState != "clean" && p.MergeableState != "has_hooks":
		// blocked (a required review or check is missing), unstable (a check did not pass), draft
		return nil, waitFor("PR #%s is not mergeable under the branch rules (state %s)", pr, p.MergeableState)
	}

	// 5. protected paths need a person's approval of this merge
	if err := s.mergeProtected(ctx, job, base, pr, p.Head.SHA); err != nil {
		return nil, err
	}

	// 5 (forge). the head is pinned: GitHub refuses the merge when the head moved after the checks above
	var out struct {
		Merged bool   `json:"merged"`
		SHA    string `json:"sha"`
	}
	code, err := s.forgeSend(ctx, http.MethodPut, base+"/merge", map[string]string{"sha": p.Head.SHA, "merge_method": mergeMethod}, &out)
	switch {
	case err != nil:
		return nil, err
	case code == http.StatusConflict:
		return nil, refuseMerge("medium", "merge refused by the forge: the head of PR #%s moved (stale approval)", pr)
	case code == http.StatusMethodNotAllowed || code == http.StatusUnprocessableEntity:
		return nil, waitFor("the forge did not merge PR #%s (HTTP %d: branch rules, or merge commits not allowed)", pr, code)
	case code/100 != 2 || !out.Merged:
		return nil, fmt.Errorf("merge of PR #%s: forge HTTP %d", pr, code)
	}
	if shaRe.MatchString(out.SHA) {
		res.Merged = out.SHA
	}
	return res, nil
}

// mergeApprover returns the login of a person whose latest review approves sha, or refuses (condition 2).
func (s *server) mergeApprover(ctx context.Context, prPath, sha string) (string, error) {
	latest := map[string]ghReview{} // lower-case login -> latest deciding review
	var order []string
	for page := 1; page <= 10; page++ {
		var rs []ghReview
		if err := s.forgeGet(ctx, fmt.Sprintf("%s/reviews?per_page=100&page=%d", prPath, page), &rs); err != nil {
			return "", fmt.Errorf("reviews: %v", err)
		}
		for _, rv := range rs {
			switch rv.State {
			case "APPROVED", "CHANGES_REQUESTED", "DISMISSED": // COMMENTED and PENDING do not change a decision
				k := strings.ToLower(rv.User.Login)
				if _, ok := latest[k]; !ok {
					order = append(order, k)
				}
				latest[k] = rv
			}
		}
		if len(rs) < 100 {
			break
		}
	}
	var stale, bots, others, dismissed []string
	for _, k := range order {
		if latest[k].State == "CHANGES_REQUESTED" {
			return "", refuseMerge("medium", "merge refused: %s requests changes", latest[k].User.Login)
		}
	}
	for _, k := range order {
		rv := latest[k]
		if rv.State == "DISMISSED" && s.isMergeApprover(rv.User.Login) {
			dismissed = append(dismissed, rv.User.Login)
		}
		if rv.State != "APPROVED" {
			continue
		}
		if rv.CommitID != sha {
			stale = append(stale, rv.User.Login)
			continue
		}
		if rv.User.Type != "User" || strings.HasSuffix(k, "[bot]") || strings.EqualFold(rv.User.Login, s.c.ForgeUser) {
			bots = append(bots, rv.User.Login)
			continue
		}
		if !ghLoginRe.MatchString(rv.User.Login) || !s.isMergeApprover(rv.User.Login) {
			others = append(others, rv.User.Login)
			continue
		}
		var perm struct {
			Permission string `json:"permission"`
		}
		if err := s.forgeGet(ctx, fmt.Sprintf("/repos/%s/collaborators/%s/permission", s.c.Repos[0], rv.User.Login), &perm); err != nil {
			return "", fmt.Errorf("permission of %s: %v", rv.User.Login, err)
		}
		switch perm.Permission {
		case "admin", "maintain", "write":
			return rv.User.Login, nil
		}
	}
	switch {
	case len(bots) > 0:
		return "", refuseMerge("high", "merge refused: approval by an agent or a bot account (%s) does not count", strings.Join(bots, ", "))
	case len(stale) > 0 && len(dismissed) == 0:
		return "", refuseMerge("medium", "merge refused: stale approval (%s approved an earlier commit, not %s)", strings.Join(stale, ", "), sha[:12])
	case len(dismissed) > 0:
		return "", refuseMerge("medium", "merge refused: the approval of %s was dismissed", strings.Join(dismissed, ", "))
	case len(others) > 0:
		return "", refuseMerge("medium", "merge refused: approval by %s, not an owner (MERGE_APPROVERS)", strings.Join(others, ", "))
	}
	return "", refuseMerge("medium", "merge refused: no approval of head %s by an owner with write access", sha[:12])
}

// mergeChecks returns "name: conclusion" of every check run and commit status on sha, or refuses or waits when one
// did not pass (condition 3). Fail closed: more than 1000 check runs is a refusal.
func (s *server) mergeChecks(ctx context.Context, repo, sha string) ([]string, error) {
	var out []string
	for page := 1; ; page++ {
		if page > 10 {
			return nil, refuseMerge("medium", "merge refused: more than 1000 check runs on %s", sha[:12])
		}
		var cr struct {
			Total int `json:"total_count"`
			Runs  []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		if err := s.forgeGet(ctx, fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=100&page=%d", repo, sha, page), &cr); err != nil {
			return nil, fmt.Errorf("check runs: %v", err)
		}
		for _, c := range cr.Runs {
			name := clip(c.Name, 100)
			if c.Status != "completed" {
				return nil, waitFor("check %s is %s", name, c.Status)
			}
			switch c.Conclusion {
			case "success", "neutral", "skipped":
				out = append(out, name+": "+c.Conclusion)
			default:
				return nil, refuseMerge("medium", "merge refused: check %s: %s", name, c.Conclusion)
			}
		}
		if len(cr.Runs) < 100 || page*100 >= cr.Total {
			break
		}
	}
	var st struct {
		State    string `json:"state"`
		Total    int    `json:"total_count"`
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := s.forgeGet(ctx, fmt.Sprintf("/repos/%s/commits/%s/status", repo, sha), &st); err != nil {
		return nil, fmt.Errorf("commit status: %v", err)
	}
	for _, c := range st.Statuses {
		name := clip(c.Context, 100)
		switch c.State {
		case "success":
			out = append(out, name+": success")
		case "pending":
			return nil, waitFor("status %s is pending", name)
		default:
			return nil, refuseMerge("medium", "merge refused: status %s: %s", name, c.State)
		}
	}
	// no statuses at all: GitHub answers state "pending"; with statuses the combined state must be success
	if st.Total > 0 && st.State != "success" {
		return nil, waitFor("combined commit status is %s", st.State)
	}
	sort.Strings(out)
	return out, nil
}

// mergeProtected (condition 5): the files of the PR against the protected paths. A hit needs a person's approval of
// this merge of this head (an approval request, source push, tool merge); without approvals it is a refusal.
func (s *server) mergeProtected(ctx context.Context, job, prPath, pr, sha string) error {
	var paths []string
	for page := 1; ; page++ {
		if page > 30 { // GitHub lists at most 3000 files; a longer list is cut, so fail closed
			return refuseMerge("medium", "merge refused: PR #%s changes more than 3000 files", pr)
		}
		var fs []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		}
		if err := s.forgeGet(ctx, fmt.Sprintf("%s/files?per_page=100&page=%d", prPath, page), &fs); err != nil {
			return fmt.Errorf("files: %v", err)
		}
		for _, f := range fs {
			paths = append(paths, f.Filename)
			if f.PreviousFilename != "" {
				paths = append(paths, f.PreviousFilename)
			}
		}
		if len(fs) < 100 {
			break
		}
	}
	hits := protectedHits(paths, s.c.Protected)
	if len(hits) == 0 {
		return nil
	}
	if s.appr == nil {
		return refuseMerge("high", "merge refused: PR #%s changes protected paths (%s); a person has to merge it", pr, strings.Join(hits, ", "))
	}
	return s.appr.mergeApproval(ctx, "pr-"+pr, pr, sha, hits)
}

// mergeApproval asks a person to allow this merge of this head commit (the approval requests of protected pushes,
// #67). It does not hold the call: a pending request is "wait", and the loop asks again on a later pass (the same
// fingerprint reuses the record).
func (a *approvals) mergeApproval(ctx context.Context, job, pr, sha string, paths []string) error {
	key := job + "\x00merge\x00" + sha
	a.mu.Lock()
	ok := a.jobTools[key]
	a.mu.Unlock()
	if ok {
		return nil
	}
	input := fmt.Sprintf("merge PR #%s at %s, changes protected paths: %s", pr, sha, strings.Join(paths, ", "))
	guard := fmt.Sprintf("the merge of PR #%s brings changes to protected paths (%s) into its base branch; the protected paths of this agent are %s (`PROTECTED_PATHS`).",
		pr, a.codeList(paths), a.codeList(a.s.c.Protected))
	r, err := a.request(job, "push", "merge", input, "merge\x00"+job+"\x00"+sha, why{guard: guard})
	if err != nil {
		return refuseMerge("high", "merge refused: PR #%s changes protected paths and no approval could be requested: %v", pr, err)
	}
	switch d := a.check(ctx, r); d.State {
	case "allowed":
		a.mu.Lock()
		delete(a.reqs, r.fp)
		a.jobTools[key] = true
		a.mu.Unlock()
		return nil
	case "pending":
		return waitFor("the merge of PR #%s waits for approval %s (protected paths)", pr, r.ID)
	default:
		return refuseMerge("high", "merge refused: approval %s for protected paths %s", r.ID, d.State)
	}
}

// openOn reports a pending approval request of job other than a merge request (condition 4).
func (a *approvals) openOn(job string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for fp, r := range a.reqs {
		if r.Job == job && r.state == "pending" && !strings.HasPrefix(fp, "merge\x00") {
			return true
		}
	}
	return false
}

// forgeSend sends a JSON body with the agent's credential and returns the HTTP status; out receives a 2xx answer.
func (s *server) forgeSend(ctx context.Context, method, path string, body, out any) (int, error) {
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	auth, err := s.driver().authorization(ctx)
	if err != nil {
		return 0, errors.New("forge credential unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, method, s.driver().apiURL(path, ""), bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, errors.New("forge unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 && out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("forge answer: %v", err)
		}
	}
	return resp.StatusCode, nil
}

const auditPrefix = "AGENTDEV_AUDIT "

// auditMerge writes the agent-merge audit entry: one stdout line (the cluster log keeps it), and the API's audit
// table through its internal listener (best effort: the merge is done; an older API without the endpoint answers 404).
func (s *server) auditMerge(ctx context.Context, res *mergeResult) {
	e := map[string]any{"time": time.Now().UTC().Format(time.RFC3339), "source": "forge", "action": "agent-merge",
		"agent": s.c.AgentName, "result": "ok", "merge": res}
	b, _ := json.Marshal(e)
	eventMu.Lock()
	fmt.Fprintf(eventOut, "%s%s\n", auditPrefix, b)
	eventMu.Unlock()
	if s.c.APIURL == "" {
		return
	}
	body, _ := json.Marshal(res)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	u := fmt.Sprintf("%s/internal/merges/%s", strings.TrimRight(s.c.APIURL, "/"), url.PathEscape(s.c.AgentName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		s.logf("merge audit: API unavailable (the stdout line stays)")
		return
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		s.logf("merge audit: API answered HTTP %d (the stdout line stays)", resp.StatusCode)
	}
}
