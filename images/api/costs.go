package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Claude cost in the UI (#196, ADR 0002 U5, owner decision 5a).
//
//	GET /api/costs                    per-job cost of the last 7 days from claude-proxy, only of agents whose
//	                                  repository the caller can read on the forge (admins: all; review of #279)
//	PUT /api/costs/budgets/{agent}    agentdev admins only: the agent's limit in ConfigMap claude-proxy-budgets
//
// The proxy stays the enforcer (D1): the API only writes its configuration. The API's ServiceAccount may patch only
// that ConfigMap (resourceNames). The value is checked here and again in the proxy; each change is audited and
// recorded as a security event.

const proxyBudgetsCM = "claude-proxy-budgets"

var (
	jobIDRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	unitsRe   = regexp.MustCompile(`^[0-9]{1,15}$`)
	costsHTTP = &http.Client{Timeout: 3 * time.Second}
)

// jobCost is one job of GET /status (credproxy jobs.go), as the UI shows it.
type jobCost struct {
	Agent    string           `json:"agent"`
	Job      string           `json:"job"`
	Units    int64            `json:"units"`
	USD      *float64         `json:"usd,omitempty"`
	Tokens   map[string]int64 `json:"tokens"`
	Requests int64            `json:"requests"`
	First    time.Time        `json:"first"`
	Last     time.Time        `json:"last"`
	Stopped  bool             `json:"stopped,omitempty"`
}

// jobsSource returns the jobs from the proxy, or nil when it is not reachable.
var jobsSource = func(ctx context.Context) ([]jobCost, bool) {
	st := fetchProxyStatus(ctx, os.Getenv("CLAUDE_PROXY_STATUS_URL"), costsHTTP)
	if st == nil {
		return nil, false
	}
	return st.Jobs, true
}

// validJobs drops entries with values that do not fit (the proxy is trusted, but its answer still passes a check
// before it reaches the UI).
func validJobs(in []jobCost) []jobCost {
	out := make([]jobCost, 0, len(in))
	for _, j := range in {
		if !agentNameRe.MatchString(j.Agent) || !jobIDRe.MatchString(j.Job) || j.Units < 0 || j.Requests < 0 || (j.USD != nil && *j.USD < 0) {
			continue
		}
		tok := map[string]int64{}
		for k, v := range j.Tokens {
			switch k {
			case "input", "cache_write_5m", "cache_write_1h", "cache_read", "output":
				if v >= 0 {
					tok[k] = v
				}
			}
		}
		j.Tokens = tok
		out = append(out, j)
		if len(out) == 2000 {
			break
		}
	}
	return out
}

// costsAPI serves GET /api/costs. Job ids are issue and PR numbers, so a caller sees only the jobs of agents whose
// repository it can read (the same live forge check as for writes, permRead), unless it is an admin.
type costsAPI struct {
	w      *writer
	agents agentLister
}

func (c *costsAPI) get(rw http.ResponseWriter, r *http.Request) {
	jobs, ok := jobsSource(r.Context())
	if !ok {
		writeJSON(rw, http.StatusOK, map[string]any{"available": false, "jobs": []jobCost{}})
		return
	}
	jobs = validJobs(jobs)
	if c.w == nil || !c.w.isAdmin(r.Context().Value(ctxIdentity{}).(*identity)) {
		jobs = c.visible(r.Context(), jobs)
	}
	// per-job figures come from the X-Agentdev-Job header the agent sends: a report, not a bill (R110)
	writeJSON(rw, http.StatusOK, map[string]any{"available": true, "unit": "units", "attribution": "agent-reported", "jobs": jobs})
}

// visible keeps the jobs of agents whose repository the caller can read; one forge check per repository and request.
// Fail closed: no writer, an unknown agent or a forge error hides the job.
func (c *costsAPI) visible(ctx context.Context, jobs []jobCost) []jobCost {
	out := []jobCost{}
	if c.w == nil || c.agents == nil {
		return out
	}
	list, err := c.agents.List(ctx)
	if err != nil {
		return out
	}
	repoOf := map[string][2]string{}
	for _, a := range list {
		if len(a.Repos) > 0 {
			f := a.Forge
			if f == "" {
				f = "gitea"
			}
			repoOf[a.Name] = [2]string{f, a.Repos[0]}
		}
	}
	id := ctx.Value(ctxIdentity{}).(*identity)
	can := map[[2]string]bool{}
	for _, j := range jobs {
		fr, ok := repoOf[j.Agent]
		if !ok {
			continue
		}
		allowed, seen := can[fr]
		if !seen {
			p, err := c.w.repoPerm(ctx, id, fr[0], fr[1])
			allowed = err == nil && p >= permRead
			can[fr] = allowed
		}
		if allowed {
			out = append(out, j)
		}
	}
	return out
}

// budgetStore patches one key of the proxy's budgets ConfigMap.
type budgetStore interface {
	SetAgentBudget(ctx context.Context, agent string, units *int64) (before string, err error) // nil = remove the override
}

func (k *kubeAgents) SetAgentBudget(ctx context.Context, agent string, units *int64) (string, error) {
	cm, err := k.c.CoreV1().ConfigMaps(k.ns).Get(ctx, proxyBudgetsCM, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	before := cm.Data[agent]
	var v any // JSON merge patch: null removes the key
	if units != nil {
		v = strconv.FormatInt(*units, 10)
	}
	patch, _ := json.Marshal(map[string]any{"data": map[string]any{agent: v}})
	_, err = k.c.CoreV1().ConfigMaps(k.ns).Patch(ctx, proxyBudgetsCM, types.MergePatchType, patch, metav1.PatchOptions{})
	return before, err
}

type budgetAudit struct {
	Units  *int64 `json:"units"` // nil = back to the default CLAUDE_BUDGET_UNITS
	Before string `json:"before,omitempty"`
}

// putAgentBudget: PUT /api/costs/budgets/{name} {"units": N} or {"units": null}. agentdev admins only.
func (w *writer) putAgentBudget(rw http.ResponseWriter, r *http.Request) {
	id := r.Context().Value(ctxIdentity{}).(*identity)
	name := r.PathValue("name")
	ba := &budgetAudit{}
	e := auditEntry{Who: w.who(id), Action: "set-budget", Agent: name, Request: ba}
	if !agentNameRe.MatchString(name) || len(name) > 40 {
		w.fail(rw, e, invalid("bad agent name"))
		return
	}
	if !w.isAdmin(id) {
		w.fail(rw, e, denied("only agentdev administrators can change Claude budgets"))
		return
	}
	e.Grant = "admin-group"
	var body struct {
		Units json.RawMessage `json:"units"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		w.fail(rw, e, invalid(`body must be {"units": <non-negative integer or null>}`))
		return
	}
	if raw := string(body.Units); raw != "null" && raw != "" {
		if !unitsRe.MatchString(raw) { // the same rule as the proxy: a plain non-negative integer
			w.fail(rw, e, invalid("units must be a non-negative integer (0 = no limit) or null (the default)"))
			return
		}
		n, _ := strconv.ParseInt(raw, 10, 64)
		ba.Units = &n
	}
	// setting a value needs the agent (a typo would create a key that silently does nothing); removing an override
	// (null) also works for a deleted agent, to clean up
	if ba.Units != nil && !w.agentExists(r.Context(), name) {
		w.fail(rw, e, &httpErr{http.StatusNotFound, "invalid", "no such agent: " + name})
		return
	}
	bs, ok := w.store.(budgetStore)
	if !ok {
		w.fail(rw, e, errors.New("budget store not available"))
		return
	}
	before, err := bs.SetAgentBudget(r.Context(), name, ba.Units)
	ba.Before = before
	if apierrors.IsNotFound(err) {
		w.fail(rw, e, &httpErr{http.StatusConflict, "error", "claude-proxy-budgets does not exist (is the Claude proxy deployed?)"})
		return
	}
	if err != nil {
		w.fail(rw, e, err)
		return
	}
	e.Result = "ok"
	audit(e)
	if w.events != nil { // a budget change is security relevant (ADR 0002 U5): one event per change
		val := "default"
		if ba.Units != nil {
			val = strconv.FormatInt(*ba.Units, 10)
		}
		ev := securityEvent{Schema: schemaV1, Time: now().UTC(), Agent: name, Source: "agentdev-api", Rule: "budget-changed",
			Severity: "low", Action: "Claude budget set to " + val + " units per 24 h by " + adminName(id) + " (was " + orDefault(before) + ")"}
		h := sha256.Sum256(canonical(ev))
		ev.Hash = hex.EncodeToString(h[:16])
		w.events(r.Context(), ev)
	}
	writeJSON(rw, http.StatusOK, map[string]any{"agent": name, "units": ba.Units})
}

// orDefault shows a stored override: "default" when none, "invalid" when it is not a plain integer (set by hand).
func orDefault(s string) string {
	if s == "" {
		return "default"
	}
	if !unitsRe.MatchString(s) {
		return "invalid"
	}
	return s
}

func adminName(id *identity) string {
	if n := validForgeLogin(id.Username); n != "" {
		return n
	}
	return "an administrator"
}

func (w *writer) agentExists(ctx context.Context, name string) bool {
	_, err := w.store.Get(ctx, name)
	return err == nil
}
