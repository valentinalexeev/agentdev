package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

const testSigning = "0123456789abcdef0123456789abcdef0123456789abcdef"

func proxyFixture(objs ...client.Object) (*AgentReconciler, client.Client, *v1.Agent) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1.AddToScheme(s)
	a := &v1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "ola", Namespace: ns, UID: "u1"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, a)...).Build()
	return &AgentReconciler{Client: c, D: Defaults{ClaudeProxyURL: "http://claude-proxy:8080"}}, c, a
}

func signingSecret(key string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: proxySigningSecret, Namespace: ns},
		Data: map[string][]byte{"key": []byte(key + "\n")}}
}

// The value must equal the playbook's openssl HMAC and credproxy's agentKey, or every agent call gets 401.
func TestProxyKeyMatchesCredproxyFormat(t *testing.T) {
	// printf 'agentdev-claude-proxy/v1:ola' | openssl dgst -sha256 -hmac 0123...cdef -r
	got := proxyKey([]byte(testSigning), "ola")
	if want := "ola.d18c27b6c7aa75346d58d3ca8bb08bed8b3135485d776b72fa5dca5ee403f55b"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestEnsureProxyKeyWritesSecret(t *testing.T) {
	r, c, a := proxyFixture(signingSecret(testSigning))
	ok, err := r.ensureProxyKey(context.Background(), a)
	if err != nil || !ok {
		t.Fatalf("ensureProxyKey: %v %v", ok, err)
	}
	var s corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-ola-proxy"}, &s); err != nil {
		t.Fatal(err)
	}
	if string(s.Data["ANTHROPIC_AUTH_TOKEN"]) != proxyKey([]byte(testSigning), "ola") {
		t.Fatalf("value differs")
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Name != "ola" {
		t.Fatalf("owner: %+v", s.OwnerReferences)
	}
	rv := s.ResourceVersion
	if _, err := r.ensureProxyKey(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-ola-proxy"}, &s)
	if s.ResourceVersion != rv {
		t.Fatalf("second run changed the Secret (would restart the pod)")
	}
}

func TestEnsureProxyKeyRotates(t *testing.T) {
	r, c, a := proxyFixture(signingSecret(testSigning))
	if _, err := r.ensureProxyKey(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	var sig corev1.Secret
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: proxySigningSecret}, &sig)
	other := "fedcba9876543210fedcba9876543210fedcba9876543210"
	sig.Data["key"] = []byte(other)
	_ = c.Update(context.Background(), &sig)
	if _, err := r.ensureProxyKey(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	var s corev1.Secret
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-ola-proxy"}, &s)
	if string(s.Data["ANTHROPIC_AUTH_TOKEN"]) != proxyKey([]byte(other), "ola") {
		t.Fatalf("not rotated")
	}
}

func TestEnsureProxyKeyWithoutSigningOrProxy(t *testing.T) {
	r, c, a := proxyFixture()
	if ok, err := r.ensureProxyKey(context.Background(), a); ok || err != nil {
		t.Fatalf("no signing secret: %v %v", ok, err)
	}
	r2, _, a2 := proxyFixture(signingSecret("short"))
	if _, err := r2.ensureProxyKey(context.Background(), a2); err == nil {
		t.Fatalf("a short signing key must fail")
	}
	r.D.ClaudeProxyURL = ""
	if ok, _ := r.ensureProxyKey(context.Background(), a); ok {
		t.Fatalf("proxy off: no key")
	}
	var l corev1.SecretList
	_ = c.List(context.Background(), &l)
	if len(l.Items) != 0 {
		t.Fatalf("secrets written: %d", len(l.Items))
	}
}

func TestReloadAnnotationOnAgent(t *testing.T) {
	r := &AgentReconciler{D: Defaults{ClaudeProxyURL: "http://p", EgressProxy: "http://e:3128"}}
	a := &v1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "ola"}}
	if got := r.reloadSecrets(a, "agent-ola"); got != "agent-ola,agent-ola-mcp,egress-forge-proxy,agent-ola-proxy" {
		t.Fatalf("reload: %s", got)
	}
}
