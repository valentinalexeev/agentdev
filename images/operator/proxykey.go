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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// Proxy keys (#381): the operator writes Secret agent-<name>-proxy (ANTHROPIC_AUTH_TOKEN) from the signing key in
// Secret claude-proxy-signing (synced by the External Secrets Operator). Before, the playbook computed the same HMAC in
// shell; a GitOps install has no such step. The key format is the one images/credproxy/keys.go checks:
// <agent>.<hex HMAC-SHA256(signing, "agentdev-claude-proxy/v1:<agent>")>. The key only names the agent to the proxy
// and is worth nothing outside the cluster (docs/security.md R11).
//
// The operator reads only claude-proxy-signing (by name, uncached) and writes only agent-<name>-proxy Secrets (create,
// and get/update by name pattern; RBAC cannot narrow create by name, as for executor-egress-proxy).

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update

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
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "agent-" + a.Name + "-proxy", Namespace: a.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, s, func() error {
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		s.Labels["app"], s.Labels["agent"] = "coding-agent", a.Name
		if string(s.Data["ANTHROPIC_AUTH_TOKEN"]) != want {
			s.Data = map[string][]byte{"ANTHROPIC_AUTH_TOKEN": []byte(want)}
		}
		return controllerutil.SetControllerReference(a, s, r.Scheme())
	})
	if err != nil {
		return false, fmt.Errorf("proxy key secret: %w", err)
	}
	return true, nil
}
