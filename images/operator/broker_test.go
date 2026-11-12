package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

func brokerPS(name, scope, owner string) *v1.ProjectSecret {
	ps := &v1.ProjectSecret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1.ProjectSecretSpec{Scope: scope, Mode: "broker", Broker: &v1.ProjectSecretBroker{Upstream: "https://npm.example.com/base"}}}
	if scope == "project" {
		ps.Spec.Project = owner
	} else { // scope agent records the agent's repository (fixtures: owner/repo, see agent())
		ps.Spec.Agent, ps.Spec.Project = owner, "owner/repo"
	}
	return ps
}

func envPS(name, scope, owner, variable string) *v1.ProjectSecret {
	ps := brokerPS(name, scope, owner)
	ps.Spec.Mode, ps.Spec.Broker = "env", nil
	ps.Spec.Env = &v1.ProjectSecretEnv{Name: variable, RiskAccepted: true}
	return ps
}

func TestBrokerRoutesScopes(t *testing.T) {
	agents := []v1.Agent{*agent("alice", nil), *agent("bob", nil), *agent("carol", func(a *v1.Agent) { a.Spec.Repo = "other/repo" })}
	secrets := []v1.ProjectSecret{*brokerPS("npm", "project", "owner/repo"), func() v1.ProjectSecret {
		p := brokerPS("mine", "agent", "carol")
		p.Spec.Project = "other/repo"
		return *p
	}(),
		*envPS("db", "project", "owner/repo", "DB_PASSWORD")}
	routes, src := brokerRoutes(secrets, agents)
	var m map[string]brokerRoute
	if err := json.Unmarshal([]byte(routes), &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || strings.Join(m["npm"].Agents, ",") != "alice,bob" || strings.Join(m["mine"].Agents, ",") != "carol" {
		t.Fatalf("routes %s", routes)
	}
	if m["npm"].Header != "Authorization" || m["npm"].Scheme != "bearer" || m["npm"].Upstream != "https://npm.example.com/base" {
		t.Errorf("defaults %+v", m["npm"])
	}
	if len(src) != 2 || src[0].Secret.Name != "ps-mine" || src[1].Secret.Items[0].Path != "npm" || src[1].Secret.Items[0].Key != "value" || !*src[0].Secret.Optional {
		t.Errorf("sources %+v", src)
	}
	again, _ := brokerRoutes(secrets, agents)
	if again != routes {
		t.Error("routes not stable")
	}
}

func TestBrokerSyncWritesRoutesAndVolume(t *testing.T) {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: brokerName, Namespace: ns}}
	dep.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: brokerValuesVol, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}}}}
	_, c := setup(t, agent("alice", nil), brokerPS("npm", "project", "owner/repo"), dep)
	s := &BrokerSync{Client: c, Direct: c, NS: ns}
	ctx := context.Background()
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: brokerRoutesCM}, &cm); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cm.Data[brokerRoutesKey], `"agents":["alice"]`) {
		t.Errorf("routes %s", cm.Data[brokerRoutesKey])
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: brokerName}, dep); err != nil {
		t.Fatal(err)
	}
	if src := dep.Spec.Template.Spec.Volumes[0].Projected.Sources; len(src) != 1 || src[0].Secret.Name != "ps-npm" {
		t.Errorf("volume sources %+v", src)
	}
	rv := dep.ResourceVersion
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	c.Get(ctx, types.NamespacedName{Namespace: ns, Name: brokerName}, dep)
	if dep.ResourceVersion != rv {
		t.Error("unchanged secrets restarted the broker")
	}
}

func TestSecretEnvAndBrokerEnv(t *testing.T) {
	r, c := setup(t, agent("alice", nil), envPS("db", "project", "owner/repo", "DB_PASSWORD"),
		envPS("other", "agent", "bob", "BOB_ONLY"), envPS("dup", "agent", "alice", "DB_PASSWORD"), brokerPS("npm", "project", "owner/repo"))
	r.D.Secrets, r.D.BrokerURL = true, "http://credential-broker.coding-agents.svc.cluster.local:8080"
	reconcile(t, r, "alice")
	s := getSTS(t, c, "alice")
	ag := container(s, "agent")
	var got []string
	for _, e := range ag.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && strings.HasPrefix(e.ValueFrom.SecretKeyRef.Name, "ps-") {
			got = append(got, e.Name+"="+e.ValueFrom.SecretKeyRef.Name)
		}
	}
	// dup and db both name DB_PASSWORD: the first by name (db) wins; bob's secret is not alice's
	if strings.Join(got, " ") != "DB_PASSWORD=ps-db" {
		t.Errorf("env secrets %v", got)
	}
	if envOf(s, "AGENTDEV_BROKER_URL") != r.D.BrokerURL || !hasEnv(ag, "AGENTDEV_BROKER_KEY") {
		t.Error("broker env missing")
	}
	fc := container(s, "forge")
	if hasEnv(fc, "DB_PASSWORD") {
		t.Error("env secret in the forge container")
	}
	// #202: the sidecar gets the value as a file for redaction; the agent container does not mount it
	if envOfC(fc, "REDACT_DIR") != redactDir {
		t.Error("REDACT_DIR missing in the forge container")
	}
	for _, m := range ag.VolumeMounts {
		if m.Name == "redact" {
			t.Error("redact volume in the agent container")
		}
	}
	var redact *corev1.Volume
	for i := range s.Spec.Template.Spec.Volumes {
		if s.Spec.Template.Spec.Volumes[i].Name == "redact" {
			redact = &s.Spec.Template.Spec.Volumes[i]
		}
	}
	if redact == nil || len(redact.Projected.Sources) != 1 || redact.Projected.Sources[0].Secret.Name != "ps-db" {
		t.Errorf("redact volume %+v", redact)
	}
	var cm corev1.ConfigMap
	c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: guidesCM}, &cm)
	if !strings.Contains(cm.Data["alice"], "npm: "+r.D.BrokerURL+"/s/npm/ in place of https://npm.example.com/base") {
		t.Errorf("guide %q", cm.Data["alice"])
	}
}

func TestSecretsOffByDefault(t *testing.T) {
	r, c := setup(t, agent("alice", nil), envPS("db", "project", "owner/repo", "DB_PASSWORD"))
	reconcile(t, r, "alice")
	if hasEnv(container(getSTS(t, c, "alice"), "agent"), "DB_PASSWORD") || hasEnv(container(getSTS(t, c, "alice"), "agent"), "AGENTDEV_BROKER_URL") {
		t.Error("secrets applied without SECRETS_STORE")
	}
}

func TestBrokerEventLineSource(t *testing.T) {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "credential-broker-x", Namespace: ns, UID: "b1", Labels: map[string]string{"app": "credential-broker"}}}
	spec, ok := parseProxyLine(`2026-11-08T03:00:00Z AGENTDEV_SECURITY_EVENT {"source":"credential-broker","rule":"broker-secret-denied","severity":"high","agent":"bob","action":"GET /s/npm"}`, p)
	if !ok || spec.Source != "credential-broker" || spec.Agent != "bob" || spec.Rule != "broker-secret-denied" {
		t.Fatalf("%+v %v", spec, ok)
	}
	spec, _ = parseProxyLine(`2026-11-08T03:00:00Z AGENTDEV_SECURITY_EVENT {"rule":"broker-key-invalid","severity":"medium","agent":"","action":"x"}`, p)
	if spec.Agent != "credential-broker" {
		t.Errorf("agent %q", spec.Agent)
	}
}

func envOfC(c *corev1.Container, k string) string {
	for _, e := range c.Env {
		if e.Name == k {
			return e.Value
		}
	}
	return ""
}

// Review of #272: a repository is (forge, name), and scope agent is bound to the agent's repository at creation.
func TestUsableByForgeAndRecreatedAgent(t *testing.T) {
	gh := agent("alice", func(a *v1.Agent) { a.Spec.Forge = "github" })
	if usableBy(brokerPS("npm", "project", "owner/repo"), gh) {
		t.Error("a Gitea project secret reached the GitHub agent of the same repository name")
	}
	ps := brokerPS("npm", "project", "owner/repo")
	ps.Spec.Forge = "github"
	if !usableBy(ps, gh) || usableBy(ps, agent("bob", nil)) {
		t.Error("forge github: wrong agents")
	}
	recreated := agent("carol", func(a *v1.Agent) { a.Spec.Repo = "else/where" })
	if usableBy(brokerPS("mine", "agent", "carol"), recreated) {
		t.Error("an agent recreated under the same name for another repository inherited the secret")
	}
	if !usableBy(brokerPS("mine", "agent", "carol"), agent("carol", nil)) || usableBy(brokerPS("mine", "agent", "carol"), agent("dave", nil)) {
		t.Error("scope agent: wrong agents")
	}
	if usableBy(&v1.ProjectSecret{Spec: v1.ProjectSecretSpec{Scope: "agent", Agent: "carol"}}, agent("carol", nil)) {
		t.Error("scope agent without a recorded repository applied")
	}
}

// Review of #272 (and #277): a mode-env secret named like a variable agentdev sets never overrides it, even when
// the CRD rule does not refuse the name; the attempt is a SecurityEvent.
func TestSecretEnvCannotOverridePlatformEnv(t *testing.T) {
	r, c := setup(t, agent("alice", nil), envPS("a-repo", "project", "owner/repo", "REPO"),
		envPS("b-poll", "project", "owner/repo", "POLL_SECONDS"), envPS("c-noproxy", "project", "owner/repo", "NO_PROXY"),
		envPS("d-db", "project", "owner/repo", "DB_PASSWORD"))
	r.D.Secrets, r.D.EgressProxy = true, "http://egress:3128"
	reconcile(t, r, "alice")
	ag := container(getSTS(t, c, "alice"), "agent")
	count := map[string]int{}
	for _, e := range ag.Env {
		count[e.Name]++
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && strings.HasPrefix(e.ValueFrom.SecretKeyRef.Name, "ps-") && e.Name != "DB_PASSWORD" {
			t.Errorf("%s comes from %s", e.Name, e.ValueFrom.SecretKeyRef.Name)
		}
	}
	for _, n := range []string{"REPO", "POLL_SECONDS", "NO_PROXY", "no_proxy", "DB_PASSWORD"} {
		if count[n] != 1 {
			t.Errorf("%s rendered %d times", n, count[n])
		}
	}
	if envOf(getSTS(t, c, "alice"), "POLL_SECONDS") != "20" || envOf(getSTS(t, c, "alice"), "REPO") != "owner/repo" {
		t.Error("platform value lost")
	}
	var evs v1.SecurityEventList
	_ = c.List(context.Background(), &evs)
	rules := 0
	for _, e := range evs.Items {
		if e.Spec.Rule == "secret-env-reserved" && e.Spec.Agent == "alice" && e.Spec.Severity == "high" {
			rules++
		}
	}
	if rules != 3 {
		t.Errorf("%d secret-env-reserved events, want 3", rules)
	}
	reconcile(t, r, "alice") // idempotent
	_ = c.List(context.Background(), &evs)
	if len(evs.Items) != 3 {
		t.Errorf("%d events after a second reconcile", len(evs.Items))
	}
}
