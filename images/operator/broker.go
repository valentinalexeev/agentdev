package main

// Credential broker wiring (#202, ADR 0003 D2 mode broker). The broker is the credproxy image with MODE=broker
// (Deployment credential-broker, manifests/credential-broker.yaml.tmpl). The operator keeps two things of it current:
//
//   - ConfigMap credential-broker-routes, key routes.json: for every ProjectSecret in mode broker its upstream, header,
//     scheme and the agents allowed to use it (scope agent: that agent; scope project: every Agent whose spec.repo is
//     the project). The broker re-reads it on every request.
//   - the projected volume "values" of the Deployment: one optional source per broker secret, ps-<name> key value as
//     file <name>. A new or removed secret changes the pod template (one restart of the broker); a rotated value
//     arrives in the mounted file without one.
//
// The operator never reads a ps-* Secret: it only names them.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/valentinalexeev/agentdev/images/operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;update,resourceNames=credential-broker-routes
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;update;patch,resourceNames=credential-broker

const (
	brokerName      = "credential-broker"
	brokerRoutesCM  = "credential-broker-routes"
	brokerRoutesKey = "routes.json"
	brokerValuesVol = "values"
)

type brokerRoute struct {
	Upstream string   `json:"upstream"`
	Header   string   `json:"header"`
	Scheme   string   `json:"scheme"`
	Agents   []string `json:"agents"`
}

// allowedAgents: the agents that may use a ProjectSecret, sorted.
func allowedAgents(ps *v1.ProjectSecret, agents []v1.Agent) []string {
	out := []string{}
	for _, a := range agents {
		if (ps.Spec.Scope == "agent" && a.Name == ps.Spec.Agent) || (ps.Spec.Scope == "project" && a.Spec.Repo == ps.Spec.Project) {
			out = append(out, a.Name)
		}
	}
	sort.Strings(out)
	return out
}

// usableBy: the ProjectSecret applies to agent a.
func usableBy(ps *v1.ProjectSecret, a *v1.Agent) bool {
	return (ps.Spec.Scope == "agent" && ps.Spec.Agent == a.Name) || (ps.Spec.Scope == "project" && ps.Spec.Project == a.Spec.Repo)
}

// brokerRoutes renders routes.json and the value sources for all broker secrets.
func brokerRoutes(secrets []v1.ProjectSecret, agents []v1.Agent) (string, []corev1.VolumeProjection) {
	routes := map[string]brokerRoute{}
	var names []string
	for i := range secrets {
		ps := &secrets[i]
		if ps.Spec.Mode != "broker" || ps.Spec.Broker == nil || !ps.DeletionTimestamp.IsZero() {
			continue
		}
		h, s := ps.Spec.Broker.Header, ps.Spec.Broker.Scheme
		if h == "" {
			h = "Authorization"
		}
		if s == "" {
			s = "bearer"
		}
		routes[ps.Name] = brokerRoute{Upstream: ps.Spec.Broker.Upstream, Header: h, Scheme: s, Agents: allowedAgents(ps, agents)}
		names = append(names, ps.Name)
	}
	sort.Strings(names)
	src := []corev1.VolumeProjection{}
	for _, n := range names {
		src = append(src, corev1.VolumeProjection{Secret: &corev1.SecretProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: ProjectSecretPrefix + n}, Optional: ptr.To(true),
			Items: []corev1.KeyToPath{{Key: ProjectSecretKey, Path: n}}}})
	}
	b, _ := json.Marshal(routes) // map keys are sorted: same input, same text
	return string(b), src
}

// BrokerSync is a manager.Runnable that keeps the broker's routes and value volume current.
type BrokerSync struct {
	Client client.Client
	// Direct reads the ConfigMap and the Deployment uncached: the Role grants get on these names only, not list/watch
	Direct client.Reader
	NS     string
	Every  time.Duration
}

func (s *BrokerSync) Start(ctx context.Context) error {
	t := time.NewTicker(s.Every)
	defer t.Stop()
	for {
		if err := s.Sync(ctx); err != nil && ctx.Err() == nil {
			log.FromContext(ctx).Error(err, "credential broker sync")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (s *BrokerSync) Sync(ctx context.Context) error {
	var pl v1.ProjectSecretList
	if err := s.Client.List(ctx, &pl, client.InNamespace(s.NS)); err != nil {
		return err
	}
	var al v1.AgentList
	if err := s.Client.List(ctx, &al, client.InNamespace(s.NS)); err != nil {
		return err
	}
	routes, sources := brokerRoutes(pl.Items, al.Items)

	var cm corev1.ConfigMap
	err := s.Direct.Get(ctx, types.NamespacedName{Namespace: s.NS, Name: brokerRoutesCM}, &cm)
	switch {
	case apierrors.IsNotFound(err):
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: brokerRoutesCM, Namespace: s.NS,
			Labels: map[string]string{"app.kubernetes.io/part-of": "agentdev"}}, Data: map[string]string{brokerRoutesKey: routes}}
		if err := s.Client.Create(ctx, &cm); err != nil {
			return err
		}
	case err != nil:
		return err
	case cm.Data[brokerRoutesKey] != routes:
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[brokerRoutesKey] = routes
		if err := s.Client.Update(ctx, &cm); err != nil {
			return err
		}
	}

	var dep appsv1.Deployment
	if err := s.Direct.Get(ctx, types.NamespacedName{Namespace: s.NS, Name: brokerName}, &dep); err != nil {
		return client.IgnoreNotFound(err) // broker not deployed: routes only
	}
	vols := dep.Spec.Template.Spec.Volumes
	for i := range vols {
		if vols[i].Name != brokerValuesVol || vols[i].Projected == nil {
			continue
		}
		if equality.Semantic.DeepEqual(vols[i].Projected.Sources, sources) ||
			(len(vols[i].Projected.Sources) == 0 && len(sources) == 0) {
			return nil
		}
		vols[i].Projected.Sources = sources
		log.FromContext(ctx).Info("credential broker secrets changed", "count", len(sources))
		return s.Client.Update(ctx, &dep)
	}
	return nil
}

// brokerEnv: the agent container calls the broker with its proxy key (the same per-agent key as for claude-proxy;
// worth nothing outside the cluster). Nothing without a broker URL.
func (r *AgentReconciler) brokerEnv(n string) []corev1.EnvVar {
	if r.D.BrokerURL == "" {
		return nil
	}
	return []corev1.EnvVar{{Name: "AGENTDEV_BROKER_URL", Value: r.D.BrokerURL},
		{Name: "AGENTDEV_BROKER_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "agent-" + n + "-proxy"}, Key: "ANTHROPIC_AUTH_TOKEN", Optional: ptr.To(true)}}}}
}

// secretEnv: ProjectSecrets in mode env for this agent become variables of the agent container (explicit opt-in,
// riskAccepted, ADR 0003 D2). Optional: a value not synced yet never blocks the pod. Sorted by variable name; when two
// secrets name the same variable the first by name wins and the other is left out.
func secretEnv(a *v1.Agent, secrets []v1.ProjectSecret) []corev1.EnvVar {
	sorted := append([]v1.ProjectSecret(nil), secrets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	seen := map[string]bool{}
	var out []corev1.EnvVar
	for i := range sorted {
		ps := &sorted[i]
		if ps.Spec.Mode != "env" || ps.Spec.Env == nil || !ps.Spec.Env.RiskAccepted || !usableBy(ps, a) || seen[ps.Spec.Env.Name] {
			continue
		}
		seen[ps.Spec.Env.Name] = true
		out = append(out, corev1.EnvVar{Name: ps.Spec.Env.Name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: ProjectSecretPrefix + ps.Name}, Key: ProjectSecretKey, Optional: ptr.To(true)}}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// secretsFor lists the ProjectSecrets that apply to agent a; none when the store is off.
func (r *AgentReconciler) secretsFor(ctx context.Context, a *v1.Agent) ([]v1.ProjectSecret, error) {
	if !r.D.Secrets {
		return nil, nil
	}
	var l v1.ProjectSecretList
	if err := r.List(ctx, &l, client.InNamespace(a.Namespace)); err != nil {
		return nil, err
	}
	var out []v1.ProjectSecret
	for _, ps := range l.Items {
		if usableBy(&ps, a) && ps.DeletionTimestamp.IsZero() {
			out = append(out, ps)
		}
	}
	return out, nil
}

// agentsOfSecret: the Agents a ProjectSecret applies to.
func (r *AgentReconciler) agentsOfSecret(ctx context.Context, o client.Object) []ctrlreconcile.Request {
	ps, ok := o.(*v1.ProjectSecret)
	if !ok {
		return nil
	}
	var l v1.AgentList
	if err := r.List(ctx, &l, client.InNamespace(ps.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "cannot list agents for a ProjectSecret change", "projectsecret", ps.Name)
		return nil
	}
	var out []ctrlreconcile.Request
	for i := range l.Items {
		if usableBy(ps, &l.Items[i]) {
			out = append(out, ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: l.Items[i].Namespace, Name: l.Items[i].Name}})
		}
	}
	return out
}

// secretsGuide tells the agent which broker secrets it may use and how (harness guide, #206). Names and upstream hosts
// only, never a value; at most guideMaxExecs lines.
func (r *AgentReconciler) secretsGuide(ctx context.Context, a *v1.Agent) string {
	if r.D.BrokerURL == "" || !r.D.Secrets {
		return ""
	}
	l := v1.ProjectSecretList{}
	if err := r.List(ctx, &l, client.InNamespace(a.Namespace)); err != nil {
		return ""
	}
	var lines []string
	for i := range l.Items {
		ps := &l.Items[i]
		if ps.Spec.Mode != "broker" || ps.Spec.Broker == nil || !usableBy(ps, a) || !guideWordRe.MatchString(ps.Name) {
			continue
		}
		u, err := url.Parse(ps.Spec.Broker.Upstream)
		if err != nil || !guideHostRe.MatchString(u.Hostname()) {
			continue
		}
		lines = append(lines, fmt.Sprintf("  - %s: %s/s/%s/ in place of https://%s%s", ps.Name, r.D.BrokerURL, ps.Name, u.Host, u.EscapedPath()))
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Strings(lines)
	if len(lines) > guideMaxExecs {
		lines = lines[:guideMaxExecs]
	}
	return "- Credentials: use these base URLs; the broker adds the credential. Authenticate with $AGENTDEV_BROKER_KEY " +
		"(Bearer token, or the password of basic auth). You never see the credential itself:\n" + strings.Join(lines, "\n") + "\n"
}

// redactDir: where the forge sidecar finds the mode-env values it removes from approval text (#202).
const redactDir = "/var/run/agentdev/redact"

// redactSources: one optional projected source per mode-env ProjectSecret of the agent, same order as secretEnv.
func redactSources(a *v1.Agent, secrets []v1.ProjectSecret) []corev1.VolumeProjection {
	var out []corev1.VolumeProjection
	for _, e := range secretEnv(a, secrets) {
		ref := e.ValueFrom.SecretKeyRef
		out = append(out, corev1.VolumeProjection{Secret: &corev1.SecretProjection{LocalObjectReference: ref.LocalObjectReference,
			Optional: ptr.To(true), Items: []corev1.KeyToPath{{Key: ProjectSecretKey, Path: ref.Name}}}})
	}
	return out
}
