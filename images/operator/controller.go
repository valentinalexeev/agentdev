package main

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// Defaults is the cluster-wide part of an agent (what 06-agents.sh takes from config.env).
type Defaults struct {
	WorkerImage, PullPolicy, ForgeURL, ForgePublicURL, Reviewer, BotboardURL string
	PollSeconds, IdleSeconds                                                 string
}

type AgentReconciler struct {
	client.Client
	D Defaults
}

// +kubebuilder:rbac:groups=agentdev.dev,resources=agents;agents/status,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

// Reconcile makes the agent's Service and StatefulSet match its spec. Children carry an owner reference, so deleting
// the Agent removes them (Kubernetes garbage collection); the state volume is kept (StatefulSet PVC retention) so a
// re-created agent resumes, as with the scripts. Replicas: paused or blocked = 0; with scale-to-zero the router owns
// replicas once the agent exists, so the operator only sets them on creation.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	var a v1.Agent
	if err := r.Get(ctx, req.NamespacedName, &a); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agent-" + a.Name, Namespace: a.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Spec.Selector = map[string]string{"agent": a.Name}
		svc.Spec.Ports = []corev1.ServicePort{{Name: "webhook", Port: 8080, TargetPort: intstr.FromString("webhook")}}
		return controllerutil.SetControllerReference(&a, svc, r.Scheme())
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("service: %w", err)
	}

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "agent-" + a.Name, Namespace: a.Namespace}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		blocked := sts.Annotations["agentdev/blocked"] == "true"
		creating := sts.ResourceVersion == "" // not yet in the API server
		current := sts.Spec.Replicas          // what the router or a previous reconcile set
		r.render(&a, sts)
		switch {
		case a.Spec.Paused || blocked:
			sts.Spec.Replicas = ptr.To[int32](0)
		case creating || !a.Spec.ScaleToZero || current == nil:
			sts.Spec.Replicas = ptr.To[int32](1)
		default: // scale-to-zero and existing: the router owns replicas
			sts.Spec.Replicas = current
		}
		return controllerutil.SetControllerReference(&a, sts, r.Scheme())
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("statefulset: %w", err)
	}
	if op != controllerutil.OperationResultNone {
		l.Info("statefulset "+string(op), "agent", a.Name)
	}

	return ctrl.Result{}, r.updateStatus(ctx, &a, sts)
}

func (r *AgentReconciler) updateStatus(ctx context.Context, a *v1.Agent, sts *appsv1.StatefulSet) error {
	want := int32(0)
	if sts.Spec.Replicas != nil {
		want = *sts.Spec.Replicas
	}
	s := v1.AgentStatus{Ready: sts.Status.ReadyReplicas, ObservedGeneration: a.Generation, Conditions: a.Status.Conditions}
	switch {
	case sts.Annotations["agentdev/blocked"] == "true":
		s.Phase = "blocked"
	case a.Spec.Paused:
		s.Phase = "paused"
	case want == 0:
		s.Phase = "stopped"
	case sts.Status.ReadyReplicas >= want:
		s.Phase = "running"
	default:
		s.Phase = "starting"
	}
	meta.SetStatusCondition(&s.Conditions, metav1.Condition{Type: "Reconciled", Status: metav1.ConditionTrue,
		Reason: "Applied", Message: "service and statefulset match the spec", ObservedGeneration: a.Generation})
	if equalStatus(a.Status, s) {
		return nil
	}
	a.Status = s
	if err := r.Status().Update(ctx, a); err != nil && !apierrors.IsConflict(err) {
		return err
	}
	return nil
}

func equalStatus(x, y v1.AgentStatus) bool {
	return x.Phase == y.Phase && x.Ready == y.Ready && x.ObservedGeneration == y.ObservedGeneration && len(x.Conditions) == len(y.Conditions)
}

func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1.Agent{}).Owns(&appsv1.StatefulSet{}).Owns(&corev1.Service{}).
		Named("agent").Complete(r)
}
