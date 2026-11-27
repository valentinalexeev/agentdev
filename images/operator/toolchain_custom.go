package main

// Custom toolchains of the deployment (#598, ADR 0010 D2 amendment). The presets of toolchain.go are compiled into the
// operator, so every new tool would be an agentdev release. A deployment defines its own toolchains in the instance
// config (Ansible toolchains / operator_toolchains), which reaches the operator as the JSON of its environment variable
// TOOLCHAINS. An Agent names them in spec.toolchain.custom. Only the administrator writes either: never the repository
// or issue text.
//
// A custom toolchain is declarative: pinned downloads (an archive, a single file or a Debian package, each with a
// SHA-256 per architecture), PATH entries, verify commands, environment and allow entries. There are no free-form
// install commands. The init container unpacks each download with its own fixed steps (toolchain-install.sh
// install_custom) without the setup proxy credential, the same rule as for the presets (review of #537).
//
// Download hosts: every URL host and every extra host (redirect targets such as objects.githubusercontent.com) must be
// in the setup egress allow-list of the instance (SETUP_EGRESS_HOSTS, the same list as the egress-setup-proxy Squid
// ACL). A toolchain with another host is invalid, so an Agent that names it is InvalidToolchain (fail closed).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

const (
	customMaxToolchains = 20
	customMaxFiles      = 10
	customMaxVerify     = 5
	customMaxArgs       = 20
	customMaxEnv        = 20
	customMaxAllow      = 20
	customMaxHosts      = 10
)

// customFile is one pinned download of a custom toolchain. Installed below custom/<toolchain>/:
//   - tar:  an archive (gzip, xz or bzip2), unpacked into <name>/, strip leading path components;
//   - file: a single executable, installed as bin/<name>;
//   - deb:  a Debian package, unpacked into <name>/ with dpkg-deb -x (no maintainer scripts, no dependencies).
//
// Arch: amd64 and arm64, or "all" for an architecture-independent download.
type customFile struct {
	Name  string              `json:"name"`
	Kind  string              `json:"kind"`
	Strip int                 `json:"strip,omitempty"`
	Arch  map[string]download `json:"arch"`
}

// customToolchain is one entry of TOOLCHAINS. Files, Path and Verify go into the toolchain spec (and so into the key);
// Hosts, Env and Allow are the operator's only, so a new allow entry does not install again.
type customToolchain struct {
	Files  []customFile      `json:"files"`
	Path   []string          `json:"path,omitempty"`   // below custom/<toolchain>, for example "bin" or "make/usr/bin"
	Verify [][]string        `json:"verify,omitempty"` // each must exit 0 after the install
	Hosts  []string          `json:"hosts,omitempty"`  // extra download hosts (redirect targets)
	Env    map[string]string `json:"env,omitempty"`    // agent container; a leading "$TOOLCHAIN/" becomes the toolchain dir
	Allow  []string          `json:"allow,omitempty"`  // added to CLAUDE_TOOLS_EXTRA
	err    string            // why the entry is invalid; "" when valid
}

// customSpec is the part of a custom toolchain in TOOLCHAIN_SPEC (toolchainSpec.Custom).
type customSpec struct {
	Name   string       `json:"name"`
	Files  []customFile `json:"files"`
	Path   []string     `json:"path"`
	Verify [][]string   `json:"verify"`
}

var (
	customNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	customFileRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)
	customPathRE = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$`)
	customEnvRE  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	customHostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	sha256RE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	noCtrlRE     = regexp.MustCompile(`^[^\x00-\x1f\x7f]*$`)
)

// customEnvReserved: names a custom toolchain cannot set. The operator and the worker set these (proxy, Claude,
// forge, the toolchain itself), and some change what the shell or the loader runs. withCustomEnv also skips every
// name that is already in the container's env, so this list is the second check, also for names the worker scripts
// set at run time.
var (
	customEnvReserved = map[string]bool{"PATH": true, "TOOLCHAIN": true, "HOME": true, "USER": true, "SHELL": true,
		"TMPDIR": true, "LD_PRELOAD": true, "LD_AUDIT": true, "BASH_ENV": true, "ENV": true, "PROMPT_COMMAND": true,
		"HTTPS_PROXY": true, "HTTP_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true, "SSL_CERT_FILE": true,
		"SSL_CERT_DIR": true, "NODE_OPTIONS": true, "NODE_EXTRA_CA_CERTS": true, "REPO": true, "REVIEWER": true}
	customEnvReservedPrefix = []string{"CLAUDE", "ANTHROPIC", "AGENT", "EGRESS_", "FORGE", "GIT", "GH_", "GITEA", "OTEL_",
		"NPM_CONFIG_", "INFRA_", "POLL_", "IDLE_", "APPROVAL", "BOTBOARD", "TELEMETRY", "PERMISSION", "WAKE_", "MCP"}
)

func customEnvAllowed(k string) bool {
	if !customEnvRE.MatchString(k) || customEnvReserved[k] {
		return false
	}
	for _, p := range customEnvReservedPrefix {
		if strings.HasPrefix(k, p) {
			return false
		}
	}
	return true
}

// hostAllowed: h is in the allow-list, as Squid dstdomain reads it: "example.com" only that host, ".example.com" the
// domain and every subdomain.
func hostAllowed(h string, allow []string) bool {
	for _, a := range allow {
		if a == h || (strings.HasPrefix(a, ".") && (h == a[1:] || strings.HasSuffix(h, a))) {
			return true
		}
	}
	return false
}

// customURLHost: the host of a download URL; https only, no user info, port, query or fragment.
func customURLHost(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" ||
		u.Opaque != "" || len(raw) > 500 || !noCtrlRE.MatchString(raw) || strings.ContainsAny(raw, " '\"\\") {
		return "", fmt.Errorf("%q is not a plain https URL", raw)
	}
	h := u.Hostname()
	if !customHostRE.MatchString(h) {
		return "", fmt.Errorf("%q: host %q is not a DNS name", raw, h)
	}
	return h, nil
}

// checkCustom returns why a custom toolchain is invalid, "" when it is valid.
func checkCustom(c *customToolchain, allow []string) string {
	if len(c.Files) == 0 || len(c.Files) > customMaxFiles {
		return fmt.Sprintf("files: 1 to %d entries", customMaxFiles)
	}
	names := map[string]bool{}
	hosts := append([]string{}, c.Hosts...)
	for i, f := range c.Files {
		if !customFileRE.MatchString(f.Name) || f.Name == "bin" || names[f.Name] {
			return fmt.Sprintf("files[%d].name: %q is empty, not a plain file name, \"bin\" or used twice", i, f.Name)
		}
		names[f.Name] = true
		switch f.Kind {
		case "tar":
			if f.Strip < 0 || f.Strip > 5 {
				return fmt.Sprintf("files[%d].strip: 0 to 5", i)
			}
		case "file", "deb":
			if f.Strip != 0 {
				return fmt.Sprintf("files[%d].strip: only for kind tar", i)
			}
		default:
			return fmt.Sprintf("files[%d].kind: %q is not tar, file or deb", i, f.Kind)
		}
		if len(f.Arch) == 0 {
			return fmt.Sprintf("files[%d].arch: no download", i)
		}
		if _, all := f.Arch["all"]; all && len(f.Arch) > 1 {
			return fmt.Sprintf("files[%d].arch: \"all\" cannot be combined with amd64 or arm64", i)
		}
		for a, d := range f.Arch {
			if a != "amd64" && a != "arm64" && a != "all" {
				return fmt.Sprintf("files[%d].arch: %q is not amd64, arm64 or all", i, a)
			}
			if !sha256RE.MatchString(d.SHA256) {
				return fmt.Sprintf("files[%d].arch.%s.sha256: not 64 lower-case hex characters", i, a)
			}
			h, err := customURLHost(d.URL)
			if err != nil {
				return fmt.Sprintf("files[%d].arch.%s.url: %v", i, a, err)
			}
			hosts = append(hosts, h)
		}
	}
	if len(c.Hosts) > customMaxHosts {
		return fmt.Sprintf("hosts: more than %d entries", customMaxHosts)
	}
	for _, h := range hosts {
		if !customHostRE.MatchString(h) {
			return fmt.Sprintf("hosts: %q is not a DNS name", h)
		}
		if !hostAllowed(h, allow) {
			return fmt.Sprintf("host %s is not in the setup egress allow-list (SETUP_EGRESS_HOSTS)", h)
		}
	}
	if len(c.Path) > 10 {
		return "path: more than 10 entries"
	}
	for _, p := range c.Path {
		if len(p) > 200 || !customPathRE.MatchString(p) {
			return fmt.Sprintf("path: %q is not a relative path below the toolchain (no segment starts with a dot)", p)
		}
	}
	if len(c.Verify) > customMaxVerify {
		return fmt.Sprintf("verify: more than %d commands", customMaxVerify)
	}
	for i, v := range c.Verify {
		if len(v) == 0 || len(v) > customMaxArgs || v[0] == "" {
			return fmt.Sprintf("verify[%d]: 1 to %d arguments, the first one not empty", i, customMaxArgs)
		}
		for _, s := range v {
			if len(s) > 200 || !noCtrlRE.MatchString(s) {
				return fmt.Sprintf("verify[%d]: an argument is longer than 200 or has a control character", i)
			}
		}
	}
	if len(c.Env) > customMaxEnv {
		return fmt.Sprintf("env: more than %d entries", customMaxEnv)
	}
	for k, v := range c.Env {
		if !customEnvAllowed(k) {
			return fmt.Sprintf("env: %q is not an upper-case name, or agentdev sets it", k)
		}
		if len(v) > 500 || !noCtrlRE.MatchString(v) {
			return fmt.Sprintf("env.%s: longer than 500 or has a control character", k)
		}
	}
	if len(c.Allow) > customMaxAllow {
		return fmt.Sprintf("allow: more than %d entries", customMaxAllow)
	}
	for _, s := range c.Allow {
		if len(s) > 200 || !toolChars.MatchString(s) {
			return "allow: an entry is empty, longer than 200 or has a comma or a control character"
		}
	}
	return ""
}

// parseToolchains reads TOOLCHAINS (a JSON object, name -> toolchain; empty: none) against the setup egress allow-list
// (SETUP_EGRESS_HOSTS, space separated). A malformed document is an error (the operator does not start). An invalid
// entry is kept with its reason, so only the Agents that name it become InvalidToolchain.
func parseToolchains(raw, setupHosts string) (map[string]*customToolchain, error) {
	out := map[string]*customToolchain{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var m map[string]*customToolchain
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("TOOLCHAINS: %w", err)
	}
	if len(m) > customMaxToolchains {
		return nil, fmt.Errorf("TOOLCHAINS: more than %d toolchains", customMaxToolchains)
	}
	allow := strings.Fields(setupHosts)
	for name, c := range m {
		if c == nil {
			c = &customToolchain{}
		}
		if !customNameRE.MatchString(name) {
			return nil, fmt.Errorf("TOOLCHAINS: %q is not a toolchain name (lower case, digits and dashes, at most 40)", name)
		}
		if _, preset := toolchainCatalogue[name]; preset {
			c.err = "the name of a preset"
		} else {
			c.err = checkCustom(c, allow)
		}
		out[name] = c
	}
	return out, nil
}

// customsOf: the custom toolchains the Agent names, in the order of spec.toolchain.custom. A message for the first
// unknown or invalid one.
func customsOf(t *v1.AgentToolchain, cat map[string]*customToolchain) ([]string, string) {
	seen := map[string]bool{}
	for _, n := range t.Custom {
		if seen[n] {
			return nil, fmt.Sprintf("spec.toolchain.custom: %q is named twice", n)
		}
		seen[n] = true
		c, ok := cat[n]
		if !ok {
			return nil, fmt.Sprintf("spec.toolchain.custom: %q is not a toolchain of this instance (instance config toolchains)", n)
		}
		if c.err != "" {
			return nil, fmt.Sprintf("spec.toolchain.custom: toolchain %q is invalid in the instance config: %s", n, c.err)
		}
	}
	return t.Custom, ""
}

// customSpecs: the spec part of the named custom toolchains (valid ones only, see customsOf).
func customSpecs(names []string, cat map[string]*customToolchain) []customSpec {
	var out []customSpec
	for _, n := range names {
		c := cat[n]
		if c == nil || c.err != "" {
			continue
		}
		path, verify := c.Path, c.Verify
		if path == nil {
			path = []string{}
		}
		if verify == nil {
			verify = [][]string{}
		}
		out = append(out, customSpec{Name: n, Files: c.Files, Path: path, Verify: verify})
	}
	return out
}

// customDir: the directory of a custom toolchain below the toolchain's current directory.
func customDir(cur, name string) string { return cur + "/custom/" + name }

// withCustomEnv appends the variables of the Agent's custom toolchains to the agent container's env. A name that is
// already in env (set by agentdev or a preset) is skipped, so a custom toolchain never overrides one; "$TOOLCHAIN" or a
// leading "$TOOLCHAIN/" in a value becomes the toolchain directory.
func withCustomEnv(env []corev1.EnvVar, a *v1.Agent, cat map[string]*customToolchain) []corev1.EnvVar {
	if a.Spec.Toolchain == nil {
		return env
	}
	have := map[string]bool{}
	for _, e := range env {
		have[e.Name] = true
	}
	cur := toolchainDir + "/current"
	cs, _ := customsOf(a.Spec.Toolchain, cat)
	for _, n := range cs {
		c := cat[n]
		keys := make([]string, 0, len(c.Env))
		for k := range c.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if have[k] || !customEnvAllowed(k) {
				continue
			}
			have[k] = true
			v := c.Env[k]
			if v == "$TOOLCHAIN" || strings.HasPrefix(v, "$TOOLCHAIN/") {
				v = cur + strings.TrimPrefix(v, "$TOOLCHAIN")
			}
			env = append(env, corev1.EnvVar{Name: k, Value: v})
		}
	}
	return env
}
