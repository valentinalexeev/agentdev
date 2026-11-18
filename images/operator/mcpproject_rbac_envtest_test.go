package main

// #452: with the Roles of manifests/operator.yaml.tmpl, the operator ServiceAccount runs the McpProject controller and
// cannot read claude-shared (or any other Secret) in the agents namespace. envtest runs kube-apiserver with RBAC.
//   KUBEBUILDER_ASSETS=$(setup-envtest use 1.35.x -p path) go test -run Envtest ./...

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

func TestEnvtestMcpOperatorCannotReadClaudeShared(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set")
	}
	env := &envtest.Environment{CRDInstallOptions: envtest.CRDInstallOptions{
		Paths: []string{filepath.Join("config", "crd", "agentdev.dev_mcpprojects.yaml")}}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = env.Stop() }()
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1.AddToScheme(s)
	admin, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, n := range []string{ns, platformNS} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}}); err != nil {
			t.Fatal(err)
		}
	}
	// the operator's Roles and RoleBindings exactly as the template renders them
	docs := renderTmpl(t, "../../manifests/operator.yaml.tmpl", map[string]string{"AGENTS_NS": ns, "PLATFORM_NS": platformNS,
		"EXEC_NS": "agentdev-executors"})
	for _, d := range docs {
		var tm metav1.TypeMeta
		_ = yaml.Unmarshal([]byte(d), &tm)
		var obj client.Object
		switch tm.Kind {
		case "Role":
			obj = &rbacv1.Role{}
		case "RoleBinding":
			obj = &rbacv1.RoleBinding{}
		default:
			continue
		}
		if err := yaml.Unmarshal([]byte(d), obj); err != nil {
			t.Fatal(err)
		}
		if n := obj.GetNamespace(); n != ns && n != platformNS {
			continue
		}
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("%s %s: %v", tm.Kind, obj.GetName(), err)
		}
	}
	for _, sec := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Name: CFAdminSecret, Namespace: platformNS}, Data: map[string][]byte{cfAdminKey: []byte("Aa1!pw")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "claude-shared", Namespace: ns}, Data: map[string][]byte{"token": []byte("sk-ant")}},
	} {
		if err := admin.Create(ctx, sec); err != nil {
			t.Fatal(err)
		}
	}
	sa := "system:serviceaccount:" + platformNS + ":agentdev-operator"
	ocfg := rest.CopyConfig(cfg)
	ocfg.Impersonate = rest.ImpersonationConfig{UserName: sa,
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + platformNS, "system:authenticated"}}
	op, err := client.New(ocfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}

	// kubectl auth can-i get/list/watch secret/claude-shared --as <operator SA> = no
	for _, verb := range []string{"get", "list", "watch"} {
		sar := &authv1.SubjectAccessReview{Spec: authv1.SubjectAccessReviewSpec{User: sa,
			Groups:             ocfg.Impersonate.Groups,
			ResourceAttributes: &authv1.ResourceAttributes{Namespace: ns, Verb: verb, Resource: "secrets", Name: "claude-shared"}}}
		if err := admin.Create(ctx, sar); err != nil {
			t.Fatal(err)
		}
		if sar.Status.Allowed {
			t.Errorf("operator can %s secret/claude-shared", verb)
		}
	}
	var got corev1.Secret
	if err := op.Get(ctx, types.NamespacedName{Namespace: ns, Name: "claude-shared"}, &got); !apierrors.IsForbidden(err) {
		t.Fatalf("get claude-shared as the operator: %v", err)
	}

	// the controller still does its whole job as the operator
	if err := admin.Create(ctx, mcpProject(docsServer, "alice")); err != nil {
		t.Fatal(err)
	}
	cf := newFakeCF()
	r := &McpProjectReconciler{Client: op, Direct: op, NS: ns, PlatformNS: platformNS, CFURL: "http://cf:4444",
		AdminEmail: "a", Allowed: ParseMcpAllowed("docs-mcp.tools.svc.cluster.local"), Now: time.Now, Resync: time.Hour,
		Dial: func(context.Context, string, string) (ContextForge, error) { return cf, nil }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "web"}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var p v1.McpProject
	if err := admin.Get(ctx, req.NamespacedName, &p); err != nil {
		t.Fatal(err)
	}
	if c := ready(&p); c == nil || c.Reason != "Synced" {
		t.Fatalf("Ready %+v", c)
	}
	if err := admin.Get(ctx, types.NamespacedName{Namespace: ns, Name: "agent-alice-mcp"}, &got); err != nil || len(got.Data["CF_TOKEN"]) == 0 {
		t.Fatalf("agent-alice-mcp: %v", err)
	}
	// renew (server changed) patches the Secret in place
	p.Status.Tokens[0].ServerID = "old"
	if err := admin.Status().Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if cf.minted != 2 || len(cf.revoked) != 1 {
		t.Fatalf("renew: minted %d revoked %v", cf.minted, cf.revoked)
	}
	if err := admin.Delete(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := admin.Get(ctx, types.NamespacedName{Namespace: ns, Name: "agent-alice-mcp"}, &got); !apierrors.IsNotFound(err) {
		t.Fatalf("Secret not gone: %v", err)
	}
}
