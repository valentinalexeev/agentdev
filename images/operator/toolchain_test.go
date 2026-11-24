package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

func TestNoToolchainNoSetup(t *testing.T) { // #499: an agent without toolchain keeps today's pod template
	r, c := setup(t, agent("ida", nil))
	reconcile(t, r, "ida")
	p := getSTS(t, c, "ida").Spec.Template.Spec
	if len(p.InitContainers) != 1 || p.InitContainers[0].Name != "state-init" {
		t.Fatalf("init containers %v, want only state-init", p.InitContainers)
	}
	if v := envOf(getSTS(t, c, "ida"), "PATH"); v != "<unset>" {
		t.Fatalf("PATH set without toolchain: %q", v)
	}
}

func TestToolchainSetupContainer(t *testing.T) { // #499
	r, c := setup(t, agent("joe", func(a *v1.Agent) {
		a.Spec.Toolchain = &v1.Toolchain{Steps: []v1.ToolchainStep{{Name: "go", Run: []string{"sh", "-c", "true"},
			Verify: []string{"/state/toolchain/go/bin/go", "version"}}}, Path: []string{"/state/toolchain/go/bin"}, Refresh: "24h"}
	}))
	r.D.EgressProxy = "http://egress:3128"
	reconcile(t, r, "joe")
	s := getSTS(t, c, "joe")
	p := s.Spec.Template.Spec
	if len(p.InitContainers) != 2 || p.InitContainers[1].Name != "toolchain" {
		t.Fatalf("init containers %v, want state-init then toolchain", p.InitContainers)
	}
	tc := p.InitContainers[1]
	if tc.Image != "worker:dev" {
		t.Fatalf("setup image %q, want the worker image", tc.Image)
	}
	// no credential of any kind: only plain values, the state volume and the proxy
	for _, e := range tc.Env {
		if e.ValueFrom != nil {
			t.Fatalf("setup container reads %s from %v", e.Name, e.ValueFrom)
		}
	}
	if len(tc.EnvFrom) != 0 || len(tc.VolumeMounts) != 1 || tc.VolumeMounts[0].Name != "state" {
		t.Fatalf("setup container: envFrom %v, mounts %v", tc.EnvFrom, tc.VolumeMounts)
	}
	envs := map[string]string{}
	for _, e := range tc.Env {
		envs[e.Name] = e.Value
	}
	if envs["HTTPS_PROXY"] != "http://egress:3128" || envs["TOOLCHAIN_REFRESH_SECONDS"] != "86400" {
		t.Fatalf("setup env %v", envs)
	}
	var spec v1.Toolchain
	if err := json.Unmarshal([]byte(envs["TOOLCHAIN_SPEC"]), &spec); err != nil || spec.Steps[0].Name != "go" {
		t.Fatalf("TOOLCHAIN_SPEC %q: %v", envs["TOOLCHAIN_SPEC"], err)
	}
	if sc := tc.SecurityContext; sc == nil || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem {
		t.Fatalf("setup security context %v", sc)
	}
	if v := envOf(s, "PATH"); v != "/state/toolchain/go/bin:"+imagePath {
		t.Fatalf("agent PATH %q", v)
	}
}

func TestCRDHasToolchain(t *testing.T) {
	b, err := os.ReadFile("config/crd/agentdev.dev_agents.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"toolchain:", "failurePolicy:", "timeoutSeconds:", "refresh:"} {
		if !strings.Contains(string(b), k) {
			t.Fatalf("CRD has no %s (run controller-gen)", k)
		}
	}
}

// runSetup runs the embedded setup script as the init container would, with dir as /state/toolchain.
func runSetup(t *testing.T, dir string, tc v1.Toolchain, refresh string) (string, int) {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	spec, _ := json.Marshal(tc)
	cmd := exec.Command(py, "-I", "-c", toolchainSetup)
	cmd.Env = []string{"TOOLCHAIN_SPEC=" + string(spec), "TOOLCHAIN_DIR=" + dir, "TOOLCHAIN_REFRESH_SECONDS=" + refresh}
	out, _ := cmd.CombinedOutput()
	return string(out), cmd.ProcessState.ExitCode()
}

func TestToolchainSetupScript(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "toolchain")
	ok := v1.Toolchain{Steps: []v1.ToolchainStep{{Name: "mk", Run: []string{"sh", "-c", "echo run >> count; mkdir -p bin"},
		Verify: []string{"echo", "tool 1.2"}}}}
	if out, code := runSetup(t, dir, ok, "0"); code != 0 || !strings.Contains(out, "step mk ok") {
		t.Fatalf("first run: exit %d\n%s", code, out)
	}
	var st struct {
		Steps []map[string]any `json:"steps"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "status.json"))
	if err := json.Unmarshal(b, &st); err != nil || st.Steps[0]["version"] != "tool 1.2" {
		t.Fatalf("status %s: %v", b, err)
	}
	// same steps, all passed: a wake skips them
	if out, code := runSetup(t, dir, ok, "0"); code != 0 || !strings.Contains(out, "up to date") {
		t.Fatalf("second run: exit %d\n%s", code, out)
	}
	if c, _ := os.ReadFile(filepath.Join(dir, "count")); strings.Count(string(c), "run") != 1 {
		t.Fatalf("step ran %d times, want 1", strings.Count(string(c), "run"))
	}
	// refresh passed (1 s, status older): the steps run again
	st2 := map[string]any{}
	_ = json.Unmarshal(b, &st2)
	st2["finished"] = 0
	b2, _ := json.Marshal(st2)
	_ = os.WriteFile(filepath.Join(dir, "status.json"), b2, 0o644)
	if out, code := runSetup(t, dir, ok, "1"); code != 0 || strings.Contains(out, "up to date") {
		t.Fatalf("refresh run: exit %d\n%s", code, out)
	}

	bad := v1.Toolchain{Steps: []v1.ToolchainStep{{Name: "bad", Run: []string{"false"}}, {Name: "never", Run: []string{"true"}}}}
	if out, code := runSetup(t, dir, bad, "0"); code != 1 || !strings.Contains(out, "step bad failed") || strings.Contains(out, "step never") {
		t.Fatalf("failing step, policy Fail: exit %d\n%s", code, out)
	}
	bad.FailurePolicy = "Continue"
	if out, code := runSetup(t, dir, bad, "0"); code != 0 || !strings.Contains(out, "FAILED") {
		t.Fatalf("failing step, policy Continue: exit %d\n%s", code, out)
	}
	slow := v1.Toolchain{Steps: []v1.ToolchainStep{{Name: "slow", Run: []string{"sleep", "5"}, TimeoutSeconds: 1}}}
	if out, code := runSetup(t, dir, slow, "0"); code != 1 || !strings.Contains(out, "step slow timeout") {
		t.Fatalf("time limit: exit %d\n%s", code, out)
	}
}
