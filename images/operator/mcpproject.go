package main

// MCP per project (#14, ADR 0001), as an operator controller (#383); it replaces scripts/mcp-sync.sh. For each
// McpProject the operator makes ContextForge match the spec, idempotent:
//   - each server registered once as gateway <project>-<server>; ContextForge discovers its tools
//   - one virtual server project-<project> with exactly those tools
//   - one API token per agent, scoped to that virtual server, in Secret agent-<agent>-mcp {CF_URL, CF_TOKEN}; the
//     agent's pod reads it (optional secretRef). A token never leaves the cluster and is never logged or put in status.
// The ContextForge admin password is read by name from the Kubernetes Secret contextforge in the platform namespace
// (PLATFORM_ADMIN_PASSWORD, the value ContextForge itself starts with). Hosts outside MCP_ALLOWED_HOSTS are refused
// before any change, with the reason in status (docs/security.md R51).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups=agentdev.dev,resources=mcpprojects,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agentdev.dev,resources=mcpprojects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentdev.dev,resources=mcpprojects/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=create;patch;delete

const (
	mcpFinalizer     = "agentdev.dev/mcp-tokens"
	mcpFieldOwner    = "agentdev-operator-mcp"
	mcpLabelProject  = "agentdev/mcp-project" // same label and annotations as scripts/mcp-sync.sh: its Secrets are adopted
	mcpAnnServer     = "agentdev/mcp-server"
	mcpAnnTokenID    = "agentdev/mcp-token-id"
	mcpAnnRenewAfter = "agentdev/mcp-renew-after"
	mcpTokenDays     = 365 // ContextForge 1.1 rejects a token without an expiry
	mcpRenewDays     = 335 // re-minted in the last 30 days
	// CFAdminSecret holds the ContextForge admin login (manifests/platform.yaml.tmpl).
	CFAdminSecret = "contextforge"
	cfAdminKey    = "PLATFORM_ADMIN_PASSWORD"
)

var (
	mcpNameRE = regexp.MustCompile(`^[a-z0-9-]+$`)
	mcpURLRE  = regexp.MustCompile(`^https?://([^/?#@\s]+)(/\S*)?$`)
)

// McpAllowed is the instance allow-list of MCP hosts (host or host:port, lower case).
type McpAllowed map[string]bool

// ParseMcpAllowed splits MCP_ALLOWED_HOSTS on spaces and commas.
func ParseMcpAllowed(s string) McpAllowed {
	a := McpAllowed{}
	for _, h := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ' ' || r == ',' || r == '\t' || r == '\n' }) {
		a[h] = true
	}
	return a
}

// validateMcpServers is the check scripts/mcp-sync.sh made before any change: names, URL form, allow-listed host.
// It returns the condition reason and a message for status, or "" when every server is fine.
func validateMcpServers(servers []v1.McpServer, allowed McpAllowed) (reason, msg string) {
	for _, s := range servers {
		if !mcpNameRE.MatchString(s.Name) {
			return "InvalidServer", fmt.Sprintf("server name %q must be a-z0-9-", s.Name)
		}
		m := mcpURLRE.FindStringSubmatch(s.URL)
		if m == nil {
			return "InvalidServer", fmt.Sprintf("server %s: url must be http(s)://host[:port]/path without credentials", s.Name)
		}
		hp := strings.ToLower(m[1])
		h, _, _ := strings.Cut(hp, ":")
		if !allowed[hp] && !allowed[h] {
			return "HostNotAllowed", fmt.Sprintf("server %s: host %s is not in MCP_ALLOWED_HOSTS (mcp_allowed_hosts); add it only if you trust that server", s.Name, hp)
		}
	}
	return "", ""
}

// ContextForge is the admin API the controller uses. One value per reconcile (logged in).
type ContextForge interface {
	Gateways(ctx context.Context) ([]string, error) // names
	AddGateway(ctx context.Context, name, url string) error
	Tools(ctx context.Context) ([]CFTool, error)
	Servers(ctx context.Context) (map[string]string, error) // name -> id
	PutServer(ctx context.Context, id, name string, toolIDs []string) (string, error)
	MintToken(ctx context.Context, name, serverID string) (token, id string, err error)
	RevokeToken(ctx context.Context, id string) error
}

// CFTool is one tool as ContextForge lists it.
type CFTool struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	GatewaySlug string `json:"gatewaySlug"`
}

// CFDialer logs in to ContextForge with the admin credential.
type CFDialer func(ctx context.Context, email, password string) (ContextForge, error)

// McpProjectReconciler keeps ContextForge and the agents' token Secrets in line with each McpProject.
type McpProjectReconciler struct {
	client.Client
	Direct     client.Reader // API reader: Secrets are read by name, never cached (no list/watch on Secrets)
	NS         string        // agents namespace: McpProjects and agent-<name>-mcp Secrets
	PlatformNS string        // ContextForge and its Secret
	CFURL      string        // in-cluster base URL, also the agents' CF_URL prefix
	AdminEmail string
	Allowed    McpAllowed
	Dial       CFDialer
	Now        func() time.Time
	Resync     time.Duration // rediscover tools and renew tokens
}

func mcpSecretName(agent string) string { return "agent-" + agent + "-mcp" }

func (r *McpProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p v1.McpProject
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !p.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&p, mcpFinalizer) {
			return ctrl.Result{}, nil
		}
		cf, err := r.login(ctx)
		if err != nil {
			// ContextForge gone (removed with the last project): the Secrets still go, tokens die with its database
			log.FromContext(ctx).Info("mcp: ContextForge not reachable on delete; removing Secrets only", "reason", err.Error())
		}
		if err := r.dropSecrets(ctx, cf, &p, nil); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(&p, mcpFinalizer)
		return ctrl.Result{}, r.Update(ctx, &p)
	}
	if !controllerutil.ContainsFinalizer(&p, mcpFinalizer) {
		controllerutil.AddFinalizer(&p, mcpFinalizer)
		if err := r.Update(ctx, &p); err != nil {
			return ctrl.Result{}, err
		}
	}
	// allow-list first: nothing in ContextForge changes for a spec that names an untrusted server
	if reason, msg := validateMcpServers(p.Spec.Servers, r.Allowed); reason != "" {
		return r.status(ctx, &p, metav1.ConditionFalse, reason, msg, 0)
	}
	cf, err := r.login(ctx)
	if err != nil {
		return r.status(ctx, &p, metav1.ConditionFalse, "ContextForgeUnavailable", err.Error(), time.Minute)
	}
	keep := []string{}
	if len(p.Spec.Servers) > 0 {
		for _, a := range p.Spec.Agents {
			keep = append(keep, mcpSecretName(a))
		}
	}
	if err := r.dropSecrets(ctx, cf, &p, keep); err != nil {
		return r.status(ctx, &p, metav1.ConditionFalse, "SyncFailed", err.Error(), time.Minute)
	}
	if len(p.Spec.Servers) == 0 {
		p.Status.ServerID, p.Status.Tools = "", nil
		return r.status(ctx, &p, metav1.ConditionTrue, "NoServers", "no servers: nothing registered, no tokens", 0)
	}
	if reason, msg, err := r.sync(ctx, cf, &p); err != nil {
		return r.status(ctx, &p, metav1.ConditionFalse, reason, msg+": "+err.Error(), time.Minute)
	}
	return r.status(ctx, &p, metav1.ConditionTrue, "Synced",
		fmt.Sprintf("%d tools, %d agent tokens", len(p.Status.Tools), len(p.Status.TokenSecrets)), r.Resync)
}

// sync does steps 1 to 3 of scripts/mcp-sync.sh. On error it returns the condition reason and the step.
func (r *McpProjectReconciler) sync(ctx context.Context, cf ContextForge, p *v1.McpProject) (string, string, error) {
	project := p.Name
	gws, err := cf.Gateways(ctx)
	if err != nil {
		return "SyncFailed", "list gateways", err
	}
	prefixes := []string{}
	for _, s := range p.Spec.Servers {
		g := project + "-" + s.Name
		prefixes = append(prefixes, g)
		if slices.Contains(gws, g) {
			continue
		}
		if err := cf.AddGateway(ctx, g, s.URL); err != nil {
			return "SyncFailed", "register " + g + " (it must be reachable from ContextForge, cluster Service range only)", err
		}
	}
	all, err := cf.Tools(ctx)
	if err != nil {
		return "SyncFailed", "list tools", err
	}
	ids, names := []string{}, []string{}
	for _, t := range all {
		if slices.Contains(prefixes, t.GatewaySlug) {
			ids, names = append(ids, t.ID), append(names, t.Name)
		}
	}
	if len(ids) == 0 {
		return "NoTools", "no tools discovered for " + project, errors.New("check the servers are up")
	}
	sort.Strings(names)
	servers, err := cf.Servers(ctx)
	if err != nil {
		return "SyncFailed", "list virtual servers", err
	}
	sid, err := cf.PutServer(ctx, servers["project-"+project], "project-"+project, ids)
	if err != nil || sid == "" {
		return "SyncFailed", "virtual server project-" + project, errors.Join(err, errors.New("no id"))
	}
	p.Status.ServerID, p.Status.Tools = sid, names
	for _, a := range p.Spec.Agents {
		if err := r.agentToken(ctx, cf, p, a, sid); err != nil {
			return "SyncFailed", "token for agent " + a, err
		}
		if !slices.Contains(p.Status.TokenSecrets, mcpSecretName(a)) {
			p.Status.TokenSecrets = append(p.Status.TokenSecrets, mcpSecretName(a))
		}
	}
	sort.Strings(p.Status.TokenSecrets)
	return "", "", nil
}

// tokenRec is the status record of one token Secret, or nil.
func tokenRec(p *v1.McpProject, name string) *v1.McpToken {
	for i := range p.Status.Tokens {
		if p.Status.Tokens[i].Secret == name {
			return &p.Status.Tokens[i]
		}
	}
	return nil
}

// otherOwner is the name of another McpProject in the namespace that records the Secret name, or "".
func (r *McpProjectReconciler) otherOwner(ctx context.Context, p *v1.McpProject, name string) (string, error) {
	var all v1.McpProjectList
	if err := r.List(ctx, &all, client.InNamespace(r.NS)); err != nil {
		return "", err
	}
	for _, o := range all.Items {
		if o.Name != p.Name && (slices.Contains(o.Status.TokenSecrets, name) || tokenRec(&o, name) != nil) {
			return o.Name, nil
		}
	}
	return "", nil
}

// agentToken keeps agent-<agent>-mcp: a token scoped to sid, re-minted 30 days before it expires or when the
// virtual server changed. The operator never reads the Secret (#452): what it needs (token id, server, renew time)
// is in the McpProject status, and the Secret is written by server-side apply (no get). A Secret name another
// McpProject records is never taken over.
func (r *McpProjectReconciler) agentToken(ctx context.Context, cf ContextForge, p *v1.McpProject, agent, sid string) error {
	name := mcpSecretName(agent)
	rec := tokenRec(p, name)
	if rec != nil && rec.ServerID == sid && r.Now().Unix() < rec.RenewAfter {
		return nil
	}
	if owner, err := r.otherOwner(ctx, p, name); err != nil {
		return err
	} else if owner != "" {
		return fmt.Errorf("Secret %s belongs to project %q (one project per agent)", name, owner)
	}
	tok, tid, err := cf.MintToken(ctx, fmt.Sprintf("agent-%s-%s-%s", agent, p.Name, r.Now().UTC().Format("20060102150405")), sid)
	if err != nil {
		return err
	}
	renew := r.Now().Add(mcpRenewDays * 24 * time.Hour).Unix()
	ac := corev1ac.Secret(name, r.NS).
		WithLabels(map[string]string{mcpLabelProject: p.Name, "app.kubernetes.io/managed-by": "agentdev-operator"}).
		WithAnnotations(map[string]string{mcpAnnServer: sid, mcpAnnTokenID: tid, mcpAnnRenewAfter: strconv.FormatInt(renew, 10)}).
		WithType(corev1.SecretTypeOpaque).
		WithData(map[string][]byte{"CF_URL": []byte(r.CFURL + "/servers/" + sid + "/mcp"), "CF_TOKEN": []byte(tok)})
	if err := r.Apply(ctx, ac, client.FieldOwner(mcpFieldOwner), client.ForceOwnership); err != nil {
		_ = cf.RevokeToken(ctx, tid) // never leave a live token that no Secret records
		return fmt.Errorf("store Secret %s: %s", name, apierrors.ReasonForError(err))
	}
	old := ""
	if rec == nil {
		p.Status.Tokens = append(p.Status.Tokens, v1.McpToken{Secret: name})
		rec = &p.Status.Tokens[len(p.Status.Tokens)-1]
	} else {
		old = rec.TokenID
	}
	rec.TokenID, rec.ServerID, rec.RenewAfter = tid, sid, renew
	if old != "" && old != tid {
		if err := cf.RevokeToken(ctx, old); err != nil {
			log.FromContext(ctx).Info("mcp: could not revoke the replaced token", "secret", name, "tokenID", old)
		}
	}
	return nil
}

// dropSecrets revokes and deletes the token Secrets in status that are not in keep (all of them for keep == nil).
// cf may be nil (ContextForge gone): then only the Secrets are deleted. The Secret is deleted by name, never read.
func (r *McpProjectReconciler) dropSecrets(ctx context.Context, cf ContextForge, p *v1.McpProject, keep []string) error {
	left, recs := []string{}, []v1.McpToken{}
	for _, name := range p.Status.TokenSecrets {
		if slices.Contains(keep, name) {
			left = append(left, name)
			continue
		}
		if owner, err := r.otherOwner(ctx, p, name); err != nil {
			return err
		} else if owner != "" {
			continue // another project's now
		}
		if rec := tokenRec(p, name); rec != nil && rec.TokenID != "" && cf != nil {
			if err := cf.RevokeToken(ctx, rec.TokenID); err != nil {
				log.FromContext(ctx).Info("mcp: could not revoke token", "secret", name, "tokenID", rec.TokenID)
			}
		}
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: r.NS, Name: name}}
		if err := r.Delete(ctx, s); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	for _, t := range p.Status.Tokens {
		if slices.Contains(left, t.Secret) {
			recs = append(recs, t)
		}
	}
	p.Status.TokenSecrets, p.Status.Tokens = left, recs
	return nil
}

func (r *McpProjectReconciler) login(ctx context.Context) (ContextForge, error) {
	var s corev1.Secret
	if err := r.Direct.Get(ctx, client.ObjectKey{Namespace: r.PlatformNS, Name: CFAdminSecret}, &s); err != nil {
		return nil, fmt.Errorf("read Secret %s/%s: %s", r.PlatformNS, CFAdminSecret, apierrors.ReasonForError(err))
	}
	pw := strings.TrimRight(string(s.Data[cfAdminKey]), "\n")
	if pw == "" {
		return nil, fmt.Errorf("Secret %s/%s has no %s", r.PlatformNS, CFAdminSecret, cfAdminKey)
	}
	return r.Dial(ctx, r.AdminEmail, pw)
}

func (r *McpProjectReconciler) status(ctx context.Context, p *v1.McpProject, st metav1.ConditionStatus, reason, msg string, requeue time.Duration) (ctrl.Result, error) {
	p.Status.ObservedGeneration = p.Generation
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: "Ready", Status: st, Reason: reason,
		Message: clipMsg(msg), ObservedGeneration: p.Generation})
	if err := r.Status().Update(ctx, p); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *McpProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1.McpProject{}).Named("mcpproject").Complete(r)
}

// --- HTTP client of the ContextForge admin API (v1.1) ---

type cfHTTP struct {
	base, bearer string
	hc           *http.Client
}

// DialContextForge logs in at base/auth/login and returns a client with the admin bearer token.
func DialContextForge(base string, hc *http.Client) CFDialer {
	return func(ctx context.Context, email, password string) (ContextForge, error) {
		c := &cfHTTP{base: strings.TrimRight(base, "/"), hc: hc}
		var out struct {
			AccessToken string `json:"access_token"`
		}
		if err := c.do(ctx, http.MethodPost, "/auth/login", map[string]string{"email": email, "password": password}, &out); err != nil {
			return nil, fmt.Errorf("ContextForge admin login: %w", err)
		}
		if out.AccessToken == "" {
			return nil, errors.New("ContextForge admin login: no token")
		}
		c.bearer = out.AccessToken
		return c, nil
	}
}

// do never puts a response body in an error: it can contain a token.
func (c *cfHTTP) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, strings.SplitN(path, "?", 2)[0], err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("%s %s: HTTP %d", method, strings.SplitN(path, "?", 2)[0], resp.StatusCode)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

func (c *cfHTTP) Gateways(ctx context.Context) ([]string, error) {
	var gs []struct {
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, "/gateways", nil, &gs); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return out, nil
}

func (c *cfHTTP) AddGateway(ctx context.Context, name, url string) error {
	return c.do(ctx, http.MethodPost, "/gateways", map[string]string{"name": name, "url": url, "transport": "STREAMABLEHTTP"}, nil)
}

func (c *cfHTTP) Tools(ctx context.Context) ([]CFTool, error) {
	var ts []CFTool
	return ts, c.do(ctx, http.MethodGet, "/tools?limit=0", nil, &ts)
}

func (c *cfHTTP) Servers(ctx context.Context) (map[string]string, error) {
	var ss []struct{ ID, Name string }
	if err := c.do(ctx, http.MethodGet, "/servers", nil, &ss); err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, s := range ss {
		if _, ok := m[s.Name]; !ok {
			m[s.Name] = s.ID
		}
	}
	return m, nil
}

func (c *cfHTTP) PutServer(ctx context.Context, id, name string, toolIDs []string) (string, error) {
	body := map[string]any{"name": name, "description": "agentdev project (operator)", "associated_tools": toolIDs, "visibility": "private"}
	var out struct {
		ID string `json:"id"`
	}
	if id != "" {
		return id, c.do(ctx, http.MethodPut, "/servers/"+id, body, nil)
	}
	// newer ContextForge wraps the body in {"server": ...}; 1.1 takes it as is (as scripts/mcp-sync.sh tried both)
	if err := c.do(ctx, http.MethodPost, "/servers", map[string]any{"server": body}, &out); err == nil && out.ID != "" {
		return out.ID, nil
	}
	return out.ID, c.do(ctx, http.MethodPost, "/servers", body, &out)
}

func (c *cfHTTP) MintToken(ctx context.Context, name, serverID string) (string, string, error) {
	var out map[string]json.RawMessage
	err := c.do(ctx, http.MethodPost, "/tokens", map[string]any{"name": name, "description": "agentdev agent token (operator)",
		"expires_in_days": mcpTokenDays, "scope": map[string]any{"server_id": serverID, "permissions": []string{}}}, &out)
	if err != nil {
		return "", "", err
	}
	str := func(raw json.RawMessage) string {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	tok, id := str(out["access_token"]), str(out["id"])
	if tok == "" {
		tok = str(out["token"])
	}
	var inner struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(out["token"], &inner) == nil && inner.ID != "" {
		id = inner.ID
	}
	if tok == "" {
		return "", "", errors.New("POST /tokens: no token in the response")
	}
	return tok, id, nil
}

func (c *cfHTTP) RevokeToken(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/tokens/"+id, nil, nil)
}

// setupMcpProjects starts the controller. It runs whenever the McpProject CRD is installed, also without ContextForge:
// then every McpProject is Ready=False (ContextForgeUnavailable) and a deleted one still loses its token Secrets.
// MCP_CONTEXTFORGE_URL=off turns it off (the CRD is not watched).
func setupMcpProjects(mgr ctrl.Manager, ns string, getenv func(string) string) (bool, error) {
	pns := getenv("PLATFORM_NS")
	base := getenv("MCP_CONTEXTFORGE_URL")
	if base == "off" {
		return false, nil
	}
	if pns == "" {
		return false, errors.New("PLATFORM_NS is not set (namespace of ContextForge and its Secret)")
	}
	if base == "" {
		base = "http://contextforge." + pns + ".svc.cluster.local:4444"
	}
	resync := 6 * time.Hour
	if m, err := strconv.Atoi(getenv("MCP_RESYNC_MINUTES")); err == nil && m > 0 {
		resync = time.Duration(m) * time.Minute
	}
	email := getenv("MCP_ADMIN_EMAIL")
	if email == "" {
		email = "admin@agentdev.dev"
	}
	base = strings.TrimRight(base, "/")
	r := &McpProjectReconciler{Client: mgr.GetClient(), Direct: mgr.GetAPIReader(), NS: ns, PlatformNS: pns,
		CFURL: base, AdminEmail: email, Allowed: ParseMcpAllowed(getenv("MCP_ALLOWED_HOSTS")), Now: time.Now,
		Resync: resync, Dial: DialContextForge(base, &http.Client{Timeout: 60 * time.Second})}
	return true, r.SetupWithManager(mgr)
}
