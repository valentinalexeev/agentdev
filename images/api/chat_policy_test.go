package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// #424: a console chat pod is the agent's pod (testdata/agent-pod-*.json: what the operator's render.go produces,
// kept equal by images/operator chatparity_test.go) with only the interactive changes of chatPod.

const (
	testWorkerImage = "ghcr.io/example/agentdev-worker:v0.0.0"
	testForgeImage  = "ghcr.io/example/agentdev-forge:v0.0.0"
)

func agentFixture(t *testing.T, forge string) corev1.PodTemplateSpec {
	t.Helper()
	b, err := os.ReadFile("testdata/agent-pod-" + forge + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var p corev1.PodTemplateSpec
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func chatJobFor(t *testing.T, forge string) *batchv1.Job {
	t.Helper()
	k := &kubeChat{cfg: chatJobConfig{Namespace: "coding-agents", WorkerImage: testWorkerImage, IdleSeconds: 1800, MaxSeconds: 28800}}
	j, err := k.jobFrom(agentFixture(t, forge), chatSession{ID: "chat-alice-0000abcd", Agent: "alice", Repo: "owner/repo", StartedBy: "wendy"}, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func containerNamed(p corev1.PodSpec, n string) *corev1.Container {
	for i := range p.Containers {
		if p.Containers[i].Name == n {
			return &p.Containers[i]
		}
	}
	return nil
}

func envMap(c *corev1.Container) map[string]corev1.EnvVar {
	m := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		m[e.Name] = e
	}
	return m
}

// The chat pod equals the agent pod except for the interactive entry point: the forge sidecar is the agent's sidecar
// (image, Secrets, egress key, security context, mounts) minus router registration and wakes.
func TestChatPodIsAgentPod(t *testing.T) {
	for _, forge := range []string{"gitea", "github"} {
		agent := agentFixture(t, forge).Spec
		p := chatJobFor(t, forge).Spec.Template.Spec
		af, cf := containerNamed(agent, "forge"), containerNamed(p, "forge")
		if cf == nil || containerNamed(p, "chat") == nil || len(p.Containers) != 2 {
			t.Fatalf("%s: containers %v", forge, p.Containers)
		}
		if cf.Image != af.Image || cf.SecurityContext.String() != af.SecurityContext.String() || cf.Resources.String() != af.Resources.String() {
			t.Errorf("%s: forge sidecar differs from the agent's", forge)
		}
		if len(cf.VolumeMounts) != len(af.VolumeMounts) {
			t.Errorf("%s: forge mounts %v, agent %v", forge, cf.VolumeMounts, af.VolumeMounts)
		}
		want, got := envMap(af), envMap(cf)
		for n, e := range want {
			e := e
			switch n {
			case "WAKE_TOKEN", "REGISTER_TOKEN", "SELF_URL":
				if _, ok := got[n]; ok {
					t.Errorf("%s: chat sidecar keeps %s (router registration)", forge, n)
				}
			case "APPROVALS":
				if got[n].Value != "false" {
					t.Errorf("%s: chat sidecar APPROVALS=%q", forge, got[n].Value)
				}
			default:
				if g := got[n]; g.String() != e.String() {
					t.Errorf("%s: forge env %s = %v, agent %v", forge, n, got[n], e)
				}
			}
		}
		if len(cf.Ports) != 0 {
			t.Errorf("%s: chat sidecar listens for wakes: %v", forge, cf.Ports)
		}
		// pod level: same identity, runtime, scheduling, volumes (plus the emptyDir state)
		if p.ServiceAccountName != agent.ServiceAccountName || *p.RuntimeClassName != *agent.RuntimeClassName ||
			*p.AutomountServiceAccountToken || p.SecurityContext.RunAsUser == nil || *p.ShareProcessNamespace ||
			len(p.Tolerations) != len(agent.Tolerations) || len(p.Volumes) != len(agent.Volumes)+1 || len(p.InitContainers) != len(agent.InitContainers) {
			t.Errorf("%s: pod spec differs from the agent's: %+v", forge, p)
		}
		// chat container: the agent container's env, Secrets and mounts, plus the session settings
		ac, cc := containerNamed(agent, "agent"), containerNamed(p, "chat")
		ce := envMap(cc)
		for n, e := range envMap(ac) {
			e := e
			if n == "IDLE_SECONDS" { // the session's idle end (CHAT_IDLE_SECONDS), not the agent's scale-to-zero
				continue
			}
			if got := ce[n]; got.String() != e.String() {
				t.Errorf("%s: chat env %s = %v, agent %v", forge, n, got, e)
			}
		}
		if ce["FORGE_API"].Value != "http://127.0.0.1:7070" || ce["CHAT_MODE"].Value != "console" || ce["CHAT_CONTROL_SHA256"].Value != strings.Repeat("a", 64) {
			t.Errorf("%s: chat env %v", forge, ce)
		}
		if cc.Image != ac.Image || cc.Command[0] != "agent-chat-session" || len(cc.EnvFrom) != len(ac.EnvFrom) || len(cc.VolumeMounts) != len(ac.VolumeMounts) {
			t.Errorf("%s: chat container %+v", forge, cc)
		}
		for _, e := range cc.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && (e.ValueFrom.SecretKeyRef.Name == "agent-alice" || e.ValueFrom.SecretKeyRef.Name == "egress-forge-proxy") {
				t.Errorf("%s: the chat container reads %s", forge, e.ValueFrom.SecretKeyRef.Name)
			}
			if strings.Contains(e.Value, "EGRESS_FORGE_KEY") || e.Name == "FORGE_TOKEN" || e.Name == "EGRESS_FORGE_KEY" {
				t.Errorf("%s: the chat container has %s", forge, e.Name)
			}
		}
		if lb := chatJobFor(t, forge).Spec.Template.Labels; lb["app"] != "agent-chat" || lb["agent"] != "" {
			t.Errorf("%s: pod labels %v", forge, lb)
		}
	}
}

// ---- admission policy agentdev-api-chat-jobs (manifests/api.yaml.tmpl), evaluated with CEL on Job objects

type vapDoc struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Variables []struct {
			Name       string `json:"name"`
			Expression string `json:"expression"`
		} `json:"variables"`
		Validations []struct {
			Expression string `json:"expression"`
			Message    string `json:"message"`
		} `json:"validations"`
	} `json:"spec"`
}

func chatPolicy(t *testing.T) vapDoc {
	t.Helper()
	b, err := os.ReadFile("../../manifests/api.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.NewReplacer("${AGENTS_NS}", "coding-agents", "${PLATFORM_NS}", "agentdev-platform",
		"${CHAT_WORKER_IMAGE}", testWorkerImage, "${CHAT_FORGE_IMAGE}", testForgeImage).Replace(string(b))
	for _, d := range strings.Split(s, "\n---\n") {
		var v vapDoc
		if yaml.Unmarshal([]byte(d), &v) == nil && v.Kind == "ValidatingAdmissionPolicy" && v.Metadata.Name == "agentdev-api-chat-jobs" {
			return v
		}
	}
	t.Fatal("policy agentdev-api-chat-jobs not found")
	return vapDoc{}
}

// admit returns the messages of the failed validations ("" = admitted).
func admit(t *testing.T, pol vapDoc, j *batchv1.Job) []string {
	t.Helper()
	b, _ := json.Marshal(j)
	var obj map[string]any
	_ = json.Unmarshal(b, &obj)
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("variables", cel.MapType(cel.StringType, cel.DynType)))
	if err != nil {
		t.Fatal(err)
	}
	eval := func(expr string, vars map[string]any) (any, error) {
		ast, iss := env.Compile(expr)
		if iss != nil && iss.Err() != nil {
			t.Fatalf("compile %q: %v", expr, iss.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := prg.Eval(map[string]any{"object": obj, "variables": vars})
		if err != nil {
			return nil, err
		}
		return out.Value(), nil
	}
	vars := map[string]any{}
	for _, v := range pol.Spec.Variables {
		val, err := eval(v.Expression, vars)
		if err != nil {
			t.Fatalf("variable %s: %v", v.Name, err)
		}
		vars[v.Name] = val
	}
	var failed []string
	for _, v := range pol.Spec.Validations {
		ok, err := eval(v.Expression, vars)
		if err != nil || ok != true {
			failed = append(failed, v.Message)
		}
	}
	return failed
}

func TestChatJobAdmission(t *testing.T) {
	pol := chatPolicy(t)
	for _, forge := range []string{"gitea", "github"} {
		if f := admit(t, pol, chatJobFor(t, forge)); len(f) != 0 {
			t.Errorf("%s: the chat Job of the API is refused: %v", forge, f)
		}
	}
	secretEnv := func(n, sec string) corev1.EnvVar {
		return corev1.EnvVar{Name: n, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: sec}, Key: n}}}
	}
	cases := []struct {
		name string
		mut  func(*corev1.PodSpec, *batchv1.Job)
	}{
		{"forge token on the chat container", func(p *corev1.PodSpec, _ *batchv1.Job) {
			c := containerNamed(*p, "chat")
			c.Env = append(c.Env, secretEnv("FORGE_TOKEN", "agent-alice"))
		}},
		{"egress forge key on the chat container", func(p *corev1.PodSpec, _ *batchv1.Job) {
			c := containerNamed(*p, "chat")
			c.Env = append(c.Env, corev1.EnvVar{Name: "EGRESS_FORGE_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "egress-forge-proxy"}, Key: "password"}}})
		}},
		{"another agent's Secret in the sidecar", func(p *corev1.PodSpec, _ *batchv1.Job) {
			c := containerNamed(*p, "forge")
			c.Env = append(c.Env, secretEnv("FORGE_TOKEN", "agent-bob"))
		}},
		{"another agent's MCP Secret", func(p *corev1.PodSpec, _ *batchv1.Job) {
			c := containerNamed(*p, "chat")
			c.EnvFrom = append(c.EnvFrom, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "agent-bob-mcp"}}})
		}},
		{"a platform Secret", func(p *corev1.PodSpec, _ *batchv1.Job) {
			c := containerNamed(*p, "chat")
			c.Env = append(c.Env, secretEnv("X", "agentdev-api-forge"))
		}},
		{"an extra container", func(p *corev1.PodSpec, _ *batchv1.Job) {
			p.Containers = append(p.Containers, corev1.Container{Name: "x", Image: testWorkerImage})
		}},
		{"no forge sidecar", func(p *corev1.PodSpec, _ *batchv1.Job) { p.Containers = p.Containers[:1] }},
		{"another image in the sidecar", func(p *corev1.PodSpec, _ *batchv1.Job) { containerNamed(*p, "forge").Image = "busybox:latest" }},
		{"the forge image as the chat container", func(p *corev1.PodSpec, _ *batchv1.Job) { containerNamed(*p, "chat").Image = testForgeImage }},
		{"another init container", func(p *corev1.PodSpec, _ *batchv1.Job) {
			p.InitContainers = append(p.InitContainers, corev1.Container{Name: "clone", Image: testWorkerImage})
		}},
		{"another service account", func(p *corev1.PodSpec, _ *batchv1.Job) { p.ServiceAccountName = "agent-bob" }},
		{"a Secret volume", func(p *corev1.PodSpec, _ *batchv1.Job) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "x", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "agentdev-api-forge"}}})
		}},
		{"another agent's GitHub App key", func(p *corev1.PodSpec, _ *batchv1.Job) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "github-app", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "agent-bob"}}})
		}},
		{"the GitHub App key in the chat container", func(p *corev1.PodSpec, _ *batchv1.Job) {
			c := containerNamed(*p, "chat")
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "github-app", MountPath: "/x"})
		}},
		{"a shared process namespace", func(p *corev1.PodSpec, _ *batchv1.Job) { p.ShareProcessNamespace = ptr(true) }},
		{"added capabilities", func(p *corev1.PodSpec, _ *batchv1.Job) {
			containerNamed(*p, "chat").SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}}}
		}},
		{"the agent label (agent Service, approvals)", func(_ *corev1.PodSpec, j *batchv1.Job) { j.Spec.Template.Labels["agent"] = "alice" }},
	}
	for _, tc := range cases {
		j := chatJobFor(t, "github")
		tc.mut(&j.Spec.Template.Spec, j)
		if f := admit(t, pol, j); len(f) == 0 {
			t.Errorf("admitted: %s", tc.name)
		}
	}
}

// #499: an agent with a toolchain has the init container toolchain with its own setup proxy key, and chat and forge
// mount /state/toolchain read-only. Anything that weakens that is refused.
func TestChatJobAdmissionToolchain(t *testing.T) {
	pol := chatPolicy(t)
	if f := admit(t, pol, chatJobFor(t, "toolchain")); len(f) != 0 {
		t.Fatalf("the chat Job of an agent with a toolchain is refused: %v", f)
	}
	init := func(p *corev1.PodSpec, n string) *corev1.Container {
		for i := range p.InitContainers {
			if p.InitContainers[i].Name == n {
				return &p.InitContainers[i]
			}
		}
		t.Fatalf("no init container %s", n)
		return nil
	}
	cases := []struct {
		name string
		mut  func(*corev1.PodSpec)
	}{
		{"another agent's setup key", func(p *corev1.PodSpec) {
			c := init(p, "toolchain")
			for i := range c.Env {
				if r := c.Env[i].ValueFrom; r != nil && r.SecretKeyRef != nil {
					r.SecretKeyRef.Name = "agent-bob-setup-proxy"
				}
			}
		}},
		{"the setup key in the chat container", func(p *corev1.PodSpec) {
			c := containerNamed(*p, "chat")
			c.Env = append(c.Env, corev1.EnvVar{Name: "K", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "agent-alice-setup-proxy"}, Key: "password"}}})
		}},
		{"the forge token in toolchain", func(p *corev1.PodSpec) {
			c := init(p, "toolchain")
			c.Env = append(c.Env, corev1.EnvVar{Name: "T", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "agent-alice"}, Key: "token"}}})
		}},
		{"another agent's toolchain hash", func(p *corev1.PodSpec) {
			c := init(p, "toolchain")
			for i := range c.Env {
				if r := c.Env[i].ValueFrom; r != nil && r.ConfigMapKeyRef != nil {
					r.ConfigMapKeyRef.Key = "bob"
				}
			}
		}},
		{"toolchain as root", func(p *corev1.PodSpec) { init(p, "toolchain").SecurityContext.RunAsUser = ptr(int64(0)) }},
		{"toolchain with a writable root", func(p *corev1.PodSpec) { init(p, "toolchain").SecurityContext.ReadOnlyRootFilesystem = ptr(false) }},
		{"toolchain with another image", func(p *corev1.PodSpec) { init(p, "toolchain").Image = "busybox:latest" }},
		{"two toolchain init containers", func(p *corev1.PodSpec) {
			p.InitContainers = append(p.InitContainers, *init(p, "toolchain").DeepCopy())
		}},
		{"/state/toolchain writable in chat", func(p *corev1.PodSpec) {
			c := containerNamed(*p, "chat")
			for i := range c.VolumeMounts {
				if c.VolumeMounts[i].MountPath == "/state/toolchain" {
					c.VolumeMounts[i].ReadOnly = false
				}
			}
		}},
		{"no /state/toolchain mount in forge", func(p *corev1.PodSpec) {
			c := containerNamed(*p, "forge")
			ms := c.VolumeMounts[:0]
			for _, m := range c.VolumeMounts {
				if m.MountPath != "/state/toolchain" {
					ms = append(ms, m)
				}
			}
			c.VolumeMounts = ms
		}},
	}
	for _, tc := range cases {
		j := chatJobFor(t, "toolchain")
		tc.mut(&j.Spec.Template.Spec)
		if f := admit(t, pol, j); len(f) == 0 {
			t.Errorf("admitted: %s", tc.name)
		}
	}
}
