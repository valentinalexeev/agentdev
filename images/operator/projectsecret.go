package main

// Scoped secrets store (#202, ADR 0003 D1/D2). For each ProjectSecret the operator keeps one ExternalSecret
// (External Secrets Operator) that syncs the remote key agentdev-ps-<name> from the instance store into the Secret
// ps-<name>, and mirrors the sync state into the ProjectSecret's status. The value itself never passes through the
// operator: it reads no Secret and the ExternalSecret only names the remote key.
//
// The store is the namespaced SecretStore SECRETS_STORE in the agents namespace (manifests/secrets-store-*.yaml.tmpl):
// GCP Secret Manager on GKE, a Kubernetes Secret store over the namespace agentdev-secret-values on kind. Empty
// SECRETS_STORE: the feature is off and the ExternalSecret kind is not watched (its CRD may be missing).

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups=agentdev.dev,resources=projectsecrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentdev.dev,resources=projectsecrets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=external-secrets.io,resources=externalsecrets,verbs=get;list;watch;create;update;patch;delete

var externalSecretGVK = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"}

const (
	// ProjectSecretPrefix names the synced Secret ps-<name>; CEL in manifests/secrets-store-*.yaml.tmpl reserves the
	// prefix for External Secrets Operator.
	ProjectSecretPrefix = "ps-"
	// RemoteKeyPrefix names the value in the store: agentdev-ps-<name>. The API writes exactly this key.
	RemoteKeyPrefix = "agentdev-ps-"
	// ProjectSecretKey is the only key of ps-<name>.
	ProjectSecretKey = "value"
)

// ProjectSecretReconciler keeps ExternalSecret ps-<name> per ProjectSecret.
type ProjectSecretReconciler struct {
	client.Client
	NS      string        // agents namespace: ProjectSecrets, ExternalSecrets and ps-* Secrets live here
	Store   string        // SecretStore name
	Refresh time.Duration // ESO refresh interval; a rotated value arrives within it
	// Property is the key inside the remote value: "value" for the Kubernetes provider (kind: the value Secret has one
	// key, value), "" for GCP Secret Manager (the payload is the value itself)
	Property string
}

// desiredExternalSecret is the ExternalSecret for one ProjectSecret. creationPolicy Owner: ESO owns ps-<name> and
// deletes it with the ExternalSecret; deletionPolicy Delete: a value removed from the store removes the Secret.
func (r *ProjectSecretReconciler) desiredExternalSecret(ps *v1.ProjectSecret) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(externalSecretGVK)
	u.SetNamespace(ps.Namespace)
	u.SetName(ProjectSecretPrefix + ps.Name)
	u.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "agentdev-operator", "agentdev.dev/projectsecret": ps.Name})
	u.Object["spec"] = map[string]any{
		"refreshInterval": r.Refresh.String(),
		"secretStoreRef":  map[string]any{"kind": "SecretStore", "name": r.Store},
		"target": map[string]any{
			"name":           ProjectSecretPrefix + ps.Name,
			"creationPolicy": "Owner",
			"deletionPolicy": "Delete",
			"template": map[string]any{
				"type":     "Opaque",
				"metadata": map[string]any{"labels": map[string]any{"agentdev.dev/projectsecret": ps.Name}},
			},
		},
		"data": []any{map[string]any{
			"secretKey": ProjectSecretKey,
			"remoteRef": r.remoteRef(ps.Name),
		}},
	}
	return u
}

func (r *ProjectSecretReconciler) remoteRef(name string) map[string]any {
	ref := map[string]any{"key": RemoteKeyPrefix + name}
	if r.Property != "" {
		ref["property"] = r.Property
	}
	return ref
}

// esStatus reads phase and message from the ExternalSecret's Ready condition.
func esStatus(es *unstructured.Unstructured) (phase, msg string, synced *metav1.Time) {
	conds, _, _ := unstructured.NestedSlice(es.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != "Ready" {
			continue
		}
		msg, _ = m["message"].(string)
		if s, _, _ := unstructured.NestedString(es.Object, "status", "refreshTime"); s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				synced = &metav1.Time{Time: t}
			}
		}
		switch m["status"] {
		case "True":
			return "ready", clipMsg(msg), synced
		case "False":
			// SecretSyncedError with a missing remote key means: no value set yet
			if r, _ := m["reason"].(string); r == "SecretSyncedError" {
				return "pending", clipMsg(msg), synced
			}
			return "error", clipMsg(msg), synced
		}
	}
	return "pending", "waiting for External Secrets Operator", nil
}

func clipMsg(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func (r *ProjectSecretReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ps v1.ProjectSecret
	if err := r.Get(ctx, req.NamespacedName, &ps); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err) // deleted: the owner reference removes the ExternalSecret
	}
	want := r.desiredExternalSecret(&ps)
	if err := controllerutil.SetControllerReference(&ps, want, r.Scheme()); err != nil {
		return ctrl.Result{}, err
	}
	have := &unstructured.Unstructured{}
	have.SetGroupVersionKind(externalSecretGVK)
	err := r.Get(ctx, client.ObjectKeyFromObject(want), have)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, want); err != nil {
			return ctrl.Result{}, fmt.Errorf("create ExternalSecret: %w", err)
		}
		have = want
	case err != nil:
		return ctrl.Result{}, err
	default:
		// an ExternalSecret of the same name not owned by this ProjectSecret is never taken over
		if !metav1.IsControlledBy(have, &ps) {
			return r.setStatus(ctx, &ps, "error", "ExternalSecret "+want.GetName()+" exists and is not owned by this ProjectSecret", nil)
		}
		have.Object["spec"] = want.Object["spec"]
		have.SetLabels(want.GetLabels())
		if err := r.Update(ctx, have); err != nil {
			return ctrl.Result{}, fmt.Errorf("update ExternalSecret: %w", err)
		}
	}
	phase, msg, synced := esStatus(have)
	return r.setStatus(ctx, &ps, phase, msg, synced)
}

func (r *ProjectSecretReconciler) setStatus(ctx context.Context, ps *v1.ProjectSecret, phase, msg string, synced *metav1.Time) (ctrl.Result, error) {
	st := v1.ProjectSecretStatus{Phase: phase, Message: msg, SecretName: ProjectSecretPrefix + ps.Name, SyncedAt: synced, ObservedGeneration: ps.Generation}
	if ps.Status.Phase != st.Phase || ps.Status.Message != st.Message || ps.Status.ObservedGeneration != st.ObservedGeneration ||
		ps.Status.SecretName != st.SecretName || !synced.Equal(ps.Status.SyncedAt) {
		ps.Status = st
		if err := r.Status().Update(ctx, ps); err != nil {
			log.FromContext(ctx).Error(err, "projectsecret status")
			return ctrl.Result{}, err
		}
	}
	if phase != "ready" {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *ProjectSecretReconciler) SetupWithManager(mgr ctrl.Manager) error {
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(externalSecretGVK)
	return ctrl.NewControllerManagedBy(mgr).For(&v1.ProjectSecret{}).Owns(es).Named("projectsecret").Complete(r)
}
