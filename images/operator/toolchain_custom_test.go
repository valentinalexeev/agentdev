package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

const (
	sumA = "1111111111111111111111111111111111111111111111111111111111111111"
	sumB = "2222222222222222222222222222222222222222222222222222222222222222"
	sumC = "3333333333333333333333333333333333333333333333333333333333333333"
	// the setup egress allow-list of the tests: the presets' hosts plus the admin's additions
	testSetupHosts = "dl.google.com static.rust-lang.org .github.com .githubusercontent.com deb.debian.org"
)

// shellTools is the #598 acceptance example: bats, make and shellcheck for one agent through instance config alone.
func shellTools() string {
	return `{"shell-tools": {
  "files": [
    {"name": "bats", "kind": "tar", "strip": 1,
     "arch": {"all": {"url": "https://github.com/bats-core/bats-core/archive/refs/tags/v1.11.1.tar.gz", "sha256": "` + sumA + `"}}},
    {"name": "shellcheck", "kind": "tar", "strip": 1,
     "arch": {"amd64": {"url": "https://github.com/koalaman/shellcheck/releases/download/v0.10.0/shellcheck-v0.10.0.linux.x86_64.tar.xz", "sha256": "` + sumB + `"},
              "arm64": {"url": "https://github.com/koalaman/shellcheck/releases/download/v0.10.0/shellcheck-v0.10.0.linux.aarch64.tar.xz", "sha256": "` + sumB + `"}}},
    {"name": "make", "kind": "deb",
     "arch": {"amd64": {"url": "https://deb.debian.org/debian/pool/main/m/make-dfsg/make_4.3-4.1_amd64.deb", "sha256": "` + sumC + `"},
              "arm64": {"url": "https://deb.debian.org/debian/pool/main/m/make-dfsg/make_4.3-4.1_arm64.deb", "sha256": "` + sumC + `"}}}
  ],
  "hosts": ["codeload.github.com", "objects.githubusercontent.com"],
  "path": ["bats/bin", "shellcheck", "make/usr/bin"],
  "verify": [["bats", "--version"], ["shellcheck", "--version"], ["make", "--version"]],
  "env": {"BATS_LIB_PATH": "$TOOLCHAIN/custom/shell-tools/bats/lib"},
  "allow": ["Bash(bats:*)", "Bash(make:*)", "Bash(shellcheck:*)"]
}}`
}

func withShellTools(a *v1.Agent) {
	a.Spec.Toolchain = &v1.AgentToolchain{Custom: []string{"shell-tools"}}
}

func customSetup(t *testing.T, raw string, a *v1.Agent) (*AgentReconciler, func() *appsv1.StatefulSet, func() *v1.Agent) {
	t.Helper()
	cat, err := parseToolchains(raw, testSetupHosts)
	if err != nil {
		t.Fatal(err)
	}
	r, c := setup(t, a)
	r.D.Toolchains = cat
	reconcile(t, r, a.Name)
	sts := func() *appsv1.StatefulSet { return getSTS(t, c, a.Name) }
	ag := func() *v1.Agent {
		var x v1.Agent
		_ = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: a.Name}, &x)
		return &x
	}
	return r, sts, ag
}

func specOf(t *testing.T, s *appsv1.StatefulSet) (string, toolchainSpec) {
	t.Helper()
	for _, ic := range s.Spec.Template.Spec.InitContainers {
		if ic.Name != toolchainContainer {
			continue
		}
		for _, e := range ic.Env {
			if e.Name == "TOOLCHAIN_SPEC" {
				var sp toolchainSpec
				if err := json.Unmarshal([]byte(e.Value), &sp); err != nil {
					t.Fatal(err)
				}
				return e.Value, sp
			}
		}
	}
	t.Fatal("no toolchain init container")
	return "", toolchainSpec{}
}

// #598 acceptance 1: an instance adds bats, make and shellcheck to one agent through instance config alone. The init
// container gets the downloads, the agent gets PATH, env and allow entries, the other agent nothing.
func TestCustomToolchainRendered(t *testing.T) {
	_, sts, _ := customSetup(t, shellTools(), agent("headlamp", withShellTools))
	s := sts()
	raw, sp := specOf(t, s)
	if len(sp.Presets) != 0 || !strings.Contains(raw, `"presets":[]`) || len(sp.Custom) != 1 {
		t.Fatalf("TOOLCHAIN_SPEC = %s", raw)
	}
	c := sp.Custom[0]
	if c.Name != "shell-tools" || len(c.Files) != 3 || c.Files[2].Kind != "deb" || c.Files[0].Arch["all"].SHA256 != sumA ||
		len(c.Verify) != 3 || strings.Join(c.Path, ",") != "bats/bin,shellcheck,make/usr/bin" {
		t.Errorf("custom spec = %+v", c)
	}
	// hosts, env and allow are the operator's: not in the spec, so a new allow entry does not install again
	for _, k := range []string{`"hosts"`, `"allow"`, `"env"`, "Bash(bats"} {
		if strings.Contains(raw, k) {
			t.Errorf("TOOLCHAIN_SPEC has %s: %s", k, raw)
		}
	}
	cur := "/state/toolchain/current/custom/shell-tools/"
	if got := envOf(s, "PATH"); got != cur+"bats/bin:"+cur+"shellcheck:"+cur+"make/usr/bin:"+imagePath {
		t.Errorf("PATH = %s", got)
	}
	if got := envOf(s, "BATS_LIB_PATH"); got != cur+"bats/lib" {
		t.Errorf("BATS_LIB_PATH = %s", got)
	}
	if got := envOf(s, "CLAUDE_TOOLS_EXTRA"); got != "Bash(bats:*),Bash(make:*),Bash(shellcheck:*)" {
		t.Errorf("CLAUDE_TOOLS_EXTRA = %s", got)
	}
	_, other, _ := customSetup(t, shellTools(), agent("alice", nil))
	if len(other().Spec.Template.Spec.InitContainers) != 1 || envOf(other(), "CLAUDE_TOOLS_EXTRA") != "<unset>" {
		t.Error("an agent without spec.toolchain got the custom toolchain")
	}
}

// #598: presets and custom toolchains together; the key of a presets-only agent does not change (no "custom" field).
func TestCustomToolchainWithPresets(t *testing.T) {
	_, sts, _ := customSetup(t, shellTools(), agent("alice", func(a *v1.Agent) {
		a.Spec.Toolchain = &v1.AgentToolchain{Presets: v1.ToolchainPresets{Go: "1.27.4"}, Custom: []string{"shell-tools"}}
	}))
	_, sp := specOf(t, sts())
	if len(sp.Presets) != 1 || len(sp.Custom) != 1 {
		t.Errorf("spec = %+v", sp)
	}
	if got := envOf(sts(), "PATH"); !strings.HasPrefix(got, "/state/toolchain/current/go/bin:/state/toolchain/current/custom/shell-tools/bats/bin:") {
		t.Errorf("PATH = %s", got)
	}
	if got := envOf(sts(), "CLAUDE_TOOLS_EXTRA"); !strings.HasPrefix(got, "Bash(go mod:*),") || !strings.HasSuffix(got, ",Bash(shellcheck:*)") {
		t.Errorf("CLAUDE_TOOLS_EXTRA = %s", got)
	}
	_, gosts, _ := customSetup(t, shellTools(), agent("bob", withGo))
	if raw, _ := specOf(t, gosts()); strings.Contains(raw, "custom") {
		t.Errorf("presets-only spec has a custom field: %s", raw)
	}
}

// #598: an Agent that names an unknown or an invalid custom toolchain is InvalidToolchain (no pod), with the reason.
func TestCustomToolchainInvalidAgent(t *testing.T) {
	bad := strings.Replace(shellTools(), `"deb.debian.org`, `"evil.example.com`, 1)
	bad = strings.Replace(bad, "https://deb.debian.org", "https://evil.example.com", -1)
	for name, tc := range map[string]struct {
		raw, want string
		custom    []string
	}{
		"unknown":       {shellTools(), "not a toolchain of this instance", []string{"nope"}},
		"twice":         {shellTools(), "named twice", []string{"shell-tools", "shell-tools"}},
		"host not open": {bad, "evil.example.com is not in the setup egress allow-list", []string{"shell-tools"}},
		"empty":         {shellTools(), "at least one preset or custom", nil},
	} {
		_, _, ag := customSetup(t, tc.raw, agent("mallory", func(a *v1.Agent) {
			a.Spec.Toolchain = &v1.AgentToolchain{Custom: tc.custom}
		}))
		a := ag()
		cond := meta.FindStatusCondition(a.Status.Conditions, "Ready")
		if a.Status.Phase != "invalid" || cond == nil || cond.Reason != "InvalidToolchain" || !strings.Contains(cond.Message, tc.want) {
			t.Errorf("%s: phase %q, condition %+v", name, a.Status.Phase, cond)
		}
	}
}

// #598: the checks of one custom toolchain. Each case breaks the valid example in one place.
func TestCustomToolchainChecks(t *testing.T) {
	cases := map[string][2]string{
		"preset name":        {`{"shell-tools":`, `{"go":`},
		"http url":           {`https://github.com/bats-core`, `http://github.com/bats-core`},
		"user info":          {`https://github.com/bats-core`, `https://x@github.com/bats-core`},
		"query":              {`v1.11.1.tar.gz"`, `v1.11.1.tar.gz?x=1"`},
		"short sha256":       {`"sha256": "` + sumA + `"`, `"sha256": "abc"`},
		"unknown kind":       {`"kind": "deb"`, `"kind": "sh"`},
		"strip on deb":       {`"kind": "deb"`, `"kind": "deb", "strip": 1`},
		"unknown arch":       {`"arm64": {"url": "https://deb.debian.org`, `"riscv64": {"url": "https://deb.debian.org`},
		"all with amd64":     {`"arch": {"all": {`, `"arch": {"amd64": {"url": "https://github.com/x", "sha256": "` + sumA + `"}, "all": {`},
		"file name dots":     {`"name": "bats"`, `"name": "../bats"`},
		"file name bin":      {`"name": "bats"`, `"name": "bin"`},
		"extra host closed":  {`"objects.githubusercontent.com"`, `"objects.example.org"`},
		"bad extra host":     {`"objects.githubusercontent.com"`, `"objects..githubusercontent.com"`},
		"path escape":        {`"make/usr/bin"`, `"make/../../.."`},
		"path absolute":      {`"make/usr/bin"`, `"/usr/bin"`},
		"empty verify":       {`["make", "--version"]`, `[]`},
		"reserved env":       {`"BATS_LIB_PATH"`, `"ANTHROPIC_BASE_URL"`},
		"reserved env PATH":  {`"BATS_LIB_PATH"`, `"PATH"`},
		"lower-case env":     {`"BATS_LIB_PATH"`, `"bats_lib"`},
		"allow with a comma": {`"Bash(make:*)"`, `"Bash(make:*),Bash(curl:*)"`},
	}
	for name, c := range cases {
		raw := strings.Replace(shellTools(), c[0], c[1], 1)
		if raw == shellTools() {
			t.Fatalf("%s: the case does not change the example", name)
		}
		cat, err := parseToolchains(raw, testSetupHosts)
		if err != nil {
			t.Errorf("%s: whole document refused: %v", name, err)
			continue
		}
		for n, tc := range cat {
			if tc.err == "" {
				t.Errorf("%s: %s accepted", name, n)
			}
		}
	}
	cat, err := parseToolchains(shellTools(), testSetupHosts)
	if err != nil || cat["shell-tools"] == nil || cat["shell-tools"].err != "" {
		t.Fatalf("valid example refused: %v %+v", err, cat["shell-tools"])
	}
	// a malformed document, an unknown field or a bad name stops the operator
	for _, raw := range []string{"not json", `{"x": {"files": [], "steps": []}}`, `{"Shell Tools": {}}`} {
		if _, err := parseToolchains(raw, testSetupHosts); err == nil {
			t.Errorf("%q: accepted", raw)
		}
	}
	// without a setup egress allow-list no custom toolchain is valid (fail closed)
	if cat, _ := parseToolchains(shellTools(), ""); cat["shell-tools"].err == "" {
		t.Error("accepted without SETUP_EGRESS_HOSTS")
	}
	if cat, err := parseToolchains("", testSetupHosts); err != nil || len(cat) != 0 {
		t.Errorf("empty TOOLCHAINS: %v %v", cat, err)
	}
}

// #598: a custom toolchain cannot set a variable that agentdev or a preset sets for the agent container (the proxy,
// Claude, the forge, the allow-list, PATH, GOPATH): a name already in the env is skipped, and the reserved names and
// prefixes are refused when the instance config is read.
func TestCustomToolchainEnvNoOverride(t *testing.T) {
	r, c := setup(t, agent("alice", func(a *v1.Agent) {
		a.Spec.Toolchain = &v1.AgentToolchain{Presets: v1.ToolchainPresets{Go: "1.27.4"}, Custom: []string{"shell-tools"}}
		a.Spec.Tools = &v1.AgentTools{Allow: []string{"Bash(npm ci:*)"}}
		a.Spec.Permission = &v1.AgentPermission{Mode: "auto"}
		a.Spec.MaxConcurrent = 2
	}))
	r.D.EgressProxy, r.D.ClaudeProxyURL, r.D.Approvals = "http://egress-proxy:3128", "http://claude-proxy:8080", "true"
	r.D.Toolchains, _ = parseToolchains(shellTools(), testSetupHosts)
	reconcile(t, r, "alice")
	before := map[string]string{}
	for _, e := range getSTS(t, c, "alice").Spec.Template.Spec.Containers[0].Env {
		before[e.Name] = e.Value
	}
	// every name of the rendered agent container as a custom variable: none may change, none may appear twice
	tc := r.D.Toolchains["shell-tools"]
	for k := range before {
		if k != "BATS_LIB_PATH" {
			tc.Env[k] = "pwned"
		}
	}
	if err := c.Delete(context.Background(), getSTS(t, c, "alice")); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "alice")
	seen := map[string]bool{}
	for _, e := range getSTS(t, c, "alice").Spec.Template.Spec.Containers[0].Env {
		if seen[e.Name] {
			t.Errorf("%s twice", e.Name)
		}
		seen[e.Name] = true
		if e.Value == "pwned" {
			t.Errorf("a custom toolchain overrides %s", e.Name)
		}
	}
	if before["BATS_LIB_PATH"] == "" {
		t.Error("the custom variable is missing")
	}
	for _, k := range []string{"PATH", "HTTPS_PROXY", "ANTHROPIC_BASE_URL", "CLAUDE_TOOLS_EXTRA", "FORGE_URL", "GIT_SSH_COMMAND",
		"LD_PRELOAD", "AGENT_NAME", "BASH_ENV"} {
		if customEnvAllowed(k) {
			t.Errorf("%s is not reserved", k)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	allow := strings.Fields(testSetupHosts)
	for h, want := range map[string]bool{"github.com": true, "api.github.com": true, "github.com.evil.org": false, "objects.githubusercontent.com": true,
		"githubusercontent.com": true, "evilgithubusercontent.com": false, "deb.debian.org": true} {
		if hostAllowed(h, allow) != want {
			t.Errorf("%s: want %v", h, want)
		}
	}
}

// #598: TOOLCHAINS reaches the operator intact. Ansible renders the JSON as the content of the double-quoted YAML string
// in manifests/operator.yaml.tmpl ((toolchains | to_json | replace('$', '\\u0024') | to_json)[1:-1]); quotes,
// backslashes and $ survive.
func TestCustomToolchainsManifestEnv(t *testing.T) {
	raw := strings.Replace(shellTools(), `"bats", "--version"`, `"sh", "-c", "echo \"a\\b\" $HOME"`, 1)
	if _, err := parseToolchains(raw, testSetupHosts); err != nil {
		t.Fatal(err)
	}
	var compact any
	if err := json.Unmarshal([]byte(raw), &compact); err != nil {
		t.Fatal(err)
	}
	inner, _ := json.Marshal(compact)
	inner = []byte(strings.ReplaceAll(string(inner), "$", `\u0024`))
	quoted, _ := json.Marshal(string(inner))
	docs := renderTmpl(t, "../../manifests/operator.yaml.tmpl", map[string]string{"AGENTS_NS": ns, "PLATFORM_NS": "agentdev-platform",
		"EXEC_NS": "agentdev-executors", "TELEMETRY_SCRAPER_SELECTOR": "{}", "TOOLCHAINS": string(quoted[1 : len(quoted)-1]),
		"SETUP_EGRESS_HOSTS": testSetupHosts})
	found := false
	for _, d := range docs {
		var dep appsv1.Deployment
		if yaml.Unmarshal([]byte(d), &dep) != nil || dep.Kind != "Deployment" || dep.Name != "agentdev-operator" {
			continue
		}
		env := map[string]string{}
		for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
			env[e.Name] = e.Value
		}
		if env["TOOLCHAINS"] != string(inner) || env["SETUP_EGRESS_HOSTS"] != testSetupHosts {
			t.Errorf("TOOLCHAINS = %s, SETUP_EGRESS_HOSTS = %s", env["TOOLCHAINS"], env["SETUP_EGRESS_HOSTS"])
		}
		cat, err := parseToolchains(env["TOOLCHAINS"], env["SETUP_EGRESS_HOSTS"])
		if err != nil || cat["shell-tools"] == nil || cat["shell-tools"].err != "" || cat["shell-tools"].Verify[0][2] != `echo "a\b" $HOME` {
			t.Errorf("parsed: %v %+v", err, cat["shell-tools"])
		}
		found = true
	}
	if !found {
		t.Fatal("no operator Deployment")
	}
}

// #598: with no custom toolchain in use the default is unchanged. An empty TOOLCHAINS is no catalogue, and a
// deployment that defines toolchains does not change the pod template of an agent without spec.toolchain or with
// presets only (no restart on upgrade).
func TestCustomToolchainDefaultUnchanged(t *testing.T) {
	for _, raw := range []string{"", "  ", "{}"} {
		if cat, err := parseToolchains(raw, ""); err != nil || len(cat) != 0 {
			t.Errorf("parseToolchains(%q) = %v, %v", raw, cat, err)
		}
	}
	for _, mut := range []func(*v1.Agent){nil, withGo} {
		r0, c0 := setup(t, agent("alice", mut))
		reconcile(t, r0, "alice")
		base := getSTS(t, c0, "alice").Spec.Template
		_, sts, _ := customSetup(t, shellTools(), agent("alice", mut))
		got := sts().Spec.Template
		b0, _ := json.Marshal(base)
		b1, _ := json.Marshal(got)
		if string(b0) != string(b1) {
			t.Errorf("pod template changed with TOOLCHAINS set:\n%s\n%s", b0, b1)
		}
	}
}
