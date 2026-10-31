package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// Writes to Agent objects (#57 step 3). Rule decided on #57 ("Default ok", 2026-10-31):
//   - pause, resume, maxConcurrent, scaleToZero: WRITE (or admin) on the agent's repository, checked live on the forge
//   - members of the admin group (agentdev-admins) may always pause any agent (emergency stop), without the forge
//   - create and delete: ADMIN on the repository, or the admin group
//   - every attempt is audit-logged: AGENTDEV_AUDIT {json}
// When the forge cannot answer, the change is refused (fail closed).

var (
	agentNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	repoRe      = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// agentState is the writable view of an Agent, used in the audit log and responses.
type agentState struct {
	Repo          string `json:"repo"`
	Forge         string `json:"forge"`
	ClaudeSecret  string `json:"claudeSecret,omitempty"`
	MaxConcurrent int32  `json:"maxConcurrent"`
	ScaleToZero   bool   `json:"scaleToZero"`
	Paused        bool   `json:"paused"`
}

// agentStore reads and writes Agent objects.
type agentStore interface {
	Get(ctx context.Context, name string) (*unstructured.Unstructured, error)
	Create(ctx context.Context, u *unstructured.Unstructured) (*unstructured.Unstructured, error)
	Patch(ctx context.Context, name string, patch []byte) (*unstructured.Unstructured, error)
	Delete(ctx context.Context, name string) error
}

func (k *kubeAgents) Get(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	return k.d.Resource(agentsGVR).Namespace(k.ns).Get(ctx, name, metav1.GetOptions{})
}

func (k *kubeAgents) Create(ctx context.Context, u *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return k.d.Resource(agentsGVR).Namespace(k.ns).Create(ctx, u, metav1.CreateOptions{})
}

func (k *kubeAgents) Patch(ctx context.Context, name string, patch []byte) (*unstructured.Unstructured, error) {
	return k.d.Resource(agentsGVR).Namespace(k.ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
}

func (k *kubeAgents) Delete(ctx context.Context, name string) error {
	return k.d.Resource(agentsGVR).Namespace(k.ns).Delete(ctx, name, metav1.DeleteOptions{})
}

func stateOf(u *unstructured.Unstructured) *agentState {
	if u == nil {
		return nil
	}
	a := fromAgentObject(*u)
	cs, _, _ := unstructured.NestedString(u.Object, "spec", "claudeSecret")
	s := &agentState{Forge: a.Forge, ClaudeSecret: cs, MaxConcurrent: a.MaxConcurrent, ScaleToZero: a.ScaleToZero, Paused: a.Paused}
	if len(a.Repos) > 0 {
		s.Repo = a.Repos[0]
	}
	if s.MaxConcurrent == 0 {
		s.MaxConcurrent = 2 // CRD default
	}
	return s
}

// auditEntry is one AGENTDEV_AUDIT line.
type auditEntry struct {
	Time      string      `json:"time"`
	Who       auditWho    `json:"who"`
	Action    string      `json:"action"` // patch | create | delete
	Agent     string      `json:"agent"`
	Result    string      `json:"result"` // ok | denied | invalid | error
	Reason    string      `json:"reason,omitempty"`
	Request   any         `json:"request,omitempty"`
	Before    *agentState `json:"before"`
	After     *agentState `json:"after"`
	Grant     string      `json:"grant,omitempty"` // why it was allowed: repo-write, repo-admin, admin-group
	KeepState *bool       `json:"keepState,omitempty"`
}

type auditWho struct {
	Subject   string   `json:"sub"`
	Username  string   `json:"username"`
	ForgeUser string   `json:"forgeUser"`
	Groups    []string `json:"groups,omitempty"`
}

// auditOut receives the audit lines (stdout through the standard logger by default; tests replace it).
var auditOut io.Writer = log.Writer()

var now = func() time.Time { return time.Now().UTC() }

func audit(e auditEntry) {
	e.Time = now().Format(time.RFC3339)
	b, err := json.Marshal(e)
	if err != nil {
		b = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
	}
	fmt.Fprintf(auditOut, "AGENTDEV_AUDIT %s\n", b)
}

// writer holds what the write handlers need.
type writer struct {
	store      agentStore
	perms      permChecker
	adminGroup string
}

func (w *writer) who(id *identity) auditWho {
	return auditWho{Subject: id.Subject, Username: id.Username, ForgeUser: forgeUserOf(id), Groups: id.Groups}
}

func (w *writer) isAdmin(id *identity) bool {
	return w.adminGroup != "" && slices.Contains(id.Groups, w.adminGroup)
}

// repoPerm asks the forge. The error is already wrapped for the response (fail closed).
func (w *writer) repoPerm(ctx context.Context, id *identity, forge, repo string) (perm, error) {
	user := forgeUserOf(id)
	if user == "" {
		return permNone, nil
	}
	if w.perms == nil {
		return permNone, fmt.Errorf("%w: no permission checker configured", errForgeUnavailable)
	}
	return w.perms.Permission(ctx, forge, repo, user)
}

type httpErr struct {
	code   int
	result string // audit result
	msg    string
}

func (e *httpErr) Error() string { return e.msg }

func denied(msg string) *httpErr  { return &httpErr{http.StatusForbidden, "denied", msg} }
func invalid(msg string) *httpErr { return &httpErr{http.StatusBadRequest, "invalid", msg} }

// authorize: need is the forge level that allows the action; adminOK says whether the admin group alone suffices.
func (w *writer) authorize(ctx context.Context, id *identity, forge, repo string, need perm, adminOK bool) (string, error) {
	if adminOK && w.isAdmin(id) {
		return "admin-group", nil
	}
	p, err := w.repoPerm(ctx, id, forge, repo)
	if err != nil {
		log.Printf("forge permission check %s on %s: %v", forgeUserOf(id), repo, err)
		return "", &httpErr{http.StatusServiceUnavailable, "error", "cannot check your permission on the forge; try again later"}
	}
	if p < need {
		return "", denied(fmt.Sprintf("you need %s access to %s (you have %s)", need, repo, p))
	}
	return "repo-" + need.String(), nil
}

// decodeObject reads a JSON object (max 16 KiB) as raw fields.
func decodeObject(r *http.Request) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, invalid("body must be a JSON object")
	}
	if dec.More() {
		return nil, invalid("body must be one JSON object")
	}
	return m, nil
}

func boolField(m map[string]json.RawMessage, k string) (bool, error) {
	var b bool
	if err := json.Unmarshal(m[k], &b); err != nil || bytes.Equal(m[k], []byte("null")) {
		return false, invalid(k + " must be true or false")
	}
	return b, nil
}

func maxConcField(m map[string]json.RawMessage) (int32, error) {
	var n int32
	if err := json.Unmarshal(m["maxConcurrent"], &n); err != nil || n < 1 || n > 8 {
		return 0, invalid("maxConcurrent must be an integer from 1 to 8")
	}
	return n, nil
}

// patchSpec validates a JSON merge patch body: only paused, maxConcurrent and scaleToZero; null is not allowed.
func patchSpec(m map[string]json.RawMessage) (map[string]any, error) {
	if len(m) == 0 {
		return nil, invalid("nothing to change")
	}
	spec := map[string]any{}
	for k := range m {
		switch k {
		case "paused", "scaleToZero":
			b, err := boolField(m, k)
			if err != nil {
				return nil, err
			}
			spec[k] = b
		case "maxConcurrent":
			n, err := maxConcField(m)
			if err != nil {
				return nil, err
			}
			spec[k] = n
		default:
			return nil, invalid(fmt.Sprintf("field %q cannot be changed (allowed: paused, maxConcurrent, scaleToZero)", k))
		}
	}
	return spec, nil
}

func (w *writer) fail(rw http.ResponseWriter, e auditEntry, err error) {
	var he *httpErr
	if !errors.As(err, &he) {
		log.Printf("%s agent %s: %v", e.Action, e.Agent, err)
		he = &httpErr{http.StatusBadGateway, "error", "cannot change the agent"}
		if apierrors.IsConflict(err) {
			he = &httpErr{http.StatusConflict, "error", "the agent changed at the same time; try again"}
		}
	}
	e.Result, e.Reason = he.result, he.msg
	audit(e)
	writeJSON(rw, he.code, map[string]string{"error": he.msg})
}

func (w *writer) getManaged(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	if !agentNameRe.MatchString(name) || len(name) > 40 {
		return nil, &httpErr{http.StatusNotFound, "invalid", "no such agent"}
	}
	u, err := w.store.Get(ctx, name)
	if apierrors.IsNotFound(err) {
		return nil, &httpErr{http.StatusNotFound, "invalid", "no such agent (or it has no Agent object yet)"}
	}
	return u, err
}

// PATCH /api/agents/{name}: application/merge-patch+json (or application/json) with paused, maxConcurrent, scaleToZero.
func (w *writer) patch(rw http.ResponseWriter, r *http.Request) {
	id := r.Context().Value(ctxIdentity{}).(*identity)
	name := r.PathValue("name")
	e := auditEntry{Who: w.who(id), Action: "patch", Agent: name}
	m, err := decodeObject(r)
	if err != nil {
		w.fail(rw, e, err)
		return
	}
	spec, err := patchSpec(m)
	e.Request = spec
	if err != nil {
		e.Request = m
		w.fail(rw, e, err)
		return
	}
	cur, err := w.getManaged(r.Context(), name)
	if err != nil {
		w.fail(rw, e, err)
		return
	}
	e.Before = stateOf(cur)
	pauseOnly := len(spec) == 1 && spec["paused"] == true
	grant, err := w.authorize(r.Context(), id, e.Before.Forge, e.Before.Repo, permWrite, pauseOnly)
	if err != nil {
		w.fail(rw, e, err)
		return
	}
	e.Grant = grant
	// resourceVersion makes the patch apply only to the object we checked (no change in between).
	body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": cur.GetResourceVersion()}, "spec": spec})
	out, err := w.store.Patch(r.Context(), name, body)
	if err != nil {
		w.fail(rw, e, err)
		return
	}
	e.After, e.Result = stateOf(out), "ok"
	audit(e)
	writeJSON(rw, http.StatusOK, fromAgentObject(*out))
}

// POST /api/agents: {name, repo, forge?, maxConcurrent?, scaleToZero?, claudeSecret?}. The Secrets agent-<name> and
// the Claude Secret must exist already; the API does not read Secrets (no RBAC on them).
func (w *writer) create(rw http.ResponseWriter, r *http.Request) {
	id := r.Context().Value(ctxIdentity{}).(*identity)
	e := auditEntry{Who: w.who(id), Action: "create"}
	m, err := decodeObject(r)
	if err != nil {
		w.fail(rw, e, err)
		return
	}
	e.Request = m
	var name string
	if json.Unmarshal(m["name"], &name) != nil || !agentNameRe.MatchString(name) || len(name) > 40 {
		w.fail(rw, e, invalid("name must be a DNS label (a-z, 0-9, -) of at most 40 characters"))
		return
	}
	e.Agent = name
	st := agentState{Forge: "gitea", MaxConcurrent: 2, ClaudeSecret: "claude-shared"}
	for k := range m {
		switch k {
		case "name":
		case "repo":
			if json.Unmarshal(m[k], &st.Repo) != nil || !repoRe.MatchString(st.Repo) {
				w.fail(rw, e, invalid("repo must be owner/name"))
				return
			}
		case "forge":
			if json.Unmarshal(m[k], &st.Forge) != nil || (st.Forge != "gitea" && st.Forge != "github") {
				w.fail(rw, e, invalid("forge must be gitea or github"))
				return
			}
		case "claudeSecret":
			if json.Unmarshal(m[k], &st.ClaudeSecret) != nil || (st.ClaudeSecret != "claude-shared" && st.ClaudeSecret != "claude-"+name) {
				w.fail(rw, e, invalid("claudeSecret must be claude-shared or claude-"+name))
				return
			}
		case "maxConcurrent":
			if st.MaxConcurrent, err = maxConcField(m); err != nil {
				w.fail(rw, e, err)
				return
			}
		case "scaleToZero":
			if st.ScaleToZero, err = boolField(m, k); err != nil {
				w.fail(rw, e, err)
				return
			}
		default:
			w.fail(rw, e, invalid(fmt.Sprintf("unknown field %q", k)))
			return
		}
	}
	if st.Repo == "" {
		w.fail(rw, e, invalid("repo is required"))
		return
	}
	grant, err := w.authorize(r.Context(), id, st.Forge, st.Repo, permAdmin, true)
	if err != nil {
		w.fail(rw, e, err)
		return
	}
	e.Grant = grant
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentdev.dev/v1alpha1", "kind": "Agent",
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{"repo": st.Repo, "forge": st.Forge, "forgeSecret": "agent-" + name,
			"claudeSecret": st.ClaudeSecret, "maxConcurrent": int64(st.MaxConcurrent), "scaleToZero": st.ScaleToZero},
	}}
	out, err := w.store.Create(r.Context(), u)
	if apierrors.IsAlreadyExists(err) {
		err = &httpErr{http.StatusConflict, "invalid", "an agent with this name exists"}
	}
	if err != nil {
		w.fail(rw, e, err)
		return
	}
	e.After, e.Result = stateOf(out), "ok"
	audit(e)
	writeJSON(rw, http.StatusCreated, fromAgentObject(*out))
}

// DELETE /api/agents/{name}[?keepState=true]. The volumes (state, claude) always stay: the operator sets the
// StatefulSet to Retain them, and the API has no RBAC on PersistentVolumeClaims. keepState=false is refused.
func (w *writer) delete(rw http.ResponseWriter, r *http.Request) {
	id := r.Context().Value(ctxIdentity{}).(*identity)
	name := r.PathValue("name")
	keep := true
	e := auditEntry{Who: w.who(id), Action: "delete", Agent: name, KeepState: &keep}
	switch r.URL.Query().Get("keepState") {
	case "", "true":
	default:
		w.fail(rw, e, invalid("keepState=false is not supported: the volumes stay; an administrator removes them with kubectl"))
		return
	}
	cur, err := w.getManaged(r.Context(), name)
	if err != nil {
		w.fail(rw, e, err)
		return
	}
	e.Before = stateOf(cur)
	grant, err := w.authorize(r.Context(), id, e.Before.Forge, e.Before.Repo, permAdmin, true)
	if err != nil {
		w.fail(rw, e, err)
		return
	}
	e.Grant = grant
	if err := w.store.Delete(r.Context(), name); err != nil && !apierrors.IsNotFound(err) {
		w.fail(rw, e, err)
		return
	}
	e.Result = "ok"
	audit(e)
	rw.WriteHeader(http.StatusNoContent)
}
