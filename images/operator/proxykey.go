package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// Proxy keys (#381): the operator writes Secret agent-<name>-proxy (ANTHROPIC_AUTH_TOKEN) from the signing key in
// Secret claude-proxy-signing (synced by the External Secrets Operator). Before, the playbook computed the same HMAC in
// shell; a GitOps install has no such step. The key format is the one images/credproxy/keys.go checks:
// <agent>.<hex HMAC-SHA256(signing, "agentdev-claude-proxy/v1:<agent>")>. The key only names the agent to the proxy
// and is worth nothing outside the cluster (docs/security.md R11).
//
// RBAC (review of #445): this controller gets only claude-proxy-signing (resourceNames). Note: the McpProject
// controller (#383) needs get on agent-<name>-mcp, which RBAC can only grant for all Secrets of the namespace. agent-<name>-proxy is written by a server-side apply, which needs create and patch but no get.
// RBAC cannot narrow create or patch by a name pattern: the ValidatingAdmissionPolicy agentdev-operator-secrets
// (manifests/operator.yaml.tmpl, next to the Role, so applied in both modes) lets the operator write only agent-*-proxy (and agent-*-mcp) Secrets.
//
// Two writers during 1.x: in secrets mode playbook, deploy.yml also writes agent-<name>-proxy (the same HMAC, so the
// same value and no restart). The operator sets a controller owner reference on the Secret, also on the playbook's
// one, so deleting the Agent now deletes its proxy Secret too (before, the Secret stayed until the playbook removed
// it). A later deploy.yml run writes it again for an agent that is still listed.

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get,resourceNames=claude-proxy-signing
// +kubebuilder:rbac:groups="",resources=secrets,verbs=create;patch

const (
	proxySigningSecret = "claude-proxy-signing"
	proxyKeyPrefix     = "agentdev-claude-proxy/v1:"
)

// proxyKey is the agent's key for the Claude proxy (same as credproxy agentKey).
func proxyKey(signing []byte, agent string) string {
	m := hmac.New(sha256.New, signing)
	m.Write([]byte(proxyKeyPrefix + agent))
	return agent + "." + hex.EncodeToString(m.Sum(nil))
}

// ensureProxyKey writes agent-<name>-proxy when the credential proxy is on. Without the signing Secret it does
// nothing and returns false (the pod's secretKeyRef keeps it Pending until the key exists; the playbook path of 1.1
// still writes the Secret itself). An existing Secret with the same value is left alone, so no restart follows.
func (r *AgentReconciler) ensureProxyKey(ctx context.Context, a *v1.Agent) (bool, error) {
	if r.D.ClaudeProxyURL == "" {
		return false, nil
	}
	var sig corev1.Secret
	rd := r.Direct
	if rd == nil {
		rd = r.Client
	}
	if err := rd.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: proxySigningSecret}, &sig); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("signing secret: %w", err)
	}
	signing := []byte(strings.TrimSpace(string(sig.Data["key"])))
	if len(signing) < 32 {
		return false, fmt.Errorf("secret %s: key missing or shorter than 32 bytes", proxySigningSecret)
	}
	want := proxyKey(signing, a.Name)
	// server-side apply: no read of the existing Secret; an unchanged value is a no-op (no new resourceVersion, no
	// restart). Force takes the fields over from the playbook's client-side apply.
	if err := applyAgentSecret(ctx, r.Client, a, "agent-"+a.Name+"-proxy", map[string][]byte{"ANTHROPIC_AUTH_TOKEN": []byte(want)}); err != nil {
		return false, fmt.Errorf("proxy key secret: %w", err)
	}
	return true, nil
}

// applyAgentSecret writes an Opaque Secret owned by the Agent with server-side apply (field owner agentdev-operator,
// forced), through client.Client.Apply and an apply configuration (the supported API; Patch with client.Apply is
// deprecated in controller-runtime 0.23). Only the fields set here are owned: labels app/agent, the controller
// owner reference, the type and the data.
func applyAgentSecret(ctx context.Context, c client.Client, a *v1.Agent, name string, data map[string][]byte) error {
	ac := corev1ac.Secret(name, a.Namespace).
		WithLabels(map[string]string{"app": "coding-agent", "agent": a.Name}).
		WithOwnerReferences(metav1ac.OwnerReference().
			WithAPIVersion(v1.GroupVersion.String()).WithKind("Agent").WithName(a.Name).WithUID(a.UID).
			WithController(true).WithBlockOwnerDeletion(true)).
		WithType(corev1.SecretTypeOpaque).
		WithData(data)
	return c.Apply(ctx, ac, client.FieldOwner("agentdev-operator"), client.ForceOwnership)
}
