package main

import (
	"context"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

const agentProxy = "http://egress-proxy.coding-agents.svc.cluster.local:3128"

func withHosts(hs ...string) func(*v1.Agent) {
	return func(a *v1.Agent) { a.Spec.EgressHosts = hs }
}

func agentEgressSecretOf(t *testing.T, c client.Client) *corev1.Secret {
	t.Helper()
	var s corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: agentEgressSecret}, &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

func containerEnv(t *testing.T, c client.Client, agentName, container string) map[string]corev1.EnvVar {
	t.Helper()
	for _, ct := range getSTS(t, c, agentName).Spec.Template.Spec.Containers {
		if ct.Name == container {
			return egressEnvOf(t, ct)
		}
	}
	t.Fatalf("%s: no container %s", agentName, container)
	return nil
}

// #599: an agent with spec.egressHosts gets its own key and Squid rule; the rule opens only its own hosts with only
// its own credential. Another agent gets no key, no rule and the proxy environment it had before.
func TestAgentEgressHosts(t *testing.T) {
	r, c := setup(t, agent("alice", withHosts("objects.githubusercontent.com", ".example.org", "BAD_host", ".example.org")),
		agent("bob", nil))
	r.D.EgressProxy = agentProxy
	reconcile(t, r, "alice")
	reconcile(t, r, "bob")

	s := agentEgressSecretOf(t, c)
	key := string(s.Data["key-alice"])
	if !hexKeyRE.MatchString(key) {
		t.Fatalf("alice key %q", key)
	}
	if _, ok := s.Data["key-bob"]; ok {
		t.Error("bob has no egressHosts but got a key")
	}
	acl := string(s.Data[agentEgressACL])
	cred := regexp.QuoteMeta(base64.StdEncoding.EncodeToString([]byte("agent-alice:" + key)))
	for _, want := range []string{
		"acl agent_alice_client req_header Proxy-Authorization ^Basic." + cred + "$\n",
		"acl agent_alice_hosts dstdomain -n .example.org objects.githubusercontent.com\n", // sorted, unique, valid only
		"http_access allow agent_alice_client agent_alice_hosts\n",
	} {
		if !strings.Contains(acl, want) {
			t.Errorf("acl.conf lacks %q:\n%s", want, acl)
		}
	}
	if strings.Contains(acl, "bob") || strings.Contains(acl, "bad_host") {
		t.Errorf("acl.conf has more than alice's rule:\n%s", acl)
	}

	var own corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-alice-egress-proxy"}, &own); err != nil ||
		string(own.Data["password"]) != key {
		t.Errorf("alice's copy of the key: %v %v", err, own.Data)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-bob-egress-proxy"}, &own); !apierrors.IsNotFound(err) {
		t.Errorf("bob got an egress key Secret: %v", err)
	}

	// alice's agent container: the credential from its own Secret, defined before the proxy URL (kubelet expansion)
	ae := containerEnv(t, c, "alice", "agent")
	k := ae["AGENT_EGRESS_KEY"]
	if k.Value != "" || k.ValueFrom == nil || k.ValueFrom.SecretKeyRef == nil ||
		k.ValueFrom.SecretKeyRef.Name != "agent-alice-egress-proxy" || k.ValueFrom.SecretKeyRef.Key != "password" {
		t.Fatalf("AGENT_EGRESS_KEY %+v", k)
	}
	want := "http://agent-alice:$(AGENT_EGRESS_KEY)@egress-proxy.coding-agents.svc.cluster.local:3128"
	for _, n := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
		if ae[n].Value != want {
			t.Errorf("alice agent %s=%q", n, ae[n].Value)
		}
	}
	idx := map[string]int{}
	for i, ct := range getSTS(t, c, "alice").Spec.Template.Spec.Containers[0].Env {
		idx[ct.Name] = i
	}
	if idx["AGENT_EGRESS_KEY"] > idx["HTTPS_PROXY"] {
		t.Error("AGENT_EGRESS_KEY after HTTPS_PROXY")
	}
	// the forge sidecar keeps its own credential and never gets the agent's
	fe := containerEnv(t, c, "alice", "forge")
	if _, ok := fe["AGENT_EGRESS_KEY"]; ok || !strings.Contains(fe["HTTPS_PROXY"].Value, "forge:$(EGRESS_FORGE_KEY)@") {
		t.Errorf("alice forge proxy env: %v", fe["HTTPS_PROXY"])
	}
	if a := getSTS(t, c, "alice").Annotations[reloaderAnnotation]; !strings.Contains(a, "agent-alice-egress-proxy") {
		t.Errorf("reloader annotation %q", a)
	}

	// bob: exactly the proxy environment without spec.egressHosts
	be := containerEnv(t, c, "bob", "agent")
	if _, ok := be["AGENT_EGRESS_KEY"]; ok || be["HTTPS_PROXY"].Value != agentProxy {
		t.Errorf("bob agent proxy env: %v %v", be["AGENT_EGRESS_KEY"], be["HTTPS_PROXY"])
	}
	if a := getSTS(t, c, "bob").Annotations[reloaderAnnotation]; strings.Contains(a, "agent-bob-egress-proxy") {
		t.Errorf("bob reloader annotation %q", a)
	}

	// the guide names alice's own hosts for alice only
	r.D.AgentEgressHosts = "pypi.org"
	if g := r.guideText(agent("alice", withHosts(".example.org")), nil); !strings.Contains(g, "pypi.org .example.org.") {
		t.Errorf("alice guide: %s", g)
	}
	if g := r.guideText(agent("bob", nil), nil); strings.Contains(g, "example.org") {
		t.Errorf("bob guide: %s", g)
	}

	// a second pass keeps the key (no restart of alice's pod, no reload of the proxy)
	reconcile(t, r, "alice")
	if got := string(agentEgressSecretOf(t, c).Data["key-alice"]); got != key {
		t.Error("alice's key changed on a second pass")
	}
}

// #599: removing spec.egressHosts or the agent drops its key and rule; without a proxy nothing is written.
func TestAgentEgressRemoved(t *testing.T) {
	r, c := setup(t, agent("alice", withHosts(".example.org")), agent("carol", withHosts("example.net")))
	r.D.EgressProxy = agentProxy
	reconcile(t, r, "alice")
	reconcile(t, r, "carol")

	var a v1.Agent
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "alice"}, &a); err != nil {
		t.Fatal(err)
	}
	a.Spec.EgressHosts = nil
	if err := c.Update(context.Background(), &a); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "alice")
	s := agentEgressSecretOf(t, c)
	if _, ok := s.Data["key-alice"]; ok || strings.Contains(string(s.Data[agentEgressACL]), "agent_alice_") {
		t.Errorf("alice keeps her rule after the field is removed:\n%s", s.Data[agentEgressACL])
	}
	if !strings.Contains(string(s.Data[agentEgressACL]), "agent_carol_hosts dstdomain -n example.net\n") {
		t.Error("carol lost her rule")
	}
	if _, ok := containerEnv(t, c, "alice", "agent")["AGENT_EGRESS_KEY"]; ok {
		t.Error("alice's pod keeps the egress key")
	}

	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "carol"}, &a); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), &a); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "carol"}}); err != nil {
		t.Fatal(err)
	}
	s = agentEgressSecretOf(t, c)
	if _, ok := s.Data["key-carol"]; ok || strings.Contains(string(s.Data[agentEgressACL]), "carol") {
		t.Errorf("a deleted agent keeps its rule:\n%s", s.Data[agentEgressACL])
	}

	r2, c2 := setup(t, agent("dave", withHosts(".example.org")))
	reconcile(t, r2, "dave") // no EGRESS_PROXY
	var none corev1.Secret
	if err := c2.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: agentEgressSecret}, &none); !apierrors.IsNotFound(err) {
		t.Errorf("without a proxy the operator wrote %s: %v", agentEgressSecret, err)
	}
	if _, ok := containerEnv(t, c2, "dave", "agent")["AGENT_EGRESS_KEY"]; ok {
		t.Error("without a proxy the agent got an egress key")
	}
}

// #599: only valid names and hex keys reach squid.conf.
func TestAgentEgressRulesFailClosed(t *testing.T) {
	as := []v1.Agent{*agent("ok", withHosts("a.example")), *agent("Bad_Name", withHosts("a.example")),
		*agent("nokey", withHosts("a.example")), *agent("nohosts", withHosts("not a host"))}
	keys := map[string]string{"ok": strings.Repeat("a", 48), "Bad_Name": strings.Repeat("b", 48), "nokey": "x\nhttp_access allow all",
		"nohosts": strings.Repeat("c", 48)}
	got := agentEgressRules(as, keys)
	if strings.Count(got, "http_access") != 1 || !strings.Contains(got, "agent_ok_hosts") {
		t.Errorf("rules:\n%s", got)
	}
}
