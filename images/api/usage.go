package main

// Subscription usage limits (#93, decisions on #92). The operator pauses new runs of the agents on a Claude
// subscription when its 5-hour or weekly window passes a threshold (images/operator/usage.go). The thresholds live in
// the ConfigMap agentdev-usage-settings, written only here:
//
//   - an agentdev admin (ADMIN_GROUP) sets the global thresholds (defaults 80 % 5-hour, 90 % weekly);
//   - a user with write access to a project sets lower thresholds for that project. A value above the global one is
//     refused here, and the operator clamps it in any case, so a project can never raise the limit.
//
// Every attempt is written with audit() (Action "set-limits"), like the Agent writes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	usageCM         = "agentdev-usage"          // written by the operator: state and pauses (read only here)
	usageSettingsCM = "agentdev-usage-settings" // thresholds, written here
	defaultFiveHour = 80
	defaultWeekly   = 90
)

// thresholds in percent (1..100); 0 = not set (project: use the global value).
type thresholds struct {
	FiveHour int `json:"fiveHour,omitempty"`
	Weekly   int `json:"weekly,omitempty"`
}

// usageSettings is settings.json. The same format as UsageSettings in the operator.
type usageSettings struct {
	Global   thresholds            `json:"global"`
	Projects map[string]thresholds `json:"projects,omitempty"`
}

func validPct(p int) bool { return p >= 1 && p <= 100 }

// global returns the global thresholds with the defaults filled in.
func (s usageSettings) global() thresholds {
	t := thresholds{FiveHour: defaultFiveHour, Weekly: defaultWeekly}
	if validPct(s.Global.FiveHour) {
		t.FiveHour = s.Global.FiveHour
	}
	if validPct(s.Global.Weekly) {
		t.Weekly = s.Global.Weekly
	}
	return t
}

// effective is the lower of the global and the project value (same rule as the operator).
func (s usageSettings) effective(repo string) thresholds {
	t := s.global()
	if p, ok := s.Projects[repo]; ok {
		if validPct(p.FiveHour) && p.FiveHour < t.FiveHour {
			t.FiveHour = p.FiveHour
		}
		if validPct(p.Weekly) && p.Weekly < t.Weekly {
			t.Weekly = p.Weekly
		}
	}
	return t
}

// usageStore reads and writes the two ConfigMaps.
type usageStore interface {
	Settings(ctx context.Context) (usageSettings, string, error) // settings, resourceVersion ("" = not created yet)
	SaveSettings(ctx context.Context, s usageSettings, resourceVersion string) error
	State(ctx context.Context) (map[string]json.RawMessage, error) // the operator's state.json and pauses.json
}

func (k *kubeAgents) Settings(ctx context.Context) (usageSettings, string, error) {
	var s usageSettings
	cm, err := k.c.CoreV1().ConfigMaps(k.ns).Get(ctx, usageSettingsCM, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return s, "", nil
	}
	if err != nil {
		return s, "", err
	}
	if raw := cm.Data["settings.json"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			return usageSettings{}, versionOf(cm), fmt.Errorf("settings.json: %w", err)
		}
	}
	return s, versionOf(cm), nil
}

// versionOf: the resourceVersion, or a marker for "exists" (fake clients set none; "" means not created yet).
func versionOf(cm *corev1.ConfigMap) string {
	if cm.ResourceVersion == "" {
		return existsNoVersion
	}
	return cm.ResourceVersion
}

const existsNoVersion = "exists"

func (k *kubeAgents) SaveSettings(ctx context.Context, s usageSettings, rv string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: usageSettingsCM, Namespace: k.ns},
		Data: map[string]string{"settings.json": string(b)}}
	if rv != existsNoVersion {
		cm.ResourceVersion = rv
	}
	if rv == "" {
		_, err = k.c.CoreV1().ConfigMaps(k.ns).Create(ctx, cm, metav1.CreateOptions{})
	} else {
		// resourceVersion: a change made at the same time gives a conflict, not a lost update
		_, err = k.c.CoreV1().ConfigMaps(k.ns).Update(ctx, cm, metav1.UpdateOptions{})
	}
	return err
}

func (k *kubeAgents) State(ctx context.Context) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	cm, err := k.c.CoreV1().ConfigMaps(k.ns).Get(ctx, usageCM, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"state.json", "pauses.json"} {
		if v := cm.Data[key]; v != "" && json.Valid([]byte(v)) {
			out[key[:len(key)-5]] = json.RawMessage(v)
		}
	}
	return out, nil
}

// usageView is GET /api/usage.
type usageView struct {
	Global   thresholds            `json:"global"`             // with defaults filled in
	Projects map[string]thresholds `json:"projects,omitempty"` // as set (lower values only)
	// Effective per agent project, so the UI need not repeat the clamp rule.
	Effective map[string]thresholds `json:"effective"`
	State     json.RawMessage       `json:"state,omitempty"`  // per Claude Secret: windows, tokens (operator)
	Pauses    json.RawMessage       `json:"pauses,omitempty"` // per agent: until, reason (operator)
}

// GET /api/usage: every signed-in user (no secrets; the Secret names are already in /api/agents' data model).
func (w *writer) getUsage(rw http.ResponseWriter, r *http.Request) {
	s, _, err := w.usage.Settings(r.Context())
	if err != nil {
		writeJSON(rw, http.StatusBadGateway, map[string]string{"error": "cannot read the usage settings"})
		return
	}
	v := usageView{Global: s.global(), Projects: s.Projects, Effective: map[string]thresholds{}}
	if agents, ok := w.store.(agentLister); ok {
		if list, err := agents.List(r.Context()); err == nil {
			for _, a := range list {
				for _, repo := range a.Repos {
					v.Effective[repo] = s.effective(repo)
				}
			}
		}
	}
	if st, err := w.usage.State(r.Context()); err == nil {
		v.State, v.Pauses = st["state"], st["pauses"]
	}
	writeJSON(rw, http.StatusOK, v)
}

// limitsBody reads {fiveHour, weekly}. Each is an integer 1..100, or null / 0 to clear (project only).
// set lists the keys present in the body.
func limitsBody(r *http.Request, allowClear bool) (t thresholds, set map[string]bool, err error) {
	m, err := decodeObject(r)
	if err != nil {
		return thresholds{}, nil, err
	}
	set = map[string]bool{}
	for k, raw := range m {
		var p *int
		if err := json.Unmarshal(raw, &p); err != nil {
			return t, nil, invalid(k + " must be an integer from 1 to 100")
		}
		n := 0
		if p != nil {
			n = *p
		}
		if n == 0 && !allowClear {
			return t, nil, invalid(k + " must be an integer from 1 to 100")
		}
		if n != 0 && !validPct(n) {
			return t, nil, invalid(k + " must be an integer from 1 to 100")
		}
		switch k {
		case "fiveHour":
			t.FiveHour, set[k] = n, true
		case "weekly":
			t.Weekly, set[k] = n, true
		default:
			return t, nil, invalid(fmt.Sprintf("field %q is not known (allowed: fiveHour, weekly)", k))
		}
	}
	if len(m) == 0 {
		return t, nil, invalid("nothing to change")
	}
	return t, set, nil
}

type limitsAudit struct {
	Scope   string      `json:"scope"`             // global | project
	Project string      `json:"project,omitempty"` // owner/name
	Before  *thresholds `json:"before"`
	After   *thresholds `json:"after,omitempty"`
}

func (w *writer) saveUsage(ctx context.Context, s usageSettings, rv string) error {
	err := w.usage.SaveSettings(ctx, s, rv)
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		return &httpErr{http.StatusConflict, "error", "the limits changed at the same time; try again"}
	}
	if err != nil {
		return fmt.Errorf("save usage settings: %w", err)
	}
	return nil
}

// PUT /api/usage/global: agentdev admins only.
func (w *writer) putGlobal(rw http.ResponseWriter, r *http.Request) {
	id := r.Context().Value(ctxIdentity{}).(*identity)
	la := &limitsAudit{Scope: "global"}
	e := auditEntry{Who: w.who(id), Action: "set-limits", Request: la}
	if !w.isAdmin(id) {
		w.fail(rw, e, denied("only agentdev administrators can change the global usage limits"))
		return
	}
	e.Grant = "admin-group"
	t, _, err := limitsBody(r, false)
	if err != nil {
		w.fail(rw, e, err)
		return
	}
	s, rv, err := w.usage.Settings(r.Context())
	if err != nil {
		w.fail(rw, e, err)
		return
	}
	before := s.global()
	la.Before = &before
	if t.FiveHour != 0 {
		s.Global.FiveHour = t.FiveHour
	}
	if t.Weekly != 0 {
		s.Global.Weekly = t.Weekly
	}
	if err := w.saveUsage(r.Context(), s, rv); err != nil {
		w.fail(rw, e, err)
		return
	}
	after := s.global()
	la.After, e.Result = &after, "ok"
	audit(e)
	writeJSON(rw, http.StatusOK, after)
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}/[A-Za-z0-9._-]{1,100}$`)

// forgeOfRepo returns the forge of the agents on repo; a project without an agent is not known here.
func (w *writer) forgeOfRepo(ctx context.Context, repo string) (string, error) {
	agents, ok := w.store.(agentLister)
	if !ok {
		return "", errors.New("agent store cannot list")
	}
	list, err := agents.List(ctx)
	if err != nil {
		return "", err
	}
	for _, a := range list {
		for _, r := range a.Repos {
			if r == repo {
				if a.Forge == "" {
					return "gitea", nil
				}
				return a.Forge, nil
			}
		}
	}
	return "", &httpErr{http.StatusNotFound, "invalid", "no agent works on this project"}
}

// PUT /api/usage/projects/{owner}/{repo}: a user with write access to the project (or an agentdev admin). Values
// must not be above the global ones; 0 or null clears the project value.
func (w *writer) putProject(rw http.ResponseWriter, r *http.Request) {
	id := r.Context().Value(ctxIdentity{}).(*identity)
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	la := &limitsAudit{Scope: "project", Project: repo}
	e := auditEntry{Who: w.who(id), Action: "set-limits", Request: la}
	if !repoRe.MatchString(repo) {
		la.Project = ""
		w.fail(rw, e, invalid("project must be owner/name"))
		return
	}
	t, set, err := limitsBody(r, true)
	if err != nil {
		w.fail(rw, e, err)
		return
	}
	forge, err := w.forgeOfRepo(r.Context(), repo)
	if err != nil {
		w.fail(rw, e, err)
		return
	}
	grant, err := w.authorize(r.Context(), id, forge, repo, permWrite, true)
	if err != nil {
		w.fail(rw, e, err)
		return
	}
	e.Grant = grant
	s, rv, err := w.usage.Settings(r.Context())
	if err != nil {
		w.fail(rw, e, err)
		return
	}
	g := s.global()
	if t.FiveHour > g.FiveHour || t.Weekly > g.Weekly {
		w.fail(rw, e, invalid(fmt.Sprintf("a project can only lower the limits (global: %d%% 5-hour, %d%% weekly)", g.FiveHour, g.Weekly)))
		return
	}
	cur := s.Projects[repo]
	la.Before = &cur
	next := cur
	if set["fiveHour"] {
		next.FiveHour = t.FiveHour
	}
	if set["weekly"] {
		next.Weekly = t.Weekly
	}
	if s.Projects == nil {
		s.Projects = map[string]thresholds{}
	}
	if next == (thresholds{}) {
		delete(s.Projects, repo)
	} else {
		s.Projects[repo] = next
	}
	if err := w.saveUsage(r.Context(), s, rv); err != nil {
		w.fail(rw, e, err)
		return
	}
	la.After, e.Result = &next, "ok"
	audit(e)
	writeJSON(rw, http.StatusOK, map[string]any{"project": next, "effective": s.effective(repo)})
}
