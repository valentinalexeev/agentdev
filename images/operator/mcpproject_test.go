package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

const platformNS = "agentdev-platform"

type fakeCF struct {
	gateways    map[string]string // name -> url
	tools       []CFTool
	servers     map[string]string // name -> id
	serverTools map[string][]string
	tokens      map[string]string // id -> server
	minted      int
	revoked     []string
	down        bool
}

func newFakeCF() *fakeCF {
	return &fakeCF{gateways: map[string]string{}, servers: map[string]string{}, serverTools: map[string][]string{}, tokens: map[string]string{}}
}

func (f *fakeCF) Gateways(context.Context) ([]string, error) {
	out := []string{}
	for n := range f.gateways {
		out = append(out, n)
	}
	return out, nil
}
func (f *fakeCF) AddGateway(_ context.Context, name, url string) error {
	f.gateways[name] = url
	f.tools = append(f.tools, CFTool{ID: "t-" + name, Name: name + "-search", GatewaySlug: name})
	return nil
}
func (f *fakeCF) Tools(context.Context) ([]CFTool, error)            { return f.tools, nil }
func (f *fakeCF) Servers(context.Context) (map[string]string, error) { return f.servers, nil }
func (f *fakeCF) PutServer(_ context.Context, id, name string, t []string) (string, error) {
	if id == "" {
		id = "srv-" + name
		f.servers[name] = id
	}
	f.serverTools[id] = t
	return id, nil
}
func (f *fakeCF) MintToken(_ context.Context, _, sid string) (string, string, error) {
	f.minted++
	id := "tok" + string(rune('0'+f.minted))
	f.tokens[id] = sid
	return "secret-" + id, id, nil
}
func (f *fakeCF) RevokeToken(_ context.Context, id string) error {
	f.revoked = append(f.revoked, id)
	delete(f.tokens, id)
	return nil
}

func mcpFixture(t *testing.T, p *v1.McpProject, objs ...client.Object) (*McpProjectReconciler, client.Client, *fakeCF) {
	t.Helper()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1.AddToScheme(s)
	admin := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: CFAdminSecret, Namespace: platformNS},
		Data: map[string][]byte{cfAdminKey: []byte("Aa1!pw")}}
	objs = append(objs, p, admin)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(p).Build()
	cf := newFakeCF()
	now := time.Unix(1_800_000_000, 0)
	r := &McpProjectReconciler{Client: c, Direct: c, NS: ns, PlatformNS: platformNS, CFURL: "http://cf:4444",
		AdminEmail: "admin@agentdev.dev", Allowed: ParseMcpAllowed("docs-mcp.tools.svc.cluster.local, other:8080"),
		Now: func() time.Time { return now }, Resync: time.Hour,
		Dial: func(_ context.Context, email, pw string) (ContextForge, error) {
			if cf.down {
				return nil, errors.New("ContextForge admin login: HTTP 503")
			}
			if email != "admin@agentdev.dev" || pw != "Aa1!pw" {
				return nil, errors.New("bad login")
			}
			return cf, nil
		}}
	return r, c, cf
}

func mcpProject(servers []v1.McpServer, agents ...string) *v1.McpProject {
	return &v1.McpProject{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns, Generation: 1},
		Spec: v1.McpProjectSpec{Servers: servers, Agents: agents}}
}

var docsServer = []v1.McpServer{{Name: "docs", URL: "http://docs-mcp.tools.svc.cluster.local:8080/mcp"}}

func reconcileMcp(t *testing.T, r *McpProjectReconciler, c client.Client) *v1.McpProject {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "web"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var p v1.McpProject
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "web"}, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatal(err)
	}
	return &p
}

func ready(p *v1.McpProject) *metav1.Condition {
	return meta.FindStatusCondition(p.Status.Conditions, "Ready")
}

func TestMcpSyncCreatesGatewayServerAndTokens(t *testing.T) {
	r, c, cf := mcpFixture(t, mcpProject(docsServer, "alice", "bob"))
	p := reconcileMcp(t, r, c)
	if cond := ready(p); cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "Synced" {
		t.Fatalf("Ready: %+v", cond)
	}
	if cf.gateways["web-docs"] == "" {
		t.Fatalf("gateway web-docs not registered: %v", cf.gateways)
	}
	if p.Status.ServerID != "srv-project-web" || len(cf.serverTools["srv-project-web"]) != 1 {
		t.Fatalf("virtual server: %q %v", p.Status.ServerID, cf.serverTools)
	}
	for _, a := range []string{"alice", "bob"} {
		var s corev1.Secret
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-" + a + "-mcp"}, &s); err != nil {
			t.Fatal(err)
		}
		if string(s.Data["CF_URL"]) != "http://cf:4444/servers/srv-project-web/mcp" || !strings.HasPrefix(string(s.Data["CF_TOKEN"]), "secret-tok") {
			t.Fatalf("%s: %v", a, s.Data)
		}
		if s.Labels[mcpLabelProject] != "web" || s.Annotations[mcpAnnServer] != "srv-project-web" || s.Annotations[mcpAnnTokenID] == "" {
			t.Fatalf("%s: labels %v annotations %v", a, s.Labels, s.Annotations)
		}
	}
	// status never carries a token
	b, _ := json.Marshal(p.Status)
	if strings.Contains(string(b), "secret-tok") {
		t.Fatalf("token in status: %s", b)
	}
	// idempotent: a second run mints nothing and registers nothing new
	reconcileMcp(t, r, c)
	if cf.minted != 2 || len(cf.gateways) != 1 {
		t.Fatalf("second run: minted %d gateways %d", cf.minted, len(cf.gateways))
	}
}

func TestMcpHostNotAllowedChangesNothing(t *testing.T) {
	r, c, cf := mcpFixture(t, mcpProject([]v1.McpServer{{Name: "evil", URL: "https://evil.example.com/mcp"}}, "alice"))
	p := reconcileMcp(t, r, c)
	cond := ready(p)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "HostNotAllowed" || !strings.Contains(cond.Message, "evil.example.com") {
		t.Fatalf("Ready: %+v", cond)
	}
	if len(cf.gateways) != 0 || cf.minted != 0 {
		t.Fatal("ContextForge changed for a refused host")
	}
}

func TestMcpValidateServers(t *testing.T) {
	a := ParseMcpAllowed("docs other:8080")
	for _, tc := range []struct {
		url, reason string
	}{
		{"http://docs/mcp", ""},
		{"http://DOCS:9000/mcp", ""},  // host alone allows any port
		{"http://other:8080/mcp", ""}, // host:port entry
		{"http://other:9090/mcp", "HostNotAllowed"},
		{"http://user:pw@docs/mcp", "InvalidServer"},
		{"ftp://docs/mcp", "InvalidServer"},
		{"http://docsx/mcp", "HostNotAllowed"},
	} {
		if got, _ := validateMcpServers([]v1.McpServer{{Name: "s", URL: tc.url}}, a); got != tc.reason {
			t.Errorf("%s: reason %q, want %q", tc.url, got, tc.reason)
		}
	}
	if got, _ := validateMcpServers([]v1.McpServer{{Name: "Bad_Name", URL: "http://docs/mcp"}}, a); got != "InvalidServer" {
		t.Errorf("bad name: %q", got)
	}
}

func TestMcpAgentRemovedRevokesToken(t *testing.T) {
	r, c, cf := mcpFixture(t, mcpProject(docsServer, "alice", "bob"))
	p := reconcileMcp(t, r, c)
	p.Spec.Agents = []string{"alice"}
	if err := c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p = reconcileMcp(t, r, c)
	var s corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-bob-mcp"}, &s); !apierrors.IsNotFound(err) {
		t.Fatalf("agent-bob-mcp still there: %v", err)
	}
	if len(cf.revoked) != 1 || len(p.Status.TokenSecrets) != 1 {
		t.Fatalf("revoked %v, status %v", cf.revoked, p.Status.TokenSecrets)
	}
}

func TestMcpRenewsNearExpiryAndRevokesOld(t *testing.T) {
	r, c, cf := mcpFixture(t, mcpProject(docsServer, "alice"))
	reconcileMcp(t, r, c)
	later := r.Now().Add((mcpRenewDays + 1) * 24 * time.Hour)
	r.Now = func() time.Time { return later }
	reconcileMcp(t, r, c)
	if cf.minted != 2 || len(cf.revoked) != 1 || cf.revoked[0] != "tok1" {
		t.Fatalf("minted %d revoked %v", cf.minted, cf.revoked)
	}
}

func TestMcpNeverTakesAnotherProjectsSecret(t *testing.T) {
	other := &v1.McpProject{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: ns},
		Status: v1.McpProjectStatus{TokenSecrets: []string{"agent-alice-mcp"}}}
	r, c, cf := mcpFixture(t, mcpProject(docsServer, "alice"), other)
	p := reconcileMcp(t, r, c)
	if cond := ready(p); cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, `project "api"`) || cf.minted != 0 {
		t.Fatalf("Ready: %+v, minted %d", cond, cf.minted)
	}
}

// #452: the controller never gets, lists or watches a Secret in the agents namespace (so not claude-shared either).
// Every Secret read there fails; create, renew, agent removal and delete still work.
func TestMcpNeverReadsAgentSecrets(t *testing.T) {
	r, c, cf := mcpFixture(t, mcpProject(docsServer, "alice", "bob"))
	reads := 0
	guard := interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			if _, ok := o.(*corev1.Secret); ok && k.Namespace == ns {
				reads++
				return apierrors.NewForbidden(corev1.Resource("secrets"), k.Name, errors.New("no get"))
			}
			return cl.Get(ctx, k, o, opts...)
		},
		List: func(ctx context.Context, cl client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			if _, ok := l.(*corev1.SecretList); ok {
				reads++
				return apierrors.NewForbidden(corev1.Resource("secrets"), "", errors.New("no list"))
			}
			return cl.List(ctx, l, opts...)
		},
	}
	gc := interceptor.NewClient(c.(client.WithWatch), guard)
	r.Client, r.Direct = gc, gc
	p := reconcileMcp(t, r, c)
	if cond := ready(p); cond.Reason != "Synced" || len(p.Status.Tokens) != 2 {
		t.Fatalf("Ready %+v tokens %+v", cond, p.Status.Tokens)
	}
	r.Now = func() time.Time { return time.Unix(1_800_000_000, 0).Add((mcpRenewDays + 1) * 24 * time.Hour) }
	p = reconcileMcp(t, r, c)
	p.Spec.Agents = []string{"alice"}
	if err := c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	reconcileMcp(t, r, c)
	if err := c.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if p := reconcileMcp(t, r, c); p != nil {
		t.Fatalf("finalizer not removed")
	}
	if reads != 0 || cf.minted != 4 || len(cf.revoked) != 4 || len(cf.tokens) != 0 {
		t.Fatalf("secret reads %d, minted %d, revoked %v, live %v", reads, cf.minted, cf.revoked, cf.tokens)
	}
}

func TestMcpDeleteRevokesAndRemovesSecrets(t *testing.T) {
	r, c, cf := mcpFixture(t, mcpProject(docsServer, "alice"))
	p := reconcileMcp(t, r, c)
	if err := c.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if p := reconcileMcp(t, r, c); p != nil {
		t.Fatalf("finalizer not removed: %v", p.Finalizers)
	}
	var s corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-alice-mcp"}, &s); !apierrors.IsNotFound(err) {
		t.Fatalf("Secret left: %v", err)
	}
	if len(cf.revoked) != 1 {
		t.Fatalf("revoked %v", cf.revoked)
	}
}

func TestMcpContextForgeDown(t *testing.T) {
	r, c, cf := mcpFixture(t, mcpProject(docsServer, "alice"))
	cf.down = true
	p := reconcileMcp(t, r, c)
	if cond := ready(p); cond.Reason != "ContextForgeUnavailable" {
		t.Fatalf("Ready: %+v", cond)
	}
}

func TestMcpNoServersDropsTokens(t *testing.T) {
	r, c, cf := mcpFixture(t, mcpProject(docsServer, "alice"))
	p := reconcileMcp(t, r, c)
	p.Spec.Servers = nil
	if err := c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p = reconcileMcp(t, r, c)
	if cond := ready(p); cond.Reason != "NoServers" || len(cf.revoked) != 1 || len(p.Status.TokenSecrets) != 0 {
		t.Fatalf("Ready %+v revoked %v", cond, cf.revoked)
	}
}

// The HTTP client against a stand-in of the ContextForge 1.1 admin API: login, the calls of one sync, and that an
// error never carries a response body (it can contain a token).
func TestContextForgeHTTPClient(t *testing.T) {
	var gotAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		gotAuth = append(gotAuth, q.Header.Get("Authorization"))
		switch q.Method + " " + q.URL.Path {
		case "POST /auth/login":
			var b map[string]string
			_ = json.NewDecoder(q.Body).Decode(&b)
			if b["password"] != "Aa1!pw" {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"detail":"leaky-secret"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"adm"}`))
		case "GET /gateways":
			_, _ = w.Write([]byte(`[{"name":"web-docs"}]`))
		case "GET /tools":
			_, _ = w.Write([]byte(`[{"id":"1","name":"web-docs-search","gatewaySlug":"web-docs"}]`))
		case "GET /servers":
			_, _ = w.Write([]byte(`[]`))
		case "POST /servers":
			_, _ = w.Write([]byte(`{"id":"s1"}`))
		case "POST /tokens":
			_, _ = w.Write([]byte(`{"access_token":"agent-tok","token":{"id":"tid1"}}`))
		case "DELETE /tokens/tid1":
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if _, err := DialContextForge(srv.URL, srv.Client())(ctx, "a", "wrong"); err == nil || strings.Contains(err.Error(), "leaky") {
		t.Fatalf("bad login error: %v", err)
	}
	cf, err := DialContextForge(srv.URL, srv.Client())(ctx, "a", "Aa1!pw")
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := cf.Gateways(ctx); len(g) != 1 || g[0] != "web-docs" {
		t.Fatalf("gateways %v", g)
	}
	if ts, _ := cf.Tools(ctx); len(ts) != 1 || ts[0].GatewaySlug != "web-docs" {
		t.Fatalf("tools %v", ts)
	}
	if id, err := cf.PutServer(ctx, "", "project-web", []string{"1"}); err != nil || id != "s1" {
		t.Fatalf("server %q %v", id, err)
	}
	tok, tid, err := cf.MintToken(ctx, "n", "s1")
	if err != nil || tok != "agent-tok" || tid != "tid1" {
		t.Fatalf("token %q %q %v", tok, tid, err)
	}
	if err := cf.RevokeToken(ctx, "tid1"); err != nil {
		t.Fatal(err)
	}
	if gotAuth[len(gotAuth)-1] != "Bearer adm" {
		t.Fatalf("auth header %q", gotAuth[len(gotAuth)-1])
	}
}
