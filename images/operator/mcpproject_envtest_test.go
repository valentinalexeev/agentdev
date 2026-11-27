package main

// envtest (#383): the McpProject CRD and controller against a real API server (etcd + kube-apiserver). Skipped unless
// KUBEBUILDER_ASSETS points at the envtest binaries:
//   KUBEBUILDER_ASSETS=$(setup-envtest use 1.35.x -p path) go test -run Envtest ./...

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

func TestEnvtestMcpProject(t *testing.T) {
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
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, n := range []string{ns, platformNS} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}}); err != nil {
			t.Fatal(err)
		}
	}

	// schema: a bad server name and a bad agent name are refused by the API server
	bad := &v1.McpProject{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
		Spec: v1.McpProjectSpec{Servers: []v1.McpServer{{Name: "Bad_Name", URL: "http://x/mcp"}}}}
	if err := c.Create(ctx, bad); !apierrors.IsInvalid(err) {
		t.Fatalf("bad server name accepted: %v", err)
	}
	bad = &v1.McpProject{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns}, Spec: v1.McpProjectSpec{Agents: []string{"A!"}}}
	if err := c.Create(ctx, bad); !apierrors.IsInvalid(err) {
		t.Fatalf("bad agent name accepted: %v", err)
	}
	bad = &v1.McpProject{ObjectMeta: metav1.ObjectMeta{Name: "web.x", Namespace: ns}}
	if err := c.Create(ctx, bad); !apierrors.IsInvalid(err) {
		t.Fatalf("dotted project name accepted: %v", err)
	}

	// reconcile with a real API server: status subresource, finalizer, Secrets
	if err := c.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: CFAdminSecret, Namespace: platformNS},
		Data: map[string][]byte{cfAdminKey: []byte("Aa1!pw")}}); err != nil {
		t.Fatal(err)
	}
	p := mcpProject(docsServer, "alice")
	p.Generation = 0
	if err := c.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	cf := newFakeCF()
	r := &McpProjectReconciler{Client: c, Direct: c, NS: ns, PlatformNS: platformNS, CFURL: "http://cf:4444",
		AdminEmail: "a", Allowed: ParseMcpAllowed("docs-mcp.tools.svc.cluster.local"), Now: time.Now, Resync: time.Hour,
		Dial: func(context.Context, string, string) (ContextForge, error) { return cf, nil }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "web"}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var got v1.McpProject
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if cond := ready(&got); cond == nil || cond.Reason != "Synced" || len(got.Finalizers) != 1 {
		t.Fatalf("status %+v finalizers %v", got.Status, got.Finalizers)
	}
	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "agent-alice-mcp"}, &sec); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, req.NamespacedName, &got); !apierrors.IsNotFound(err) {
		t.Fatalf("McpProject not gone: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "agent-alice-mcp"}, &sec); !apierrors.IsNotFound(err) {
		t.Fatalf("Secret not gone: %v", err)
	}
}
