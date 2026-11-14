package main

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// Defaults is the cluster-wide part of an agent (what 06-agents.sh takes from config.env).
type Defaults struct {
	WorkerImage, ForgeImage, PullPolicy, ForgeURL, ForgePublicURL, Reviewer, BotboardURL string
	GitHubURL                                                                            string // FORGE_URL of forge: github agents (#2)
	PollSeconds, IdleSeconds                                                             string
	EgressProxy                                                                          string // http://egress-proxy...:3128, empty = none (#65)
	// approvals (#67): on/off, the API's internal URL the sidecar asks, and the timeout in minutes
	Approvals, ApprovalsURL, ApprovalTimeout string
	// ClaudeProxyURL (#66): when set, the agent container gets no Claude credential; it calls this URL as
	// ANTHROPIC_BASE_URL with its per-agent key (Secret agent-<name>-proxy). Empty: the credential Secret is mounted
	// as before (deprecated rollback path).
	ClaudeProxyURL string
	// CostLabels (#184): service/environment on the StatefulSet and the pod (COST_LABELS); never override own labels
	CostLabels map[string]string
	// agent volume (#173): StorageClass (empty: cluster default) and size of the one state claim
	StorageClass, StateStorage string
	// harness guide (#206): the hosts the agents' egress proxy allows (AGENT_EGRESS_HOSTS, space separated, for the text
	// only; the proxy enforces its own list) and the executors' namespace (EXEC_NS, empty: executors off)
	AgentEgressHosts, ExecNS string
	// drain before replace (#174): grace period of an agent pod in seconds (default 300) and the longest wait for
	// running jobs before an outdated pod is replaced anyway, in minutes (default 60)
	GraceSeconds, DrainMaxMinutes string
}

type AgentReconciler struct {
	client.Client
	// Direct reads PVCs uncached (the cache holds PVCs of EXEC_NS only); nil: the client
	Direct client.Reader
	D      Defaults
	// Drains: pods whose forge sidecar reported that their jobs ended (#174); nil: replace only on the timeout
	Drains *DrainSignals
	Now    func() time.Time
	wake   chan event.GenericEvent
	ns     string // agents namespace, for requeueAgent
}

// +kubebuilder:rbac:groups=agentdev.dev,resources=agents;agents/status,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=agentdev.dev,resources=projects,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch;delete

// Reconcile makes the agent's Service and StatefulSet match its spec. Children carry an owner reference, so deleting
// the Agent removes them (Kubernetes garbage collection); the state volume is kept (StatefulSet PVC retention) so a
// re-created agent resumes, as with the scripts. Replicas: paused or blocked = 0; with scale-to-zero the router owns
// replicas once the agent exists, so the operator only sets them on creation.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	var a v1.Agent
	if err := r.Get(ctx, req.NamespacedName, &a); err != nil {
		if apierrors.IsNotFound(err) { // deleted: drop its harness guide (#206)
			return ctrl.Result{}, r.writeGuide(ctx, req.Namespace, req.Name, "")
		}
		return ctrl.Result{}, err
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

	// harness guide (#206): written before the pod that mounts it. The mount is optional, so a failure here never blocks
	// the StatefulSet: it is logged and retried, and the pod starts with the security rules only meanwhile.
	guideRetry := false
	execs, err := r.executorsFor(ctx, &a)
	var proj *v1.Project
	if err == nil {
		var why string
		proj, why, err = r.projectOf(ctx, &a)
		if err == nil {
			setProjectCondition(&a, why)
		}
	}
	if err == nil {
		err = r.writeGuide(ctx, a.Namespace, a.Name, r.guideWithProject(&a, execs, proj))
	}
	if err != nil {
		l.Error(err, "harness guide not written, will retry", "agent", a.Name)
		guideRetry = true
	}

	if wait, err := r.relayout(ctx, &a); err != nil || wait {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
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
		case creating || wasPaused || !a.Spec.ScalesToZero() || current == nil:
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

	// drain before replace (#174): an outdated pod is replaced by the operator once its jobs ended (OnDelete)
	after, updated, err := r.rollout(ctx, &a, sts)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("rollout: %w", err)
	}
	if err := r.updateStatus(ctx, &a, sts, updated); err != nil {
		return ctrl.Result{}, err
	}
	if guideRetry && (after == 0 || after > 30*time.Second) {
		after = 30 * time.Second
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// relayout moves an agent to the current volume layout (#173). volumeClaimTemplates are immutable, so a StatefulSet
// in another layout (the two claims state and claude of 1.0.0-rc, or another StorageClass) is deleted with its pod
// and its old claims; the next reconcile creates it again with one new claim. The agent's local state is lost (open
// branches stay on the forge). wait: not done yet, reconcile again later.
func (r *AgentReconciler) relayout(ctx context.Context, a *v1.Agent) (wait bool, err error) {
	key := types.NamespacedName{Namespace: a.Namespace, Name: "agent-" + a.Name}
	var sts appsv1.StatefulSet
	switch err := r.Get(ctx, key, &sts); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return false, err
	case sts.DeletionTimestamp != nil:
		return true, nil
	case len(sts.Spec.VolumeClaimTemplates) == 0 || sameLayout(sts.Spec.VolumeClaimTemplates, r.D.claimTemplates()):
		return false, nil
	default:
		log.FromContext(ctx).Info("volume layout changed: recreating the statefulset and its volumes", "agent", a.Name)
		if err := r.Delete(ctx, &sts, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return false, err
		}
		for _, c := range sts.Spec.VolumeClaimTemplates {
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: c.Name + "-" + key.Name + "-0", Namespace: a.Namespace}}
			if err := r.Delete(ctx, pvc); client.IgnoreNotFound(err) != nil {
				return false, err
			}
		}
		return true, nil
	}
	// no StatefulSet: wait until the old claims are gone, so the new pod does not get a volume that is being deleted
	reader := r.Direct
	if reader == nil {
		reader = r.Client
	}
	var pvc corev1.PersistentVolumeClaim
	switch err := reader.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: "state-" + key.Name + "-0"}, &pvc); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, err
	}
	return pvc.DeletionTimestamp != nil, nil
}

func (r *AgentReconciler) updateStatus(ctx context.Context, a *v1.Agent, sts *appsv1.StatefulSet, extra ...metav1.Condition) error {
	want := int32(0)
	if sts.Spec.Replicas != nil {
		want = *sts.Spec.Replicas
	}
	// a copy: equalStatus compares against a.Status, so the new conditions must not be written into it first
	s := v1.AgentStatus{Ready: sts.Status.ReadyReplicas, ObservedGeneration: a.Generation,
		Conditions: append([]metav1.Condition(nil), a.Status.Conditions...)}
	for _, c := range extra {
		meta.SetStatusCondition(&s.Conditions, c)
	}
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

// requeueAgent asks the controller to reconcile one agent now (the collector saw its drain report, #174).
func (r *AgentReconciler) requeueAgent(agent string) {
	if r.wake == nil {
		return
	}
	select {
	case r.wake <- event.GenericEvent{Object: &v1.Agent{ObjectMeta: metav1.ObjectMeta{Name: agent, Namespace: r.ns}}}:
	default: // full: the periodic requeue of a draining agent picks it up
	}
}

func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.wake = make(chan event.GenericEvent, 64)
	b := ctrl.NewControllerManagedBy(mgr).For(&v1.Agent{}).Owns(&appsv1.StatefulSet{}).Owns(&corev1.Service{}).
		WatchesRawSource(source.Channel(r.wake, &handler.EnqueueRequestForObject{}))
	if r.D.ExecNS != "" {
		// an Executor changes the harness guide of the agents on its repository (#206)
		b = b.Watches(&v1.Executor{}, handler.EnqueueRequestsFromMapFunc(r.agentsOfExecutor))
	}
	// a Project changes the guide and the ProjectValid condition of the agents that name it (#54)
	b = b.Watches(&v1.Project{}, handler.EnqueueRequestsFromMapFunc(r.agentsOfProject))
	return b.Named("agent").Complete(r)
}

// agentsOfProject: the Agents whose spec.project names the Project.
func (r *AgentReconciler) agentsOfProject(ctx context.Context, o client.Object) []ctrlreconcile.Request {
	var l v1.AgentList
	if err := r.List(ctx, &l, client.InNamespace(o.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "project: cannot list agents", "project", o.GetName())
		return nil
	}
	var out []ctrlreconcile.Request
	for _, a := range l.Items {
		if a.Spec.Project == o.GetName() {
			out = append(out, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}})
		}
	}
	return out
}

// setProjectCondition records whether spec.project is in use: no condition without a project, False with the reason.
func setProjectCondition(a *v1.Agent, why string) {
	switch {
	case a.Spec.Project == "":
		meta.RemoveStatusCondition(&a.Status.Conditions, "ProjectValid")
	case why != "":
		meta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{Type: "ProjectValid", Status: metav1.ConditionFalse,
			Reason: "NotInProject", Message: why, ObservedGeneration: a.Generation})
	default:
		meta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{Type: "ProjectValid", Status: metav1.ConditionTrue,
			Reason: "InProject", Message: "spec.repo is a repository of project " + a.Spec.Project, ObservedGeneration: a.Generation})
	}
}

// agentsOfExecutor: the Agents that work on the Executor's repository.
func (r *AgentReconciler) agentsOfExecutor(ctx context.Context, o client.Object) []ctrlreconcile.Request {
	e, ok := o.(*v1.Executor)
	if !ok {
		return nil
	}
	var l v1.AgentList
	if err := r.List(ctx, &l); err != nil {
		log.FromContext(ctx).Error(err, "harness guide: cannot list agents for an executor change", "executor", e.Name)
		return nil
	}
	var out []ctrlreconcile.Request
	for _, a := range l.Items {
		if a.Spec.Repo == e.Spec.Repo {
			out = append(out, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Name}})
		}
	}
	return out
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
