package main

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
)

// #173: one claim per agent on the agent StorageClass, Claude state as a subPath of it
func TestOneStateVolumePerAgent(t *testing.T) {
	r, c := setup(t, agent("olga", nil))
	r.D.StorageClass, r.D.StateStorage = "agentdev-agent", "12Gi"
	reconcile(t, r, "olga")
	s := getSTS(t, c, "olga")
	v := s.Spec.VolumeClaimTemplates
	if len(v) != 1 || v[0].Name != "state" || ptr.Deref(v[0].Spec.StorageClassName, "") != "agentdev-agent" ||
		!v[0].Spec.Resources.Requests.Storage().Equal(resource.MustParse("12Gi")) {
		t.Fatalf("claims: %+v", v)
	}
	var claude *corev1.VolumeMount
	for i, m := range s.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name != "state" && m.Name != "usage" {
			t.Errorf("unexpected volume %q", m.Name)
		}
		if m.MountPath == "/home/node/.claude" {
			claude = &s.Spec.Template.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if claude == nil || claude.Name != "state" || claude.SubPath != "claude" {
		t.Errorf("claude mount: %+v", claude)
	}
	if ic := s.Spec.Template.Spec.InitContainers; len(ic) != 1 || ic[0].Command[len(ic[0].Command)-1] != "/state/claude" {
		t.Errorf("init container: %+v", ic)
	}
}

func TestDefaultStorageClassAndSize(t *testing.T) {
	v := Defaults{}.claimTemplates()
	if len(v) != 1 || v[0].Spec.StorageClassName != nil || !v[0].Spec.Resources.Requests.Storage().Equal(resource.MustParse("10Gi")) {
		t.Fatalf("defaults: %+v", v)
	}
}

func pvcNamed(name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
}

// an rc StatefulSet with two claims is deleted with its claims, then created again in the new layout
func TestOldLayoutIsRecreated(t *testing.T) {
	a := agent("rita", nil)
	old := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "agent-rita", Namespace: ns}}
	pvc := func(n string) corev1.PersistentVolumeClaim {
		return corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: n}}
	}
	old.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{pvc("state"), pvc("claude")}
	r, c := setup(t, a, old, pvcNamed("state-agent-rita-0"), pvcNamed("claude-agent-rita-0"))
	res, err := r.Reconcile(context.Background(), reqOf("rita"))
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("first pass: %v %v", res, err)
	}
	for _, n := range []string{"state-agent-rita-0", "claude-agent-rita-0"} {
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: n}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s not deleted: %v", n, err)
		}
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-rita"}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old statefulset kept: %v", err)
	}
	reconcile(t, r, "rita")
	if v := getSTS(t, c, "rita").Spec.VolumeClaimTemplates; len(v) != 1 || v[0].Name != "state" {
		t.Fatalf("new layout: %+v", v)
	}
	// stable: a second reconcile does not delete it again
	res, err = r.Reconcile(context.Background(), reqOf("rita"))
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("not stable: %v %v", res, err)
	}
}

// another StorageClass is another layout; another size is not (claims grow in place)
func TestLayoutComparison(t *testing.T) {
	want := Defaults{StorageClass: "agentdev-agent"}.claimTemplates()
	if !sameLayout(Defaults{StorageClass: "agentdev-agent", StateStorage: "20Gi"}.claimTemplates(), want) {
		t.Error("size change must not recreate")
	}
	if sameLayout(Defaults{}.claimTemplates(), want) {
		t.Error("StorageClass change must recreate")
	}
}

// the new StatefulSet is not created while the old state claim is still being deleted
func TestWaitsForOldClaim(t *testing.T) {
	p := pvcNamed("state-agent-sam-0")
	p.Finalizers = []string{"kubernetes.io/pvc-protection"}
	r, c := setup(t, agent("sam", nil), p)
	if err := c.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(context.Background(), reqOf("sam"))
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("did not wait: %v %v", res, err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "agent-sam"}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("created too early: %v", err)
	}
}

func reqOf(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}
