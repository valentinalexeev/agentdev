package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// #424: a console chat pod is the agent's pod. The API copies the pod template this operator renders (render.go) for
// the StatefulSet agent-<name> and changes only the interactive parts (images/api/chat_kube.go chatPod). This test
// keeps the API's fixtures equal to what render produces today, so the API tests (chat pod = agent pod + interactive
// diffs, and the admission policy against that shape) always run on the real agent pod. UPDATE=1 rewrites them.
func TestAgentPodTemplateFixturesForChat(t *testing.T) {
	for _, tc := range []struct {
		file string
		mut  func(*v1.Agent)
	}{
		{"agent-pod-gitea.json", nil},
		{"agent-pod-github.json", func(a *v1.Agent) { a.Spec.Forge = "github"; a.Spec.Repo = "octo-org/repo" }},
		// #499: the init container toolchain and the read-only /state/toolchain mounts (chat Job admission policy)
		{"agent-pod-toolchain.json", func(a *v1.Agent) {
			a.Spec.Toolchain = &v1.AgentToolchain{Presets: v1.ToolchainPresets{Go: "1.27.4"}}
		}},
		// #599: an agent with its own egress hosts: the agent container's proxy URL carries agent-<name>-egress-proxy
		{"agent-pod-egress.json", func(a *v1.Agent) { a.Spec.EgressHosts = []string{".example.org"} }},
	} {
		r, c := setup(t, agent("alice", tc.mut))
		r.D.ForgeImage, r.D.WorkerImage, r.D.PullPolicy = "ghcr.io/example/agentdev-forge:v0.0.0", "ghcr.io/example/agentdev-worker:v0.0.0", "IfNotPresent"
		r.D.GitHubURL = "https://github.com"
		r.D.EgressProxy = "http://egress-proxy.coding-agents.svc.cluster.local:3128"
		r.D.ClaudeProxyURL = "http://claude-proxy.coding-agents.svc.cluster.local:8080"
		r.D.Approvals, r.D.ApprovalsURL, r.D.ApprovalTimeout = "true", "http://agentdev-api-internal.agentdev-platform.svc.cluster.local:8081", "30"
		reconcile(t, r, "alice")
		s := getSTS(t, c, "alice")
		got, err := json.MarshalIndent(s.Spec.Template, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, '\n')
		path := filepath.Join("..", "api", "testdata", tc.file)
		if os.Getenv("UPDATE") == "1" {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run UPDATE=1 go test -run TestAgentPodTemplateFixturesForChat)", err)
		}
		if string(want) != string(got) {
			t.Errorf("%s is not the agent pod render.go produces: run UPDATE=1 go test -run TestAgentPodTemplateFixturesForChat in images/operator", path)
		}
	}
}
