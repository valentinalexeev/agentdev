package main

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

func psFixture() (*ProjectSecretReconciler, client.Client, *v1.ProjectSecret) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1.AddToScheme(s)
	ps := &v1.ProjectSecret{ObjectMeta: metav1.ObjectMeta{Name: "npm", Namespace: ns, UID: "u1", Generation: 1},
		Spec: v1.ProjectSecretSpec{Scope: "project", Project: "o/r", Mode: "broker",
			Broker: &v1.ProjectSecretBroker{Upstream: "https://npm.example.com", Header: "Authorization", Scheme: "bearer"}}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ps).WithStatusSubresource(ps).Build()
	return &ProjectSecretReconciler{Client: c, NS: ns, Store: "agentdev-secrets", Refresh: 60e9}, c, ps
}

func getES(t *testing.T, c client.Client) *unstructured.Unstructured {
	t.Helper()
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(externalSecretGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "ps-npm"}, es); err != nil {
		t.Fatalf("ExternalSecret ps-npm: %v", err)
	}
	return es
}

func TestProjectSecretCreatesExternalSecret(t *testing.T) {
	r, c, ps := psFixture()
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ps)}); err != nil {
		t.Fatal(err)
	}
	es := getES(t, c)
	if !metav1.IsControlledBy(es, ps) {
		t.Fatal("ExternalSecret not owned by the ProjectSecret")
	}
	key, _, _ := unstructured.NestedString(es.Object, "spec", "secretStoreRef", "name")
	if key != "agentdev-secrets" {
		t.Fatalf("store %q", key)
	}
	data, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
	if len(data) != 1 {
		t.Fatalf("data %v", data)
	}
	rk, _, _ := unstructured.NestedString(data[0].(map[string]any), "remoteRef", "key")
	if rk != "agentdev-ps-npm" {
		t.Fatalf("remote key %q: must be agentdev-ps-<name>, the key the API writes", rk)
	}
	if tgt, _, _ := unstructured.NestedString(es.Object, "spec", "target", "name"); tgt != "ps-npm" {
		t.Fatalf("target %q", tgt)
	}
	var got v1.ProjectSecret
	_ = c.Get(ctx, client.ObjectKeyFromObject(ps), &got)
	if got.Status.Phase != "pending" || got.Status.SecretName != "ps-npm" {
		t.Fatalf("status %+v", got.Status)
	}

	// ESO reports Ready: the status follows
	_ = unstructured.SetNestedSlice(es.Object, []any{map[string]any{"type": "Ready", "status": "True", "reason": "SecretSynced", "message": "ok"}}, "status", "conditions")
	_ = unstructured.SetNestedField(es.Object, "2026-11-08T01:00:00Z", "status", "refreshTime")
	if err := c.Update(ctx, es); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ps)}); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(ps), &got)
	if got.Status.Phase != "ready" || got.Status.SyncedAt == nil {
		t.Fatalf("status %+v", got.Status)
	}
}

func TestProjectSecretNoTakeover(t *testing.T) {
	r, c, ps := psFixture()
	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(externalSecretGVK)
	foreign.SetNamespace(ns)
	foreign.SetName("ps-npm")
	foreign.Object["spec"] = map[string]any{"data": []any{map[string]any{"secretKey": "value", "remoteRef": map[string]any{"key": "something-else"}}}}
	if err := c.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ps)}); err != nil {
		t.Fatal(err)
	}
	es := getES(t, c)
	data, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
	if rk, _, _ := unstructured.NestedString(data[0].(map[string]any), "remoteRef", "key"); rk != "something-else" {
		t.Fatal("a foreign ExternalSecret was taken over")
	}
	var got v1.ProjectSecret
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(ps), &got)
	if got.Status.Phase != "error" {
		t.Fatalf("status %+v", got.Status)
	}
}

func TestESStatus(t *testing.T) {
	es := &unstructured.Unstructured{Object: map[string]any{}}
	if p, _, _ := esStatus(es); p != "pending" {
		t.Fatal(p)
	}
	_ = unstructured.SetNestedSlice(es.Object, []any{map[string]any{"type": "Ready", "status": "False", "reason": "SecretSyncedError"}}, "status", "conditions")
	if p, _, _ := esStatus(es); p != "pending" {
		t.Fatal(p)
	}
	_ = unstructured.SetNestedSlice(es.Object, []any{map[string]any{"type": "Ready", "status": "False", "reason": "SecretStoreNotReady"}}, "status", "conditions")
	if p, _, _ := esStatus(es); p != "error" {
		t.Fatal(p)
	}
}
