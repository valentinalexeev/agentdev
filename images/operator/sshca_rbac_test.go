package main

import (
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// R133 (review of #264): the operator's service account must not be able to get the CA keys (Secret step-ca). Renders
// manifests/operator.yaml.tmpl and manifests/step-ca.yaml.tmpl as deploy.yml and the kind role do, evaluates every
// Role and ClusterRole bound to agentdev-operator, and checks that step-ca is not readable in its namespace, while the
// provisioner Secret is (by name only), and that the CA does not run in EXEC_NS (where the operator has an unnamed get).
func TestOperatorCannotReadCAKeys(t *testing.T) {
	const platformNS, agentsNS, execNS, caNS = "agentdev-platform", "coding-agents", "agentdev-executors", "agentdev-ssh-ca"
	vals := map[string]string{"AGENTS_NS": agentsNS, "EXEC_NS": execNS, "PLATFORM_NS": platformNS, "SSH_CA_NS": caNS,
		"STEP_CA_IMAGE": "img"}
	docs := append(renderTmpl(t, "../../manifests/operator.yaml.tmpl", vals), renderTmpl(t, "../../manifests/step-ca.yaml.tmpl", vals)...)
	sa := rbacv1.Subject{Kind: "ServiceAccount", Name: "agentdev-operator", Namespace: platformNS}
	roles := map[string][]rbacv1.PolicyRule{} // "ns/name" ("/name" for ClusterRoles)
	type binding struct{ ns, ref string }
	var bound []binding
	caNamespaces := []string{}
	for _, d := range docs {
		var m struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name, Namespace string
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(d), &m); err != nil {
			t.Fatal(err)
		}
		switch m.Kind {
		case "Role", "ClusterRole":
			var r rbacv1.Role
			if err := yaml.Unmarshal([]byte(d), &r); err != nil {
				t.Fatal(err)
			}
			roles[m.Metadata.Namespace+"/"+m.Metadata.Name] = r.Rules
		case "RoleBinding", "ClusterRoleBinding":
			var b rbacv1.RoleBinding
			if err := yaml.Unmarshal([]byte(d), &b); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(b.Subjects, sa) {
				continue
			}
			ref := "/" + b.RoleRef.Name
			if b.RoleRef.Kind == "Role" {
				ref = m.Metadata.Namespace + ref
			}
			bound = append(bound, binding{m.Metadata.Namespace, ref}) // ns "" for a ClusterRoleBinding: everywhere
		case "Deployment":
			if m.Metadata.Name == "step-ca" {
				caNamespaces = append(caNamespaces, m.Metadata.Namespace)
			}
		}
	}
	if !slices.Equal(caNamespaces, []string{caNS}) {
		t.Fatalf("step-ca runs in %v, want only %s", caNamespaces, caNS)
	}
	can := func(ns, name string) bool {
		for _, b := range bound {
			if b.ns != "" && b.ns != ns {
				continue
			}
			rules, ok := roles[b.ref]
			if !ok {
				t.Fatalf("binding to unknown role %s", b.ref)
			}
			for _, r := range rules {
				if (slices.Contains(r.APIGroups, "") || slices.Contains(r.APIGroups, "*")) &&
					(slices.Contains(r.Resources, "secrets") || slices.Contains(r.Resources, "*")) &&
					(slices.Contains(r.Verbs, "get") || slices.Contains(r.Verbs, "*")) &&
					(len(r.ResourceNames) == 0 || slices.Contains(r.ResourceNames, name)) {
					return true
				}
			}
		}
		return false
	}
	if can(caNS, "step-ca") {
		t.Error("the operator can get Secret step-ca (the CA keys)")
	}
	if !can(caNS, "step-ca-provisioner") {
		t.Error("the operator cannot get Secret step-ca-provisioner")
	}
	if can(caNS, "other") {
		t.Error("the operator can get other Secrets in the CA namespace")
	}
	if !can(execNS, "exec-x-forge") {
		t.Error("the operator lost its get on executor Secrets (exec-<name>-forge)")
	}
}
