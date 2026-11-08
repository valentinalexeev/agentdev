package main

// Project secrets (#202, ADR 0003 D1): set, rotate, list and delete. Write-only: the API never reads or returns a
// value, and its RBAC has no get or list on the value Secrets (only create, update, delete; see manifests/api.yaml.tmpl).
//
//	GET    /api/secrets                 the ProjectSecrets the caller may manage (admin group: all), without values
//	POST   /api/secrets                 {name, scope, project|agent, forge?, mode, broker|env, description?, value}
//	PUT    /api/secrets/{name}/value    {value}   rotate
//	DELETE /api/secrets/{name}          the object and the value
//
// Who: ADMIN on the repository (project maintainers; scope agent: the agent's repository), checked live on the forge,
// or the admin group. Every attempt is audit-logged; the value never is.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var projectSecretsGVR = schema.GroupVersionResource{Group: "agentdev.dev", Version: "v1alpha1", Resource: "projectsecrets"}

const (
	secretValueMax = 8192
	// remoteKeyPrefix: the store key of ProjectSecret <n> is agentdev-ps-<n> (operator projectsecret.go, CEL policy)
	remoteKeyPrefix = "agentdev-ps-"
	forgeAnnotation = "agentdev.dev/forge"
)

var (
	psNameRe      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,48}[a-z0-9])?$`)
	envNameRe     = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	envReservedRe = regexp.MustCompile(`^(AGENTDEV|ANTHROPIC|CLAUDE|FORGE|GIT|GH|GITHUB|SSH|KUBE|KUBERNETES)_`)
	envReserved   = map[string]bool{"PATH": true, "HOME": true, "HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
		"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "NODE_OPTIONS": true, "BASH_ENV": true, "ENV": true}
	upstreamRe = regexp.MustCompile(`^https://[a-z0-9]([-a-z0-9.]*[a-z0-9])?(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$`)
	headerNmRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)
)

// valueStore writes values; it cannot read them.
type valueStore interface {
	SetValue(ctx context.Context, name, value string) error
	DeleteValue(ctx context.Context, name string) error
}

// psStore reads and writes ProjectSecret objects.
type psStore interface {
	ListPS(ctx context.Context) ([]unstructured.Unstructured, error)
	GetPS(ctx context.Context, name string) (*unstructured.Unstructured, error)
	CreatePS(ctx context.Context, u *unstructured.Unstructured) (*unstructured.Unstructured, error)
	DeletePS(ctx context.Context, name, uid string) error
}

func (k *kubeAgents) ListPS(ctx context.Context) ([]unstructured.Unstructured, error) {
	l, err := k.d.Resource(projectSecretsGVR).Namespace(k.ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}

func (k *kubeAgents) GetPS(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	return k.d.Resource(projectSecretsGVR).Namespace(k.ns).Get(ctx, name, metav1.GetOptions{})
}

func (k *kubeAgents) CreatePS(ctx context.Context, u *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return k.d.Resource(projectSecretsGVR).Namespace(k.ns).Create(ctx, u, metav1.CreateOptions{})
}

func (k *kubeAgents) DeletePS(ctx context.Context, name, uid string) error {
	var pre *metav1.Preconditions
	if uid != "" {
		u := types.UID(uid)
		pre = &metav1.Preconditions{UID: &u}
	}
	return k.d.Resource(projectSecretsGVR).Namespace(k.ns).Delete(ctx, name, metav1.DeleteOptions{Preconditions: pre})
}

// kubeValues is the kind backend: Secrets agentdev-ps-<n> (key value) in the values namespace. Update is
// unconditional (no resourceVersion), so the API needs no get.
type kubeValues struct {
	k  *kubeAgents
	ns string
}

func (v *kubeValues) SetValue(ctx context.Context, name, value string) error {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: remoteKeyPrefix + name, Namespace: v.ns,
		Labels: map[string]string{"app.kubernetes.io/managed-by": "agentdev-api", "agentdev.dev/projectsecret": name}},
		Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"value": []byte(value)}}
	_, err := v.k.c.CoreV1().Secrets(v.ns).Update(ctx, s, metav1.UpdateOptions{})
	if apierrors.IsNotFound(err) {
		_, err = v.k.c.CoreV1().Secrets(v.ns).Create(ctx, s, metav1.CreateOptions{})
	}
	return err
}

func (v *kubeValues) DeleteValue(ctx context.Context, name string) error {
	err := v.k.c.CoreV1().Secrets(v.ns).Delete(ctx, remoteKeyPrefix+name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// projectSecret is the list item: never a value.
type projectSecret struct {
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	Project     string `json:"project,omitempty"`
	Agent       string `json:"agent,omitempty"`
	Forge       string `json:"forge"`
	Mode        string `json:"mode"`
	Upstream    string `json:"upstream,omitempty"`
	Header      string `json:"header,omitempty"`
	Scheme      string `json:"scheme,omitempty"`
	EnvName     string `json:"envName,omitempty"`
	Description string `json:"description,omitempty"`
	Phase       string `json:"phase"`
	Message     string `json:"message,omitempty"`
	SyncedAt    string `json:"syncedAt,omitempty"`
}

func fromPSObject(u unstructured.Unstructured) projectSecret {
	str := func(f ...string) string { v, _, _ := unstructured.NestedString(u.Object, f...); return v }
	p := projectSecret{Name: u.GetName(), Scope: str("spec", "scope"), Project: str("spec", "project"), Agent: str("spec", "agent"),
		Forge: u.GetAnnotations()[forgeAnnotation], Mode: str("spec", "mode"), Upstream: str("spec", "broker", "upstream"),
		Header: str("spec", "broker", "header"), Scheme: str("spec", "broker", "scheme"), EnvName: str("spec", "env", "name"),
		Description: str("spec", "description"), Phase: str("status", "phase"), Message: str("status", "message"),
		SyncedAt: str("status", "syncedAt")}
	if p.Forge == "" {
		p.Forge = "gitea"
	}
	if p.Phase == "" {
		p.Phase = "pending"
	}
	return p
}

type secretsAPI struct {
	w      *writer
	ps     psStore
	values valueStore
	agents agentLister
}

// repoOf: the repository whose admins manage the secret (scope agent: the agent's repository) and its forge.
func (s *secretsAPI) repoOf(ctx context.Context, p projectSecret) (repo, forge string, err error) {
	if p.Scope == "project" {
		return p.Project, p.Forge, nil
	}
	list, err := s.agents.List(ctx)
	if err != nil {
		return "", "", err
	}
	for _, a := range list {
		if a.Name == p.Agent && len(a.Repos) > 0 {
			f := a.Forge
			if f == "" {
				f = "gitea"
			}
			return a.Repos[0], f, nil
		}
	}
	return "", "", invalid("no such agent: " + p.Agent)
}

func (s *secretsAPI) audit(e auditEntry, p *projectSecret) auditEntry {
	if p != nil {
		e.Request = map[string]any{"secret": p.Name, "scope": p.Scope, "project": p.Project, "agent": p.Agent, "mode": p.Mode,
			"upstream": p.Upstream, "envName": p.EnvName}
	}
	return e
}

func (s *secretsAPI) list(rw http.ResponseWriter, r *http.Request) {
	id := r.Context().Value(ctxIdentity{}).(*identity)
	items, err := s.ps.ListPS(r.Context())
	if err != nil {
		writeJSON(rw, http.StatusBadGateway, map[string]string{"error": "cannot read project secrets"})
		return
	}
	out := []projectSecret{}
	allowed := map[string]bool{} // repo -> admin, checked once per request
	for _, u := range items {
		p := fromPSObject(u)
		if !s.w.isAdmin(id) {
			repo, forge, err := s.repoOf(r.Context(), p)
			if err != nil {
				continue
			}
			key := forge + ":" + repo
			ok, seen := allowed[key]
			if !seen {
				_, aerr := s.w.authorize(r.Context(), id, forge, repo, permAdmin, false)
				ok = aerr == nil
				allowed[key] = ok
			}
			if !ok {
				continue
			}
		}
		out = append(out, p)
	}
	writeJSON(rw, http.StatusOK, map[string]any{"secrets": out})
}

// readValue takes "value" from the body: 1..8192 bytes of UTF-8 without NUL. Leading and trailing white space is
// trimmed (the broker trims it too).
func readValue(m map[string]json.RawMessage) (string, error) {
	var v string
	if json.Unmarshal(m["value"], &v) != nil {
		return "", invalid("value must be a string")
	}
	v = strings.TrimSpace(v)
	if v == "" || len(v) > secretValueMax || !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
		return "", invalid(fmt.Sprintf("value must be 1 to %d bytes of text", secretValueMax))
	}
	return v, nil
}

func str(m map[string]json.RawMessage, k string) (string, bool) {
	var v string
	if _, ok := m[k]; !ok {
		return "", false
	}
	if json.Unmarshal(m[k], &v) != nil {
		return "", false
	}
	return v, true
}

// parseNew validates a create body into the object (without the value).
func parseNew(m map[string]json.RawMessage) (projectSecret, bool, error) {
	var p projectSecret
	risk := false
	for k := range m {
		switch k {
		case "name", "scope", "project", "agent", "forge", "mode", "broker", "env", "description", "value":
		default:
			return p, false, invalid(fmt.Sprintf("unknown field %q", k))
		}
	}
	p.Name, _ = str(m, "name")
	if !psNameRe.MatchString(p.Name) {
		return p, false, invalid("name must be a DNS label (a-z, 0-9, -) of at most 50 characters")
	}
	p.Scope, _ = str(m, "scope")
	p.Forge = "gitea"
	if f, ok := str(m, "forge"); ok {
		p.Forge = f
	}
	if p.Forge != "gitea" && p.Forge != "github" {
		return p, false, invalid("forge must be gitea or github")
	}
	switch p.Scope {
	case "project":
		p.Project, _ = str(m, "project")
		if !validRepo(p.Project) || m["agent"] != nil {
			return p, false, invalid("scope project needs project (owner/name) and no agent")
		}
	case "agent":
		p.Agent, _ = str(m, "agent")
		if !agentNameRe.MatchString(p.Agent) || len(p.Agent) > 40 || m["project"] != nil {
			return p, false, invalid("scope agent needs agent (an agent name) and no project")
		}
	default:
		return p, false, invalid("scope must be project or agent")
	}
	p.Description, _ = str(m, "description")
	if len(p.Description) > 200 {
		return p, false, invalid("description: at most 200 characters")
	}
	p.Mode, _ = str(m, "mode")
	switch p.Mode {
	case "broker":
		var b struct{ Upstream, Header, Scheme string }
		if m["env"] != nil || json.Unmarshal(m["broker"], &b) != nil {
			return p, false, invalid("mode broker needs broker {upstream, header?, scheme?} and no env")
		}
		if !upstreamRe.MatchString(b.Upstream) || len(b.Upstream) > 300 {
			return p, false, invalid("broker.upstream must be an https URL")
		}
		if b.Header == "" {
			b.Header = "Authorization"
		}
		if b.Scheme == "" {
			b.Scheme = "bearer"
		}
		if !headerNmRe.MatchString(b.Header) || (b.Scheme != "bearer" && b.Scheme != "basic" && b.Scheme != "raw") {
			return p, false, invalid("broker.header must be a header name and broker.scheme bearer, basic or raw")
		}
		p.Upstream, p.Header, p.Scheme = b.Upstream, b.Header, b.Scheme
	case "env":
		var e struct {
			Name         string
			RiskAccepted bool
		}
		if m["broker"] != nil || json.Unmarshal(m["env"], &e) != nil {
			return p, false, invalid("mode env needs env {name, riskAccepted} and no broker")
		}
		if !envNameRe.MatchString(e.Name) || envReservedRe.MatchString(e.Name) || envReserved[e.Name] {
			return p, false, invalid("env.name must be an upper-case variable name that agentdev does not use")
		}
		if !e.RiskAccepted {
			return p, false, invalid("mode env: the model can read the value; set env.riskAccepted to true to accept this")
		}
		p.EnvName, risk = e.Name, true
	default:
		return p, false, invalid("mode must be broker or env")
	}
	return p, risk, nil
}

func psObject(p projectSecret, risk bool) *unstructured.Unstructured {
	spec := map[string]any{"scope": p.Scope, "mode": p.Mode}
	if p.Project != "" {
		spec["project"] = p.Project
	}
	if p.Agent != "" {
		spec["agent"] = p.Agent
	}
	if p.Description != "" {
		spec["description"] = p.Description
	}
	if p.Mode == "broker" {
		spec["broker"] = map[string]any{"upstream": p.Upstream, "header": p.Header, "scheme": p.Scheme}
	} else {
		spec["env"] = map[string]any{"name": p.EnvName, "riskAccepted": risk}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agentdev.dev/v1alpha1", "kind": "ProjectSecret",
		"metadata": map[string]any{"name": p.Name, "annotations": map[string]any{forgeAnnotation: p.Forge}},
		"spec":     spec,
	}}
}

// kubeInvalid turns an API server validation error (CRD CEL) into a 400 with its message.
func kubeInvalid(err error) error {
	if apierrors.IsInvalid(err) {
		return invalid("refused by the cluster: " + clipStr(err.Error(), 300))
	}
	if apierrors.IsAlreadyExists(err) {
		return &httpErr{http.StatusConflict, "invalid", "a project secret with this name exists"}
	}
	return err
}

func clipStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func (s *secretsAPI) create(rw http.ResponseWriter, r *http.Request) {
	id := r.Context().Value(ctxIdentity{}).(*identity)
	e := auditEntry{Who: s.w.who(id), Action: "secret-create"}
	m, err := decodeObject(r)
	if err != nil {
		s.w.fail(rw, e, err)
		return
	}
	p, risk, err := parseNew(m)
	e = s.audit(e, &p)
	if err != nil {
		s.w.fail(rw, e, err)
		return
	}
	value, err := readValue(m)
	if err != nil {
		s.w.fail(rw, e, err)
		return
	}
	repo, forge, err := s.repoOf(r.Context(), p)
	if err != nil {
		s.w.fail(rw, e, err)
		return
	}
	if e.Grant, err = s.w.authorize(r.Context(), id, forge, repo, permAdmin, true); err != nil {
		s.w.fail(rw, e, err)
		return
	}
	p.Forge = forge
	out, err := s.ps.CreatePS(r.Context(), psObject(p, risk))
	if err != nil {
		s.w.fail(rw, e, kubeInvalid(err))
		return
	}
	if err := s.values.SetValue(r.Context(), p.Name, value); err != nil {
		_ = s.ps.DeletePS(r.Context(), p.Name, string(out.GetUID())) // no object without a value from this request
		s.w.fail(rw, e, fmt.Errorf("write value: %w", err))
		return
	}
	e.Result = "ok"
	audit(e)
	writeJSON(rw, http.StatusCreated, fromPSObject(*out))
}

// managed loads a secret and checks that the caller administers its repository.
func (s *secretsAPI) managed(r *http.Request, e *auditEntry) (*unstructured.Unstructured, projectSecret, error) {
	id := r.Context().Value(ctxIdentity{}).(*identity)
	name := r.PathValue("name")
	if !psNameRe.MatchString(name) {
		return nil, projectSecret{}, &httpErr{http.StatusNotFound, "invalid", "no such project secret"}
	}
	u, err := s.ps.GetPS(r.Context(), name)
	if apierrors.IsNotFound(err) {
		return nil, projectSecret{}, &httpErr{http.StatusNotFound, "invalid", "no such project secret"}
	}
	if err != nil {
		return nil, projectSecret{}, err
	}
	p := fromPSObject(*u)
	*e = s.audit(*e, &p)
	repo, forge, err := s.repoOf(r.Context(), p)
	if err != nil {
		if s.w.isAdmin(id) { // the agent is gone: admins can still clean up
			e.Grant = "admin-group"
			return u, p, nil
		}
		return nil, p, err
	}
	e.Grant, err = s.w.authorize(r.Context(), id, forge, repo, permAdmin, true)
	return u, p, err
}

func (s *secretsAPI) rotate(rw http.ResponseWriter, r *http.Request) {
	id := r.Context().Value(ctxIdentity{}).(*identity)
	e := auditEntry{Who: s.w.who(id), Action: "secret-rotate"}
	m, err := decodeObject(r)
	if err != nil {
		s.w.fail(rw, e, err)
		return
	}
	if len(m) != 1 {
		s.w.fail(rw, e, invalid("body must be {\"value\": \"...\"}"))
		return
	}
	value, err := readValue(m)
	if err != nil {
		s.w.fail(rw, e, err)
		return
	}
	_, p, err := s.managed(r, &e)
	if err != nil {
		s.w.fail(rw, e, err)
		return
	}
	if err := s.values.SetValue(r.Context(), p.Name, value); err != nil {
		s.w.fail(rw, e, fmt.Errorf("write value: %w", err))
		return
	}
	e.Result = "ok"
	audit(e)
	rw.WriteHeader(http.StatusNoContent)
}

func (s *secretsAPI) remove(rw http.ResponseWriter, r *http.Request) {
	id := r.Context().Value(ctxIdentity{}).(*identity)
	e := auditEntry{Who: s.w.who(id), Action: "secret-delete"}
	u, p, err := s.managed(r, &e)
	if err != nil {
		s.w.fail(rw, e, err)
		return
	}
	if err := s.ps.DeletePS(r.Context(), p.Name, string(u.GetUID())); err != nil && !apierrors.IsNotFound(err) {
		s.w.fail(rw, e, err)
		return
	}
	if err := s.values.DeleteValue(r.Context(), p.Name); err != nil {
		s.w.fail(rw, e, fmt.Errorf("delete value: %w", err))
		return
	}
	e.Result = "ok"
	audit(e)
	rw.WriteHeader(http.StatusNoContent)
}
