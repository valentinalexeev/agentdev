package main

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	WorkerImage, ForgeImage, PullPolicy, ForgeURL, ForgePublicURL, Reviewer, BotboardURL string
	PollSeconds, IdleSeconds                                                             string
	// ClaudeProxyURL (#66): when set, the agent container gets no Claude credential; it calls this URL as
	// ANTHROPIC_BASE_URL with its per-agent key (Secret agent-<name>-proxy). Empty: the credential Secret is mounted
	// as before (deprecated rollback path).
	ClaudeProxyURL string
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
	// second line of defence behind the CRD rules: never mount another agent's credentials (fail closed)
	if msg := secretsBound(&a); msg != "" {
		l.Info("refusing agent", "agent", a.Name, "reason", msg)
		meta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{Type: "Reconciled", Status: metav1.ConditionFalse,
			Reason: "SecretNotBound", Message: msg, ObservedGeneration: a.Generation})
		a.Status.Phase, a.Status.ObservedGeneration = "invalid", a.Generation
		return ctrl.Result{}, r.Status().Update(ctx, &a)
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
		wasPaused := sts.Annotations["agentdev/paused"] == "true"
		r.render(&a, sts)
		if a.Spec.Paused {
			sts.Annotations["agentdev/paused"] = "true"
		} else {
			delete(sts.Annotations, "agentdev/paused")
		}
		switch {
		case a.Spec.Paused || blocked:
			sts.Spec.Replicas = ptr.To[int32](0)
		case creating || wasPaused || !a.Spec.ScaleToZero || current == nil:
			// resume starts the agent once; with scale-to-zero it stops again when idle
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
	// a person resumed an agent the security collector paused (#70): keep the reason, mark it no longer in force
	if c := meta.FindStatusCondition(s.Conditions, "SecurityPaused"); c != nil && c.Status == metav1.ConditionTrue && !a.Spec.Paused {
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{Type: "SecurityPaused", Status: metav1.ConditionFalse,
			Reason: "Resumed", Message: "resumed after: " + c.Message, ObservedGeneration: a.Generation})
	}
	meta.SetStatusCondition(&s.Conditions, metav1.Condition{Type: "Reconciled", Status: metav1.ConditionTrue,
		Reason: "Applied", Message: "service and statefulset match the spec", ObservedGeneration: a.Generation})
	if equalStatus(a.Status, s) {
		return nil
	}
	a.Status = s
	if err := r.Status().Update(ctx, a); err != nil {
		return err // a conflict is retried by controller-runtime with backoff
	}
	return nil
}

func equalStatus(x, y v1.AgentStatus) bool {
	if x.Phase != y.Phase || x.Ready != y.Ready || x.ObservedGeneration != y.ObservedGeneration || len(x.Conditions) != len(y.Conditions) {
		return false
	}
	for _, c := range y.Conditions {
		o := meta.FindStatusCondition(x.Conditions, c.Type)
		if o == nil || o.Status != c.Status || o.Reason != c.Reason {
			return false
		}
	}
	return true
}

func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1.Agent{}).Owns(&appsv1.StatefulSet{}).Owns(&corev1.Service{}).
		Named("agent").Complete(r)
}

// secretsBound returns why the Agent's Secret references are not allowed, or "". An agent may use only its own
// token Secret (agent-<name>) and its own Claude Secret (claude-<name>) or the shared one (claude-shared).
func secretsBound(a *v1.Agent) string {
	if f := a.Spec.ForgeSecret; f != "" && f != "agent-"+a.Name {
		return fmt.Sprintf("spec.forgeSecret %q is not this agent's (agent-%s)", f, a.Name)
	}
	if c := a.Spec.ClaudeSecret; c != "claude-shared" && c != "claude-"+a.Name {
		return fmt.Sprintf("spec.claudeSecret %q is neither claude-shared nor claude-%s", c, a.Name)
	}
	return ""
}
